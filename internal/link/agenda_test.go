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
	failing := false
	var q string
	ev := func(id, title string, d time.Duration, extra string) string {
		s := time.Now().Add(d).UTC().Format(time.RFC3339)
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
