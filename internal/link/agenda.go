package link

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/korjavin/zulip-gcal-service/internal/poller"
	"github.com/korjavin/zulip-gcal-service/internal/sender"
	"github.com/korjavin/zulip-gcal-service/internal/zulip"
)

// Read-only bot commands "status" and "today" (docs/design.md §3.4).

func (l *Linker) statusText(ctx context.Context, id, status string) string {
	var email, timing string
	var lead, paused int
	l.St.DB.QueryRowContext(ctx, `SELECT a.google_email, a.paused, coalesce(s.timing, 'google'), coalesce(s.lead_minutes, 0)
		FROM accounts a LEFT JOIN settings s ON s.account_id = a.id WHERE a.id = ?`, id).Scan(&email, &paused, &timing, &lead)
	if status != "connected" {
		return "I lost access to Google Calendar for **" + sender.Escape(email) + "**, so reminders are off. Reconnect: " + l.PublicURL
	}
	r := "Connected as **" + sender.Escape(email) + "**.\n"
	if paused == 1 {
		r += "Reminders are **paused** (send **start**).\n"
	}
	if timing == "fixed" {
		r += fmt.Sprintf("Timing: %d min before each meeting.\n", lead)
	} else {
		r += "Timing: as set on each event in Google Calendar.\n"
	}
	ps := l.pending(ctx, id, "fire_at", 3)
	if len(ps) == 0 {
		return r + "No reminders pending."
	}
	r += "Next reminders:"
	for _, p := range ps {
		r += fmt.Sprintf("\n* **%s**, meeting at <time:%s>", sender.Escape(p.Title), p.Start.UTC().Format(time.RFC3339))
	}
	return r
}

// pending returns up to limit pending reminders' payloads, ordered by order.
func (l *Linker) pending(ctx context.Context, id, order string, limit int) (out []poller.Payload) {
	rows, err := l.St.DB.QueryContext(ctx, `SELECT payload FROM reminders WHERE account_id = ? AND state = 'pending' ORDER BY `+order+` LIMIT ?`, id, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var pj string
		var p poller.Payload
		if rows.Scan(&pj) == nil && json.Unmarshal([]byte(pj), &p) == nil {
			out = append(out, p)
		}
	}
	return out
}

// today lists the meetings still ahead before the end of the sender's local
// day, read live from Google Calendar (Zulip profile timezone, UTC if unknown).
func (l *Linker) today(ctx context.Context, zulipID int64, id string) string {
	now := l.now()
	loc := l.Zulip.UserLocation(ctx, zulipID)
	y, mo, d := now.In(loc).Date()
	ms, err := l.Meetings(ctx, id, now, time.Date(y, mo, d+1, 0, 0, 0, 0, loc))
	if err != nil { // never the error text: it may carry event data
		return "I couldn't read your calendar right now. Try again in a minute."
	}
	if len(ms) == 0 {
		return "No more meetings today."
	}
	return "Still ahead today:\n" + meetingList(ms)
}

func meetingList(ms []poller.Payload) string {
	lines := make([]string, len(ms))
	for i, p := range ms {
		lines[i] = fmt.Sprintf("* <time:%s> **%s**", p.Start.UTC().Format(time.RFC3339), sender.Escape(p.Title))
	}
	return strings.Join(lines, "\n")
}

func (l *Linker) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Daily agenda (opt-in on /settings): at the user's chosen local time, a DM
// listing the meetings still ahead that day. The local day is in zoneName's
// timezone, stored in settings and refreshed after each send.

const agendaLate = time.Hour // an agenda missed (service down) is still sent this long after its time

