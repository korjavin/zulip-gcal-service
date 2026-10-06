package settings

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/korjavin/zulip-gcal-service/internal/store"
	"github.com/korjavin/zulip-gcal-service/internal/zulip"
)

const acct = "a1"

type env struct {
	t          *testing.T
	st         *store.Store
	mux        *http.ServeMux
	googleDown bool
	dms        []string
	polled     []string
	paused     []string
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, mux: http.NewServeMux()}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.st = st
	e.exec(`INSERT INTO accounts (id, google_sub, google_email, zulip_user_id, created_at) VALUES (?, 's', 'ann@x', 7, 0)`, acct)
	e.exec(`INSERT INTO settings (account_id, lead_minutes) VALUES (?, 10)`, acct)

	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e.googleDown || r.URL.Path != "/users/me/calendarList" || r.Header.Get("Authorization") != "Bearer access" {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Query().Get("pageToken") == "" {
			fmt.Fprint(w, `{"nextPageToken":"p2","items":[
				{"id":"team@group","summary":"Team","backgroundColor":"#9fe1e7","accessRole":"reader"},
				{"id":"ann@x","summary":"ann@x","summaryOverride":"Ann","backgroundColor":"#123456","accessRole":"owner","primary":true}]}`)
			return
		}
		fmt.Fprint(w, `{"items":[{"id":"busy@group","summary":"Busy only","accessRole":"freeBusyReader"},
			{"id":"hol@group","summary":"Holidays","accessRole":"reader"}]}`)
	}))
	t.Cleanup(g.Close)
	z := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.URL.Path == "/api/v1/messages" && r.Form.Get("to") == "[7]" {
			e.dms = append(e.dms, r.Form.Get("content"))
			fmt.Fprint(w, `{"result":"success","id":1}`)
			return
		}
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	t.Cleanup(z.Close)
	(&Page{St: st, Zulip: zulip.New(z.URL, "bot@x", "key"), BaseURL: g.URL,
		Account: func(r *http.Request) (string, bool) { id := r.Header.Get("X-Account"); return id, id != "" },
		CSRF:    func(id string) string { return "tok-" + id },
		Tokens: func(context.Context, string) (oauth2.TokenSource, error) {
			return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "access"}), nil
		},
		Pause: func(_ context.Context, id string) error { e.paused = append(e.paused, id); return nil },
		Poll:  func(id string) { e.polled = append(e.polled, id) },
		Now:   func() time.Time { return time.Date(2026, 10, 6, 13, 0, 0, 0, time.UTC) },
	}).Register(e.mux)
	return e
}

