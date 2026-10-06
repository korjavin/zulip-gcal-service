package zulip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/korjavin/zulip-gcal-service/internal/store"
)

// Message is a new one-to-one DM to the bot from a human.
type Message struct {
	ID, SenderID int64
	Time         time.Time
	Text         string // raw, trimmed; never log it
}

// Handler applies one message. It records the receipt with
// store.MarkHandled in the same transaction as its effect (or in its own
// transaction before a reply that writes nothing), and skips the effect when
// MarkHandled returns false.
type Handler func(context.Context, Message) error

// Bot long-polls the bot's event queue and hands each new one-to-one DM to
// the handler, sequentially.
type Bot struct {
	c       *Client
	st      *store.Store
	botID   int64
	handle  Handler
	healthy atomic.Bool

	queueID   string
	lastEvent int64
	lastPurge time.Time
	maxWait   time.Duration // backoff cap
}

func NewBot(c *Client, st *store.Store, botID int64, h Handler) *Bot {
	return &Bot{c: c, st: st, botID: botID, handle: h, maxWait: time.Minute}
}

// Healthy reports whether the last queue request succeeded.
func (b *Bot) Healthy() bool { return b.healthy.Load() }

// Run polls until ctx is done.
func (b *Bot) Run(ctx context.Context) error {
	wait := min(time.Second, b.maxWait)
	for ctx.Err() == nil {
		err := b.step(ctx)
		var e *Error
		switch {
		case err == nil:
			b.healthy.Store(true)
			wait = min(time.Second, b.maxWait)
			continue
		case errors.As(err, &e) && e.Code == "BAD_EVENT_QUEUE_ID":
			slog.Info("zulip event queue expired, re-registering")
			b.queueID = ""
			continue
		case ctx.Err() != nil:
			return ctx.Err()
		}
		b.healthy.Store(false)
		slog.Warn("zulip event queue", "err", err)
		d := wait
		if errors.As(err, &e) && e.RetryAfter > d {
			d = e.RetryAfter
		}
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
		wait = min(wait*2, b.maxWait)
	}
	return ctx.Err()
}

func (b *Bot) step(ctx context.Context) error {
	if time.Since(b.lastPurge) > time.Hour {
		if err := b.st.PurgeHandled(ctx, time.Now()); err != nil {
			return err
		}
		b.lastPurge = time.Now()
	}
	if b.queueID == "" {
		var r struct {
			QueueID   string `json:"queue_id"`
			LastEvent int64  `json:"last_event_id"`
		}
		err := b.c.call(ctx, "POST", "register", url.Values{
			"event_types":    {`["message"]`},
			"narrow":         {`[["is","dm"]]`},
			"apply_markdown": {"false"},
		}, &r)
		if err != nil {
			return fmt.Errorf("register: %w", err)
		}
		if r.QueueID == "" {
			return errors.New("register: no queue_id")
		}
		b.queueID, b.lastEvent = r.QueueID, r.LastEvent
		return nil
	}
	var r struct {
		Events []struct {
			ID      int64           `json:"id"`
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		} `json:"events"`
	}
	err := b.c.call(ctx, "GET", "events", url.Values{
		"queue_id":      {b.queueID},
		"last_event_id": {strconv.FormatInt(b.lastEvent, 10)},
	}, &r)
	if err != nil {
		return err
	}
	for _, ev := range r.Events {
		if ev.Type == "message" { // never update_message: edits run nothing
			if err := b.consider(ctx, ev.Message); err != nil {
				// Not acknowledged: the queue hands this event back next time.
				return err
			}
		}
		b.lastEvent = max(b.lastEvent, ev.ID)
	}
	return nil
}

// consider passes m to the handler if it is a new one-to-one DM from a
// human. Only errors worth retrying the event for are returned.
func (b *Bot) consider(ctx context.Context, raw json.RawMessage) error {
	var m struct {
		ID         int64           `json:"id"`
		Type       string          `json:"type"`
		SenderID   int64           `json:"sender_id"`
		Timestamp  int64           `json:"timestamp"`
		Content    string          `json:"content"`
		Recipients json.RawMessage `json:"display_recipient"` // a string for channel messages
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Type != "private" || m.SenderID == b.botID {
		return nil
	}
	var rcpt []struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal(m.Recipients, &rcpt) != nil || len(rcpt) != 2 {
		return nil // group DM (or a DM to itself)
	}
	if !(rcpt[0].ID == b.botID && rcpt[1].ID == m.SenderID || rcpt[1].ID == b.botID && rcpt[0].ID == m.SenderID) {
		return nil
	}
	if done, err := b.st.Handled(ctx, m.ID); err != nil || done {
		return err
	}
	if _, err := b.c.UserByID(ctx, m.SenderID); errors.Is(err, ErrNotFound) {
		return nil // another bot
	} else if err != nil {
		return err
	}
	msg := Message{ID: m.ID, SenderID: m.SenderID, Time: time.Unix(m.Timestamp, 0), Text: strings.TrimSpace(m.Content)}
	if err := b.handle(ctx, msg); err != nil {
		// ponytail: a failing handler drops the message (the user is told to
		// resend); retrying it would block every later DM behind a poison one.
		slog.Error("zulip message handler", "message_id", m.ID, "err", err)
	}
	return nil
}
