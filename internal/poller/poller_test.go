package poller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/korjavin/zulip-gcal-service/internal/store"
)

var now = time.Date(2026, 10, 6, 9, 55, 0, 0, time.UTC)

// ev returns a Calendar API event as Google sends it; reminders is the raw
// "reminders" JSON ("" = absent).
func ev(uid string, start time.Time, reminders string, extra ...string) string {
	s := fmt.Sprintf(`{"id":"id-%s","iCalUID":"%s@google.com","status":"confirmed","summary":"Sync %s",
		"htmlLink":"https://calendar.google.com/event?eid=%s",
		"start":{"dateTime":"%s","timeZone":"Europe/Berlin"},"end":{"dateTime":"%s"}`,
		uid, uid, uid, uid, start.In(time.FixedZone("CEST", 2*3600)).Format(time.RFC3339), start.Add(30*time.Minute).Format(time.RFC3339))
	if reminders != "" {
		s += `,"reminders":` + reminders
	}
	for _, e := range extra {
		s += "," + e
	}
	return s + "}"
}

func cal(t *testing.T, id, defaults string, events ...string) Calendar {
	t.Helper()
	c := Calendar{ID: id}
	if defaults == "" {
		defaults = "[]"
	}
	body := `{"defaultReminders":` + defaults + `,"items":[` + strings.Join(events, ",") + `]}`
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

const over = `{"useDefault":false,"overrides":[%s]}`

func mins(m ...int) string {
	var parts []string
	for _, x := range m {
		parts = append(parts, fmt.Sprintf(`{"method":"popup","minutes":%d}`, x))
	}
	return fmt.Sprintf(over, strings.Join(parts, ","))
}

type want struct {
	off   string // key suffix
	state string
	fire  time.Time
}

func TestDesired(t *testing.T) {
	google := Settings{Timing: "google", LeadMinutes: 10, SkipDeclined: true}
	at10 := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	var tomorrow []string
	var tomorrowWant []want
	for i := range 20 {
		start := now.Add(20*time.Hour + time.Duration(i)*5*time.Minute)
		tomorrow = append(tomorrow, ev(fmt.Sprint("t", i), start, mins(1440)))
		tomorrowWant = append(tomorrowWant, want{"t" + fmt.Sprint(i) + "|1440", "skipped", start.Add(-24 * time.Hour)})
	}
	for _, tc := range []struct {
		name string
		s    Settings
		cals []Calendar
		want []want
	}{
		{"created 5 min ahead, 10-min lead -> immediate", Settings{Timing: "fixed", LeadMinutes: 10},
			[]Calendar{cal(t, "primary", "", ev("a", now.Add(5*time.Minute), ""))},
			[]want{{"a|10", "pending", now}}},
		{"connect 09:55 for 10:00 with 24h/60m/10m -> one DM two skipped", google,
			[]Calendar{cal(t, "primary", "", ev("a", at10, mins(1440, 60, 10)))},
			[]want{{"a|10", "pending", now}, {"a|60", "skipped", at10.Add(-time.Hour)}, {"a|1440", "skipped", at10.Add(-24 * time.Hour)}}},
		{"20 meetings tomorrow with 1-day reminders -> no burst", google,
			[]Calendar{cal(t, "primary", "", tomorrow...)}, tomorrowWant},
		{"future offsets stay at their time", google,
			[]Calendar{cal(t, "primary", "", ev("a", now.Add(3*time.Hour), mins(10, 30)))},
			[]want{{"a|10", "pending", now.Add(170 * time.Minute)}, {"a|30", "pending", now.Add(150 * time.Minute)}}},
		{"useDefault -> calendar defaults", google,
			[]Calendar{cal(t, "primary", `[{"method":"popup","minutes":30}]`, ev("a", now.Add(2*time.Hour), `{"useDefault":true}`))},
			[]want{{"a|30", "pending", now.Add(90 * time.Minute)}}},
		{"reminders absent -> calendar defaults", google,
			[]Calendar{cal(t, "primary", `[{"method":"popup","minutes":30}]`, ev("a", now.Add(2*time.Hour), ""))},
			[]want{{"a|30", "pending", now.Add(90 * time.Minute)}}},
		{"explicit no reminders -> none", google,
			[]Calendar{cal(t, "primary", `[{"method":"popup","minutes":30}]`, ev("a", now.Add(2*time.Hour), `{"useDefault":false}`))},
			nil},
		{"email-only reminder counts, duplicates merged", google,
			[]Calendar{cal(t, "primary", "", ev("a", now.Add(2*time.Hour),
				`{"useDefault":false,"overrides":[{"method":"email","minutes":15},{"method":"popup","minutes":15}]}`))},
			[]want{{"a|15", "pending", now.Add(105 * time.Minute)}}},
		{"0-min kept, over 24h dropped", google,
			[]Calendar{cal(t, "primary", "", ev("a", now.Add(25*time.Hour), mins(0, 1500)))},
			[]want{{"a|0", "pending", now.Add(25 * time.Hour)}}},
		{"same meeting in two calendars -> one row", google,
			[]Calendar{
				cal(t, "team@group.calendar.google.com", "", ev("a", now.Add(2*time.Hour), mins(10))),
				cal(t, "primary", "", ev("a", now.Add(2*time.Hour), mins(10)))},
			[]want{{"a|10", "pending", now.Add(110 * time.Minute)}}},
		{"declined skipped", google,
			[]Calendar{cal(t, "primary", "", ev("a", now.Add(2*time.Hour), mins(10), `"attendees":[{"self":true,"responseStatus":"declined"}]`))},
			nil},
		{"declined in primary wins over another calendar's copy", google,
			[]Calendar{
				cal(t, "team", "", ev("a", now.Add(2*time.Hour), mins(10))),
				cal(t, "primary", "", ev("a", now.Add(2*time.Hour), mins(10), `"attendees":[{"self":true,"responseStatus":"declined"}]`))},
			nil},
		{"declined kept when the setting is off", Settings{Timing: "google"},
			[]Calendar{cal(t, "primary", "", ev("a", now.Add(2*time.Hour), mins(10), `"attendees":[{"self":true,"responseStatus":"declined"}]`))},
			[]want{{"a|10", "pending", now.Add(110 * time.Minute)}}},
		{"someone else declining does not matter", google,
			[]Calendar{cal(t, "primary", "", ev("a", now.Add(2*time.Hour), mins(10), `"attendees":[{"email":"x@example.com","responseStatus":"declined"}]`))},
			[]want{{"a|10", "pending", now.Add(110 * time.Minute)}}},
		{"all-day skipped", google,
			[]Calendar{cal(t, "primary", `[{"method":"popup","minutes":30}]`,
				`{"id":"d","iCalUID":"d@google.com","status":"confirmed","start":{"date":"2026-10-07"},"end":{"date":"2026-10-08"}}`)},
			nil},
		{"cancelled skipped", google,
			[]Calendar{cal(t, "primary", "", strings.Replace(ev("a", now.Add(2*time.Hour), mins(10)), "confirmed", "cancelled", 1))},
			nil},
		{"started -> keep-only rows", google,
			[]Calendar{cal(t, "primary", "", ev("a", now.Add(-time.Minute), mins(0)))},
			[]want{{"a|0", "", now.Add(-time.Minute)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Desired("acc", tc.cals, tc.s, now)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d rows %+v, want %d", len(got), got, len(tc.want))
			}
			for i, w := range tc.want {
				g := got[i]
				if !strings.HasPrefix(g.Key, "acc|") || !strings.HasSuffix(g.Key, "|"+strings.Split(w.off, "|")[1]) ||
					!strings.Contains(g.Key, "|"+strings.Split(w.off, "|")[0]+"@google.com|") {
					t.Errorf("row %d key %q, want %s", i, g.Key, w.off)
				}
				if g.State != w.state || !g.FireAt.Equal(w.fire) {
					t.Errorf("row %d (%s): state %q fire %v, want %q %v", i, w.off, g.State, g.FireAt, w.state, w.fire)
				}
			}
		})
	}
}

func TestDesiredKeyAndPayload(t *testing.T) {
	start := now.Add(2 * time.Hour)
	team := cal(t, "team", "", ev("a", start, mins(10), `"location":"Room 4 https://zoom.us/j/1"`))
	team.Items[0].Summary = "team copy"
	prim := cal(t, "primary", "", ev("a", start, mins(10), `"hangoutLink":"https://meet.google.com/abc"`,
		`"conferenceData":{"entryPoints":[{"entryPointType":"phone","uri":"tel:+1"},{"entryPointType":"video","uri":"https://meet.google.com/xyz"}]}`))
	got := Desired("acc", []Calendar{team, prim}, Settings{Timing: "google"}, now)
	if len(got) != 1 {
		t.Fatalf("rows %+v", got)
	}
	r := got[0]
	if want := "acc|a@google.com|2026-10-06T11:55:00Z|10"; r.Key != want {
		t.Errorf("key %q, want %q", r.Key, want)
	}
	if r.Payload.Title != "Sync a" || r.Payload.Video != "https://meet.google.com/xyz" || !r.Payload.Start.Equal(start) ||
		r.Payload.Link != "https://calendar.google.com/event?eid=a" || r.Payload.End.Sub(r.Payload.Start) != 30*time.Minute {
		t.Errorf("payload from primary expected: %+v", r.Payload)
	}
	// Without primary: first calendar in the list; video falls back to the location URL.
	got = Desired("acc", []Calendar{team, cal(t, "other", "", ev("a", start, mins(10)))}, Settings{Timing: "google"}, now)
	if p := got[0].Payload; p.Title != "team copy" || p.Video != "https://zoom.us/j/1" || p.Location != "Room 4 https://zoom.us/j/1" {
		t.Errorf("payload from first calendar expected: %+v", p)
	}
}

// --- poll against a fake Calendar API and a real store ---

type env struct {
	t    *testing.T
	st   *store.Store
	p    *Poller
	mu   sync.Mutex
	cals map[string][]string // calendar id -> page bodies
	fail map[string]int      // "cal|page index" -> status
	hook func()              // runs inside each request
	now  time.Time
	reqs atomic.Int32
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, cals: map[string][]string{}, fail: map[string]int{}, now: now}
	var err error
	e.st, err = store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.st.Close() })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.reqs.Add(1)
		if r.Header.Get("Authorization") != "Bearer access" {
			http.Error(w, "no auth", 401)
			return
		}
		q := r.URL.Query()
		e.mu.Lock()
		t0 := e.now
		e.mu.Unlock()
		if q.Get("singleEvents") != "true" || q.Get("timeMin") != t0.Add(-2*time.Minute).Format(time.RFC3339) ||
			q.Get("timeMax") != t0.Add(26*time.Hour).Format(time.RFC3339) || !strings.Contains(q.Get("fields"), "nextPageToken") {
			http.Error(w, "bad query "+r.URL.RawQuery, 400)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/calendars/"), "/events")
		page := 0
		fmt.Sscan(q.Get("pageToken"), &page)
		e.mu.Lock()
		pages, status, hook := e.cals[id], e.fail[fmt.Sprint(id, "|", page)], e.hook
		e.mu.Unlock()
		if hook != nil {
			hook()
		}
		if status != 0 {
			http.Error(w, "boom", status)
			return
		}
		if page >= len(pages) {
			http.Error(w, "no page", 404)
			return
		}
		body := pages[page]
		if page+1 < len(pages) {
			body = strings.Replace(body, "{", fmt.Sprintf(`{"nextPageToken":"%d",`, page+1), 1)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	e.p = New(e.st, func(context.Context, string) (oauth2.TokenSource, error) {
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "access"}), nil
	}, time.Hour)
	e.p.BaseURL = srv.URL
	e.p.Now = func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
	e.exec(`INSERT INTO accounts (id, google_sub, google_email, zulip_user_id, created_at) VALUES ('acc', 'sub', 'a@example.com', 7, 0)`)
	e.exec(`INSERT INTO settings (account_id, lead_minutes, calendars) VALUES ('acc', 10, '["primary","team"]')`)
	return e
}

