package lifecycle

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/korjavin/zulip-gcal-service/internal/config"
	"github.com/korjavin/zulip-gcal-service/internal/store"
	"github.com/korjavin/zulip-gcal-service/internal/zulip"
)

type env struct {
	t  *testing.T
	o  *Ops
	st *store.Store

	mu       sync.Mutex
	alive    map[string]bool // refresh tokens the fake Google accepts
	refreshs int             // token endpoint calls
	revoked  []string
	revokeFn func(w http.ResponseWriter, r *http.Request) // nil = 200
	dms      []string
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, alive: map[string]bool{}}
	secrets, err := config.NewSecrets(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	e.st, err = store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.st.Close() })

	g := http.NewServeMux()
	g.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.refreshs++
		w.Header().Set("Content-Type", "application/json")
		if !e.alive[r.FormValue("refresh_token")] {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "Token has been expired or revoked."})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "token_type": "Bearer", "expires_in": 3599})
	})
	g.HandleFunc("POST /revoke", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.revoked = append(e.revoked, r.FormValue("token"))
		fn := e.revokeFn
		e.mu.Unlock()
		if fn != nil {
			fn(w, r)
		}
	})
	gs := httptest.NewServer(g)
	t.Cleanup(gs.Close)

	z := http.NewServeMux()
	z.HandleFunc("POST /api/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.dms = append(e.dms, r.FormValue("to")+" "+r.FormValue("content"))
		e.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"result": "success", "id": 1})
	})
	zs := httptest.NewServer(z)
	t.Cleanup(zs.Close)

	cfg := &config.Config{PublicURL: "https://cal.example.com", GoogleClientID: "cid", GoogleClientSecret: "cs", Secrets: secrets}
	e.o = New(cfg, e.st, zulip.New(zs.URL, "bot@example.com", "key"))
	e.o.oauth.Endpoint.TokenURL = gs.URL + "/token"
	e.o.revokeURL = gs.URL + "/revoke"
	return e
}

func (e *env) exec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.st.DB.Exec(q, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.st.DB.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// account creates a linked, connected account with a settings row, a link
// code and one reminder in each state.
func (e *env) account(id, refresh string, alive bool) {
	e.alive[refresh] = alive
	e.exec(`INSERT INTO accounts (id, google_sub, google_email, enc_refresh_token, token_rev, zulip_user_id, created_at)
		VALUES (?, ?, 'a@example.com', ?, 1, ?, 0)`, id, "sub-"+id, e.o.secrets.Encrypt([]byte(refresh)), 40+int(id[0]-'a')+2)
	e.exec(`INSERT INTO settings (account_id, lead_minutes) VALUES (?, 10)`, id)
	e.exec(`INSERT INTO link_codes (code, account_id, expires_at) VALUES (?, ?, 0)`, "C-"+id, id)
	for _, s := range []string{"pending", "sending", "sent", "expired", "skipped"} {
		e.exec(`INSERT INTO reminders (key, account_id, fire_at, event_start, payload, state, updated_at)
			VALUES (?, ?, 0, 0, '{}', ?, 0)`, id+"|"+s, id, s)
	}
}

func (e *env) rev(id string) int64 {
	var r int64
	if err := e.st.DB.QueryRow(`SELECT schedule_rev FROM accounts WHERE id = ?`, id).Scan(&r); err != nil {
		e.t.Fatal(err)
	}
	return r
}

// poll stands in for the poller: read schedule_rev, fetch a token, commit
// guarded by the rev read.
func (e *env) poll(id string) error {
	var rev int64
	e.st.DB.QueryRow(`SELECT schedule_rev FROM accounts WHERE id = ?`, id).Scan(&rev)
	ts, err := e.o.TokenSource(context.Background(), id)
	if err != nil {
		return err
	}
	if _, err := ts.Token(); err != nil {
		return err
	}
	return e.st.WithScheduleRev(context.Background(), id, rev, func(*sql.Tx) error { return nil })
}

func TestLostAccessOneDMAcrossPolls(t *testing.T) {
	e := newEnv(t)
	e.account("a", "dead", false)
	rev := e.rev("a")
	for range 2 {
		if err := e.poll("a"); !errors.Is(err, ErrNoAccess) {
			t.Fatalf("poll: %v", err)
		}
	}
	if len(e.dms) != 1 || !strings.Contains(e.dms[0], "[42] I lost access") || !strings.Contains(e.dms[0], "https://cal.example.com") {
		t.Fatalf("dms = %q", e.dms)
	}
	if e.refreshs != 1 {
		t.Fatalf("Google asked %d times; a disconnected account must not call it", e.refreshs)
	}
	if n := e.count(`SELECT count(*) FROM accounts WHERE id = 'a' AND status = 'disconnected'`); n != 1 {
		t.Fatal("not disconnected")
	}
	if e.rev("a") != rev+1 {
		t.Fatal("schedule_rev not bumped")
	}
	if n := e.count(`SELECT count(*) FROM reminders WHERE state IN ('pending', 'sending')`); n != 0 {
		t.Fatalf("%d live reminders left", n)
	}
	if n := e.count(`SELECT count(*) FROM reminders`); n != 3 {
		t.Fatalf("history rows = %d, want 3", n)
	}
	// A poll that read schedule_rev before LostAccess commits nothing.
	if err := e.st.WithScheduleRev(context.Background(), "a", rev, func(*sql.Tx) error { return nil }); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale poll: %v", err)
	}
}

