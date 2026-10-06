// Package lifecycle holds the account lifecycle operations of
// docs/design.md §3.3 — pause, resume, disconnect, lost access — used by the
// web pages and the bot commands alike, plus the Google token source every
// Google call goes through.
package lifecycle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"

	"github.com/korjavin/zulip-gcal-service/internal/auth"
	"github.com/korjavin/zulip-gcal-service/internal/config"
	"github.com/korjavin/zulip-gcal-service/internal/store"
	"github.com/korjavin/zulip-gcal-service/internal/web"
	"github.com/korjavin/zulip-gcal-service/internal/zulip"
)

// ErrNoAccess means the account is gone or has lost its Google access.
var ErrNoAccess = errors.New("no google access")

// TxHook runs inside an operation's write, e.g. store.MarkHandled for a bot
// command (§3.4). An error rolls the whole write back.
type TxHook func(*sql.Tx) error

type Ops struct {
	st        *store.Store
	secrets   *config.Secrets
	zc        *zulip.Client
	publicURL string
	oauth     *oauth2.Config
	httpCtx   context.Context // carries the HTTP client for token refreshes
	revokeURL string

	// Poll asks for an immediate poll of an account (resume); poller.Trigger. nil = none.
	Poll func(accountID string)

	mu    sync.Mutex
	cache map[string]*guarded // account id -> token source of its current token_rev
}

func New(cfg *config.Config, st *store.Store, zc *zulip.Client) *Ops {
	return &Ops{
		st: st, secrets: cfg.Secrets, zc: zc, publicURL: cfg.PublicURL,
		oauth: &oauth2.Config{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			Endpoint:     oauth2.Endpoint{TokenURL: auth.Google.TokenURL, AuthStyle: oauth2.AuthStyleInParams},
		},
		httpCtx:   context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Timeout: 30 * time.Second}),
		revokeURL: "https://oauth2.googleapis.com/revoke",
		cache:     map[string]*guarded{},
	}
}

// TokenSource returns the account's Google token source; access tokens are
// cached in memory per (account, token_rev). A refresh that Google answers
// with invalid_grant runs LostAccess and fails with ErrNoAccess.
func (o *Ops) TokenSource(ctx context.Context, accountID string) (oauth2.TokenSource, error) {
	// Read and publish under the lock, so a Disconnect/LostAccess commit is
	// either seen here or followed by a forget that kills what we publish.
	// ponytail: one lock for all accounts, per-account locks if it ever contends.
	o.mu.Lock()
	defer o.mu.Unlock()
	var enc []byte
	var rev int64
	var status string
	err := o.st.DB.QueryRowContext(ctx, `SELECT enc_refresh_token, token_rev, status FROM accounts WHERE id = ?`,
		accountID).Scan(&enc, &rev, &status)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (status != "connected" || enc == nil)) {
		return nil, ErrNoAccess
	}
	if err != nil {
		return nil, err
	}
	old := o.cache[accountID]
	if old != nil && old.rev == rev {
		return old, nil
	}
	rt, err := o.secrets.Decrypt(enc)
	if err != nil {
		return nil, err
	}
	o.killLocked(accountID, rev-1)
	ts := &guarded{o: o, id: accountID, rev: rev,
		ts: o.oauth.TokenSource(o.httpCtx, &oauth2.Token{RefreshToken: string(rt)})}
	o.cache[accountID] = ts
	return ts, nil
}

type guarded struct {
	o    *Ops
	id   string
	rev  int64
	ts   oauth2.TokenSource
	dead atomic.Bool // superseded, disconnected or lost: never hand out a token again
}

func (g *guarded) Token() (*oauth2.Token, error) {
	if g.dead.Load() {
		return nil, ErrNoAccess
	}
	t, err := g.ts.Token()
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if lerr := g.o.LostAccess(ctx, g.id, g.rev); lerr != nil {
			slog.Error("lost access", "err", lerr)
		}
		return nil, fmt.Errorf("%w: %w", ErrNoAccess, err)
	}
	if g.dead.Load() { // invalidated while refreshing
		return nil, ErrNoAccess
	}
	return t, err
}

// forget invalidates the account's token source if its token_rev <= upTo.
func (o *Ops) forget(accountID string, upTo int64) {
	o.mu.Lock()
	o.killLocked(accountID, upTo)
	o.mu.Unlock()
}

func (o *Ops) killLocked(accountID string, upTo int64) {
	if g := o.cache[accountID]; g != nil && g.rev <= upTo {
		g.dead.Store(true)
		delete(o.cache, accountID)
	}
}

const cancelReminders = `DELETE FROM reminders WHERE account_id = ? AND state IN ('pending', 'sending')`

