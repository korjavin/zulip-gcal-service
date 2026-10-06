// Package settings is the settings page (docs/design.md §1, §4 Timing):
// reminder timing, watched calendars, skip declined, pause, test reminder,
// disconnect. Calendar names and event contents are never logged.
package settings

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"golang.org/x/oauth2"

	"github.com/korjavin/zulip-gcal-service/internal/poller"
	"github.com/korjavin/zulip-gcal-service/internal/sender"
	"github.com/korjavin/zulip-gcal-service/internal/store"
	"github.com/korjavin/zulip-gcal-service/internal/web"
	"github.com/korjavin/zulip-gcal-service/internal/zulip"
)

type Page struct {
	St      *store.Store
	Zulip   *zulip.Client
	Account func(*http.Request) (string, bool)                                      // session check (auth.Auth.Account)
	CSRF    func(accountID string) string                                           // auth.Auth.CSRFToken
	Tokens  func(ctx context.Context, accountID string) (oauth2.TokenSource, error) // lifecycle.Ops.TokenSource
	Pause   func(ctx context.Context, accountID string) error
	Poll    func(accountID string) // poller.Trigger; nil = none
	BaseURL string                 // Calendar API; "" = Google
	Now     func() time.Time       // nil = time.Now
}

func (p *Page) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /settings", p.show)
	// CSRF: the Sec-Fetch-Site/Origin check plus the form token.
	cop := http.NewCrossOriginProtection()
	mux.Handle("POST /settings", cop.Handler(p.form(p.save)))
	mux.Handle("POST /test-reminder", cop.Handler(p.form(p.test)))
	mux.Handle("POST /pause", cop.Handler(p.form(p.pause)))
}

// form checks session and CSRF token, then calls h with the account id.
func (p *Page) form(h func(http.ResponseWriter, *http.Request, string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := p.Account(r)
		if !ok {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(p.CSRF(id))) != 1 {
			web.Error(w, http.StatusForbidden, web.Message{Title: "Page expired",
				Text: "Please open the settings page again and retry.", Button: "Open settings", URL: "/settings"})
			return
		}
		h(w, r, id)
	})
}

var leads = []int{1, 5, 10, 15, 30, 60}

type calendar struct {
	ID, Name, Color string
	Checked         bool
}

type view struct {
	CSRF           string
	Timing         string
	Lead           int
	Leads          []int
	SkipDeclined   bool
	Paused         bool
	Calendars      []calendar
	CalendarsError bool
	Saved, Tested  bool
}

