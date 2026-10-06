package link

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/korjavin/zulip-gcal-service/internal/config"
	"github.com/korjavin/zulip-gcal-service/internal/lifecycle"
	"github.com/korjavin/zulip-gcal-service/internal/store"
	"github.com/korjavin/zulip-gcal-service/internal/zulip"
)

type sent struct {
	to   string
	text string
}

type env struct {
	t       *testing.T
	st      *store.Store
	l       *Linker
	mu      sync.Mutex
	dms     []sent
	polled  []string
	revoked []string
	msgID   int64
}

// Fake Zulip: users 1 (a@x), 2 (b@x), 3 (c@x); hidden@x is not found.
func newEnv(t *testing.T) *env {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st}
	users := map[string]int64{"a@x": 1, "b@x": 2, "c@x": 3, "1": 1, "2": 2, "3": 3}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/users/{key}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, ok := users[r.PathValue("key")]
		if r.PathValue("key") == "down@x" {
			w.WriteHeader(502)
			return
		}
		if !ok {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"result":"error","msg":"No such user","code":"BAD_REQUEST"}`)
			return
		}
		fmt.Fprintf(w, `{"result":"success","user":{"user_id":%d,"email":"x","is_active":true,"timezone":"Pacific/Auckland"}}`, id)
	})
	mux.HandleFunc("POST /api/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.dms = append(e.dms, sent{r.FormValue("to"), r.FormValue("content")})
		e.mu.Unlock()
		fmt.Fprint(w, `{"result":"success","id":42}`)
	})
	mux.HandleFunc("POST /revoke", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.revoked = append(e.revoked, r.FormValue("token"))
		e.mu.Unlock()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	secrets, err := config.NewSecrets(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	zc := zulip.New(srv.URL, "bot@x", "key")
	ops := lifecycle.New(&config.Config{PublicURL: "https://cal.example", Secrets: secrets}, st, zc)
	ops.RevokeURL = srv.URL + "/revoke"
	ops.Poll = func(id string) { e.polled = append(e.polled, id) }
	e.l = &Linker{
		St:    st,
		Zulip: zc,
		Ops:   ops,
		Account: func(r *http.Request) (string, bool) {
			id := r.Header.Get("X-Account")
			return id, id != ""
		},
		BotHealthy: func() bool { return true },
		BotID:      100, PublicURL: "https://cal.example", ZulipSite: "https://zulip.example",
		Poll: func(id string) { e.polled = append(e.polled, id) },
	}
	return e
}

func (e *env) account(id, email string, authoritative bool) {
	if _, err := e.st.DB.Exec(`INSERT INTO accounts (id, google_sub, google_email, email_authoritative, created_at) VALUES (?, ?, ?, ?, 0)`,
		id, "sub-"+id, email, authoritative); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) get(path, account string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if account != "" {
		req.Header.Set("X-Account", account)
	}
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	e.l.Register(mux)
	mux.ServeHTTP(rec, req)
	return rec
}

func (e *env) linkedTo(id string) (zid int64, rev int) {
	e.st.DB.QueryRow(`SELECT coalesce(zulip_user_id, 0), schedule_rev FROM accounts WHERE id = ?`, id).Scan(&zid, &rev)
	return
}

func (e *env) lastDM() sent {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.dms) == 0 {
		return sent{}
	}
	return e.dms[len(e.dms)-1]
}

// dm feeds the handler a fresh message from sender.
func (e *env) dm(sender int64, text string) sent {
	e.t.Helper()
	e.msgID++
	n := len(e.dms)
	if err := e.l.HandleDM(context.Background(), zulip.Message{ID: e.msgID, SenderID: sender, Time: time.Now(), Text: text}); err != nil {
		e.t.Fatal(err)
	}
	if len(e.dms) != n+1 {
		e.t.Fatalf("%q: %d replies, want 1", text, len(e.dms)-n)
	}
	return e.lastDM()
}

var codeRe = regexp.MustCompile(`<b>([A-Z0-9]{6})</b>`)

func (e *env) code(account string) string {
	e.t.Helper()
	rec := e.get("/link", account)
	m := codeRe.FindStringSubmatch(rec.Body.String())
	if rec.Code != 200 || m == nil {
		e.t.Fatalf("code page: %d %s", rec.Code, rec.Body)
	}
	return m[1]
}

func TestAutoMatch(t *testing.T) {
	e := newEnv(t)
	e.account("A", "a@x", true)
	e.st.DB.Exec(`INSERT INTO link_codes VALUES ('OLDCDE', 'A', 9999999999)`)
	rec := e.get("/link", "A")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Connected") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if zid, rev := e.linkedTo("A"); zid != 1 || rev != 1 {
		t.Fatalf("linked to %d rev %d", zid, rev)
	}
	if d := e.lastDM(); d.to != "[1]" || !strings.Contains(d.text, "https://cal.example/settings") {
		t.Fatalf("welcome = %+v", d)
	}
	if fmt.Sprint(e.polled) != "[A]" {
		t.Fatalf("polled %v", e.polled)
	}
	var n int
	e.st.DB.QueryRow(`SELECT count(*) FROM link_codes`).Scan(&n)
	if n != 0 {
		t.Fatal("codes not deleted on link")
	}
	// Refresh: still Connected, no second welcome.
	if rec := e.get("/link", "A"); !strings.Contains(rec.Body.String(), "Connected") || len(e.dms) != 1 {
		t.Fatalf("refresh: %s, %d DMs", rec.Body, len(e.dms))
	}

	// Another Google account with the same Zulip user is rejected.
	e.account("B", "a@x", true)
	if rec := e.get("/link", "B"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Disconnect that one first") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if zid, _ := e.linkedTo("B"); zid != 0 {
		t.Fatal("B linked")
	}
}

func TestCodePage(t *testing.T) {
	e := newEnv(t)
	e.account("A", "a@x", false)     // not authoritative: no lookup
	e.account("H", "hidden@x", true) // authoritative but not found
	c1, c2 := e.code("A"), e.code("A")
	if c1 != c2 {
		t.Fatalf("two tabs got %s and %s", c1, c2)
	}
	if strings.ContainsAny(c1, "0O1IL") {
		t.Fatalf("ambiguous code %s", c1)
	}
	if zid, _ := e.linkedTo("A"); zid != 0 {
		t.Fatal("non-authoritative e-mail auto-matched")
	}
	if c := e.code("H"); c == c1 {
		t.Fatal("two accounts share a code")
	}
	body := e.get("/link", "A").Body.String()
	if !strings.Contains(body, `href="https://zulip.example/#narrow/dm/100"`) || strings.Contains(body, `id="down" class="card lost">`) {
		t.Fatalf("page = %s", body)
	}
	e.l.BotHealthy = func() bool { return false }
	if body := e.get("/link", "A").Body.String(); !strings.Contains(body, `id="down" class="card lost">`) {
		t.Fatalf("bot down not shown: %s", body)
	}

	// Zulip down and the bot too: no code that could not be delivered.
	e.account("D", "down@x", true)
	if rec := e.get("/link", "D"); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "reach Zulip right now") {
		t.Fatalf("zulip down: %d %s", rec.Code, rec.Body)
	}

	// Expired: a new code replaces it.
	e.st.DB.Exec(`UPDATE link_codes SET expires_at = 1 WHERE account_id = 'A'`)
	if c := e.code("A"); c == c1 {
		t.Fatal("expired code reused")
	}

	var s map[string]bool
	rec := e.get("/link/status", "A")
	json.Unmarshal(rec.Body.Bytes(), &s)
	if rec.Code != 200 || s["linked"] || s["bot_healthy"] {
		t.Fatalf("status %d %v", rec.Code, s)
	}
	if rec := e.get("/link/status", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status without session = %d", rec.Code)
	}
	if rec := e.get("/link/status", "nobody"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status for deleted account = %d", rec.Code)
	}
}

func TestBotCodes(t *testing.T) {
	e := newEnv(t)
	e.account("A", "x@x", false)
	e.account("B", "y@x", false)
	e.account("C", "z@x", false)
	code := e.code("A")

	if d := e.dm(1, "hello"); !strings.Contains(d.text, "sign in with Google") {
		t.Fatalf("help = %+v", d)
	}
	if d := e.dm(1, "ZZZZZZ"); !strings.Contains(d.text, "not valid or expired") {
		t.Fatalf("wrong = %+v", d)
	}
	// Valid code, lower case, linked; welcome to the sender; codes gone.
	if d := e.dm(1, strings.ToLower(code)); d.to != "[1]" || !strings.Contains(d.text, "remind you") {
		t.Fatalf("welcome = %+v", d)
	}
	if zid, rev := e.linkedTo("A"); zid != 1 || rev != 1 || fmt.Sprint(e.polled) != "[A]" {
		t.Fatalf("A linked to %d rev %d, polled %v", zid, rev, e.polled)
	}
	// Used code is gone.
	if d := e.dm(2, code); !strings.Contains(d.text, "not valid or expired") {
		t.Fatalf("reused code = %+v", d)
	}
	// Sender already linked elsewhere: rejected, B stays unlinked.
	if d := e.dm(1, e.code("B")); !strings.Contains(d.text, "already connected") {
		t.Fatalf("linked sender = %+v", d)
	}
	if zid, _ := e.linkedTo("B"); zid != 0 {
		t.Fatal("B linked")
	}
	// A code cannot relink a linked account.
	e.st.DB.Exec(`INSERT INTO link_codes VALUES ('RELINK', 'A', 9999999999)`)
	if d := e.dm(2, "RELINK"); !strings.Contains(d.text, "already connected") {
		t.Fatalf("relink = %+v", d)
	}
	if zid, _ := e.linkedTo("A"); zid != 1 {
		t.Fatal("A relinked")
	}
	// Expired code.
	c := e.code("C")
	e.st.DB.Exec(`UPDATE link_codes SET expires_at = 1`)
	if d := e.dm(3, c); !strings.Contains(d.text, "not valid or expired") {
		t.Fatalf("expired = %+v", d)
	}
	// Throttle: sender 3 has 1 wrong; 4 more, then even a valid code is refused.
	for range 4 {
		e.dm(3, "ZZZZZZ")
	}
	c = e.code("C")
	if d := e.dm(3, c); !strings.Contains(d.text, "Too many wrong codes") {
		t.Fatalf("throttled = %+v", d)
	}
	if zid, _ := e.linkedTo("C"); zid != 0 {
		t.Fatal("throttled sender linked")
	}
	// After 15 minutes the sender may try again.
	for i, ts := range e.l.wrong[3] {
		e.l.wrong[3][i] = ts.Add(-codeTTL)
	}
	if d := e.dm(3, c); !strings.Contains(d.text, "remind you") {
		t.Fatalf("after throttle = %+v", d)
	}

	// A replayed message is not handled twice.
	n := len(e.dms)
	if err := e.l.HandleDM(context.Background(), zulip.Message{ID: e.msgID, SenderID: 3, Text: "hello"}); err != nil || len(e.dms) != n {
		t.Fatalf("replay: %v, %d new DMs", err, len(e.dms)-n)
	}
}
