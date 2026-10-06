// Package link links a signed-in Google account to a Zulip user
// (docs/design.md §1, §3.4): auto-match by an authoritative e-mail, else a
// short code the user DMs to the bot.
package link

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/korjavin/zulip-gcal-service/internal/store"
	"github.com/korjavin/zulip-gcal-service/internal/zulip"
)

const (
	codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789" // no 0/O, 1/I/L
	codeLen      = 6
	codeTTL      = 15 * time.Minute
	maxWrong     = 5 // wrong codes per sender per codeTTL
)

type Linker struct {
	St         *store.Store
	Zulip      *zulip.Client
	Account    func(*http.Request) (string, bool) // session's account id
	BotHealthy func() bool
	BotID      int64
	PublicURL  string
	ZulipSite  string
	Poll       func(accountID string) // immediate poll after linking; nil = none yet

	wrong map[int64][]time.Time // ponytail: only touched by the sequential bot loop, no lock
}

func (l *Linker) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /link", l.page)
	mux.HandleFunc("GET /link/status", l.status)
}

func (l *Linker) welcome() string {
	// ponytail: the stop/start/disconnect line joins with zgc-civ.3.
	return "Hi! I'll remind you about your Google Calendar meetings. Settings: " + l.PublicURL + "/settings"
}

func (l *Linker) help() string {
	return "Hi! I send reminders about your Google Calendar meetings. To connect, open " + l.PublicURL + "/login" +
		" and sign in with Google. No reply from me within a minute? Send your message again or use the website."
}

// linkTx links accountID to zulipUserID if the account is still unlinked and
// the Zulip user is not linked to another account. Linking bumps
// schedule_rev, drops the account's pending reminders and all its codes.
func linkTx(ctx context.Context, tx *sql.Tx, accountID string, zulipUserID int64, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE accounts SET zulip_user_id = ?, linked_at = ?, schedule_rev = schedule_rev + 1
		WHERE id = ? AND zulip_user_id IS NULL AND NOT EXISTS (SELECT 1 FROM accounts WHERE zulip_user_id = ?)`,
		zulipUserID, now.Unix(), accountID, zulipUserID)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM reminders WHERE account_id = ? AND state IN ('pending', 'sending')`, accountID); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM link_codes WHERE account_id = ?`, accountID)
	return err == nil, err
}

func (l *Linker) linked(ctx context.Context, accountID string, zulipUserID int64) {
	if _, err := l.Zulip.SendDM(ctx, zulipUserID, l.welcome()); err != nil {
		slog.Warn("welcome DM failed", "err", err)
	}
	if l.Poll != nil {
		l.Poll(accountID)
	}
}

// liveCode returns the account's live code, creating one if there is none.
func (l *Linker) liveCode(ctx context.Context, accountID string, now time.Time) (string, error) {
	tx, err := l.St.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM link_codes WHERE expires_at <= ?`, now.Unix()); err != nil {
		return "", err
	}
	var code string
	err = tx.QueryRowContext(ctx, `SELECT code FROM link_codes WHERE account_id = ?`, accountID).Scan(&code)
	if err == nil {
		return code, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	for range 10 { // a collision (another account's live code) just draws again
		code = newCode()
		res, err := tx.ExecContext(ctx, `INSERT INTO link_codes (code, account_id, expires_at) VALUES (?, ?, ?)
			ON CONFLICT (code) DO NOTHING`, code, accountID, now.Add(codeTTL).Unix())
		if err != nil {
			return "", err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return code, tx.Commit()
		}
	}
	return "", errors.New("no free link code")
}

func newCode() string {
	b := make([]byte, codeLen)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(codeAlphabet))))
		b[i] = codeAlphabet[n.Int64()]
	}
	return string(b)
}

type accountRow struct {
	zulipUserID   sql.NullInt64
	email         string
	authoritative bool
}

func (l *Linker) account(ctx context.Context, id string) (accountRow, error) {
	var a accountRow
	err := l.St.DB.QueryRowContext(ctx, `SELECT zulip_user_id, google_email, email_authoritative FROM accounts WHERE id = ?`, id).
		Scan(&a.zulipUserID, &a.email, &a.authoritative)
	return a, err
}