func TestStaleInvalidGrantAfterReconnect(t *testing.T) {
	e := newEnv(t)
	e.account("a", "old", false)
	ts, err := e.o.TokenSource(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	// The user reconnects (new refresh token, token_rev bump) while the old
	// token source is still in use.
	e.alive["new"] = true
	e.exec(`UPDATE accounts SET enc_refresh_token = ?, token_rev = 2 WHERE id = 'a'`, e.o.secrets.Encrypt([]byte("new")))
	if _, err := ts.Token(); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("old token: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM accounts WHERE status = 'connected'`); n != 1 || len(e.dms) != 0 {
		t.Fatalf("stale failure acted: connected=%d dms=%q", n, e.dms)
	}
	// The new grant works, and its access token is cached.
	for range 2 {
		if err := e.poll("a"); err != nil {
			t.Fatal(err)
		}
	}
	if e.refreshs != 2 {
		t.Fatalf("token endpoint calls = %d, want 2 (old fail + one new refresh)", e.refreshs)
	}
}

func TestPauseResume(t *testing.T) {
	e := newEnv(t)
	e.account("a", "rt", true)
	polls := 0
	e.o.Poll = func(string) { polls++ }
	ctx := context.Background()

	rev := e.rev("a")
	if ch, err := e.o.Pause(ctx, "a"); err != nil || !ch {
		t.Fatalf("pause: %v %v", ch, err)
	}
	if e.rev("a") != rev+1 || e.count(`SELECT count(*) FROM reminders WHERE state IN ('pending', 'sending')`) != 0 {
		t.Fatal("pause did not bump/cancel")
	}
	e.exec(`INSERT INTO reminders (key, account_id, fire_at, event_start, payload, state, updated_at) VALUES ('x', 'a', 0, 0, '{}', 'pending', 0)`)
	if ch, err := e.o.Pause(ctx, "a"); err != nil || ch || e.rev("a") != rev+1 || e.count(`SELECT count(*) FROM reminders WHERE key = 'x'`) != 1 {
		t.Fatalf("repeat pause not a no-op: %v %v", ch, err)
	}
	if ch, err := e.o.Resume(ctx, "a"); err != nil || !ch || polls != 1 || e.rev("a") != rev+2 {
		t.Fatalf("resume: %v %v polls=%d", ch, err, polls)
	}
	if ch, err := e.o.Resume(ctx, "a"); err != nil || ch || polls != 1 || e.rev("a") != rev+2 {
		t.Fatalf("repeat resume not a no-op: %v %v polls=%d", ch, err, polls)
	}
	// A failing hook rolls the whole write back.
	boom := errors.New("boom")
	if _, err := e.o.Pause(ctx, "a", func(*sql.Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if e.count(`SELECT paused FROM accounts WHERE id = 'a'`) != 0 {
		t.Fatal("hook error did not roll back")
	}
}

func TestDisconnect(t *testing.T) {
	e := newEnv(t)
	e.account("a", "rt", true)
	e.account("b", "rt-b", true)
	rev := e.rev("a")
	issued, err := e.o.TokenSource(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	var atRevoke [2]int
	e.revokeFn = func(w http.ResponseWriter, r *http.Request) {
		atRevoke[0] = e.count(`SELECT count(*) FROM accounts WHERE id = 'a'`)
		atRevoke[1] = e.count(`SELECT revoking FROM tombstones WHERE google_sub = 'sub-a'`)
	}
	revoked, err := e.o.Disconnect(context.Background(), "a")
	if err != nil || !revoked {
		t.Fatalf("disconnect: %v %v", revoked, err)
	}
	if atRevoke != [2]int{0, 1} || len(e.revoked) != 1 || e.revoked[0] != "rt" {
		t.Fatalf("at revoke: rows=%v revoked=%q", atRevoke, e.revoked)
	}
	for _, tbl := range []string{"accounts", "settings", "link_codes", "reminders"} {
		if n := e.count(`SELECT count(*) FROM ` + tbl + ` WHERE ` + map[bool]string{true: "id", false: "account_id"}[tbl == "accounts"] + ` = 'a'`); n != 0 {
			t.Fatalf("%s: %d rows left", tbl, n)
		}
	}
	if e.count(`SELECT count(*) FROM reminders WHERE account_id = 'b'`) != 5 {
		t.Fatal("other account touched")
	}
	if e.count(`SELECT revoking FROM tombstones WHERE google_sub = 'sub-a'`) != 0 {
		t.Fatal("tombstone still revoking")
	}
	// A poll in flight commits nothing, and the token source is gone.
	if err := e.st.WithScheduleRev(context.Background(), "a", rev, func(*sql.Tx) error { return nil }); !errors.Is(err, store.ErrStale) {
		t.Fatalf("in-flight poll: %v", err)
	}
	if _, err := e.o.TokenSource(context.Background(), "a"); !errors.Is(err, ErrNoAccess) {
		t.Fatal(err)
	}
	// A source handed out before disconnect never refreshes again, even
	// though Google still accepts the token (revocation is best effort).
	e.mu.Lock()
	before := e.refreshs
	e.mu.Unlock()
	if _, err := issued.Token(); !errors.Is(err, ErrNoAccess) || e.refreshs != before {
		t.Fatalf("issued source after disconnect: %v", err)
	}
	if _, err := e.o.Disconnect(context.Background(), "a"); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("second disconnect: %v", err)
	}
}

func TestDisconnectRevokeTimeout(t *testing.T) {
	old := revokeTimeout
	revokeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { revokeTimeout = old })
	e := newEnv(t)
	e.account("a", "rt", true)
	e.revokeFn = func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	revoked, err := e.o.Disconnect(context.Background(), "a")
	if err != nil || revoked {
		t.Fatalf("disconnect: %v %v", revoked, err)
	}
	if e.count(`SELECT count(*) FROM accounts`) != 0 || e.count(`SELECT revoking FROM tombstones`) != 0 {
		t.Fatal("local deletion not reported done")
	}
}

func TestDisconnectHandler(t *testing.T) {
	e := newEnv(t)
	e.account("a", "rt", true)
	e.revokeFn = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadRequest) }
	mux := http.NewServeMux()
	e.o.Register(mux, func(r *http.Request) (string, bool) {
		return r.Header.Get("X-Test-Account"), r.Header.Get("X-Test-Account") != ""
	})
	do := func(acct, site string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/disconnect", nil)
		req.Header.Set("X-Test-Account", acct)
		req.Header.Set("Sec-Fetch-Site", site)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := do("a", "cross-site"); rec.Code != http.StatusForbidden || e.count(`SELECT count(*) FROM accounts`) != 1 {
		t.Fatalf("cross-site: %d", rec.Code)
	}
	if rec := do("", "same-origin"); rec.Code != http.StatusSeeOther {
		t.Fatalf("no session: %d", rec.Code)
	}
	rec := do("a", "same-origin")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "we deleted everything") ||
		!strings.Contains(rec.Body.String(), "could not confirm") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do("a", "same-origin"); rec.Code != http.StatusSeeOther {
		t.Fatalf("repeat: %d", rec.Code)
	}
}
