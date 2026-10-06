// Package sender delivers due reminders as Zulip DMs (docs/design.md §3.1,
// §5): claim with one conditional write, send with no transaction open, then
// write the outcome only if the row is still "sending".
//
// Event titles and message contents are never logged.
package sender

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/korjavin/zulip-gcal-service/internal/poller"
	"github.com/korjavin/zulip-gcal-service/internal/store"
	"github.com/korjavin/zulip-gcal-service/internal/zulip"
)

const (
	interval = 30 * time.Second
	grace    = 2 * time.Minute // deliverable until event start + grace
	keep     = 7 * 24 * time.Hour
)

type Sender struct {
	St    *store.Store
	Zulip *zulip.Client
	Now   func() time.Time
}

func New(st *store.Store, zc *zulip.Client) *Sender {
	return &Sender{St: st, Zulip: zc, Now: time.Now}
}

// Run ticks every 30 s until ctx ends.
func (s *Sender) Run(ctx context.Context) {
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.Error("sender tick", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Tick expires overdue rows, delivers due ones and purges old history.
func (s *Sender) Tick(ctx context.Context) error {
	now := s.Now()
	deadline := now.Add(-grace).Unix() // rows whose event started before this are no longer deliverable
	if _, err := s.St.DB.ExecContext(ctx, `UPDATE reminders SET state = 'expired', updated_at = ?
		WHERE state = 'pending' AND event_start < ?`, now.Unix(), deadline); err != nil {
		return err
	}
	if _, err := s.St.DB.ExecContext(ctx, `DELETE FROM reminders
		WHERE state IN ('sent', 'expired', 'skipped') AND updated_at < ?`, now.Add(-keep).Unix()); err != nil {
		return err
	}
	rows, err := s.St.DB.QueryContext(ctx, `SELECT key FROM reminders
		WHERE state = 'pending' AND fire_at <= ? ORDER BY fire_at`, now.Unix())
	if err != nil {
		return err
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// ponytail: one DM at a time; a slow Zulip delays the rest of the tick.
	// Fan out if a realm's per-minute burst ever needs it.
	for _, k := range keys {
		if err := s.deliver(ctx, k); err != nil {
			return err
		}
	}
	return nil
}

// deliver claims one row and sends it. Only DB failures are returned.
func (s *Sender) deliver(ctx context.Context, key string) error {
	var accountID, pj string
	var to int64
	// The claim: anything committed before it (pause, unlink, cancellation)
	// wins; zulip_user_id is read in the same statement.
	err := s.St.DB.QueryRowContext(ctx, `UPDATE reminders SET state = 'sending', updated_at = ?
		WHERE key = ? AND state = 'pending' AND EXISTS (SELECT 1 FROM accounts a WHERE a.id = reminders.account_id
			AND a.status = 'connected' AND a.zulip_user_id IS NOT NULL AND a.paused = 0)
		RETURNING account_id, payload, (SELECT zulip_user_id FROM accounts a WHERE a.id = reminders.account_id)`,
		s.Now().Unix(), key).Scan(&accountID, &pj, &to)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // not claimed: skip
	}
	if err != nil {
		return err
	}
	var p poller.Payload
	if err := json.Unmarshal([]byte(pj), &p); err != nil {
		return err
	}
	// ponytail: a crash or an ambiguous timeout between send and the write
	// below can deliver twice (the row goes back to pending); accepted, a loss is not.
	msgID, sendErr := s.Zulip.SendDM(ctx, to, Render(p, s.Now()))
	wctx := context.WithoutCancel(ctx) // record the outcome even while shutting down
	now := s.Now().Unix()
	switch {
	case sendErr == nil:
		_, err = s.St.DB.ExecContext(wctx, `UPDATE reminders SET state = 'sent', zulip_message_id = ?, updated_at = ?
			WHERE key = ? AND state = 'sending'`, msgID, now, key)
	case errors.Is(sendErr, zulip.ErrRecipient):
		slog.Warn("reminder recipient cannot receive DMs, unlinking account", "account", accountID)
		err = s.unlink(wctx, key, accountID, to)
	default:
		slog.Warn("reminder send failed, retrying next tick", "account", accountID, "err", logErr(sendErr))
		_, err = s.St.DB.ExecContext(wctx, `UPDATE reminders SET state = 'pending', updated_at = ?
			WHERE key = ? AND state = 'sending'`, now, key)
	}
	return err
}

// unlink handles a deactivated recipient: the row is dropped, and if the
// account is still linked to that Zulip user it is unlinked (schedule_rev
// bumped, pending reminders deleted) in the same write.
func (s *Sender) unlink(ctx context.Context, key, accountID string, zulipUserID int64) error {
	tx, err := s.St.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM reminders WHERE key = ? AND state = 'sending'`, key); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE accounts SET zulip_user_id = NULL, linked_at = NULL, schedule_rev = schedule_rev + 1
		WHERE id = ? AND zulip_user_id = ?`, accountID, zulipUserID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 1 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM reminders WHERE account_id = ? AND state IN ('pending', 'sending')`, accountID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// logErr keeps Zulip's free-text error message out of the log.
func logErr(err error) string {
	var e *zulip.Error
	if errors.As(err, &e) {
		return fmt.Sprintf("zulip HTTP %d %s", e.Status, e.Code)
	}
	return err.Error()
}

// Render is the reminder DM (§5); "in N min" is relative to now, the send time.
func Render(p poller.Payload, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📅 **%s** starts <time:%s> (%s)", escape(p.Title), p.Start.UTC().Format(time.RFC3339), until(p.Start.Sub(now)))
	var parts []string
	if p.Location != "" {
		parts = append(parts, "📍 "+escape(p.Location))
	}
	if p.Video != "" {
		label := "Join video call"
		if u, err := url.Parse(p.Video); err == nil && u.Host == "meet.google.com" {
			label = "Join Google Meet"
		}
		parts = append(parts, fmt.Sprintf("🎥 [%s](%s)", label, linkURL(p.Video)))
	}
	if p.Link != "" {
		parts = append(parts, fmt.Sprintf("[Open in Calendar](%s)", linkURL(p.Link)))
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	return b.String()
}

func until(d time.Duration) string {
	if d < time.Minute {
		return "starting now"
	}
	m := int(math.Round(d.Minutes()))
	if m < 60 {
		return fmt.Sprintf("in %d min", m)
	}
	if m%60 == 0 {
		return fmt.Sprintf("in %d h", m/60)
	}
	return fmt.Sprintf("in %d h %d min", m/60, m%60)
}

// mdEscaper backslash-escapes Python-Markdown's escapable characters (Zulip's
// renderer; others would show the backslash) and folds newlines so a title
// cannot start a block. Escaping '*' and '_' also defuses @**mentions**.
var mdEscaper = func() *strings.Replacer {
	var pairs []string
	for _, c := range "\\`*_{}[]()#+-.!>" {
		pairs = append(pairs, string(c), `\`+string(c))
	}
	return strings.NewReplacer(append(pairs, "\r\n", " ", "\n", " ", "\r", " ")...)
}()

func escape(s string) string { return mdEscaper.Replace(s) }

// linkURL keeps a URL inside markdown's (...) intact.
func linkURL(u string) string {
	return strings.NewReplacer("(", "%28", ")", "%29", " ", "%20").Replace(u)
}
