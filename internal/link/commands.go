package link

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/korjavin/zulip-gcal-service/internal/lifecycle"
	"github.com/korjavin/zulip-gcal-service/internal/poller"
	"github.com/korjavin/zulip-gcal-service/internal/sender"
	"github.com/korjavin/zulip-gcal-service/internal/store"
	"github.com/korjavin/zulip-gcal-service/internal/zulip"
)

// Bot DM commands (docs/design.md §3.4): the whole trimmed message,
// case-insensitive, optional leading "/".
var commands = map[string]string{
	"stop": "stop", "pause": "stop",
	"start": "start", "resume": "start",
	"disconnect": "disconnect",
	"help":       "help",
}

func command(text string) string {
	return commands[strings.ToLower(strings.TrimPrefix(text, "/"))]
}

var (
	errHandled = errors.New("message already handled")
	errChanged = errors.New("sender's link changed")
)

func (l *Linker) commandsHelp() string {
	return "Send me one word:\n" +
		"* **stop** — pause reminders (send **start** to resume)\n" +
		"* **disconnect** — delete everything this service stored about you and remove its Google access\n" +
		"Settings: " + l.PublicURL + "/settings\n" +
		"No reply from me within a minute? Send your message again or use the website."
}

// command runs cmd ("" = not a command: help) for the sender's account.
func (l *Linker) command(ctx context.Context, m zulip.Message, cmd string) error {
	for range 3 { // the sender's link changed under us (web link/disconnect): look again
		if err := l.runCommand(ctx, m, cmd); !errors.Is(err, errChanged) {
			return err
		}
	}
	return errChanged
}

func (l *Linker) runCommand(ctx context.Context, m zulip.Message, cmd string) error {
	var id, status string
	var linkedAt int64
	err := l.St.DB.QueryRowContext(ctx, `SELECT id, status, coalesce(linked_at, 0) FROM accounts WHERE zulip_user_id = ?`,
		m.SenderID).Scan(&id, &status, &linkedAt)
	if errors.Is(err, sql.ErrNoRows) {
		if cmd == "" || cmd == "help" {
			return l.replyOnce(ctx, m, l.help())
		}
		return l.replyOnce(ctx, m, "You are not connected. Open "+l.PublicURL+" and sign in with Google.")
	}
	if err != nil {
		return err
	}
	if m.Time.Unix() < linkedAt {
		return nil // older than the sender's current link: a replayed command must not hit it
	}
	// mark records the receipt in the operation's own write and checks the
	// account resolved above is still this sender's current link, in the
	// status the reply was chosen by (lost access meanwhile: look again).
	mark := func(tx *sql.Tx) error {
		fresh, err := store.MarkHandled(ctx, tx, m.ID, time.Now())
		if err != nil {
			return err
		}
		if !fresh {
			return errHandled
		}
		var same int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM accounts WHERE id = ? AND zulip_user_id = ? AND coalesce(linked_at, 0) = ? AND status = ?`,
			id, m.SenderID, linkedAt, status).Scan(&same); err != nil {
			return err
		}
		if same == 0 {
			return errChanged
		}
		return nil
	}
	var reply string
	switch cmd {
	case "stop":
		changed, err := l.Ops.Pause(ctx, id, mark)
		if err != nil {
			return skipHandled(err)
		}
		reply = "Already paused. Send **start** to resume."
		if changed {
			reply = "Reminders paused. Send **start** to resume."
		}
	case "start":
		if status != "connected" {
			return l.replyOnce(ctx, m, "I lost access to your Google Calendar, so reminders are off. Reconnect: "+l.PublicURL)
		}
		changed, err := l.Ops.Resume(ctx, id, mark)
		if err != nil {
			return skipHandled(err)
		}
		reply = "Reminders are on."
		if changed {
			reply = "Reminders resumed."
		}
		reply += l.next(ctx, id)
	case "disconnect":
		revoked, err := l.Ops.Disconnect(ctx, id, mark)
		if errors.Is(err, lifecycle.ErrNoAccess) {
			return errChanged // deleted meanwhile: look again
		}
		if err != nil {
			return skipHandled(err)
		}
		reply = "Disconnected. I deleted everything this service stored about you.\n"
		if revoked {
			reply += "Google access revoked.\n"
		} else {
			reply += "I could not confirm revocation with Google — you can remove access at https://myaccount.google.com/permissions\n"
		}
		reply += "Sign in again any time: " + l.PublicURL
	default:
		return l.replyOnce(ctx, m, l.commandsHelp())
	}
	return l.send(ctx, m.SenderID, reply)
}

func skipHandled(err error) error {
	if errors.Is(err, errHandled) {
		return nil
	}
	return err
}

// next describes the account's next pending reminder, "" if none is known
// (after a resume the poll that rebuilds them is still running).
func (l *Linker) next(ctx context.Context, accountID string) string {
	var pj string
	if l.St.DB.QueryRowContext(ctx, `SELECT payload FROM reminders WHERE account_id = ? AND state = 'pending'
		ORDER BY fire_at LIMIT 1`, accountID).Scan(&pj) != nil {
		return ""
	}
	var p poller.Payload
	if json.Unmarshal([]byte(pj), &p) != nil {
		return ""
	}
	return " Next reminder:\n" + sender.Render(p, time.Now())
}
