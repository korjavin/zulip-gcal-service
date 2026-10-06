package link

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/korjavin/zulip-gcal-service/internal/poller"
	"github.com/korjavin/zulip-gcal-service/internal/sender"
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

// today lists meetings with a pending reminder that start before the end of
// the sender's local day (Zulip profile timezone, UTC if unknown).
// ponytail: a meeting with no pending reminder (already reminded, or beyond
// the poll window) is not listed; query the calendar live if that matters.
func (l *Linker) today(ctx context.Context, zulipID int64, id string) string {
	loc := time.UTC
	if u, err := l.Zulip.UserByID(ctx, zulipID); err == nil && u.Timezone != "" {
		if tz, err := time.LoadLocation(u.Timezone); err == nil {
			loc = tz
		}
	}
	now := time.Now()
	y, mo, d := now.In(loc).Date()
	end := time.Date(y, mo, d+1, 0, 0, 0, 0, loc)
	var lines []string
	seen := map[string]bool{}
	for _, p := range l.pending(ctx, id, "event_start, fire_at", 1000) { // several offsets of one event repeat
		k := p.Title + p.Start.String()
		if !p.Start.Before(end) || p.Start.Before(now) || seen[k] { // pending rows outlive the start by the send grace
			continue
		}
		seen[k] = true
		lines = append(lines, fmt.Sprintf("* <time:%s> **%s**", p.Start.UTC().Format(time.RFC3339), sender.Escape(p.Title)))
	}
	if len(lines) == 0 {
		return "No more meetings today."
	}
	return "Still ahead today:\n" + strings.Join(lines, "\n")
}