// page is where sign-in lands for an unlinked account: auto-match, else the code.
func (l *Linker) page(w http.ResponseWriter, r *http.Request) {
	id, ok := l.Account(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	ctx := r.Context()
	a, err := l.account(ctx, id)
	if err != nil {
		l.fail(w, err)
		return
	}
	if a.zulipUserID.Valid {
		render(w, http.StatusOK, view{Connected: true})
		return
	}
	if a.authoritative {
		u, err := l.Zulip.UserByEmail(ctx, a.email)
		switch {
		case err == nil:
			var ok bool
			err := withTx(ctx, l.St.DB, func(tx *sql.Tx) (err error) {
				ok, err = linkTx(ctx, tx, id, u.ID, time.Now())
				return err
			})
			if err != nil {
				l.fail(w, err)
				return
			}
			if ok {
				l.linked(ctx, id, u.ID)
				render(w, http.StatusOK, view{Connected: true})
				return
			}
			if a, err = l.account(ctx, id); err == nil && a.zulipUserID.Valid { // linked by a code meanwhile
				render(w, http.StatusOK, view{Connected: true})
				return
			}
			render(w, http.StatusConflict, view{Msg: "This Zulip account is already connected to another Google account — disconnect it first."})
			return
		case !errors.Is(err, zulip.ErrNotFound):
			slog.Warn("zulip lookup by e-mail failed", "err", err) // fall back to the code
		}
	}
	code, err := l.liveCode(ctx, id, time.Now())
	if err != nil {
		l.fail(w, err)
		return
	}
	render(w, http.StatusOK, view{Code: code, ZulipDM: fmt.Sprintf("%s/#narrow/dm/%d", l.ZulipSite, l.BotID), BotDown: !l.BotHealthy()})
}

func (l *Linker) status(w http.ResponseWriter, r *http.Request) {
	id, ok := l.Account(r)
	if !ok {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	a, err := l.account(r.Context(), id)
	if err != nil {
		http.Error(w, "not signed in", http.StatusUnauthorized) // account deleted meanwhile
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]bool{"linked": a.zulipUserID.Valid, "bot_healthy": l.BotHealthy()})
}

func (l *Linker) fail(w http.ResponseWriter, err error) {
	slog.Error("link page", "err", err)
	render(w, http.StatusInternalServerError, view{Msg: "Something went wrong. Please try again."})
}

// HandleDM is the bot's message handler (zulip.Handler): a link code, else help.
func (l *Linker) HandleDM(ctx context.Context, m zulip.Message) error {
	now := time.Now()
	code := strings.ToUpper(m.Text)
	if !looksLikeCode(code) {
		return l.replyOnce(ctx, m, l.help())
	}
	if l.throttled(m.SenderID, now) {
		return l.replyOnce(ctx, m, "Too many wrong codes. Please wait 15 minutes, then open "+l.PublicURL+"/login to get a new one.")
	}
	var accountID string
	var linked, fresh bool
	err := withTx(ctx, l.St.DB, func(tx *sql.Tx) error {
		var err error
		if fresh, err = store.MarkHandled(ctx, tx, m.ID, now); err != nil || !fresh {
			return err
		}
		err = tx.QueryRowContext(ctx, `SELECT account_id FROM link_codes WHERE code = ? AND expires_at > ?`, code, now.Unix()).Scan(&accountID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		linked, err = linkTx(ctx, tx, accountID, m.SenderID, now)
		return err
	})
	switch {
	case err != nil || !fresh:
		return err
	case linked:
		l.linked(ctx, accountID, m.SenderID)
		return nil
	case accountID == "":
		l.wrong[m.SenderID] = append(l.wrong[m.SenderID], now)
		return l.send(ctx, m.SenderID, "That code is not valid or expired — open "+l.PublicURL+"/login to get a new one.")
	}
	return l.send(ctx, m.SenderID, "Your Zulip account is already connected to a Google account. To connect a different one, disconnect first, then sign in again at "+l.PublicURL+".")
}

func looksLikeCode(s string) bool {
	if len(s) != codeLen {
		return false
	}
	for _, c := range s {
		if !('A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
			return false
		}
	}
	return true
}

// throttled forgets attempts older than codeTTL and reports whether the
// sender still has maxWrong of them.
func (l *Linker) throttled(sender int64, now time.Time) bool {
	if l.wrong == nil {
		l.wrong = map[int64][]time.Time{}
	}
	ts := l.wrong[sender]
	for len(ts) > 0 && now.Sub(ts[0]) >= codeTTL {
		ts = ts[1:]
	}
	if len(ts) == 0 {
		delete(l.wrong, sender)
		return false
	}
	l.wrong[sender] = ts
	return len(ts) >= maxWrong
}

// replyOnce records the receipt, then replies (a reply that writes nothing).
func (l *Linker) replyOnce(ctx context.Context, m zulip.Message, text string) error {
	var fresh bool
	err := withTx(ctx, l.St.DB, func(tx *sql.Tx) (err error) {
		fresh, err = store.MarkHandled(ctx, tx, m.ID, time.Now())
		return err
	})
	if err != nil || !fresh {
		return err
	}
	return l.send(ctx, m.SenderID, text)
}

func (l *Linker) send(ctx context.Context, to int64, text string) error {
	_, err := l.Zulip.SendDM(ctx, to, text)
	return err
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

type view struct {
	Connected bool
	Msg       string
	Code      string
	ZulipDM   string
	BotDown   bool
}

var pageTmpl = template.Must(template.New("").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Calendar reminders</title></head>
<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:3rem auto;padding:0 1rem">
<div id="main">
{{if .Connected}}<h1>Connected</h1><p>Reminders go to you in Zulip as direct messages from <b>Calendar</b>.</p>
{{else if .Code}}<h1>One more step</h1>
<p>Send this code to the Calendar bot in Zulip:</p>
<p style="font-size:2.5rem;letter-spacing:.3rem;font-family:monospace"><b>{{.Code}}</b></p>
<p><a href="{{.ZulipDM}}" target="_blank" rel="noopener" style="display:inline-block;padding:.7rem 1.2rem;background:#6492fe;color:#fff;border-radius:.4rem;text-decoration:none">Open Zulip</a></p>
<p>The code works for 15 minutes. This page updates by itself once the bot has it.</p>
<p id="down" style="color:#b00"{{if not .BotDown}} hidden{{end}}>Zulip connection is down, try again in a minute.</p>
<p><a href="/link">Refresh</a></p>
<script>
setInterval(async () => {
  try {
    const r = await fetch("/link/status", {cache: "no-store"});
    if (!r.ok) return;
    const s = await r.json();
    if (s.linked) location.reload();
    document.getElementById("down").hidden = s.bot_healthy;
  } catch (e) {}
}, 3000);
</script>
{{else}}<p>{{.Msg}}</p><p><a href="/">Back</a></p>{{end}}
</div>
</body></html>`))

func render(w http.ResponseWriter, code int, v view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	pageTmpl.Execute(w, v)
}
