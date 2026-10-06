package link

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/korjavin/zulip-gcal-service/internal/config"
	"github.com/korjavin/zulip-gcal-service/internal/zulip"
)

// linkedAccount creates account id linked to Zulip user zid at linkedAt, with
// refresh token "rt-<id>" and one pending reminder.
func (e *env) linkedAccount(id string, zid int64, linkedAt time.Time) {
	e.t.Helper()
	secrets, _ := config.NewSecrets(bytes.Repeat([]byte{7}, 32)) // the key newEnv gives Ops
	e.account(id, id+"@x", false)
	if _, err := e.st.DB.Exec(`UPDATE accounts SET zulip_user_id = ?, linked_at = ?, enc_refresh_token = ? WHERE id = ?`,
		zid, linkedAt.Unix(), secrets.Encrypt([]byte("rt-"+id)), id); err != nil {
		e.t.Fatal(err)
	}
	e.reminder(id)
}

func (e *env) reminder(id string) {
	e.t.Helper()
	start := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if _, err := e.st.DB.Exec(`INSERT INTO reminders (key, account_id, fire_at, event_start, payload, state, updated_at)
		VALUES (?, ?, ?, 0, ?, 'pending', 0)`, id+"|"+start, id, time.Now().Add(50*time.Minute).Unix(),
		`{"title":"Standup","start":"`+start+`"}`); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) n(q string) (n int) {
	e.t.Helper()
	if err := e.st.DB.QueryRow(q).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestCommands(t *testing.T) {
	e := newEnv(t)
	e.linkedAccount("A", 1, time.Now().Add(-time.Minute))
	want := func(text, sub string) {
		t.Helper()
		if d := e.dm(1, text); d.to != "[1]" || !strings.Contains(d.text, sub) {
			t.Fatalf("%q -> %+v, want %q", text, d, sub)
		}
	}
	want("please disconnect me", "**disconnect**") // help, not a command
	want("STOP", "Reminders paused")
	// Paused with no pending rows: the sender has nothing it may claim.
	if e.n(`SELECT paused FROM accounts WHERE id = 'A'`) != 1 || e.n(`SELECT count(*) FROM reminders`) != 0 {
		t.Fatal("stop: not paused or reminders left")
	}
	want("/pause", "Already paused")
	want("start", "Reminders resumed")
	if len(e.polled) != 1 {
		t.Fatalf("polled %v", e.polled)
	}
	e.reminder("A")
	want("Resume", "Next reminder:\n📅 **Standup**")
	e.st.DB.Exec(`UPDATE accounts SET status = 'disconnected' WHERE id = 'A'`)
	want("start", "Reconnect:")
	want("disconnect", "Google access revoked")
	if e.n(`SELECT count(*) FROM accounts`) != 0 || e.n(`SELECT count(*) FROM reminders`) != 0 || len(e.revoked) != 1 || e.revoked[0] != "rt-A" {
		t.Fatalf("disconnect: rows left or revoked %v", e.revoked)
	}
	want("disconnect", "You are not connected.")
	want("hello", "sign in with Google")
}

// A disconnect replayed after a reconnect (crash before the reply, then the
// same event again; or a purged receipt) does nothing.
func TestReplayedDisconnect(t *testing.T) {
	e := newEnv(t)
	e.linkedAccount("A", 1, time.Now().Add(-time.Minute))
	old := zulip.Message{ID: 500, SenderID: 1, Time: time.Now(), Text: "disconnect"}
	if err := e.l.HandleDM(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	e.linkedAccount("B", 1, time.Now().Add(time.Second))
	n := len(e.dms)
	for _, m := range []zulip.Message{old, {ID: 501, SenderID: 1, Time: old.Time, Text: "disconnect"}} {
		if err := e.l.HandleDM(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.dms) != n || e.n(`SELECT count(*) FROM accounts WHERE id = 'B'`) != 1 || len(e.revoked) != 1 {
		t.Fatalf("replay acted: %d DMs, revoked %v", len(e.dms)-n, e.revoked)
	}
}