func (e *env) exec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.st.DB.Exec(q, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) set(cal string, pages ...string) {
	e.mu.Lock()
	e.cals[cal] = pages
	e.mu.Unlock()
}

func (e *env) setHook(f func()) {
	e.mu.Lock()
	e.hook = f
	e.mu.Unlock()
}

func page(events ...string) string { return `{"items":[` + strings.Join(events, ",") + `]}` }

// rows returns key-suffix -> "state fire_at title".
func (e *env) rows() map[string]string {
	e.t.Helper()
	rs, err := e.st.DB.Query(`SELECT key, state, fire_at, payload FROM reminders`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rs.Close()
	out := map[string]string{}
	for rs.Next() {
		var k, st, pj string
		var fire int64
		rs.Scan(&k, &st, &fire, &pj)
		var p Payload
		json.Unmarshal([]byte(pj), &p)
		parts := strings.Split(k, "|")
		out[strings.TrimSuffix(parts[1], "@google.com")+"|"+parts[3]] = fmt.Sprintf("%s %s %s", st, time.Unix(fire, 0).UTC().Format("15:04"), p.Title)
	}
	return out
}

func (e *env) check(want map[string]string) {
	e.t.Helper()
	got := e.rows()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		e.t.Errorf("rows\n got %v\nwant %v", got, want)
	}
}

