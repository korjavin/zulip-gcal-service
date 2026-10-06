package auth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/korjavin/zulip-gcal-service/internal/config"
	"github.com/korjavin/zulip-gcal-service/internal/store"
)

const clientID = "client-id"

// user is a Google account as the fake sees it.
type user struct {
	sub, email, hd string
	verified       bool
	scope          string // granted scopes; "" = all requested
	consented      bool   // Google issues a refresh token only on first consent or prompt=consent
	noRefresh      bool   // never issue a refresh token
}

type grant struct {
	u         *user
	challenge string
	refresh   string
}

// fakeGoogle serves the token endpoint and JWKS; authorize stands in for the
// consent screen.
type fakeGoogle struct {
	srv      *httptest.Server
	key      *rsa.PrivateKey
	mu       sync.Mutex
	codes    map[string]grant
	alive    map[string]bool // refresh tokens Google still accepts
	authURLs []url.Values
	issued   []string // every token string handed out
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGoogle{key: key, codes: map[string]grant{}, alive: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /certs", func(w http.ResponseWriter, r *http.Request) {
		b64 := base64.RawURLEncoding.EncodeToString
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", g.token)
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGoogle) endpoints() Endpoints {
	return Endpoints{AuthURL: g.srv.URL + "/auth", TokenURL: g.srv.URL + "/token", JWKSURL: g.srv.URL + "/certs", Issuer: g.srv.URL}
}

func (g *fakeGoogle) newToken(kind string) string {
	s := kind + "-" + rand.Text()
	g.issued = append(g.issued, s)
	return s
}

// authorize plays the consent screen for u and returns the callback path.
func (g *fakeGoogle) authorize(u *user, authURL string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	pu, _ := url.Parse(authURL)
	q := pu.Query()
	g.authURLs = append(g.authURLs, q)
	gr := grant{u: u, challenge: q.Get("code_challenge")}
	if !u.noRefresh && (!u.consented || q.Get("prompt") == "consent") {
		gr.refresh = g.newToken("refresh")
		g.alive[gr.refresh] = true
	}
	u.consented = true
	code := g.newToken("code")
	g.codes[code] = gr
	return "/oauth/callback?" + url.Values{"state": {q.Get("state")}, "code": {code}}.Encode()
}

func (g *fakeGoogle) token(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r.ParseForm()
	fail := func(e string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": e})
	}
	if r.Form.Get("client_id") != clientID || r.Form.Get("client_secret") != "client-secret" {
		fail("invalid_client")
		return
	}
	resp := map[string]any{"access_token": g.newToken("access"), "token_type": "Bearer", "expires_in": 3600}
	switch r.Form.Get("grant_type") {
	case "refresh_token":
		if !g.alive[r.Form.Get("refresh_token")] {
			fail("invalid_grant")
			return
		}
	case "authorization_code":
		gr, ok := g.codes[r.Form.Get("code")]
		delete(g.codes, r.Form.Get("code"))
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != gr.challenge {
			fail("invalid_grant")
			return
		}
		scope := gr.u.scope
		if scope == "" {
			scope = "openid https://www.googleapis.com/auth/userinfo.email " + calendarScope
		}
		resp["scope"] = scope
		resp["id_token"] = g.idToken(gr.u)
		if gr.refresh != "" {
			resp["refresh_token"] = gr.refresh
		}
	default:
		fail("unsupported_grant_type")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (g *fakeGoogle) idToken(u *user) string {
	c := map[string]any{"iss": g.srv.URL, "aud": clientID, "sub": u.sub, "email": u.email,
		"email_verified": u.verified, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	if u.hd != "" {
		c["hd"] = u.hd
	}
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	signed := enc(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"}) + "." + enc(c)
	sum := sha256.Sum256([]byte(signed))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, g.key, crypto.SHA256, sum[:])
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

type env struct {
	t   *testing.T
	g   *fakeGoogle
	st  *store.Store
	a   *Auth
	cfg *config.Config
	h   http.Handler
}

func newEnv(t *testing.T, allowed ...string) *env {
	secrets, err := config.NewSecrets(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{PublicURL: "https://cal.example.com", GoogleClientID: clientID, GoogleClientSecret: "client-secret",
		AllowedDomains: allowed, DefaultLeadMinutes: 10, Secrets: secrets}
	g := newFakeGoogle(t)
	a := New(cfg, st, g.endpoints())
	mux := http.NewServeMux()
	a.Register(mux)
	return &env{t: t, g: g, st: st, a: a, cfg: cfg, h: mux}
}

// browser keeps cookies across requests.
type browser struct {
	e   *env
	jar map[string]string
}

func (e *env) browser() *browser { return &browser{e: e, jar: map[string]string{}} }

func (b *browser) do(method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range b.jar {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	rec := httptest.NewRecorder()
	b.e.h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.jar, c.Name)
		} else {
			b.jar[c.Name] = c.Value
		}
	}
	return rec
}

// finish follows redirects through the fake consent screen until the
// service answers with something else.
func (b *browser) finish(u *user, rec *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	for range 3 {
		loc := rec.Header().Get("Location")
		if !strings.HasPrefix(loc, b.e.g.srv.URL) {
			return rec
		}
		rec = b.do("GET", b.e.g.authorize(u, loc))
	}
	b.e.t.Fatal("redirect loop")
	return nil
}

func (b *browser) signIn(u *user) *httptest.ResponseRecorder {
	return b.finish(u, b.do("GET", "/login"))
}

func (b *browser) account() (string, bool) {
	req := httptest.NewRequest("GET", "/", nil)
	for k, v := range b.jar {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	return b.e.a.Account(req)
}

type row struct {
	id                    string
	email                 string
	authoritative         bool
	refresh               string
	tokenRev, scheduleRev int64
	status                string
}

func (e *env) row(sub string) *row {
	var r row
	var enc []byte
	err := e.st.DB.QueryRow(`SELECT id, google_email, email_authoritative, enc_refresh_token, token_rev, schedule_rev, status
		FROM accounts WHERE google_sub = ?`, sub).Scan(&r.id, &r.email, &r.authoritative, &enc, &r.tokenRev, &r.scheduleRev, &r.status)
	if err != nil {
		return nil
	}
	pt, err := e.cfg.Secrets.Decrypt(enc)
	if err != nil {
		e.t.Fatal(err)
	}
	r.refresh = string(pt)
	return &r
}

func expect(t *testing.T, rec *httptest.ResponseRecorder, code int, text string) {
	t.Helper()
	if rec.Code != code || !strings.Contains(rec.Body.String()+rec.Header().Get("Location"), text) {
		t.Fatalf("got %d %q %q, want %d with %q", rec.Code, rec.Header().Get("Location"), rec.Body.String(), code, text)
	}
}

func alice() *user {
	return &user{sub: "s-alice", email: "alice@example.com", hd: "example.com", verified: true}
}

func TestNewUser(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	expect(t, b.signIn(alice()), http.StatusFound, "/")
	r := e.row("s-alice")
	if r == nil || r.tokenRev != 1 || r.status != "connected" || !r.authoritative || !e.g.alive[r.refresh] {
		t.Fatalf("account = %+v", r)
	}
	if id, ok := b.account(); !ok || id != r.id {
		t.Fatalf("session = %q %v", id, ok)
	}
	q := e.g.authURLs[0]
	if q.Get("access_type") != "offline" || q.Get("code_challenge_method") != "S256" || q.Get("prompt") != "" ||
		!strings.Contains(q.Get("scope"), calendarScope) {
		t.Fatalf("auth url = %v", q)
	}
	var lead int
	e.st.DB.QueryRow(`SELECT lead_minutes FROM settings WHERE account_id = ?`, r.id).Scan(&lead)
	if lead != 10 {
		t.Fatalf("settings lead = %d", lead)
	}
	if loc := b.signIn(alice()).Header().Get("Location"); loc != "/link" {
		t.Fatalf("unlinked account goes to %q, want /link", loc)
	}
	e.st.DB.Exec(`UPDATE accounts SET zulip_user_id = 1`)
	if loc := b.signIn(alice()).Header().Get("Location"); loc != "/" {
		t.Fatalf("linked account goes to %q, want /", loc)
	}
}

func TestReturningUserNoForcedConsent(t *testing.T) {
	e := newEnv(t)
	u := alice()
	e.browser().signIn(u)
	before := e.row(u.sub)
	b := e.browser()
	expect(t, b.signIn(u), http.StatusFound, "/")
	if len(e.g.authURLs) != 2 || e.g.authURLs[1].Get("prompt") != "" {
		t.Fatalf("auth urls = %v", e.g.authURLs)
	}
	if after := e.row(u.sub); *after != *before {
		t.Fatalf("account changed: %+v -> %+v", before, after)
	}
	if _, ok := b.account(); !ok {
		t.Fatal("no session")
	}
}

func TestDeadStoredTokenOneConsentRetry(t *testing.T) {
	e := newEnv(t)
	u := alice()
	e.browser().signIn(u)
	old := e.row(u.sub)
	e.g.alive[old.refresh] = false
	expect(t, e.browser().signIn(u), http.StatusFound, "/")
	if len(e.g.authURLs) != 3 || e.g.authURLs[2].Get("prompt") != "consent" || e.g.authURLs[2].Get("login_hint") != u.email {
		t.Fatalf("auth urls = %v", e.g.authURLs)
	}
	r := e.row(u.sub)
	if r.id != old.id || r.tokenRev != 2 || r.refresh == old.refresh || !e.g.alive[r.refresh] {
		t.Fatalf("account = %+v", r)
	}

	// Google still returns no refresh token: one retry only, then a friendly
	// error, and the stored token stays.
	u.noRefresh = true
	e.g.alive[r.refresh] = false
	expect(t, e.browser().signIn(u), http.StatusBadGateway, "offline access")
	if len(e.g.authURLs) != 5 {
		t.Fatalf("auth urls = %d", len(e.g.authURLs))
	}
	if after := e.row(u.sub); *after != *r {
		t.Fatalf("failed callback changed the account: %+v", after)
	}
}

func TestNewUserWithoutRefreshToken(t *testing.T) {
	e := newEnv(t)
	u := alice()
	u.noRefresh = true
	b := e.browser()
	expect(t, b.signIn(u), http.StatusBadGateway, "offline access")
	if e.row(u.sub) != nil {
		t.Fatal("account created")
	}
	if _, ok := b.account(); ok {
		t.Fatal("session set")
	}
}

func TestStateMismatch(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	loc := b.do("GET", "/login").Header().Get("Location")
	cb := e.g.authorize(alice(), loc)
	expect(t, b.do("GET", strings.Replace(cb, "state=", "state=x", 1)), http.StatusBadRequest, "expired")
	expect(t, e.browser().do("GET", cb), http.StatusBadRequest, "expired") // no state cookie
	expect(t, b.do("GET", cb), http.StatusBadRequest, "expired")           // cookie was consumed
	if e.row("s-alice") != nil {
		t.Fatal("account created")
	}
}

func TestPKCEVerifier(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	loc := b.do("GET", "/login").Header().Get("Location")
	cb := e.g.authorize(alice(), loc)
	for code, gr := range e.g.codes { // the code was bound to another challenge
		gr.challenge = "other"
		e.g.codes[code] = gr
	}
	expect(t, b.do("GET", cb), http.StatusBadGateway, "sign-in failed")
	if e.row("s-alice") != nil {
		t.Fatal("account created")
	}
}

func TestAllowedDomains(t *testing.T) {
	e := newEnv(t, "example.com")
	expect(t, e.browser().signIn(&user{sub: "s-bob", email: "bob@other.com", hd: "other.com", verified: true}), http.StatusForbidden, "organization")
	expect(t, e.browser().signIn(&user{sub: "s-gm", email: "x@gmail.com", verified: true}), http.StatusForbidden, "organization")
	expect(t, e.browser().signIn(&user{sub: "s-ok", email: "ok@example.com", hd: "Example.com", verified: true}), http.StatusFound, "/")
	if e.row("s-bob") != nil || e.row("s-gm") != nil || e.row("s-ok") == nil {
		t.Fatal("wrong accounts")
	}
}

func TestEmailAuthoritative(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		u    user
		want bool
	}{
		{user{sub: "1", email: "a@corp.com", hd: "corp.com", verified: true}, true},
		{user{sub: "2", email: "a@gmail.com", verified: true}, true},
		{user{sub: "3", email: "a@other.org", verified: true}, false}, // consumer account with a non-Google address
		{user{sub: "4", email: "a@corp.com", hd: "corp.com", verified: false}, false},
	} {
		e.browser().signIn(&c.u)
		if r := e.row(c.u.sub); r == nil || r.authoritative != c.want {
			t.Fatalf("%s: %+v", c.u.email, r)
		}
	}
}

func TestMissingScopeKeepsOldToken(t *testing.T) {
	e := newEnv(t)
	u := alice()
	e.browser().signIn(u)
	before := e.row(u.sub)
	u.scope = "openid email"
	u.consented = false // fresh consent with the box unticked: a new refresh token comes back
	expect(t, e.browser().signIn(u), http.StatusForbidden, "read access to your calendar")
	if after := e.row(u.sub); *after != *before {
		t.Fatalf("failed callback changed the account: %+v -> %+v", before, after)
	}
	expect(t, e.browser().signIn(&user{sub: "s-new", email: "n@example.com", hd: "example.com", verified: true, scope: "openid email"}),
		http.StatusForbidden, "read access")
	if e.row("s-new") != nil {
		t.Fatal("account created without calendar scope")
	}
}

func TestSessions(t *testing.T) {
	e := newEnv(t)
	u := alice()
	b := e.browser()
	b.signIn(u)
	id, _ := b.account()

	// Disconnected (lost access) keeps the session; signing in reconnects.
	e.st.DB.Exec(`UPDATE accounts SET status = 'disconnected', schedule_rev = 5 WHERE id = ?`, id)
	if _, ok := b.account(); !ok {
		t.Fatal("disconnected account lost its session")
	}
	b.signIn(u)
	if r := e.row(u.sub); r.status != "connected" || r.scheduleRev != 6 || r.id != id {
		t.Fatalf("reconnect: %+v", r)
	}

	// Tampered cookie.
	saved := b.jar[sessionCookie]
	b.jar[sessionCookie] = strings.Replace(saved, ".", "x.", 1)
	if _, ok := b.account(); ok {
		t.Fatal("tampered cookie accepted")
	}
	b.jar[sessionCookie] = saved

	// A state cookie is not a session cookie.
	other := e.browser()
	other.do("GET", "/login")
	other.jar[sessionCookie] = other.jar[stateCookie]
	if _, ok := other.account(); ok {
		t.Fatal("state cookie accepted as session")
	}

	// Logout; a cross-site POST cannot log anyone out.
	req := httptest.NewRequest("POST", "/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("cross-site logout = %d %v", rec.Code, rec.Result().Cookies())
	}
	c := e.browser()
	c.jar[sessionCookie] = saved
	expect(t, c.do("POST", "/logout"), http.StatusSeeOther, "/")
	if _, ok := c.account(); ok {
		t.Fatal("still signed in after logout")
	}

	// Deleted account rejects the cookie.
	e.st.DB.Exec(`DELETE FROM accounts WHERE id = ?`, id)
	if _, ok := b.account(); ok {
		t.Fatal("deleted account still signed in")
	}
}

func TestTombstone(t *testing.T) {
	e := newEnv(t)
	u := alice()

	// Login started before the disconnect, finished after: rejected.
	b := e.browser()
	start := b.do("GET", "/login")
	e.st.DB.Exec(`INSERT INTO tombstones (google_sub, created_at) VALUES (?, ?)`, u.sub, time.Now().Unix())
	expect(t, b.finish(u, start), http.StatusConflict, "Disconnect in progress")
	if e.row(u.sub) != nil {
		t.Fatal("account resurrected")
	}

	// Revocation still running: rejected even for a newer login.
	e.st.DB.Exec(`UPDATE tombstones SET created_at = ?, revoking = 1`, time.Now().Add(-time.Minute).Unix())
	expect(t, e.browser().signIn(u), http.StatusConflict, "Disconnect in progress")

	// New login after revocation finished: a new account.
	e.st.DB.Exec(`UPDATE tombstones SET revoking = 0`)
	expect(t, e.browser().signIn(u), http.StatusFound, "/")
	if e.row(u.sub) == nil {
		t.Fatal("no account")
	}
}

func TestTokensNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := newEnv(t)
	u := alice()
	e.browser().signIn(u)
	e.g.alive[e.row(u.sub).refresh] = false
	u.noRefresh = true
	e.browser().signIn(u) // dead token, consent retry, failure
	b := e.browser()
	cb := e.g.authorize(u, b.do("GET", "/login").Header().Get("Location"))
	for code, gr := range e.g.codes {
		gr.challenge = "other"
		e.g.codes[code] = gr
	}
	b.do("GET", cb) // failed exchange gets logged
	if !strings.Contains(buf.String(), "exchange failed") {
		t.Fatalf("expected a logged failure: %s", buf.String())
	}
	for _, s := range e.g.issued {
		if strings.Contains(buf.String(), s) {
			t.Fatalf("log contains a token: %s", buf.String())
		}
	}
}