func (e *env) exec(q string, args ...any) {
	if _, err := e.st.DB.Exec(q, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) do(method, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Account", acct)
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *env) get(t *testing.T, query ...string) string {
	t.Helper()
	rec := e.do("GET", "/settings"+strings.Join(query, ""), nil)
	if rec.Code != 200 {
		t.Fatalf("GET /settings = %d", rec.Code)
	}
	return rec.Body.String()
}

func contains(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("page lacks %q", w)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	e := newEnv(t)
	body := e.get(t)
	contains(t, body, `value="tok-a1"`, `value="google" checked`, `<option value="10" selected>`,
		`name="skip_declined" value="1" checked`, `value="primary" checked`, ">Ann</label>", "Holidays", "#9fe1e7")
	if strings.Contains(body, "Busy only") {
		t.Error("free/busy-only calendar listed")
	}
	if strings.Index(body, "Team") > strings.Index(body, ">Ann<") || strings.Index(body, ">Ann<") > strings.Index(body, "Holidays") {
		t.Error("calendar order not kept")
	}

	e.exec(`INSERT INTO reminders (key, account_id, fire_at, event_start, payload, state, updated_at)
		VALUES ('k1', ?, 0, 0, '{}', 'pending', 0), ('k2', ?, 0, 0, '{}', 'sent', 0)`, acct, acct)
	rec := e.do("POST", "/settings", url.Values{"csrf": {"tok-a1"}, "timing": {"fixed"}, "lead": {"30"},
		"calendar": {"team@group", "primary", "team@group"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings?saved=1" {
		t.Fatalf("save = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Error("save touched cookies; the session must stay")
	}
	var timing, cals string
	var lead, skip, rev, n int
	e.st.DB.QueryRow(`SELECT timing, lead_minutes, skip_declined, calendars FROM settings`).Scan(&timing, &lead, &skip, &cals)
	if timing != "fixed" || lead != 30 || skip != 0 || cals != `["team@group","primary"]` {
		t.Errorf("stored %s %d %d %s", timing, lead, skip, cals)
	}
	e.st.DB.QueryRow(`SELECT schedule_rev FROM accounts`).Scan(&rev)
	e.st.DB.QueryRow(`SELECT count(*) FROM reminders WHERE state = 'pending'`).Scan(&n)
	if rev != 1 || n != 0 {
		t.Errorf("schedule_rev=%d pending=%d", rev, n)
	}
	e.st.DB.QueryRow(`SELECT count(*) FROM reminders WHERE state = 'sent'`).Scan(&n)
	if n != 1 {
		t.Error("history row deleted")
	}
	if len(e.polled) != 1 {
		t.Errorf("polls = %v", e.polled)
	}
	body = e.get(t, "?saved=1")
	contains(t, body, "Saved.", `value="fixed" checked`, `<option value="30" selected>`, `value="team@group" checked`)
	if strings.Contains(body, `value="hol@group" checked`) || strings.Contains(body, `name="skip_declined" value="1" checked`) {
		t.Error("unchecked boxes shown checked")
	}
}

func TestCSRF(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/settings", "/test-reminder", "/pause"} {
		for _, tok := range []string{"", "tok-other"} {
			rec := e.do("POST", path, url.Values{"csrf": {tok}, "timing": {"fixed"}, "lead": {"5"}, "calendar": {"primary"}})
			if rec.Code != http.StatusForbidden {
				t.Errorf("POST %s csrf=%q = %d", path, tok, rec.Code)
			}
		}
	}
	var timing string
	e.st.DB.QueryRow(`SELECT timing FROM settings`).Scan(&timing)
	if timing != "google" || len(e.dms) != 0 || len(e.paused) != 0 {
		t.Error("a request without a valid token had an effect")
	}
}

func TestInvalidForm(t *testing.T) {
	e := newEnv(t)
	for _, f := range []url.Values{
		{"timing": {"weekly"}, "lead": {"5"}, "calendar": {"primary"}},
		{"timing": {"fixed"}, "lead": {"0"}, "calendar": {"primary"}},
		{"timing": {"fixed"}, "lead": {"5"}},
	} {
		f.Set("csrf", "tok-a1")
		if rec := e.do("POST", "/settings", f); rec.Code != http.StatusBadRequest {
			t.Errorf("%v = %d", f, rec.Code)
		}
	}
}

func TestGoogleDown(t *testing.T) {
	e := newEnv(t)
	e.googleDown = true
	e.exec(`UPDATE settings SET calendars = '["primary","team@group"]'`)
	e.exec(`INSERT INTO reminders (key, account_id, fire_at, event_start, payload, state, updated_at)
		VALUES ('k1', ?, 0, 0, '{}', 'pending', 0), ('k2', ?, 0, 0, '{}', 'sending', 0)`, acct, acct)
	contains(t, e.get(t), "can't load your calendar list", `value="primary" checked`, `value="team@group" checked`)
	rec := e.do("POST", "/settings", url.Values{"csrf": {"tok-a1"}, "timing": {"google"}, "lead": {"10"},
		"skip_declined": {"1"}, "calendar": {"primary"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save = %d", rec.Code)
	}
	var n int
	e.st.DB.QueryRow(`SELECT count(*) FROM reminders`).Scan(&n)
	if n != 0 {
		t.Errorf("%d pending rows left", n)
	}
}

func TestTestReminder(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/test-reminder", url.Values{"csrf": {"tok-a1"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings?tested=1" {
		t.Fatalf("test = %d", rec.Code)
	}
	if len(e.dms) != 1 || !strings.Contains(e.dms[0], "**Example meeting** starts <time:2026-10-06T13:10:00Z> (in 10 min)") {
		t.Fatalf("dms = %q", e.dms)
	}
	contains(t, e.get(t, "?tested=1"), "Test reminder sent")
}

func TestPause(t *testing.T) {
	e := newEnv(t)
	if rec := e.do("POST", "/pause", url.Values{"csrf": {"tok-a1"}}); rec.Code != http.StatusSeeOther || len(e.paused) != 1 {
		t.Fatalf("pause = %d %v", rec.Code, e.paused)
	}
	e.exec(`UPDATE accounts SET paused = 1`)
	contains(t, e.get(t), `action="/resume"`)
}
