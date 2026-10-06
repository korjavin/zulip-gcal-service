// Package poller turns each account's Google Calendar events into reminder
// rows (docs/design.md §4), with the concurrency model of §3.1: fetch with no
// DB transaction open, then one write guarded by schedule_rev.
//
// Event titles and contents are never logged.
package poller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/korjavin/zulip-gcal-service/internal/lifecycle"
	"github.com/korjavin/zulip-gcal-service/internal/store"
)

const (
	window   = 26 * time.Hour
	catchUp  = time.Hour // overdue offsets fire once only if the meeting starts within this
	maxOff   = 1440      // minutes
	workers  = 4
	attempts = 3 // polls per trigger when the guarded write keeps finding a stale schedule_rev
)

// Reminder is a Google reminder (any method).
type Reminder struct {
	Minutes int `json:"minutes"`
}

// EventTime: DateTime is zero for all-day events (Date set instead).
type EventTime struct {
	DateTime time.Time `json:"dateTime"`
	Date     string    `json:"date"`
}

// Event is the subset of a Calendar API event the poller reads.
type Event struct {
	ID          string    `json:"id"`
	ICalUID     string    `json:"iCalUID"`
	Status      string    `json:"status"`
	Summary     string    `json:"summary"`
	Location    string    `json:"location"`
	HTMLLink    string    `json:"htmlLink"`
	HangoutLink string    `json:"hangoutLink"`
	Start       EventTime `json:"start"`
	End         EventTime `json:"end"`
	Attendees   []struct {
		Self           bool   `json:"self"`
		ResponseStatus string `json:"responseStatus"`
	} `json:"attendees"`
	Reminders *struct { // nil (absent) = calendar defaults
		UseDefault bool       `json:"useDefault"`
		Overrides  []Reminder `json:"overrides"`
	} `json:"reminders"`
	ConferenceData struct {
		EntryPoints []struct {
			EntryPointType string `json:"entryPointType"`
			URI            string `json:"uri"`
		} `json:"entryPoints"`
	} `json:"conferenceData"`
}

// Calendar is one watched calendar's complete snapshot (all pages).
type Calendar struct {
	ID               string     `json:"-"` // as in settings; "primary" is the user's primary calendar
	DefaultReminders []Reminder `json:"defaultReminders"`
	Items            []Event    `json:"items"`
}

type Settings struct {
	Timing       string // "google" or "fixed"
	LeadMinutes  int
	SkipDeclined bool
}