// RunAgenda sends due agendas every 30 s until ctx ends.
func (l *Linker) RunAgenda(ctx context.Context) {
	for {
		if err := l.AgendaTick(ctx); err != nil && ctx.Err() == nil {
			slog.Error("agenda tick", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

type agendaRow struct {
	id, tz, sent string
	zid, rev     int64
	minute       int
	workdays     bool
}

// agendaGuard makes a write lose to any pause, unlink or settings save
// (all bump schedule_rev) committed since the snapshot. Args: zid, rev.
const agendaGuard = ` AND EXISTS (SELECT 1 FROM accounts a WHERE a.id = settings.account_id
	AND a.status = 'connected' AND a.zulip_user_id = ? AND a.paused = 0 AND a.schedule_rev = ?)`

// AgendaTick sends every agenda that is due. Only DB failures are returned.
func (l *Linker) AgendaTick(ctx context.Context) error {
	rows, err := l.St.DB.QueryContext(ctx, `SELECT a.id, a.zulip_user_id, a.schedule_rev, s.agenda_minute, s.agenda_workdays,
		s.agenda_tz, s.agenda_sent FROM settings s JOIN accounts a ON a.id = s.account_id
		WHERE s.agenda_minute IS NOT NULL AND a.status = 'connected' AND a.zulip_user_id IS NOT NULL AND a.paused = 0`)
	if err != nil {
		return err
	}
	var due []agendaRow
	for rows.Next() {
		var r agendaRow
		if err := rows.Scan(&r.id, &r.zid, &r.rev, &r.minute, &r.workdays, &r.tz, &r.sent); err != nil {
			rows.Close()
			return err
		}
		due = append(due, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// ponytail: one account at a time, like the sender; fan out if a large realm needs it.
	for _, r := range due {
		if err := l.agenda(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

func (l *Linker) agenda(ctx context.Context, r agendaRow) error {
	if r.tz == "" {
		if r.tz = l.zoneName(ctx, r.zid, r.id); r.tz == "" {
			return nil // Zulip or Google not reachable: next tick
		}
		res, err := l.St.DB.ExecContext(ctx, `UPDATE settings SET agenda_tz = ? WHERE account_id = ? AND agenda_tz = ''`+agendaGuard,
			r.tz, r.id, r.zid, r.rev)
		if n, err2 := rowsAffected(res, err); err2 != nil || n == 0 {
			return err2 // changed meanwhile: next tick
		}
	}
	loc, err := time.LoadLocation(r.tz)
	if err != nil {
		loc = time.UTC
	}
	now := l.now().In(loc)
	y, mo, d := now.Date()
	at := time.Date(y, mo, d, 0, r.minute, 0, 0, loc)
	day := now.Format(time.DateOnly)
	if wd := now.Weekday(); r.sent == day || now.Before(at) || now.Sub(at) >= agendaLate ||
		r.workdays && (wd == time.Saturday || wd == time.Sunday) {
		return nil
	}
	ms, err := l.Meetings(ctx, r.id, now, time.Date(y, mo, d+1, 0, 0, 0, 0, loc))
	if err != nil {
		slog.Warn("agenda: calendar not readable, retrying next tick", "account", r.id) // never the error text: it may carry event data
		return nil
	}
	// The claim, after the fetch so nothing slow sits between it and the
	// send: at most one agenda per local day, and changes committed since
	// the snapshot win.
	res, err := l.St.DB.ExecContext(ctx, `UPDATE settings SET agenda_sent = ? WHERE account_id = ? AND agenda_sent = ? AND agenda_tz = ?`+agendaGuard,
		day, r.id, r.sent, r.tz, r.zid, r.rev)
	if n, err := rowsAffected(res, err); err != nil || n == 0 {
		return err
	}
	// ponytail: a crash between the claim and the send loses that day's agenda; accepted, it is a convenience.
	// Once: a 429 releases the claim; the retry passes the next tick's checks.
	_, err = l.Zulip.Once().SendDM(ctx, r.zid, agendaText(ms, l.PublicURL))
	if err != nil && !errors.Is(err, zulip.ErrRecipient) { // a deactivated recipient: the sender unlinks it
		slog.Warn("agenda not sent, retrying next tick", "account", r.id) // never the error text: it may carry event data
		_, err := l.St.DB.ExecContext(context.WithoutCancel(ctx), `UPDATE settings SET agenda_sent = ? WHERE account_id = ? AND agenda_sent = ?`,
			r.sent, r.id, day)
		return err
	}
	// The user may have moved: tomorrow's agenda follows the profile.
	if tz := l.zoneName(ctx, r.zid, r.id); tz != "" && tz != r.tz {
		_, err := l.St.DB.ExecContext(ctx, `UPDATE settings SET agenda_tz = ? WHERE account_id = ? AND agenda_tz = ?`+agendaGuard,
			tz, r.id, r.tz, r.zid, r.rev)
		return err
	}
	return nil
}

func rowsAffected(res sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// zoneName is the user's Zulip profile timezone, else their primary Google
// calendar's, else "UTC"; "" when Zulip or Google cannot be asked. Unlike
// UserLocation, a transient error must never store a fallback.
func (l *Linker) zoneName(ctx context.Context, zid int64, accountID string) string {
	valid := func(tz string) bool { _, err := time.LoadLocation(tz); return tz != "" && err == nil }
	u, err := l.Zulip.UserByID(ctx, zid)
	if errors.Is(err, zulip.ErrNotFound) {
		return "UTC" // deactivated: the send fails and the sender unlinks
	}
	if err != nil {
		return ""
	}
	if valid(u.Timezone) {
		return u.Timezone
	}
	if l.CalendarZone != nil {
		tz, err := l.CalendarZone(ctx, accountID)
		if err != nil {
			return "" // never the error text
		}
		if valid(tz) {
			return tz
		}
	}
	return "UTC"
}

func agendaText(ms []poller.Payload, publicURL string) string {
	r := "📅 No more meetings today."
	if len(ms) == 1 {
		r = "📅 Today: 1 meeting\n" + meetingList(ms)
	} else if len(ms) > 1 {
		r = fmt.Sprintf("📅 Today: %d meetings\n", len(ms)) + meetingList(ms)
	}
	return r + "\n\nDaily agenda settings: " + publicURL + "/settings"
}
