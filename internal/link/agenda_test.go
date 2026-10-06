package link

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/korjavin/zulip-gcal-service/internal/poller"
)

func TestStatus(t *testing.T) {
	e := newEnv(t)
	e.linkedAccount("A", 1, time.Now().Add(-time.Minute))
	e.st.DB.Exec(`INSERT INTO settings (account_id, timing, lead_minutes) VALUES ('A', 'fixed', 7)`)
	for _, c := range []struct {
		text string
		in   []string
	}{
		{"status", []string{"Connected as **A@x**", "7 min before", "**Standup**, meeting at <time:"}},
		{"help", []string{"**status**", "**today**"}},
	} {
		d := e.dm(1, c.text)
		for _, s := range c.in {
			if !strings.Contains(d.text, s) {
				t.Errorf("%q -> %q, want %q", c.text, d.text, s)
			}
		}
	}
}

// "today" reads the calendar live: meetings already reminded still show, and
// started, declined, cancelled, all-day, duplicate and tomorrow's ones do not.
func TestToday(t *testing.T) {
	e := newEnv(t)
	e.linkedAccount("A", 1, time.Now().Add(-time.Minute))
	e.st.DB.Exec(`DELETE FROM reminders`) // nothing pending: the answer must not depend on reminders
	e.st.DB.Exec(`INSERT INTO settings (account_id, lead_minutes, calendars) VALUES ('A', 10, '["primary"]')`)
	now := time.Date(2030, 1, 15, 0, 0, 0, 0, time.UTC) // 13:00 in the fake user's Pacific/Auckland
	e.l.Now = func() time.Time { return now }
	failing := false
	var q string
	ev := func(id, title string, d time.Duration, extra string) string {
		s := now.Add(d).UTC().Format(time.RFC3339)
		return fmt.Sprintf(`{"id":"%s","summary":"%s","start":{"dateTime":"%s"},"end":{"dateTime":"%s"}%s}`, id, title, s, s, extra)
	}
	items := []string{
		ev("1", "Soon", time.Minute, ""),
		ev("2", "Soon2", 2*time.Minute, ""),
		ev("1", "Soon", time.Minute, ""), // same occurrence again
		ev("3", "Started", -time.Hour, ""),
		ev("4", "Declined", 3*time.Minute, `,"attendees":[{"self":true,"responseStatus":"declined"}]`),
		ev("5", "Cancelled", 3*time.Minute, `,"status":"cancelled"`),
		`{"id":"6","summary":"AllDay","start":{"date":"2026-01-01"},"end":{"date":"2026-01-02"}}`,
		ev("7", "Tomorrow", 72*time.Hour, ""),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.RawQuery
		if failing {
			http.Error(w, "boom secret-event-title", 500)
			return
		}
		fmt.Fprintf(w, `{"items":[%s]}`, strings.Join(items, ","))
	}))
	t.Cleanup(srv.Close)
	p := poller.New(e.st, func(context.Context, string) (oauth2.TokenSource, error) {
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"}), nil
	}, time.Hour)
	p.BaseURL = srv.URL
	e.l.Meetings = p.Meetings

	d := e.dm(1, "/Today")
	if !strings.Contains(d.text, "Still ahead today:\n* <time:") || !strings.Contains(d.text, "**Soon**") ||
		strings.Index(d.text, "**Soon**") > strings.Index(d.text, "**Soon2**") || strings.Count(d.text, "\n*") != 2 {
		t.Errorf("today -> %q", d.text)
	}
	if !strings.Contains(q, "singleEvents=true") {
		t.Errorf("query %q", q)
	}
	items = nil
	if d := e.dm(1, "today"); !strings.Contains(d.text, "No more meetings today.") {
		t.Errorf("empty -> %q", d.text)
	}
	failing = true
	if d := e.dm(1, "today"); !strings.Contains(d.text, "couldn't read your calendar") || strings.Contains(d.text, "secret") {
		t.Errorf("error -> %q", d.text)
	}
}

