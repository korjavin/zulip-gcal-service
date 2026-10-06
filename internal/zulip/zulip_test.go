package zulip

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
	"testing"
	"time"

	"github.com/korjavin/zulip-gcal-service/internal/store"
)

const botID = 100

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func noSuchUser(w http.ResponseWriter) {
	reply(w, 400, map[string]string{"result": "error", "msg": "No such user", "code": "BAD_REQUEST"})
}

// users served by the fake: id 1 human (a@x), 2 deactivated, 3 bot.
func fakeUsers(w http.ResponseWriter, r *http.Request) {
	users := map[string]User{
		"1": {ID: 1, Email: "a@x", IsActive: true}, "a@x": {ID: 1, Email: "a@x", IsActive: true},
		"2": {ID: 2, Email: "gone@x"}, "3": {ID: 3, Email: "bot@x", IsActive: true, IsBot: true},
	}
	u, ok := users[r.PathValue("key")]
	if !ok {
		noSuchUser(w) // unknown and hidden addresses look the same
		return
	}
	reply(w, 200, map[string]any{"result": "success", "user": u})
}

func newClient(t *testing.T, mux *http.ServeMux) *Client {
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(srv.URL+"/", "bot@x", "key")
}

func TestUserLookup(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/users/{key}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("key") {
		case "unauth@x":
			reply(w, 401, map[string]string{"result": "error", "msg": "Invalid API key", "code": "UNAUTHORIZED"})
		case "denied@x":
			reply(w, 403, map[string]string{"result": "error", "msg": "Forbidden", "code": "BAD_REQUEST"})
		case "broken@x":
			w.Write([]byte("<html>"))
		case "down@x":
			w.WriteHeader(502)
		default:
			if e, k, ok := r.BasicAuth(); !ok || e != "bot@x" || k != "key" {
				t.Errorf("basic auth = %q %q", e, k)
			}
			fakeUsers(w, r)
		}
	})
	c := newClient(t, mux)
	ctx := context.Background()
	if u, err := c.UserByEmail(ctx, "a@x"); err != nil || u.ID != 1 {
		t.Fatalf("found: %+v %v", u, err)
	}
	for _, email := range []string{"nobody@x", "hidden@x", "gone@x", "bot@x"} {
		if _, err := c.UserByEmail(ctx, email); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", email, err)
		}
	}
	for email, temp := range map[string]bool{"unauth@x": false, "denied@x": false, "broken@x": true, "down@x": true} {
		_, err := c.UserByEmail(ctx, email)
		if err == nil || errors.Is(err, ErrNotFound) || Temporary(err) != temp {
			t.Errorf("%s: %v (temporary=%v), want real error, temporary=%v", email, err, Temporary(err), temp)
		}
		if strings.Contains(err.Error(), strings.TrimSuffix(email, "@x")) {
			t.Errorf("%s: error leaks the e-mail: %v", email, err)
		}
	}
	// Transport failure: *url.Error would carry the URL with the e-mail.
	srv := httptest.NewServer(mux)
	srv.Close()
	_, err := New(srv.URL, "bot@x", "key").UserByEmail(ctx, "secret.person@x")
	if err == nil || !Temporary(err) || strings.Contains(err.Error(), "secret") {
		t.Errorf("transport failure: %v (temporary=%v)", err, Temporary(err))
	}
}