func (e *env) lastPollOK() sql.NullInt64 {
	var v sql.NullInt64
	e.st.DB.QueryRow(`SELECT last_poll_ok_at FROM accounts WHERE id = 'acc'`).Scan(&v)
	return v
}

func TestPollReconcile(t *testing.T) {
	e := newEnv(t)
	at11 := now.Add(65 * time.Minute) // 11:00
	e.set("primary", page(ev("a", at11, mins(10))), page(ev("b", at11, mins(30))))
	e.set("team", page(ev("c", at11, mins(5)), ev("a", at11, mins(10))))
	if err := e.p.Poll(context.Background(), "acc"); err != nil {
		t.Fatal(err)
	}
	e.check(map[string]string{"a|10": "pending 10:50 Sync a", "b|30": "pending 10:30 Sync b", "c|5": "pending 10:55 Sync c"})
	if !e.lastPollOK().Valid {
		t.Error("last_poll_ok_at not set")
	}
	// b was already sent: history. Title change, move, cancel, and a new event.
	e.exec(`UPDATE reminders SET state = 'sent' WHERE key LIKE '%|b@google.com|%'`)
	renamed := strings.Replace(ev("a", at11, mins(10)), "Sync a", "Renamed", 1)
	e.set("primary", page(renamed), page(ev("d", at11, mins(10))))
	e.set("team", page(ev("c", at11.Add(time.Hour), mins(5))))
	if err := e.p.Poll(context.Background(), "acc"); err != nil {
		t.Fatal(err)
	}
	e.check(map[string]string{"a|10": "pending 10:50 Renamed", "b|30": "sent 10:30 Sync b",
		"c|5": "pending 11:55 Sync c", "d|10": "pending 10:50 Sync d"})
}

