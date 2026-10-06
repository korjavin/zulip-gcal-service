// Package zulip is a minimal Zulip API client (plain net/http, basic auth
// with the bot's e-mail and API key) and the bot's event-queue loop
// (docs/design.md §3.4).
package zulip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MinFeatureLevel is Zulip Server 10.0: lookup by real e-mail address.
const MinFeatureLevel = 302

// ErrNotFound: no such user, address hidden, deactivated, or a bot.
var ErrNotFound = errors.New("zulip: user not found")

// ErrRecipient: a DM recipient that cannot receive (deactivated or gone);
// retrying will not help.
var ErrRecipient = errors.New("zulip: recipient cannot receive messages")

// Error is a non-200 Zulip API response.
type Error struct {
	Status     int
	Code, Msg  string
	RetryAfter time.Duration // 429 only
}

func (e *Error) Error() string {
	return fmt.Sprintf("zulip: HTTP %d %s: %s", e.Status, e.Code, e.Msg)
}

// Temporary reports whether retrying later may succeed: 5xx, 429 and network
// failures. Other API errors (401, 403, 400) are permanent.
func Temporary(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Status == http.StatusTooManyRequests || e.Status >= 500
	}
	return !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrRecipient)
}

type Client struct {
	site, email, key string
	http             *http.Client
	maxRetryWait     time.Duration // longer 429 waits are returned, not slept
}

func New(site, email, apiKey string) *Client {
	return &Client{site: strings.TrimRight(site, "/"), email: email, key: apiKey,
		http: &http.Client{}, maxRetryWait: time.Minute}
}

// Once returns a copy of c that returns every 429 instead of sleeping it
// out and retrying.
func (c *Client) Once() *Client {
	cc := *c
	cc.maxRetryWait = -1
	return &cc
}

// User is the subset of a Zulip user object we use.
type User struct {
	ID       int64  `json:"user_id"`
	Email    string `json:"email"`
	FullName string `json:"full_name"`
	IsActive bool   `json:"is_active"`
	IsBot    bool   `json:"is_bot"`
}

// CheckServer refuses servers older than MinFeatureLevel and returns the
// bot's own user id; bad credentials get an actionable message.
func (c *Client) CheckServer(ctx context.Context) (botID int64, err error) {
	var s struct {
		Level int `json:"zulip_feature_level"`
	}
	if err := c.call(ctx, "GET", "server_settings", nil, &s); err != nil {
		return 0, fmt.Errorf("zulip server settings: %w", err)
	}
	if s.Level < MinFeatureLevel {
		return 0, fmt.Errorf("zulip server too old (feature level %d): Zulip Server 10 or newer (feature level %d+) is required", s.Level, MinFeatureLevel)
	}
	var me struct {
		ID int64 `json:"user_id"`
	}
	if err := c.call(ctx, "GET", "users/me", nil, &me); err != nil {
		var e *Error
		if errors.As(err, &e) && e.Status == http.StatusUnauthorized {
			return 0, errors.New("zulip rejected the bot credentials (HTTP 401): check ZULIP_BOT_EMAIL and ZULIP_BOT_API_KEY")
		}
		return 0, fmt.Errorf("zulip users/me: %w", err)
	}
	return me.ID, nil
}

// UserByEmail looks up an active human by e-mail. ErrNotFound covers an
// unknown or hidden address; any other error (401, 403, 5xx, malformed
// reply) is a real failure, never "not found".
func (c *Client) UserByEmail(ctx context.Context, email string) (User, error) {
	return c.user(ctx, "users/"+url.PathEscape(email))
}

// UserByID is UserByEmail by Zulip user id.
func (c *Client) UserByID(ctx context.Context, id int64) (User, error) {
	return c.user(ctx, "users/"+strconv.FormatInt(id, 10))
}

func (c *Client) user(ctx context.Context, path string) (User, error) {
	var r struct {
		User *User `json:"user"`
	}
	err := c.call(ctx, "GET", path, nil, &r)
	var e *Error
	switch {
	case errors.As(err, &e) && e.Status == http.StatusBadRequest && e.Code == "BAD_REQUEST":
		return User{}, ErrNotFound // "No such user"; also what a hidden address gets
	case err != nil:
		return User{}, err
	case r.User == nil || r.User.ID == 0:
		return User{}, errors.New("zulip: malformed user response")
	case !r.User.IsActive || r.User.IsBot:
		return User{}, ErrNotFound
	}
	return *r.User, nil
}

// SendDM sends a direct message and returns its message id. A recipient that
// is deactivated or gone yields ErrRecipient.
func (c *Client) SendDM(ctx context.Context, userID int64, content string) (int64, error) {
	var r struct {
		ID int64 `json:"id"`
	}
	err := c.call(ctx, "POST", "messages", url.Values{
		"type":    {"direct"},
		"to":      {fmt.Sprintf("[%d]", userID)},
		"content": {content},
	}, &r)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusBadRequest {
		// Zulip answers every bad request with BAD_REQUEST; ask about the
		// recipient instead of parsing (translatable) message text.
		if _, uerr := c.UserByID(ctx, userID); errors.Is(uerr, ErrNotFound) {
			return 0, fmt.Errorf("%w: %w", ErrRecipient, err)
		}
	}
	if err != nil {
		return 0, err
	}
	return r.ID, nil
}

// call does one API request, sleeping out up to two 429s whose Retry-After
// is at most maxRetryWait.
func (c *Client) call(ctx context.Context, method, path string, params url.Values, out any) error {
	for attempt := 0; ; attempt++ {
		err := c.once(ctx, method, path, params, out)
		var e *Error
		if attempt < 2 && errors.As(err, &e) && e.Status == http.StatusTooManyRequests && e.RetryAfter <= c.maxRetryWait {
			select {
			case <-time.After(e.RetryAfter):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return err
	}
}

func (c *Client) once(ctx context.Context, method, path string, params url.Values, out any) error {
	timeout := 30 * time.Second
	if path == "events" {
		timeout = 3 * time.Minute // long poll; the server sends a heartbeat well before
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u := c.site + "/api/v1/" + path
	var body io.Reader
	if method == "GET" {
		if len(params) > 0 {
			u += "?" + params.Encode()
		}
	} else {
		body = strings.NewReader(params.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.email, c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// *url.Error carries the URL, i.e. user e-mails: never return it.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("zulip: %s %s: %w", method, endpoint(path), err)
	}
	defer resp.Body.Close()
	// No size cap: ZULIP_SITE is admin-configured, and a capped /events
	// backlog would be retried forever without ever being acknowledged.
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		e := &Error{Status: resp.StatusCode}
		var r struct{ Code, Msg string }
		json.Unmarshal(raw, &r) // best effort: proxies return HTML
		e.Code, e.Msg = r.Code, r.Msg
		if e.Status == http.StatusTooManyRequests {
			e.RetryAfter = time.Second
			if s, err := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); err == nil && s >= 0 {
				e.RetryAfter = time.Duration(s * float64(time.Second))
			}
		}
		return e
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("zulip: malformed %s response: %w", endpoint(path), err)
		}
	}
	return nil
}

// endpoint is path without user data, for error messages (they get logged).
func endpoint(path string) string {
	if strings.HasPrefix(path, "users/") && path != "users/me" {
		return "users/{user}"
	}
	return path
}
