// Package store owns the SQLite database: connection setup, embedded
// migrations and the guarded (conditional) writes of docs/design.md §3.1.
// There are no mutexes: every write checks the state it was based on.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrStale means a guarded write found the account's revision (or state)
// changed since it was read; the caller discards its result.
var ErrStale = errors.New("stale")

type Store struct {
	DB *sql.DB
}

// Open opens (creating if needed) the database file, applies pending
// migrations and resets reminders left in "sending" by a previous run.
func Open(ctx context.Context, path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Set("_txlock", "immediate") // take the write lock at BEGIN: no upgrade deadlocks
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	s := &Store{DB: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// Possible duplicate, never a loss (design §3.1).
	if _, err := db.ExecContext(ctx, `UPDATE reminders SET state = 'pending' WHERE state = 'sending'`); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

// migrate applies migrations/NNN_*.sql in name order; PRAGMA user_version
// records how many have run.
func (s *Store) migrate(ctx context.Context) error {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	var version int
	if err := s.DB.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(names); i++ {
		body, err := migrations.ReadFile(names[i])
		if err != nil {
			return err
		}
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: %w", names[i], err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// NewAccountID returns a random 128-bit id; ids are never derived from
// rowids, so a deleted account's id is never handed out again.
func NewAccountID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// WithScheduleRev runs fn in one transaction only if the account still has
// schedule_rev = rev and is connected, linked and not paused; otherwise it
// returns ErrStale and fn does not run.
func (s *Store) WithScheduleRev(ctx context.Context, accountID string, rev int64, fn func(*sql.Tx) error) error {
	return s.guarded(ctx, `id = ? AND schedule_rev = ? AND status = 'connected'
		AND zulip_user_id IS NOT NULL AND paused = 0`, fn, accountID, rev)
}

// WithTokenRev runs fn in one transaction only if the account still has
// token_rev = rev; otherwise it returns ErrStale and fn does not run.
func (s *Store) WithTokenRev(ctx context.Context, accountID string, rev int64, fn func(*sql.Tx) error) error {
	return s.guarded(ctx, `id = ? AND token_rev = ?`, fn, accountID, rev)
}

func (s *Store) guarded(ctx context.Context, where string, fn func(*sql.Tx) error, args ...any) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// A no-op UPDATE: rows affected says whether the guard holds, and the
	// write lock is already ours (BEGIN IMMEDIATE) until commit.
	res, err := tx.ExecContext(ctx, `UPDATE accounts SET id = id WHERE `+where, args...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrStale
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