func TestPollKeepsPendingFireAt(t *testing.T) {
	e := newEnv(t)
	e.set("primary", page(ev("a", now.Add(5*time.Minute), mins(10))))
	e.set("team", page())
	e.exec(`INSERT INTO reminders (key, account_id, fire_at, event_start, payload, state, updated_at)
		VALUES ('acc|a@google.com|2026-10-06T10:00:00Z|10', 'acc', ?, 0, '{}', 'pending', 0)`, now.Add(-3*time.Minute).Unix())
	if err := e.p.Poll(context.Background(), "acc"); err != nil {
		t.Fatal(err)
	}
	e.check(map[string]string{"a|10": "pending 09:52 Sync a"}) // waiting for a Zulip retry: due time kept
}

func TestPollCancelDeletesSending(t *testing.T) {
	// A row mid-send whose meeting was cancelled is deleted, so a failed
	// send cannot put it back to pending; a still-desired one is left alone.
	e := newEnv(t)
	e.set("primary", page(ev("a", now.Add(5*time.Minute), mins(10))))
	e.set("team", page())
	for _, k := range []string{"acc|a@google.com|2026-10-06T10:00:00Z|10", "acc|gone@google.com|2026-10-06T10:00:00Z|10"} {
		e.exec(`INSERT INTO reminders (key, account_id, fire_at, event_start, payload, state, updated_at)
			VALUES (?, 'acc', ?, 0, '{}', 'sending', 0)`, k, now.Unix())
	}
	if err := e.p.Poll(context.Background(), "acc"); err != nil {
		t.Fatal(err)
	}
	e.check(map[string]string{"a|10": "sending 09:55 "})
}

