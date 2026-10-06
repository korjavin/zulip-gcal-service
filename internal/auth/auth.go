// Package auth is the Google sign-in (docs/design.md §3.2): OAuth code flow
// with PKCE and offline access, ID-token verification, encrypted refresh
// token storage, and the signed session cookie.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/korjavin/zulip-gcal-service/internal/config"
	"github.com/korjavin/zulip-gcal-service/internal/store"
)

const (
	calendarScope = "https://www.googleapis.com/auth/calendar.readonly"
	sessionCookie = "session"
	stateCookie   = "oauth_state"
	sessionTTL    = 30 * 24 * time.Hour
	stateTTL      = 10 * time.Minute
)

// Endpoints are Google's OAuth/OIDC URLs; tests point them at a fake.
type Endpoints struct {
	AuthURL, TokenURL, JWKSURL, Issuer string
}

var Google = Endpoints{
	AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
	TokenURL: "https://oauth2.googleapis.com/token",
	JWKSURL:  "https://www.googleapis.com/oauth2/v3/certs",
	Issuer:   "https://accounts.google.com",
}

type Auth struct {
	cfg      *config.Config
	st       *store.Store
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

func New(cfg *config.Config, st *store.Store, ep Endpoints) *Auth {
	// The key fetch is shared by all callbacks and outlives each request: bound it.
	jwksCtx := oidc.ClientContext(context.Background(), &http.Client{Timeout: 10 * time.Second})
	return &Auth{
		cfg: cfg,
		st:  st,
		oauth: &oauth2.Config{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			RedirectURL:  cfg.PublicURL + "/oauth/callback",
			Scopes:       []string{"openid", "email", calendarScope},
			Endpoint:     oauth2.Endpoint{AuthURL: ep.AuthURL, TokenURL: ep.TokenURL, AuthStyle: oauth2.AuthStyleInParams},
		},
		verifier: oidc.NewVerifier(ep.Issuer, oidc.NewRemoteKeySet(jwksCtx, ep.JWKSURL),
			&oidc.Config{ClientID: cfg.GoogleClientID}),
	}
}

func (a *Auth) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", a.login)
	mux.HandleFunc("GET /oauth/callback", a.callback)
	// CSRF: stdlib Sec-Fetch-Site/Origin check, no token needed for a bare logout.
	mux.Handle("POST /logout", http.NewCrossOriginProtection().Handler(http.HandlerFunc(a.logout)))
}

// loginState travels in the signed state cookie between /login and the callback.
type loginState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Started  int64  `json:"t"` // login start, unix seconds (tombstone check, §3.3)
	Consent  bool   `json:"c"` // already retried with prompt=consent
	Expires  int64  `json:"e"`
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	a.redirectToGoogle(w, r, loginState{Started: time.Now().Unix()}, "")
}

func (a *Auth) redirectToGoogle(w http.ResponseWriter, r *http.Request, ls loginState, hint string) {
	ls.State = rand.Text()
	ls.Verifier = oauth2.GenerateVerifier()
	ls.Expires = time.Now().Add(stateTTL).Unix()
	a.setCookie(w, stateCookie, "state", ls, stateTTL)
	opts := []oauth2.AuthCodeOption{oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(ls.Verifier)}
	if ls.Consent {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "consent"))
		if hint != "" {
			opts = append(opts, oauth2.SetAuthURLParam("login_hint", hint))
		}
	}
	http.Redirect(w, r, a.oauth.AuthCodeURL(ls.State, opts...), http.StatusFound)
}

type claims struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	HD            string `json:"hd"`
}

func (a *Auth) callback(w http.ResponseWriter, r *http.Request) {
	var ls loginState
	ok := a.readCookie(r, stateCookie, "state", &ls) && ls.Expires > time.Now().Unix()
	clearCookie(w, stateCookie)
	q := r.URL.Query()
	if !ok || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(ls.State)) != 1 {
		page(w, http.StatusBadRequest, "This sign-in link has expired. Please try again.")
		return
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		page(w, http.StatusBadRequest, "Sign-in was cancelled.")
		return
	}
	ctx := r.Context()
	tok, err := a.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(ls.Verifier))
	if err != nil {
		slog.Warn("google code exchange failed", "err", err) // error responses carry no tokens
		page(w, http.StatusBadGateway, "Google sign-in failed. Please try again.")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := a.verifier.Verify(ctx, raw)
	var c claims
	if err == nil {
		err = idt.Claims(&c)
	}
	if err != nil || c.Sub == "" {
		slog.Warn("google id token rejected", "err", err)
		page(w, http.StatusBadGateway, "Google sign-in failed. Please try again.")
		return
	}
	if len(a.cfg.AllowedDomains) > 0 && !slices.Contains(a.cfg.AllowedDomains, strings.ToLower(c.HD)) {
		page(w, http.StatusForbidden, "This service is only for accounts of this organization. Sign in with your work Google account.")
		return
	}
	scope, _ := tok.Extra("scope").(string)
	if !slices.Contains(strings.Fields(scope), calendarScope) {
		page(w, http.StatusForbidden, "We need read access to your calendar to send reminders. Please sign in again and allow calendar access.")
		return
	}

	var enc []byte // new refresh token, encrypted; nil = keep the stored one
	if tok.RefreshToken != "" {
		enc = a.cfg.Secrets.Encrypt([]byte(tok.RefreshToken))
	} else if !a.storedTokenWorks(ctx, c.Sub) {
		if !ls.Consent { // one retry with forced consent: Google then returns a refresh token
			a.redirectToGoogle(w, r, loginState{Started: ls.Started, Consent: true}, c.Email)
			return
		}
		page(w, http.StatusBadGateway, "Google did not grant offline access to your calendar. Please try again.")
		return
	}

	id, err := a.upsert(ctx, c, enc, ls.Started)
	if errors.Is(err, errDisconnecting) {
		page(w, http.StatusConflict, "Disconnect in progress, try again in a moment.")
		return
	}
	if err != nil {
		slog.Error("store account", "err", err)
		page(w, http.StatusInternalServerError, "Something went wrong. Please try again.")
		return
	}
	a.setCookie(w, sessionCookie, "session", session{Account: id, Expires: time.Now().Add(sessionTTL).Unix()}, sessionTTL)
	http.Redirect(w, r, "/", http.StatusFound) // ponytail: Zulip linking (zgc-civ.2) takes over from "/"
}

