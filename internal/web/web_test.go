package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/korjavin/zulip-gcal-service/internal/store"
	"github.com/korjavin/zulip-gcal-service/internal/zulip"
)

var now = time.Date(2026, 10, 6, 13, 0, 0, 0, time.UTC)

type env struct {
	t       *testing.T
	st      *store.Store
	mux     *http.ServeMux
	resumed []string
}

// Fake Zulip knows user 7 (Ann Lee); everything else fails like an outage.
func newEnv(t *testing.T) *env {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	zm := http.NewServeMux()
	zm.HandleFunc("GET /api/v1/users/7", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"result":"success","user":{"user_id":7,"full_name":"Ann Lee","is_active":true}}`)
	})
	zs := httptest.NewServer(zm)
	t.Cleanup(zs.Close)
	e := &env{t: t, st: st, mux: http.NewServeMux()}
	s := &Site{St: st, Zulip: zulip.New(zs.URL, "bot@x", "key"),
		Account: func(r *http.Request) (string, bool) { id := r.Header.Get("X-Account"); return id, id != "" },
		Resume: func(_ context.Context, id string) error {
			e.resumed = append(e.resumed, id)
			_, err := st.DB.Exec(`UPDATE accounts SET paused = 0 WHERE id = ?`, id)
			return err
		},
		Now: func() time.Time { return now }}
	s.Register(e.mux)
	return e
}

func (e *env) do(method, path, account string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if account != "" {
		req.Header.Set("X-Account", account)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *env) exec(q string, args ...any) {
	if _, err := e.st.DB.Exec(q, args...); err != nil {
		e.t.Fatal(err)
	}
}

// expect GETs / as account and checks the body has every want.
func (e *env) expect(t *testing.T, account string, want ...string) string {
	t.Helper()
	rec := e.do("GET", "/", account)
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("GET / = %d %s", rec.Code, body)
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Fatalf("missing %q in\n%s", w, body)
		}
	}
	if dir := os.Getenv("WEB_SNAPSHOT_DIR"); dir != "" { // for PR screenshots
		os.WriteFile(filepath.Join(dir, strings.ReplaceAll(t.Name(), "/", "_")+".html"), rec.Body.Bytes(), 0o644)
	}
	return body
}

func TestLanding(t *testing.T) {
	e := newEnv(t)
	e.expect(t, "", `href="/login"`, "Sign in with Google", "Read-only access. You can disconnect anytime.")
	e.expect(t, "gone", "Sign in with Google") // session of a deleted account
	if rec := e.do("GET", "/style.css", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "prefers-color-scheme") {
		t.Fatalf("style.css = %d", rec.Code)
	}
	if rec := e.do("GET", "/nope", ""); rec.Code != 404 {
		t.Fatalf("/nope = %d", rec.Code)
	}
}

func TestStatus(t *testing.T) {
	e := newEnv(t)
	recent := now.Add(-2 * time.Minute).Unix()
	e.exec(`INSERT INTO accounts (id, google_sub, google_email, created_at) VALUES ('U', 's-u', 'u@x', 0)`)
	if rec := e.do("GET", "/", "U"); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/link" {
		t.Fatalf("unlinked: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	e.exec(`INSERT INTO accounts (id, google_sub, google_email, zulip_user_id, linked_at, last_poll_ok_at, created_at)
		VALUES ('A', 's-a', 'ann@example.com', 7, 0, ?, 0)`, recent)
	t.Run("none", func(t *testing.T) {
		e.expect(t, "A", "Connected as <b>ann@example.com</b> → Zulip <b>Ann Lee</b>", "No reminders scheduled", `href="/settings"`, `action="/disconnect"`)
	})

	start := now.Add(70 * time.Minute)
	e.exec(`INSERT INTO reminders VALUES ('k1', 'A', ?, ?, ?, 'pending', NULL, 0)`, start.Add(-10*time.Minute).Unix(), start.Unix(),
		fmt.Sprintf(`{"title":"Weekly <sync>","start":%q}`, start.Format(time.RFC3339)))
	e.exec(`INSERT INTO reminders VALUES ('k0', 'A', ?, ?, '{}', 'sent', NULL, 0)`, now.Add(-time.Hour).Unix(), now.Unix())
	t.Run("next", func(t *testing.T) {
		e.expect(t, "A", "Next: <b>Weekly &lt;sync&gt;</b>",
			`<time datetime="2026-10-06T14:10:00Z">Tue 14:10 UTC</time>`, `<time datetime="2026-10-06T14:00:00Z">Tue 14:00 UTC</time>`)
	})

	e.exec(`UPDATE accounts SET last_poll_ok_at = ? WHERE id = 'A'`, now.Add(-16*time.Minute).Unix())
	t.Run("stale", func(t *testing.T) { e.expect(t, "A", "We can't read your calendar right now, retrying") })
	e.exec(`UPDATE accounts SET last_poll_ok_at = NULL, linked_at = ? WHERE id = 'A'`, recent) // just linked, first poll pending
	t.Run("fresh link", func(t *testing.T) { e.expect(t, "A", "Next: ") })

	e.exec(`UPDATE accounts SET paused = 1 WHERE id = 'A'`)
	t.Run("paused", func(t *testing.T) { e.expect(t, "A", "Reminders are paused", `action="/resume"`) })
	e.exec(`UPDATE accounts SET status = 'disconnected' WHERE id = 'A'`)
	t.Run("lost", func(t *testing.T) {
		e.expect(t, "A", "We lost access to your Google Calendar", `href="/login">Reconnect`)
	})

	// Zulip unreachable: the page still works, just without the name.
	e.exec(`UPDATE accounts SET zulip_user_id = 8, status = 'connected', paused = 0 WHERE id = 'A'`)
	t.Run("no name", func(t *testing.T) { e.expect(t, "A", "Connected as <b>ann@example.com</b> → Zulip</p>") })
}

func TestResume(t *testing.T) {
	e := newEnv(t)
	e.exec(`INSERT INTO accounts (id, google_sub, google_email, zulip_user_id, paused, created_at) VALUES ('A', 's-a', 'a@x', 7, 1, 0)`)
	if rec := e.do("POST", "/resume", ""); rec.Code != http.StatusSeeOther || len(e.resumed) != 0 {
		t.Fatalf("signed out: %d %v", rec.Code, e.resumed)
	}
	if rec := e.do("POST", "/resume", "A"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" || len(e.resumed) != 1 {
		t.Fatalf("resume: %d %v", rec.Code, e.resumed)
	}
	req := httptest.NewRequest("POST", "/resume", nil) // cross-site form post
	req.Header.Set("X-Account", "A")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || len(e.resumed) != 1 {
		t.Fatalf("cross-site: %d %v", rec.Code, e.resumed)
	}
}

// TestErrorPages renders each friendly error page (and snapshots them).
func TestErrorPages(t *testing.T) {
	for name, m := range map[string]Message{"domain": DomainNotAllowed, "scope": ScopeNotGranted, "google": GoogleError, "zulip": ZulipUnreachable} {
		rec := httptest.NewRecorder()
		Error(rec, 400, m)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), `href="`+m.URL+`"`) || !strings.Contains(rec.Body.String(), "/style.css") {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
		if dir := os.Getenv("WEB_SNAPSHOT_DIR"); dir != "" {
			os.WriteFile(filepath.Join(dir, "error_"+name+".html"), rec.Body.Bytes(), 0o644)
		}
	}
	rec := httptest.NewRecorder()
	Render(rec, 200, "link", map[string]any{"Code": "K7Q4MZ", "ZulipDM": "https://zulip.example/#narrow/dm/1"})
	if !strings.Contains(rec.Body.String(), "<b>K7Q4MZ</b>") {
		t.Fatalf("link page: %s", rec.Body)
	}
	if dir := os.Getenv("WEB_SNAPSHOT_DIR"); dir != "" {
		os.WriteFile(filepath.Join(dir, "link_code.html"), rec.Body.Bytes(), 0o644)
	}
}