func TestSendDM(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	getCalls := func() int { mu.Lock(); defer mu.Unlock(); return calls }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/users/{key}", fakeUsers)
	mux.HandleFunc("POST /api/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if r.FormValue("type") != "direct" || r.FormValue("content") != "hi" {
			t.Errorf("form = %v", r.Form)
		}
		switch to := r.FormValue("to"); {
		case to == "[2]":
			reply(w, 400, map[string]string{"result": "error", "msg": "'gone@x' is no longer using Zulip.", "code": "BAD_REQUEST"})
		case to == "[1]" && n == 1:
			w.Header().Set("Retry-After", "0.01")
			reply(w, 429, map[string]string{"result": "error", "code": "RATE_LIMIT_HIT"})
		case to == "[1]":
			reply(w, 200, map[string]any{"result": "success", "id": 42})
		case to == "[5]":
			reply(w, 400, map[string]string{"result": "error", "msg": "Message must not be empty", "code": "BAD_REQUEST"})
		default:
			w.WriteHeader(503)
		}
	})
	mux.HandleFunc("GET /api/v1/users/5", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"user": User{ID: 5, IsActive: true}})
	})
	c := newClient(t, mux)
	ctx := context.Background()
	if id, err := c.SendDM(ctx, 1, "hi"); err != nil || id != 42 || getCalls() != 2 {
		t.Fatalf("send after 429: id=%d err=%v", id, err)
	}
	if _, err := c.SendDM(ctx, 2, "hi"); !errors.Is(err, ErrRecipient) || Temporary(err) {
		t.Errorf("deactivated: %v", err)
	}
	if _, err := c.SendDM(ctx, 5, "hi"); err == nil || errors.Is(err, ErrRecipient) || Temporary(err) {
		t.Errorf("other 400: %v", err)
	}
	if _, err := c.SendDM(ctx, 9, "hi"); !Temporary(err) {
		t.Errorf("503: %v", err)
	}
	c.maxRetryWait = 0
	mu.Lock()
	calls = 0
	mu.Unlock()
	if _, err := c.SendDM(ctx, 1, "hi"); !Temporary(err) || getCalls() != 1 {
		t.Errorf("long Retry-After: %v", err)
	}
	srvDown := New("http://127.0.0.1:1", "e", "k")
	if _, err := srvDown.SendDM(ctx, 1, "hi"); !Temporary(err) {
		t.Errorf("network: %v", err)
	}
}

func TestCheckServer(t *testing.T) {
	for _, tc := range []struct {
		level, status int
		want          string
	}{
		{302, 200, ""},
		{301, 200, "too old"},
		{400, 401, "ZULIP_BOT_API_KEY"},
	} {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v1/server_settings", func(w http.ResponseWriter, r *http.Request) {
			reply(w, 200, map[string]any{"result": "success", "zulip_feature_level": tc.level})
		})
		mux.HandleFunc("GET /api/v1/users/me", func(w http.ResponseWriter, r *http.Request) {
			reply(w, tc.status, map[string]any{"result": "success", "user_id": botID})
		})
		id, err := newClient(t, mux).CheckServer(context.Background())
		if tc.want == "" && (err != nil || id != botID) || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("level %d status %d: id=%d err=%v", tc.level, tc.status, id, err)
		}
	}
}

// fakeQueue serves register/events from a script of event batches.
type fakeQueue struct {
	mu        sync.Mutex
	batches   [][]any
	registers int
	acked     []string
}

func dm(id, sender int64, text string, rcpt ...int64) map[string]any {
	var rs []map[string]any
	for _, r := range rcpt {
		rs = append(rs, map[string]any{"id": r, "email": fmt.Sprint(r)})
	}
	return map[string]any{"type": "message", "id": id, "message": map[string]any{
		"id": id, "type": "private", "sender_id": sender, "timestamp": 1700000000,
		"content": text, "display_recipient": rs}}
}