// Payload is what the sender renders; stored as JSON.
type Payload struct {
	Title    string    `json:"title"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Location string    `json:"location,omitempty"`
	Video    string    `json:"video,omitempty"`
	Link     string    `json:"link,omitempty"`
}

// Row is a desired reminder. State is "pending", "skipped", or "" for a
// started occurrence: never inserted, but an existing pending row survives.
type Row struct {
	Key     string
	FireAt  time.Time
	Start   time.Time
	Payload Payload
	State   string
}

// Desired computes the reminder rows an account should have (§4). cals are
// in the user's settings order. Pure.
func Desired(accountID string, cals []Calendar, s Settings, now time.Time) []Row {
	type occ struct {
		ev       *Event
		prec     int
		defaults []Reminder
	}
	occs := map[string]*occ{}
	var order []string
	for i, c := range cals {
		prec := i + 1
		if c.ID == "primary" {
			prec = 0
		}
		for j := range c.Items {
			ev := &c.Items[j]
			if ev.Status == "cancelled" || ev.Start.DateTime.IsZero() || (s.SkipDeclined && declined(ev)) {
				continue
			}
			uid := ev.ICalUID
			if uid == "" {
				uid = ev.ID
			}
			k := accountID + "|" + uid + "|" + ev.Start.DateTime.UTC().Format(time.RFC3339)
			if o := occs[k]; o == nil {
				occs[k] = &occ{ev, prec, c.DefaultReminders}
				order = append(order, k)
			} else if prec < o.prec {
				*o = occ{ev, prec, c.DefaultReminders}
			}
		}
	}
	var rows []Row
	for _, k := range order {
		o := occs[k]
		start := o.ev.Start.DateTime
		p := payload(o.ev)
		caught := false
		for _, off := range offsets(o.ev, o.defaults, s) { // ascending: the closest overdue offset fires
			r := Row{Key: k + "|" + strconv.Itoa(off), FireAt: start.Add(-time.Duration(off) * time.Minute),
				Start: start, Payload: p, State: "pending"}
			switch {
			case !start.After(now):
				r.State = ""
			case r.FireAt.After(now):
			case !caught && start.Sub(now) <= catchUp:
				caught, r.FireAt = true, now
			default:
				r.State = "skipped"
			}
			rows = append(rows, r)
		}
	}
	return rows
}

func declined(ev *Event) bool {
	for _, a := range ev.Attendees {
		if a.Self && a.ResponseStatus == "declined" {
			return true
		}
	}
	return false
}

func offsets(ev *Event, defaults []Reminder, s Settings) []int {
	if s.Timing == "fixed" {
		return []int{s.LeadMinutes}
	}
	rs := defaults
	if ev.Reminders != nil && !ev.Reminders.UseDefault {
		rs = ev.Reminders.Overrides // empty = explicitly no reminders
	}
	var out []int
	for _, r := range rs {
		if r.Minutes >= 0 && r.Minutes <= maxOff && !slices.Contains(out, r.Minutes) {
			out = append(out, r.Minutes)
		}
	}
	slices.Sort(out)
	return out
}

var httpsURL = regexp.MustCompile(`https://[^\s<>"]+`)

func payload(ev *Event) Payload {
	p := Payload{Title: ev.Summary, Start: ev.Start.DateTime.UTC(), End: ev.End.DateTime.UTC(),
		Location: ev.Location, Link: ev.HTMLLink}
	for _, e := range ev.ConferenceData.EntryPoints {
		if e.EntryPointType == "video" && e.URI != "" {
			p.Video = e.URI
			break
		}
	}
	if p.Video == "" {
		p.Video = ev.HangoutLink
	}
	if p.Video == "" {
		p.Video = httpsURL.FindString(ev.Location)
	}
	return p
}

// Poller polls every eligible account every Interval, and on demand
// (Trigger). At most one poll per account is in flight.
type Poller struct {
	St       *store.Store
	Tokens   func(ctx context.Context, accountID string) (oauth2.TokenSource, error)
	Interval time.Duration
	BaseURL  string // Calendar API base; tests point it at a fake
	Now      func() time.Time

	mu       sync.Mutex
	inflight map[string]bool // queued or polling; true = poll again when done
	jobs     chan string
	done     chan struct{}
}

func New(st *store.Store, tokens func(context.Context, string) (oauth2.TokenSource, error), interval time.Duration) *Poller {
	return &Poller{St: st, Tokens: tokens, Interval: interval, BaseURL: "https://www.googleapis.com/calendar/v3",
		Now: time.Now, inflight: map[string]bool{}, jobs: make(chan string), done: make(chan struct{})}
}

// Trigger asks for a poll of the account soon; never blocks. A trigger for
// an account already queued or polling schedules one more poll after it.
func (p *Poller) Trigger(accountID string) { p.enqueue(accountID, true) }

// enqueue queues an idle account; for a busy one, repoll says whether to
// poll it once more afterwards (on-demand: yes; periodic tick: no, so slow
// accounts cannot pin the workers).
func (p *Poller) enqueue(accountID string, repoll bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.inflight[accountID]; ok {
		if repoll {
			p.inflight[accountID] = true
		}
		return
	}
	p.inflight[accountID] = false
	go func() {
		select {
		case p.jobs <- accountID:
		case <-p.done:
		}
	}()
}

// Run polls until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	defer close(p.done)
	for range workers {
		go p.work(ctx)
	}
	for {
		p.tick(ctx)
		// ponytail: jitter is +0-10% per round; the worker pool bounds the rate.
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.Interval + rand.N(p.Interval/10+1)):
		}
	}
}

func (p *Poller) tick(ctx context.Context) {
	rows, err := p.St.DB.QueryContext(ctx, `SELECT id FROM accounts
		WHERE status = 'connected' AND zulip_user_id IS NOT NULL AND paused = 0`)
	if err != nil {
		slog.Error("poll: list accounts", "err", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		p.enqueue(id, false)
	}
}

func (p *Poller) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-p.jobs:
			for {
				p.pollRetry(ctx, id)
				p.mu.Lock()
				again := p.inflight[id]
				if again {
					p.inflight[id] = false
				} else {
					delete(p.inflight, id)
				}
				p.mu.Unlock()
				if !again {
					break
				}
			}
		}
	}
}

func (p *Poller) pollRetry(ctx context.Context, id string) {
	for range attempts {
		err := p.Poll(ctx, id)
		if errors.Is(err, store.ErrStale) {
			continue // schedule changed during the fetch: re-poll
		}
		if err != nil && ctx.Err() == nil {
			slog.Warn("poll failed, keeping previous reminders", "account", id, "err", err)
		}
		return
	}
}

