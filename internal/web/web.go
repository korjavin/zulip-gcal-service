// Package web is the look of every page (one layout, one embedded CSS) and
// the start page: landing when signed out, status when signed in
// (docs/design.md §1, §6).
package web

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/korjavin/zulip-gcal-service/internal/store"
	"github.com/korjavin/zulip-gcal-service/internal/zulip"
)

//go:embed templates/*.html style.css
var files embed.FS

var funcs = template.FuncMap{
	"iso": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	// no-JS fallback; the layout's script rewrites it in the browser's timezone
	"utc": func(t time.Time) string { return t.UTC().Format("Mon 15:04 UTC") },
}

var pages = map[string]*template.Template{}

func init() {
	for _, p := range []string{"landing", "status", "message", "link", "disconnected", "settings"} {
		pages[p] = template.Must(template.New("").Funcs(funcs).ParseFS(files, "templates/layout.html", "templates/"+p+".html"))
	}
}

// Render writes page (a templates/<page>.html) inside the shared layout.
func Render(w http.ResponseWriter, code int, page string, data any) {
	var buf bytes.Buffer
	if err := pages[page].ExecuteTemplate(&buf, "layout", data); err != nil {
		slog.Error("render", "page", page, "err", err)
		http.Error(w, "Something went wrong. Please try again.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	buf.WriteTo(w)
}

// Message is a friendly page: a heading, one sentence and an optional button.
type Message struct{ Title, Text, Button, URL string }

func Error(w http.ResponseWriter, code int, m Message) { Render(w, code, "message", m) }

var (
	DomainNotAllowed = Message{"Use your work account", "This service is only for accounts of this organization. Please sign in with your work Google account.",
		"Choose another account", "/login?switch=1"}
	ScopeNotGranted = Message{"Calendar access needed", "To send reminders we need read access to your calendar. Please sign in again and allow calendar access.",
		"Sign in again", "/login"}
	GoogleError = Message{"Google sign-in failed", "Something went wrong on the way back from Google. Please try again.",
		"Try again", "/login"}
	ZulipUnreachable = Message{"Zulip is not reachable", "We can't reach Zulip right now. Please try again in a few minutes.",
		"Try again", "/link"}
	Oops = Message{"Something went wrong", "Please try again.", "", ""}
)

// staleAfter: no successful poll for this long (or 3 poll intervals, if
// longer) shows "can't read your calendar".
const staleAfter = 15 * time.Minute

// Site serves the start page, the stylesheet and POST /resume.
type Site struct {
	St           *store.Store
	Zulip        *zulip.Client
	Account      func(*http.Request) (string, bool) // session check (auth.Auth.Account)
	Resume       func(ctx context.Context, accountID string) error
	PollInterval time.Duration
	Now          func() time.Time // nil = time.Now
}

func (s *Site) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /style.css", func(w http.ResponseWriter, r *http.Request) { http.ServeFileFS(w, r, files, "style.css") })
	// CSRF: stdlib Sec-Fetch-Site/Origin check, as for /logout and /disconnect.
	mux.Handle("POST /resume", http.NewCrossOriginProtection().Handler(http.HandlerFunc(s.resume)))
}

type status struct {
	Email, ZulipName string
	State            string // lost, paused, stale, ok
	Next             *next
	FireAt           time.Time
}

// next is the part of a reminder payload (poller.Payload) the page shows;
// importing poller here would be an import cycle.
type next struct {
	Title string    `json:"title"`
	Start time.Time `json:"start"`
}

func (s *Site) home(w http.ResponseWriter, r *http.Request) {
	id, ok := s.Account(r)
	if !ok {
		Render(w, http.StatusOK, "landing", nil)
		return
	}
	ctx := r.Context()
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	var v status
	var zid sql.NullInt64
	var lost, paused bool
	var checked int64 // last successful poll; linking counts as a fresh start
	err := s.St.DB.QueryRowContext(ctx, `SELECT google_email, zulip_user_id, status = 'disconnected', paused,
		coalesce(last_poll_ok_at, linked_at, 0) FROM accounts WHERE id = ?`, id).Scan(&v.Email, &zid, &lost, &paused, &checked)
	if errors.Is(err, sql.ErrNoRows) { // deleted meanwhile
		Render(w, http.StatusOK, "landing", nil)
		return
	}
	if err != nil {
		slog.Error("status page", "err", err)
		Error(w, http.StatusInternalServerError, Oops)
		return
	}
	if !zid.Valid {
		http.Redirect(w, r, "/link", http.StatusFound) // the code step (internal/link)
		return
	}
	switch {
	case lost:
		v.State = "lost"
	case paused:
		v.State = "paused"
	case now.Sub(time.Unix(checked, 0)) > max(staleAfter, 3*s.PollInterval):
		v.State = "stale"
	default:
		v.State = "ok"
		var payload string
		var fire int64
		err := s.St.DB.QueryRowContext(ctx, `SELECT payload, fire_at FROM reminders WHERE account_id = ? AND state = 'pending'
			ORDER BY fire_at LIMIT 1`, id).Scan(&payload, &fire)
		var p next
		if err == nil && json.Unmarshal([]byte(payload), &p) == nil {
			if p.Title == "" {
				p.Title = "(No title)"
			}
			v.Next, v.FireAt = &p, time.Unix(fire, 0)
		} else if !errors.Is(err, sql.ErrNoRows) {
			slog.Warn("status page: next reminder", "err", err)
		}
	}
	// The name is a nicety: a slow or unreachable Zulip just leaves it out.
	zctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if u, err := s.Zulip.UserByID(zctx, zid.Int64); err == nil {
		v.ZulipName = u.FullName
	}
	Render(w, http.StatusOK, "status", v)
}

func (s *Site) resume(w http.ResponseWriter, r *http.Request) {
	if id, ok := s.Account(r); ok {
		if err := s.Resume(r.Context(), id); err != nil {
			slog.Error("resume", "err", err)
			Error(w, http.StatusInternalServerError, Oops)
			return
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