func TestBotLoop(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	q := &fakeQueue{batches: [][]any{
		{
			dm(1, 1, "  stop \n", botID, 1),                   // handled
			dm(2, 1, "group", botID, 1, 7),                    // group DM
			dm(3, botID, "mine", botID, 1),                    // bot's own
			dm(4, 3, "from a bot", botID, 3),                  // sender is a bot
			map[string]any{"type": "update_message", "id": 5}, // edit
			map[string]any{"type": "message", "id": 6, "message": map[string]any{
				"id": 6, "type": "stream", "sender_id": 1, "display_recipient": "general"}},
		},
		{"BAD_EVENT_QUEUE_ID"},
		{dm(7, 1, "start", 1, botID), dm(1, 1, "stop", botID, 1)}, // 1 is a replay
		{"500"},
		{dm(8, 1, "help", botID, 1)},
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/users/{key}", fakeUsers)
	mux.HandleFunc("POST /api/v1/register", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("narrow") != `[["is","dm"]]` || r.FormValue("event_types") != `["message"]` || r.FormValue("apply_markdown") != "false" {
			t.Errorf("register form = %v", r.Form)
		}
		q.mu.Lock()
		q.registers++
		n := q.registers
		q.mu.Unlock()
		reply(w, 200, map[string]any{"result": "success", "queue_id": fmt.Sprint("q", n), "last_event_id": -1})
	})
	mux.HandleFunc("GET /api/v1/events", func(w http.ResponseWriter, r *http.Request) {
		q.mu.Lock()
		defer q.mu.Unlock()
		q.acked = append(q.acked, r.FormValue("queue_id")+"/"+r.FormValue("last_event_id"))
		if len(q.batches) == 0 {
			reply(w, 200, map[string]any{"result": "success", "events": []any{map[string]any{"type": "heartbeat", "id": 99}}})
			return
		}
		b := q.batches[0]
		q.batches = q.batches[1:]
		switch b[0] {
		case "BAD_EVENT_QUEUE_ID":
			reply(w, 400, map[string]string{"result": "error", "code": "BAD_EVENT_QUEUE_ID", "msg": "Bad event queue ID"})
		case "500":
			w.WriteHeader(500)
		default:
			reply(w, 200, map[string]any{"result": "success", "events": b})
		}
	})
	c := newClient(t, mux)

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan Message, 10)
	bot := NewBot(c, st, botID, func(ctx context.Context, m Message) error {
		tx, err := st.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if ok, err := store.MarkHandled(ctx, tx, m.ID, time.Now()); err != nil || !ok {
			return err
		}
		got <- m
		return tx.Commit()
	})
	bot.maxWait = 10 * time.Millisecond
	done := make(chan error)
	go func() { done <- bot.Run(ctx) }()

	var ids []int64
	for len(ids) < 3 {
		select {
		case m := <-got:
			if m.ID == 1 && (m.Text != "stop" || m.SenderID != 1 || m.Time.Unix() != 1700000000) {
				t.Errorf("message 1 = %+v", m)
			}
			ids = append(ids, m.ID)
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout, handled %v", ids)
		}
	}
	for !bot.Healthy() {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if fmt.Sprint(ids) != "[1 7 8]" {
		t.Errorf("handled %v, want [1 7 8]", ids)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.registers != 2 {
		t.Errorf("registers = %d, want 2", q.registers)
	}
	// Acknowledgement advances past every event of a processed batch.
	if q.acked[0] != "q1/-1" || q.acked[1] != "q1/6" || q.acked[2] != "q2/-1" || q.acked[3] != "q2/7" {
		t.Errorf("acked = %v", q.acked)
	}
}

func TestPurgeHandled(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now()
	mark := func(id int64, at time.Time) bool {
		var ok bool
		err := withTx(st.DB, func(tx *sql.Tx) (err error) { ok, err = store.MarkHandled(ctx, tx, id, at); return })
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !mark(1, now.Add(-8*24*time.Hour)) || !mark(2, now) || mark(2, now) {
		t.Fatal("MarkHandled insert-first semantics")
	}
	if err := st.PurgeHandled(ctx, now); err != nil {
		t.Fatal(err)
	}
	if h, _ := st.Handled(ctx, 1); h {
		t.Error("old receipt not purged")
	}
	if h, _ := st.Handled(ctx, 2); !h {
		t.Error("fresh receipt purged")
	}
}

func withTx(db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