// The daily agenda: at the chosen time in the Zulip profile timezone (else
// the Google calendar's, else UTC), once per local day, workdays only if
// asked, retried after a failure.
func TestDailyAgenda(t *testing.T) {
	e := newEnv(t)
	e.linkedAccount("A", 1, time.Unix(0, 0)) // Pacific/Auckland: UTC+13 in January
	e.linkedAccount("B", 3, time.Unix(0, 0)) // no profile timezone: UTC
	e.linkedAccount("P", 2, time.Unix(0, 0)) // paused
	e.st.DB.Exec(`INSERT INTO settings (account_id, lead_minutes, agenda_minute, agenda_workdays) VALUES
		('A', 10, 480, 1), ('B', 10, 480, 0), ('P', 10, 480, 0)`)
	e.st.DB.Exec(`UPDATE accounts SET paused = 1 WHERE id = 'P'`)
	var now time.Time
	e.l.Now = func() time.Time { return now }
	failing, saveDuring := false, false
	var asked []string
	e.l.Meetings = func(_ context.Context, id string, from, to time.Time) ([]poller.Payload, error) {
		asked = append(asked, fmt.Sprintf("%s %s %s", id, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339)))
		if failing {
			return nil, fmt.Errorf("boom")
		}
		if saveDuring { // a settings save lands while Google is being read
			saveDuring = false
			e.st.DB.Exec(`UPDATE accounts SET schedule_rev = schedule_rev + 1 WHERE id = ?`, id)
		}
		s := from.Add(time.Hour)
		return []poller.Payload{{Title: "Plan_ning", Start: s}, {Title: "Review", Start: s.Add(time.Hour)}}, nil
	}
	// Fake Calendar API for B's fallback: the primary calendar's timeZone.
	calDown, calTZ := true, "Asia/Tokyo"
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calDown || r.URL.Path != "/calendars/primary" || r.Header.Get("Authorization") != "Bearer x" {
			http.Error(w, "down", 503)
			return
		}
		fmt.Fprintf(w, `{"timeZone":%q}`, calTZ)
	}))
	t.Cleanup(g.Close)
	p := poller.New(e.st, func(context.Context, string) (oauth2.TokenSource, error) {
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"}), nil
	}, time.Hour)
	p.BaseURL = g.URL
	e.l.CalendarZone = p.TimeZone
	tzOf := func(id string) (tz string) {
		e.st.DB.QueryRow(`SELECT agenda_tz FROM settings WHERE account_id = ?`, id).Scan(&tz)
		return tz
	}
	tick := func(at string) []sent {
		t.Helper()
		now, _ = time.Parse(time.RFC3339, at)
		n := len(e.dms)
		if err := e.l.AgendaTick(context.Background()); err != nil {
			t.Fatal(err)
		}
		return e.dms[n:]
	}

	// Wed 2030-01-16 07:59 in Auckland: not yet. Google is down: B's zone
	// stays unresolved rather than falling back to UTC.
	if d := tick("2030-01-15T18:59:00Z"); len(d) != 0 {
		t.Fatalf("early: %v", d)
	}
	if tzOf("A") != "Pacific/Auckland" || tzOf("B") != "" {
		t.Fatalf("zones %q %q", tzOf("A"), tzOf("B"))
	}
	calDown = false
	d := tick("2030-01-15T19:00:00Z") // 08:00 NZDT
	if len(d) != 1 || d[0].to != "[1]" || !strings.Contains(d[0].text, "Today: 2 meetings\n* <time:2030-01-15T20:00:00Z> **Plan\\_ning**") ||
		!strings.Contains(d[0].text, "https://cal.example/settings") {
		t.Fatalf("agenda: %v", d)
	}
	if asked[0] != "A 2030-01-15T19:00:00Z 2030-01-16T11:00:00Z" { // until local midnight
		t.Errorf("window %q", asked[0])
	}
	if d := tick("2030-01-15T19:05:00Z"); len(d) != 0 {
		t.Fatalf("second agenda the same day: %v", d)
	}

	// Thu: the calendar read fails, then a concurrent settings save wins, then
	// the Zulip send fails (the day's claim is released); the next tick sends it.
	failing = true
	if d := tick("2030-01-16T19:00:00Z"); len(d) != 0 {
		t.Fatalf("failed: %v", d)
	}
	failing, saveDuring = false, true
	if d := tick("2030-01-16T19:00:30Z"); len(d) != 0 {
		t.Fatalf("sent despite a save: %v", d)
	}
	e.failSends = 1
	if d := tick("2030-01-16T19:01:00Z"); len(d) != 0 || e.failSends != 0 {
		t.Fatalf("send failure: %v, %d", d, e.failSends)
	}
	if d := tick("2030-01-16T19:01:30Z"); len(d) != 1 {
		t.Fatalf("retry: %v", d)
	}
	// Fri: the service was down until an hour after; Sat: workdays only.
	if d := tick("2030-01-17T20:00:00Z"); len(d) != 0 {
		t.Fatalf("late: %v", d)
	}
	if d := tick("2030-01-18T19:00:00Z"); len(d) != 0 {
		t.Fatalf("Saturday: %v", d)
	}

	// B, with no profile timezone, follows its Google calendar: 08:00 in
	// Tokyo. P is paused.
	if tzOf("B") != "Asia/Tokyo" {
		t.Fatalf("B zone %q", tzOf("B"))
	}
	d = tick("2030-01-15T23:00:00Z")
	if len(d) != 1 || d[0].to != "[3]" {
		t.Fatalf("calendar-zone user: %v", d)
	}
	// Neither the profile nor Google has a zone: UTC.
	calTZ = ""
	if tz := e.l.zoneName(context.Background(), 3, "B"); tz != "UTC" {
		t.Errorf("no zone anywhere -> %q", tz)
	}
}
