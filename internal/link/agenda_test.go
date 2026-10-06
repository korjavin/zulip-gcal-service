package link

import (
	"strings"
	"testing"
	"time"
)

func TestStatusToday(t *testing.T) {
	e := newEnv(t)
	e.linkedAccount("A", 1, time.Now().Add(-time.Minute))
	e.st.DB.Exec(`INSERT INTO settings (account_id, timing, lead_minutes) VALUES ('A', 'fixed', 7)`)
	add := func(title string, d time.Duration) {
		start := time.Now().Add(d).UTC().Format(time.RFC3339)
		if _, err := e.st.DB.Exec(`INSERT INTO reminders (key, account_id, fire_at, event_start, payload, state, updated_at)
			VALUES (?, 'A', ?, ?, ?, 'pending', 0)`, title+start, time.Now().Add(d).Unix(), time.Now().Add(d).Unix(),
			`{"title":"`+title+`","start":"`+start+`"}`); err != nil {
			t.Fatal(err)
		}
	}
	add("Past", -time.Minute) // started, reminder still pending
	add("Soon", time.Minute)
	add("Later", 72*time.Hour)
	for _, c := range []struct {
		text    string
		in, out []string
	}{
		{"status", []string{"Connected as **A@x**", "7 min before", "**Soon**"}, []string{"Later"}}, // only the next 3
		{"/Today", []string{"Still ahead today:", "**Soon**"}, []string{"Later", "Past"}},
		{"help", []string{"**status**", "**today**"}, nil},
	} {
		d := e.dm(1, c.text)
		for _, s := range c.in {
			if !strings.Contains(d.text, s) {
				t.Errorf("%q -> %q, want %q", c.text, d.text, s)
			}
		}
		for _, s := range c.out {
			if strings.Contains(d.text, s) {
				t.Errorf("%q -> %q, must not have %q", c.text, d.text, s)
			}
		}
	}
	e.st.DB.Exec(`DELETE FROM reminders`)
	if d := e.dm(1, "today"); !strings.Contains(d.text, "No more meetings") {
		t.Fatal(d.text)
	}
}
