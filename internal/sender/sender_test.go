package sender

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/korjavin/zulip-gcal-service/internal/poller"
	"github.com/korjavin/zulip-gcal-service/internal/store"
	"github.com/korjavin/zulip-gcal-service/internal/zulip"
)

var t0 = time.Date(2026, 10, 6, 9, 50, 0, 0, time.UTC)

type env struct {
	t      *testing.T
	st     *store.Store
	s      *Sender
	mu     sync.Mutex
	now    time.Time
	status int    // POST /messages reply status; 0 = success
	gone   bool   // user 7 deactivated
	hook   func() // runs inside POST /messages
	sent   []string
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, now: t0}
	var err error
	e.st, err = store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.st.Close() })
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, status int, v any) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("POST /api/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		hook, status, gone := e.hook, e.status, e.gone
		e.mu.Unlock()
		if hook != nil {
			hook()
		}
		switch {
		case gone:
			reply(w, 400, map[string]string{"result": "error", "msg": "Invalid user ID 7", "code": "BAD_REQUEST"})
		case status != 0:
			w.WriteHeader(status)
		default:
			if r.FormValue("to") != "[7]" || r.FormValue("type") != "direct" {
				t.Errorf("send to %q type %q", r.FormValue("to"), r.FormValue("type"))
			}
			e.mu.Lock()
			e.sent = append(e.sent, r.FormValue("content"))
			n := len(e.sent)
			e.mu.Unlock()
			reply(w, 200, map[string]any{"result": "success", "id": 1000 + n})
		}
	})
	mux.HandleFunc("GET /api/v1/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		gone := e.gone
		e.mu.Unlock()
		reply(w, 200, map[string]any{"result": "success", "user": map[string]any{"user_id": 7, "is_active": !gone}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	e.s = New(e.st, zulip.New(srv.URL, "bot@x", "key"))
	e.s.Now = func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
	e.exec(`INSERT INTO accounts (id, google_sub, google_email, zulip_user_id, created_at) VALUES ('acc', 'sub', 'a@x', 7, 0)`)
	return e
}

func (e *env) exec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.st.DB.Exec(q, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) set(f func()) { e.mu.Lock(); f(); e.mu.Unlock() }

// add inserts a pending reminder named key for a meeting at start.
func (e *env) add(key string, fire, start time.Time) {
	e.t.Helper()
	pj, _ := json.Marshal(poller.Payload{Title: "Sync " + key, Start: start, End: start.Add(30 * time.Minute)})
	e.exec(`INSERT INTO reminders (key, account_id, fire_at, event_start, payload, state, updated_at)
		VALUES (?, 'acc', ?, ?, ?, 'pending', 0)`, key, fire.Unix(), start.Unix(), string(pj))
}

func (e *env) tick() {
	e.t.Helper()
	if err := e.s.Tick(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

// state returns "state msgid" for key, or "gone".
func (e *env) state(key string) string {
	e.t.Helper()
	var st string
	var id *int64
	if err := e.st.DB.QueryRow(`SELECT state, zulip_message_id FROM reminders WHERE key = ?`, key).Scan(&st, &id); err != nil {
		return "gone"
	}
	if id != nil {
		return fmt.Sprint(st, " ", *id)
	}
	return st
}

func (e *env) sentCount() int { e.mu.Lock(); defer e.mu.Unlock(); return len(e.sent) }

func TestSentOnceAcrossTicks(t *testing.T) {
	e := newEnv(t)
	start := t0.Add(10 * time.Minute)
	e.add("a", t0, start)
	e.add("later", t0.Add(time.Minute), start)
	e.tick()
	e.tick()
	if e.sentCount() != 1 || e.state("a") != "sent 1001" || e.state("later") != "pending" {
		t.Fatalf("sent %v, a %s, later %s", e.sent, e.state("a"), e.state("later"))
	}
	if !strings.Contains(e.sent[0], "**Sync a** starts <time:2026-10-06T10:00:00Z> (in 10 min)") {
		t.Errorf("message %q", e.sent[0])
	}
}

func TestRetryThenSentThenExpired(t *testing.T) {
	e := newEnv(t)
	start := t0.Add(10 * time.Minute)
	e.add("a", t0, start)
	e.add("b", t0, start)
	e.set(func() { e.status = 502 })
	e.tick()
	if e.state("a") != "pending" || e.state("b") != "pending" {
		t.Fatalf("after 5xx: a %s b %s", e.state("a"), e.state("b"))
	}
	// Recovers 3 minutes later: "in N min" is computed at send time.
	e.set(func() { e.status = 0; e.now = t0.Add(3*time.Minute + 10*time.Second) })
	e.exec(`UPDATE reminders SET state = 'skipped' WHERE key = 'b'`) // only a in this round
	e.tick()
	if e.state("a") != "sent 1001" || !strings.Contains(e.sent[0], "(in 7 min)") {
		t.Fatalf("a %s, sent %q", e.state("a"), e.sent)
	}
	// Still failing after start+2min -> expired.
	e.exec(`UPDATE reminders SET state = 'pending' WHERE key = 'b'`)
	e.set(func() { e.status = 503; e.now = start.Add(2 * time.Minute) })
	e.tick()
	if e.state("b") != "pending" {
		t.Fatalf("at start+2min b %s, want still deliverable", e.state("b"))
	}
	e.set(func() { e.now = start.Add(2*time.Minute + time.Second) })
	e.tick()
	if e.state("b") != "expired" || e.sentCount() != 1 {
		t.Fatalf("b %s, %d sent", e.state("b"), e.sentCount())
	}
}

func TestZeroMinuteReminder(t *testing.T) {
	e := newEnv(t)
	e.add("a", t0, t0)
	e.set(func() { e.now = t0.Add(30 * time.Second) })
	e.tick()
	if e.sentCount() != 1 || !strings.Contains(e.sent[0], "(starting now)") {
		t.Fatalf("sent %q", e.sent)
	}
}

func TestPausedBeforeClaimNotSent(t *testing.T) {
	for _, q := range []string{`UPDATE accounts SET paused = 1`, `UPDATE accounts SET zulip_user_id = NULL`,
		`UPDATE accounts SET status = 'disconnected'`} {
		e := newEnv(t)
		e.add("a", t0, t0.Add(10*time.Minute))
		e.exec(q) // the row itself left in place: the claim's own guard must hold
		e.tick()
		if e.sentCount() != 0 || e.state("a") != "pending" {
			t.Errorf("%s: sent %d, a %s", q, e.sentCount(), e.state("a"))
		}
	}
}

// pause is what lifecycle.Pause commits.
const pause = `UPDATE accounts SET paused = 1, schedule_rev = schedule_rev + 1;
	DELETE FROM reminders WHERE state IN ('pending', 'sending')`

func TestCancelledWhileSending(t *testing.T) {
	for _, tc := range []struct {
		name, q string
		status  int
		sent    int
	}{
		{"claimed then paused -> may finish", pause, 0, 1},
		{"claim, pause, 5xx -> not retried", pause, 500, 0},
		{"claim, cancellation poll, 5xx -> not retried", `DELETE FROM reminders WHERE key = 'a' AND state IN ('pending', 'sending')`, 500, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.add("a", t0, t0.Add(10*time.Minute))
			e.set(func() { e.status = tc.status; e.hook = func() { e.exec(tc.q) } })
			e.tick()
			e.set(func() { e.status, e.hook = 0, nil })
			e.exec(`UPDATE accounts SET paused = 0`)
			e.tick()
			if e.state("a") != "gone" || e.sentCount() != tc.sent {
				t.Fatalf("a %s, sent %d", e.state("a"), e.sentCount())
			}
		})
	}
}

func TestDeactivatedRecipientUnlinks(t *testing.T) {
	e := newEnv(t)
	e.add("a", t0, t0.Add(10*time.Minute))
	e.add("b", t0.Add(time.Hour), t0.Add(2*time.Hour))
	e.exec(`INSERT INTO reminders (key, account_id, fire_at, event_start, payload, state, updated_at)
		VALUES ('old', 'acc', 0, 0, '{}', 'sent', ?)`, t0.Unix())
	e.set(func() { e.gone = true })
	e.tick()
	var linked bool
	var rev int
	e.st.DB.QueryRow(`SELECT zulip_user_id IS NOT NULL, schedule_rev FROM accounts`).Scan(&linked, &rev)
	if linked || rev != 1 {
		t.Errorf("linked %v rev %d", linked, rev)
	}
	if e.state("a") != "gone" || e.state("b") != "gone" || e.state("old") != "sent" {
		t.Errorf("a %s b %s old %s", e.state("a"), e.state("b"), e.state("old"))
	}
}

func TestHousekeeping(t *testing.T) {
	e := newEnv(t)
	old, recent := t0.Add(-8*24*time.Hour).Unix(), t0.Add(-6*24*time.Hour).Unix()
	for _, r := range []struct {
		key, state string
		at         int64
	}{{"s-old", "sent", old}, {"x-old", "expired", old}, {"k-old", "skipped", old}, {"s-new", "sent", recent}, {"p-old", "pending", old}} {
		e.exec(`INSERT INTO reminders (key, account_id, fire_at, event_start, payload, state, updated_at)
			VALUES (?, 'acc', ?, ?, '{}', ?, ?)`, r.key, t0.Add(time.Hour).Unix(), t0.Add(2*time.Hour).Unix(), r.state, r.at)
	}
	e.tick()
	for k, want := range map[string]string{"s-old": "gone", "x-old": "gone", "k-old": "gone", "s-new": "sent", "p-old": "pending"} {
		if got := e.state(k); got != want {
			t.Errorf("%s: %s, want %s", k, got, want)
		}
	}
}

func TestRender(t *testing.T) {
	start := time.Date(2026, 10, 6, 14, 0, 0, 0, time.UTC)
	p := poller.Payload{Title: "Weekly sync", Start: start, Location: "Room 4",
		Video: "https://meet.google.com/abc-defg-hij", Link: "https://calendar.google.com/event?eid=x"}
	want := "📅 **Weekly sync** starts <time:2026-10-06T14:00:00Z> (in 10 min)\n" +
		"📍 Room 4 · 🎥 [Join Google Meet](https://meet.google.com/abc-defg-hij) · [Open in Calendar](https://calendar.google.com/event?eid=x)"
	if got := Render(p, start.Add(-10*time.Minute)); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	for d, w := range map[time.Duration]string{59 * time.Second: "(starting now)", -time.Minute: "(starting now)",
		9*time.Minute + 40*time.Second: "(in 10 min)", time.Hour: "(in 1 h)", 90 * time.Minute: "(in 1 h 30 min)", 24 * time.Hour: "(in 24 h)"} {
		if got := Render(poller.Payload{Title: "x", Start: start}, start.Add(-d)); !strings.HasSuffix(got, w) {
			t.Errorf("%v before: %q, want %s", d, got, w)
		}
	}
	got := Render(poller.Payload{Title: "*Q4* [plan](x) @**all** `code`\n> quote", Start: start,
		Location: "Room_1 #2", Video: "https://zoom.us/j/1)"}, start)
	want = "📅 **\\*Q4\\* \\[plan\\]\\(x\\) @\\*\\*all\\*\\* \\`code\\` \\> quote** starts <time:2026-10-06T14:00:00Z> (starting now)\n" +
		"📍 Room\\_1 \\#2 · 🎥 [Join video call](https://zoom.us/j/1%29)"
	if got != want {
		t.Errorf("escaping:\n got %q\nwant %q", got, want)
	}
}