func (p *Page) show(w http.ResponseWriter, r *http.Request) {
	id, ok := p.Account(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	ctx := r.Context()
	v := view{CSRF: p.CSRF(id), Leads: leads, Saved: r.URL.Query().Has("saved"), Tested: r.URL.Query().Has("tested")}
	var calsJSON string
	err := p.St.DB.QueryRowContext(ctx, `SELECT s.timing, s.lead_minutes, s.skip_declined, s.calendars, a.paused
		FROM settings s JOIN accounts a ON a.id = s.account_id WHERE a.id = ?`, id).
		Scan(&v.Timing, &v.Lead, &v.SkipDeclined, &calsJSON, &v.Paused)
	if errors.Is(err, sql.ErrNoRows) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if err != nil {
		slog.Error("settings page", "err", err)
		web.Error(w, http.StatusInternalServerError, web.Oops)
		return
	}
	if !slices.Contains(v.Leads, v.Lead) { // DEFAULT_LEAD_MINUTES may be off the list
		v.Leads = append(slices.Clone(leads), v.Lead)
		slices.Sort(v.Leads)
	}
	var watched []string
	if err := json.Unmarshal([]byte(calsJSON), &watched); err != nil {
		slog.Error("settings calendars", "err", err)
	}
	list, err := p.calendarList(ctx, id)
	if err != nil {
		slog.Warn("settings: calendar list", "err", err)
		// Still let the user uncheck what is watched; names come back with Google.
		v.CalendarsError = true
		for _, c := range watched {
			name := c
			if c == "primary" {
				name = "Main calendar"
			}
			list = append(list, calendar{ID: c, Name: name})
		}
	}
	for i := range list {
		list[i].Checked = slices.Contains(watched, list[i].ID)
	}
	v.Calendars = list
	web.Render(w, http.StatusOK, "settings", v)
}

// calendarList is the user's calendarList (order kept) they can read events
// of; the primary calendar gets the id "primary", as in settings.
func (p *Page) calendarList(ctx context.Context, accountID string) ([]calendar, error) {
	ts, err := p.Tokens(ctx, accountID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := &http.Client{Transport: &oauth2.Transport{Source: ts}}
	base := p.BaseURL
	if base == "" {
		base = "https://www.googleapis.com/calendar/v3"
	}
	q := url.Values{"maxResults": {"250"}, "fields": {"nextPageToken,items(id,summary,summaryOverride,backgroundColor,accessRole,primary)"}}
	var out []calendar
	for {
		req, err := http.NewRequestWithContext(ctx, "GET", base+"/users/me/calendarList?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		var page struct {
			NextPageToken string `json:"nextPageToken"`
			Items         []struct {
				ID, Summary, SummaryOverride, BackgroundColor, AccessRole string
				Primary                                                   bool
			} `json:"items"`
		}
		if resp.StatusCode == http.StatusOK {
			err = json.NewDecoder(resp.Body).Decode(&page)
		} else {
			err = fmt.Errorf("calendar API: %s", resp.Status)
		}
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			if it.AccessRole != "reader" && it.AccessRole != "writer" && it.AccessRole != "owner" {
				continue // freeBusyReader sees no event details
			}
			c := calendar{ID: it.ID, Name: it.Summary, Color: it.BackgroundColor}
			if it.SummaryOverride != "" {
				c.Name = it.SummaryOverride
			}
			if it.Primary {
				c.ID = "primary"
			}
			out = append(out, c)
		}
		if page.NextPageToken == "" {
			return out, nil
		}
		q.Set("pageToken", page.NextPageToken)
	}
}

// save stores the settings, bumps schedule_rev and cancels pending reminders
// in one write (§3.1) — so it applies even while Google is down — then asks
// for a poll that rebuilds them.
func (p *Page) save(w http.ResponseWriter, r *http.Request, id string) {
	timing := r.PostFormValue("timing")
	lead, err := strconv.Atoi(r.PostFormValue("lead"))
	cals := r.PostForm["calendar"]
	var uniq []string
	for _, c := range cals {
		if c != "" && len(c) <= 255 && !slices.Contains(uniq, c) {
			uniq = append(uniq, c)
		}
	}
	if (timing != "google" && timing != "fixed") || err != nil || lead < 1 || lead > 60 || len(uniq) > 100 {
		web.Error(w, http.StatusBadRequest, web.Message{Title: "Could not save", Text: "Please check the form and try again.",
			Button: "Back to settings", URL: "/settings"})
		return
	}
	if len(uniq) == 0 {
		web.Error(w, http.StatusBadRequest, web.Message{Title: "Choose a calendar",
			Text:   "Pick at least one calendar to get reminders for. To stop all reminders, use Pause instead.",
			Button: "Back to settings", URL: "/settings"})
		return
	}
	calsJSON, _ := json.Marshal(uniq)
	err = withTx(r.Context(), p.St.DB, func(tx *sql.Tx) error {
		ctx := r.Context()
		if _, err := tx.ExecContext(ctx, `UPDATE settings SET timing = ?, lead_minutes = ?, skip_declined = ?, calendars = ?
			WHERE account_id = ?`, timing, lead, r.PostFormValue("skip_declined") != "", string(calsJSON), id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET schedule_rev = schedule_rev + 1 WHERE id = ?`, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM reminders WHERE account_id = ? AND state IN ('pending', 'sending')`, id)
		return err
	})
	if err != nil {
		slog.Error("save settings", "err", err)
		web.Error(w, http.StatusInternalServerError, web.Oops)
		return
	}
	if p.Poll != nil {
		p.Poll(id)
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

// test sends a reminder for a made-up meeting right away (§5: sent directly,
// not through the reminders table).
func (p *Page) test(w http.ResponseWriter, r *http.Request, id string) {
	var zid sql.NullInt64
	if err := p.St.DB.QueryRowContext(r.Context(), `SELECT zulip_user_id FROM accounts WHERE id = ?`, id).Scan(&zid); err != nil || !zid.Valid {
		http.Redirect(w, r, "/", http.StatusSeeOther) // gone, or linking first
		return
	}
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	start := now.Add(10 * time.Minute).Truncate(time.Minute)
	msg := "This is a test. Your reminders look like this:\n\n" + sender.Render(poller.Payload{
		Title: "Example meeting", Start: start, End: start.Add(30 * time.Minute), Location: "Room 1"}, now)
	if _, err := p.Zulip.SendDM(r.Context(), zid.Int64, msg); err != nil {
		slog.Warn("test reminder failed", "err", err)
		web.Error(w, http.StatusBadGateway, web.Message{Title: "Zulip is not reachable",
			Text: "We could not send the test message. Please try again in a few minutes.", Button: "Back to settings", URL: "/settings"})
		return
	}
	http.Redirect(w, r, "/settings?tested=1", http.StatusSeeOther)
}

func (p *Page) pause(w http.ResponseWriter, r *http.Request, id string) {
	if err := p.Pause(r.Context(), id); err != nil {
		slog.Error("pause", "err", err)
		web.Error(w, http.StatusInternalServerError, web.Oops)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther) // the status page shows "paused" and Resume
}

func withTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