// storedTokenWorks reports whether the account for sub has a refresh token
// that Google still accepts.
func (a *Auth) storedTokenWorks(ctx context.Context, sub string) bool {
	var enc []byte
	err := a.st.DB.QueryRowContext(ctx, `SELECT enc_refresh_token FROM accounts WHERE google_sub = ?`, sub).Scan(&enc)
	if err != nil || enc == nil {
		return false
	}
	rt, err := a.cfg.Secrets.Decrypt(enc)
	if err != nil {
		return false
	}
	_, err = a.oauth.TokenSource(ctx, &oauth2.Token{RefreshToken: string(rt)}).Token()
	return err == nil
}

var errDisconnecting = errors.New("disconnect in progress")

// upsert creates or updates the account for c.Sub in one write. enc != nil
// stores a new refresh token (token_rev bump); a disconnected account is
// reconnected (schedule_rev bump). A tombstone for the sub that is still
// revoking or newer than the login start rejects the callback (§3.3).
func (a *Auth) upsert(ctx context.Context, c claims, enc []byte, started int64) (string, error) {
	tx, err := a.st.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tombstones WHERE google_sub = ? AND (revoking = 1 OR created_at >= ?)`,
		c.Sub, started).Scan(&n); err != nil {
		return "", err
	}
	if n > 0 {
		return "", errDisconnecting
	}
	domain := c.Email[strings.LastIndexByte(c.Email, '@')+1:]
	authoritative := c.EmailVerified && (c.HD != "" || strings.EqualFold(domain, "gmail.com"))
	var encArg any // a nil []byte would bind as an empty blob, not NULL
	if enc != nil {
		encArg = enc
	}
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM accounts WHERE google_sub = ?`, c.Sub).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if enc == nil { // the account vanished after storedTokenWorks
			return "", errors.New("account without refresh token")
		}
		id = store.NewAccountID()
		if _, err := tx.ExecContext(ctx, `INSERT INTO accounts (id, google_sub, google_email, email_authoritative, enc_refresh_token, token_rev, created_at)
			VALUES (?, ?, ?, ?, ?, 1, ?)`, id, c.Sub, c.Email, authoritative, enc, time.Now().Unix()); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (account_id, lead_minutes) VALUES (?, ?)`, id, a.cfg.DefaultLeadMinutes); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	default:
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET google_email = ?, email_authoritative = ?,
			enc_refresh_token = coalesce(?, enc_refresh_token),
			token_rev = token_rev + (? IS NOT NULL),
			schedule_rev = schedule_rev + (status = 'disconnected'),
			status = 'connected'
			WHERE id = ?`, c.Email, authoritative, encArg, encArg, id); err != nil {
			return "", err
		}
	}
	return id, tx.Commit()
}

type session struct {
	Account string `json:"a"`
	Expires int64  `json:"e"`
}

// Account returns the signed-in account id, if the session cookie is valid,
// unexpired and its account still exists.
func (a *Auth) Account(r *http.Request) (string, bool) {
	var s session
	if !a.readCookie(r, sessionCookie, "session", &s) || s.Expires <= time.Now().Unix() {
		return "", false
	}
	var one int
	if a.st.DB.QueryRowContext(r.Context(), `SELECT 1 FROM accounts WHERE id = ?`, s.Account).Scan(&one) != nil {
		return "", false
	}
	return s.Account, true
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	clearCookie(w, sessionCookie)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// setCookie stores v as base64(json).base64(hmac(purpose, json)); purpose
// keeps a state cookie from being replayed as a session and vice versa.
func (a *Auth) setCookie(w http.ResponseWriter, name, purpose string, v any, ttl time.Duration) {
	body, _ := json.Marshal(v)
	val := base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(a.mac(purpose, body))
	http.SetCookie(w, &http.Cookie{Name: name, Value: val, Path: "/", MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}

func (a *Auth) readCookie(r *http.Request, name, purpose string, v any) bool {
	ck, err := r.Cookie(name)
	if err != nil {
		return false
	}
	b64, sig64, ok := strings.Cut(ck.Value, ".")
	body, err1 := base64.RawURLEncoding.DecodeString(b64)
	sig, err2 := base64.RawURLEncoding.DecodeString(sig64)
	if !ok || err1 != nil || err2 != nil || !hmac.Equal(sig, a.mac(purpose, body)) {
		return false
	}
	return json.Unmarshal(body, v) == nil
}

func (a *Auth) mac(purpose string, body []byte) []byte {
	m := hmac.New(sha256.New, a.cfg.Secrets.CookieKey)
	m.Write([]byte(purpose + "\x00"))
	m.Write(body)
	return m.Sum(nil)
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}

var pageTmpl = template.Must(template.New("").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Calendar reminders</title></head>
<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:3rem auto;padding:0 1rem">
<p>{{.}}</p><p><a href="/login">Sign in with Google</a></p>
</body></html>`))

// page is the friendly error page: a message and a way to try again.
func page(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	pageTmpl.Execute(w, msg)
}
