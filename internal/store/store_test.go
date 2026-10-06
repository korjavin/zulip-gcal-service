package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func exec(t *testing.T, s *Store, q string, args ...any) {
	t.Helper()
	if _, err := s.DB.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func count(t *testing.T, s *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// addAccount inserts a connected, linked account with one row in every child table.
func addAccount(t *testing.T, s *Store, sub string, zulipID int) string {
	t.Helper()
	id := NewAccountID()
	exec(t, s, `INSERT INTO accounts (id, google_sub, google_email, zulip_user_id, created_at) VALUES (?, ?, ?, ?, 0)`, id, sub, sub+"@example.com", zulipID)
	exec(t, s, `INSERT INTO settings (account_id, lead_minutes) VALUES (?, 10)`, id)
	exec(t, s, `INSERT INTO reminders (key, account_id, fire_at, event_start, payload, state, updated_at) VALUES (?, ?, 0, 0, '{}', 'pending', 0)`, id+"|uid|t|10", id)
	exec(t, s, `INSERT INTO link_codes (code, account_id, expires_at) VALUES (?, ?, 0)`, "C"+sub, id)
	return id
}

func TestMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	s := open(t, path)
	addAccount(t, s, "a", 1)
	s.Close()
	s = open(t, path) // second Open must not re-run migrations or lose data
	if n := count(t, s, `SELECT count(*) FROM accounts`); n != 1 {
		t.Fatalf("accounts = %d", n)
	}
	var v int
	s.DB.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != 1 {
		t.Fatalf("user_version = %d", v)
	}
	var mode string
	s.DB.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Fatalf("journal_mode = %s", mode)
	}
}

func TestAccountIDsNotReused(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "db"))
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		id := addAccount(t, s, "same-sub", 1)
		if seen[id] || len(id) != 32 {
			t.Fatalf("bad or reused id %q", id)
		}
		seen[id] = true
		exec(t, s, `DELETE FROM accounts WHERE id = ?`, id)
	}
}

func TestCascadeDelete(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "db"))
	id := addAccount(t, s, "a", 1)
	keep := addAccount(t, s, "b", 2)
	exec(t, s, `INSERT INTO handled_messages (message_id, handled_at) VALUES (1, 0)`)
	exec(t, s, `INSERT INTO tombstones (google_sub, created_at) VALUES ('a', 0)`)
	exec(t, s, `DELETE FROM accounts WHERE id = ?`, id)
	for _, tbl := range []string{"settings", "reminders", "link_codes"} {
		if n := count(t, s, `SELECT count(*) FROM `+tbl+` WHERE account_id = ?`, id); n != 0 {
			t.Errorf("%s: %d rows left", tbl, n)
		}
		if n := count(t, s, `SELECT count(*) FROM `+tbl+` WHERE account_id = ?`, keep); n != 1 {
			t.Errorf("%s: other account lost its row", tbl)
		}
	}
	// Not tied to accounts.
	if count(t, s, `SELECT count(*) FROM handled_messages`)+count(t, s, `SELECT count(*) FROM tombstones`) != 2 {
		t.Error("handled_messages/tombstones must survive account deletion")
	}
}

func TestGuardedWrites(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "db"))
	id := addAccount(t, s, "a", 1)
	ran := false
	mark := func(*sql.Tx) error { ran = true; return nil }

	if err := s.WithScheduleRev(ctx, id, 0, mark); err != nil || !ran {
		t.Fatalf("fresh rev: %v ran=%v", err, ran)
	}
	for _, tc := range []struct{ name, change, undo string }{
		{"rev bumped", `schedule_rev = 1`, `schedule_rev = 0`},
		{"paused", `paused = 1`, `paused = 0`},
		{"unlinked", `zulip_user_id = NULL`, `zulip_user_id = 1`},
		{"disconnected", `status = 'disconnected'`, `status = 'connected'`},
	} {
		exec(t, s, `UPDATE accounts SET `+tc.change)
		ran = false
		if err := s.WithScheduleRev(ctx, id, 0, mark); !errors.Is(err, ErrStale) || ran {
			t.Errorf("%s: want ErrStale without running fn, got %v ran=%v", tc.name, err, ran)
		}
		exec(t, s, `UPDATE accounts SET `+tc.undo)
	}

	exec(t, s, `UPDATE accounts SET token_rev = 2`)
	if err := s.WithTokenRev(ctx, id, 1, mark); !errors.Is(err, ErrStale) {
		t.Fatalf("stale token_rev: %v", err)
	}
	err := s.WithTokenRev(ctx, id, 2, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE accounts SET status = 'disconnected' WHERE id = ?`, id)
		return err
	})
	if err != nil || count(t, s, `SELECT count(*) FROM accounts WHERE status = 'disconnected'`) != 1 {
		t.Fatalf("token_rev write: %v", err)
	}

	// fn error rolls back.
	boom := errors.New("boom")
	err = s.WithTokenRev(ctx, id, 2, func(tx *sql.Tx) error {
		tx.Exec(`DELETE FROM reminders`)
		return boom
	})
	if !errors.Is(err, boom) || count(t, s, `SELECT count(*) FROM reminders`) != 1 {
		t.Fatalf("rollback: %v", err)
	}
	if err := s.WithTokenRev(ctx, "nope", 0, mark); !errors.Is(err, ErrStale) {
		t.Fatalf("missing account: %v", err)
	}
}

func TestSendingResetOnStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	s := open(t, path)
	id := addAccount(t, s, "a", 1)
	exec(t, s, `UPDATE reminders SET state = 'sending'`)
	exec(t, s, `INSERT INTO reminders (key, account_id, fire_at, event_start, payload, state, updated_at) VALUES ('sent1', ?, 0, 0, '{}', 'sent', 0)`, id)
	s.Close()
	s = open(t, path)
	if n := count(t, s, `SELECT count(*) FROM reminders WHERE state = 'pending'`); n != 1 {
		t.Fatalf("pending = %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM reminders WHERE state = 'sent'`); n != 1 {
		t.Fatalf("sent rows must not change, got %d", n)
	}
}