// LostAccess marks the account disconnected after Google rejected the
// refresh token of token_rev rev. A stale failure (token_rev changed since)
// or a repeat does nothing; otherwise the linked user gets one DM.
func (o *Ops) LostAccess(ctx context.Context, accountID string, rev int64) error {
	var zulipID sql.NullInt64
	err := o.write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `UPDATE accounts SET status = 'disconnected', schedule_rev = schedule_rev + 1
			WHERE id = ? AND token_rev = ? AND status = 'connected' RETURNING zulip_user_id`, accountID, rev).Scan(&zulipID)
		if err != nil {
			return err // sql.ErrNoRows: stale or already handled
		}
		_, err = tx.ExecContext(ctx, cancelReminders, accountID)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	o.forget(accountID, rev) // a newer grant's source stays
	if zulipID.Valid {
		msg := "I lost access to your Google Calendar, reminders are off. Reconnect: " + o.publicURL
		if _, err := o.zc.SendDM(ctx, zulipID.Int64, msg); err != nil {
			slog.Warn("lost-access DM failed", "err", err)
		}
	}
	return nil
}

// Pause stops reminders, keeping calendar access. changed is false when the
// account was already paused.
func (o *Ops) Pause(ctx context.Context, accountID string, hooks ...TxHook) (changed bool, err error) {
	return o.setPaused(ctx, accountID, true, hooks)
}

// Resume restarts reminders and triggers a poll. changed is false when the
// account was not paused.
func (o *Ops) Resume(ctx context.Context, accountID string, hooks ...TxHook) (changed bool, err error) {
	changed, err = o.setPaused(ctx, accountID, false, hooks)
	if changed && o.Poll != nil {
		o.Poll(accountID)
	}
	return changed, err
}

func (o *Ops) setPaused(ctx context.Context, accountID string, paused bool, hooks []TxHook) (changed bool, err error) {
	err = o.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE accounts SET paused = ?, schedule_rev = schedule_rev + 1
			WHERE id = ? AND paused <> ?`, paused, accountID, paused)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if changed = n == 1; changed {
			if _, err := tx.ExecContext(ctx, cancelReminders, accountID); err != nil {
				return err
			}
		}
		return runHooks(tx, hooks)
	})
	return changed && err == nil, err
}

// Disconnect deletes the account and everything stored for it in one write
// (with a revoking tombstone for its Google sub, §3.3), then tries to revoke
// the grant at Google. revoked reports whether Google confirmed it. An
// account that no longer exists yields ErrNoAccess.
func (o *Ops) Disconnect(ctx context.Context, accountID string, hooks ...TxHook) (revoked bool, err error) {
	var sub string
	var enc []byte
	err = o.write(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix() // taken under the write lock (BEGIN IMMEDIATE)
		err := tx.QueryRowContext(ctx, `DELETE FROM accounts WHERE id = ? RETURNING google_sub, enc_refresh_token`,
			accountID).Scan(&sub, &enc)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoAccess
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tombstones (google_sub, created_at, revoking) VALUES (?, ?, 1)
			ON CONFLICT (google_sub) DO UPDATE SET created_at = excluded.created_at, revoking = 1`, sub, now); err != nil {
			return err
		}
		return runHooks(tx, hooks)
	})
	if err != nil {
		return false, err
	}
	o.forget(accountID, math.MaxInt64)
	if enc != nil {
		if rt, derr := o.secrets.Decrypt(enc); derr == nil {
			revoked = o.revoke(ctx, string(rt))
		}
	}
	// Not cancelled with the request: a tombstone stuck in "revoking" would
	// block sign-in until it is purged.
	if _, err := o.st.DB.ExecContext(context.WithoutCancel(ctx),
		`UPDATE tombstones SET revoking = 0 WHERE google_sub = ?`, sub); err != nil {
		slog.Error("clear tombstone", "err", err)
	}
	return revoked, nil
}

var revokeTimeout = 5 * time.Second // tests shorten it

func (o *Ops) revoke(ctx context.Context, token string) bool {
	ctx, cancel := context.WithTimeout(ctx, revokeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", o.revokeURL, strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Warn("google revoke failed", "err", err) // url.Error carries the URL, not the form body
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (o *Ops) write(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := o.st.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func runHooks(tx *sql.Tx, hooks []TxHook) error {
	for _, h := range hooks {
		if err := h(tx); err != nil {
			return err
		}
	}
	return nil
}

// Register serves POST /disconnect; account is the session check
// (auth.Auth.Account).
func (o *Ops) Register(mux *http.ServeMux, account func(*http.Request) (string, bool)) {
	// CSRF: stdlib Sec-Fetch-Site/Origin check, as for /logout.
	mux.Handle("POST /disconnect", http.NewCrossOriginProtection().Handler(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			id, ok := account(r)
			if !ok {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			revoked, err := o.Disconnect(r.Context(), id)
			if errors.Is(err, ErrNoAccess) { // a concurrent disconnect won
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			if err != nil {
				slog.Error("disconnect", "err", err)
				web.Render(w, http.StatusInternalServerError, "disconnected", nil)
				return
			}
			web.Render(w, http.StatusOK, "disconnected", map[string]bool{"Revoked": revoked})
		})))
}