func TestPollStartedKeepsPending(t *testing.T) {
	// 0-min reminder, meeting 10:00, poll 10:00:05: the pending row survives for the sender.
	e := newEnv(t)
	at10 := now.Add(5 * time.Minute)
	e.set("primary", page(ev("a", at10, mins(0)), ev("b", at10, mins(0))))
	e.set("team", page())
	if err := e.p.Poll(context.Background(), "acc"); err != nil {
		t.Fatal(err)
	}
	e.exec(`DELETE FROM reminders WHERE key LIKE '%|b@google.com|%'`)
	e.mu.Lock()
	e.now = at10.Add(5 * time.Second)
	e.mu.Unlock()
	if err := e.p.Poll(context.Background(), "acc"); err != nil {
		t.Fatal(err)
	}
	e.check(map[string]string{"a|0": "pending 10:00 Sync a"}) // b started without a row: none created
}

func TestPollFailedPageChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.set("primary", page(ev("a", now.Add(time.Hour), mins(10))))
	e.set("team", page())
	if err := e.p.Poll(context.Background(), "acc"); err != nil {
		t.Fatal(err)
	}
	ok := e.lastPollOK()
	before := e.rows()
	e.set("primary", page(), page(ev("b", now.Add(time.Hour), mins(10)))) // a gone...
	e.mu.Lock()
	e.fail["primary|1"] = 500 // ...but the second page fails
	e.mu.Unlock()
	if err := e.p.Poll(context.Background(), "acc"); err == nil || strings.Contains(err.Error(), "Sync") {
		t.Fatalf("want an error without event contents, got %v", err)
	}
	e.check(before)
	if e.lastPollOK() != ok {
		t.Error("last_poll_ok_at changed")
	}
}

func TestPollStaleScheduleRev(t *testing.T) {
	e := newEnv(t)
	e.set("primary", page(ev("a", now.Add(time.Hour), mins(10))))
	e.set("team", page())
	e.setHook(func() { e.exec(`UPDATE accounts SET schedule_rev = schedule_rev + 1`) }) // e.g. settings saved mid-fetch
	if err := e.p.Poll(context.Background(), "acc"); !errors.Is(err, store.ErrStale) {
		t.Fatalf("err %v, want stale", err)
	}
	e.check(map[string]string{})
	e.setHook(nil)
	if err := e.p.Poll(context.Background(), "acc"); err != nil {
		t.Fatal(err)
	}
	if len(e.rows()) != 1 {
		t.Error("re-poll did not commit")
	}
}

func TestPollIneligibleAccounts(t *testing.T) {
	for _, q := range []string{`UPDATE accounts SET paused = 1`, `UPDATE accounts SET zulip_user_id = NULL`,
		`UPDATE accounts SET status = 'disconnected'`} {
		e := newEnv(t)
		e.set("primary", page(ev("a", now.Add(time.Hour), mins(10))))
		e.exec(q)
		if err := e.p.Poll(context.Background(), "acc"); err != nil || e.reqs.Load() != 0 || len(e.rows()) != 0 {
			t.Errorf("%s: err %v, %d requests, rows %v", q, err, e.reqs.Load(), e.rows())
		}
	}
}

func TestTriggerSingleFlight(t *testing.T) {
	// On-demand triggers during a poll coalesce into one re-poll.
	singleFlight(t, func(p *Poller) { p.Trigger("acc"); p.Trigger("acc"); p.Trigger("acc") }, 4)
	// A periodic tick never re-polls a busy account (slow accounts must not pin the workers).
	singleFlight(t, func(p *Poller) { p.enqueue("acc", false) }, 2)
}

func singleFlight(t *testing.T, during func(*Poller), wantReqs int32) {
	e := newEnv(t)
	e.set("primary", page())
	e.set("team", page())
	entered, release := make(chan struct{}, 10), make(chan struct{})
	e.setHook(func() { entered <- struct{}{}; <-release })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.p.Interval = time.Hour
	go e.p.Run(ctx) // the first tick triggers acc
	<-entered       // first poll is fetching
	during(e.p)
	close(release)
	deadline := time.After(5 * time.Second)
	for {
		e.p.mu.Lock()
		_, busy := e.p.inflight["acc"]
		e.p.mu.Unlock()
		if !busy {
			break
		}
		select {
		case <-deadline:
			t.Fatal("poll never finished")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if n := e.reqs.Load(); n != wantReqs { // 2 calendars per poll
		t.Errorf("%d requests, want %d", n, wantReqs)
	}
}