// Poll fetches the account's next 26h and reconciles its reminders. An
// account that is not connected, linked and unpaused is a no-op. Any fetch
// error changes nothing. store.ErrStale: schedule_rev changed, result
// discarded.
func (p *Poller) Poll(ctx context.Context, accountID string) error {
	var rev int64
	var s Settings
	var calsJSON string
	err := p.St.DB.QueryRowContext(ctx, `SELECT a.schedule_rev, s.timing, s.lead_minutes, s.skip_declined, s.calendars
		FROM accounts a JOIN settings s ON s.account_id = a.id
		WHERE a.id = ? AND a.status = 'connected' AND a.zulip_user_id IS NOT NULL AND a.paused = 0`, accountID).
		Scan(&rev, &s.Timing, &s.LeadMinutes, &s.SkipDeclined, &calsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var ids []string
	if err := json.Unmarshal([]byte(calsJSON), &ids); err != nil {
		return fmt.Errorf("settings calendars: %w", err)
	}
	ts, err := p.Tokens(ctx, accountID)
	if errors.Is(err, lifecycle.ErrNoAccess) {
		return nil
	}
	if err != nil {
		return err
	}
	// Not oauth2.NewClient: its ReuseTokenSource would skip ts on every
	// request, and with it the token source's invalidation check (disconnect).
	client := &http.Client{Timeout: 30 * time.Second, Transport: &oauth2.Transport{Source: ts}}
	now := p.Now()
	cals := make([]Calendar, len(ids))
	for i, id := range ids {
		if cals[i], err = p.fetch(ctx, client, id, now); err != nil {
			if errors.Is(err, lifecycle.ErrNoAccess) {
				return nil // LostAccess already ran
			}
			return err
		}
	}
	now = p.Now() // fetching takes time; catch-up decisions use the clock at commit
	rows := Desired(accountID, cals, s, now)
	return p.St.WithScheduleRev(ctx, accountID, rev, func(tx *sql.Tx) error {
		if err := reconcile(ctx, tx, accountID, rows, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE accounts SET last_poll_ok_at = ? WHERE id = ?`, now.Unix(), accountID)
		return err
	})
}

const fields = "nextPageToken,defaultReminders(minutes),items(id,iCalUID,status,summary,location,htmlLink,hangoutLink," +
	"start,end,attendees(self,responseStatus),reminders,conferenceData/entryPoints(entryPointType,uri))"

func (p *Poller) fetch(ctx context.Context, client *http.Client, calID string, now time.Time) (Calendar, error) {
	c := Calendar{ID: calID}
	q := url.Values{
		"singleEvents": {"true"},
		"timeMin":      {now.UTC().Format(time.RFC3339)},
		"timeMax":      {now.Add(window).UTC().Format(time.RFC3339)},
		"maxAttendees": {"1"}, // more attendees → Google returns only the user's own entry
		"fields":       {fields},
	}
	for {
		req, err := http.NewRequestWithContext(ctx, "GET", p.BaseURL+"/calendars/"+url.PathEscape(calID)+"/events?"+q.Encode(), nil)
		if err != nil {
			return c, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return c, err
		}
		var page struct {
			Calendar
			NextPageToken string `json:"nextPageToken"`
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return c, fmt.Errorf("calendar API: %s", resp.Status) // body may echo event data: not logged
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return c, fmt.Errorf("calendar API: decode response: %T", err) // syntax errors quote input
		}
		c.Items = append(c.Items, page.Items...)
		if c.DefaultReminders == nil {
			c.DefaultReminders = page.DefaultReminders
		}
		if page.NextPageToken == "" {
			return c, nil
		}
		q.Set("pageToken", page.NextPageToken)
	}
}

// reconcile applies desired rows (§4): insert new keys, refresh the payload
// of pending rows (keeping fire_at), delete pending rows no longer desired;
// rows in any other state are history and never change.
func reconcile(ctx context.Context, tx *sql.Tx, accountID string, rows []Row, now time.Time) error {
	existing := map[string]string{}
	rs, err := tx.QueryContext(ctx, `SELECT key, state FROM reminders WHERE account_id = ?`, accountID)
	if err != nil {
		return err
	}
	for rs.Next() {
		var k, st string
		if err := rs.Scan(&k, &st); err != nil {
			rs.Close()
			return err
		}
		existing[k] = st
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return err
	}
	for _, r := range rows {
		pj, err := json.Marshal(r.Payload)
		if err != nil {
			return err
		}
		st, ok := existing[r.Key]
		delete(existing, r.Key)
		switch {
		case st == "pending":
			_, err = tx.ExecContext(ctx, `UPDATE reminders SET payload = ?, updated_at = ? WHERE key = ? AND state = 'pending'`,
				pj, now.Unix(), r.Key)
		case !ok && r.State != "":
			_, err = tx.ExecContext(ctx, `INSERT INTO reminders (key, account_id, fire_at, event_start, payload, state, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, r.Key, accountID, r.FireAt.Unix(), r.Start.Unix(), pj, r.State, now.Unix())
		}
		if err != nil {
			return err
		}
	}
	for k, st := range existing {
		if st != "pending" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM reminders WHERE key = ? AND state = 'pending'`, k); err != nil {
			return err
		}
	}
	return nil
}
