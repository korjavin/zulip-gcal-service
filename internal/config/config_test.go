package config

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

var secret = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32))

func valid() map[string]string {
	return map[string]string{
		"PUBLIC_URL":           "https://calendar.example.com/",
		"GOOGLE_CLIENT_ID":     "cid",
		"GOOGLE_CLIENT_SECRET": "csecret-value",
		"ZULIP_SITE":           "https://zulip.example.com",
		"ZULIP_BOT_EMAIL":      "calendar-bot@zulip.example.com",
		"ZULIP_BOT_API_KEY":    "apikey-value",
		"APP_SECRET":           secret,
	}
}

func load(env map[string]string) (*Config, error) {
	return Load(func(k string) string { return env[k] })
}

func TestLoadValidDefaults(t *testing.T) {
	c, err := load(valid())
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://calendar.example.com" || c.DataDir != "/data" ||
		c.DefaultLeadMinutes != 10 || c.PollInterval != 3*time.Minute || c.AllowedDomains != nil || c.Secrets == nil {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoadOptional(t *testing.T) {
	env := valid()
	env["ALLOWED_DOMAINS"] = " Example.com, ,b.org"
	env["DATA_DIR"] = "/tmp/x"
	env["DEFAULT_LEAD_MINUTES"] = "60"
	env["POLL_INTERVAL"] = "90s"
	c, err := load(env)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.AllowedDomains, ",") != "example.com,b.org" || c.DataDir != "/tmp/x" ||
		c.DefaultLeadMinutes != 60 || c.PollInterval != 90*time.Second {
		t.Fatalf("unexpected: %+v", c)
	}
}

func TestLoadInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  map[string]string
		want []string
	}{
		{"missing all", nil, []string{"PUBLIC_URL", "GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET", "ZULIP_SITE", "ZULIP_BOT_EMAIL", "ZULIP_BOT_API_KEY", "APP_SECRET"}},
		{"bad url", map[string]string{"PUBLIC_URL": "calendar.example.com"}, []string{"PUBLIC_URL"}},
		{"secret not base64", map[string]string{"APP_SECRET": "not base64!!"}, []string{"APP_SECRET must be base64"}},
		{"secret short", map[string]string{"APP_SECRET": base64.StdEncoding.EncodeToString(make([]byte, 31))}, []string{"at least 32 bytes"}},
		{"lead 0", map[string]string{"DEFAULT_LEAD_MINUTES": "0"}, []string{"DEFAULT_LEAD_MINUTES"}},
		{"lead 61", map[string]string{"DEFAULT_LEAD_MINUTES": "61"}, []string{"DEFAULT_LEAD_MINUTES"}},
		{"lead text", map[string]string{"DEFAULT_LEAD_MINUTES": "ten"}, []string{"DEFAULT_LEAD_MINUTES"}},
		{"poll bad", map[string]string{"POLL_INTERVAL": "soon"}, []string{"POLL_INTERVAL"}},
		{"poll negative", map[string]string{"POLL_INTERVAL": "-1m"}, []string{"POLL_INTERVAL"}},
		{"two problems", map[string]string{"ZULIP_SITE": "", "POLL_INTERVAL": "0s"}, []string{"ZULIP_SITE", "POLL_INTERVAL"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := valid()
			if tc.set == nil {
				env = map[string]string{}
			}
			for k, v := range tc.set {
				env[k] = v
			}
			_, err := load(env)
			if err == nil {
				t.Fatal("want error")
			}
			msg := err.Error()
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("error %q does not mention %q", msg, w)
				}
			}
			for _, v := range env {
				if len(v) > 3 && strings.Contains(msg, v) {
					t.Errorf("error leaks value %q", v)
				}
			}
		})
	}
}

func TestEncryptRoundTrip(t *testing.T) {
	s, err := NewSecrets([]byte(strings.Repeat("s", 32)))
	if err != nil {
		t.Fatal(err)
	}
	tok := []byte("1//refresh-token")
	a, b := s.Encrypt(tok), s.Encrypt(tok)
	if bytes.Equal(a, b) || bytes.Contains(a, tok) {
		t.Fatal("ciphertext must be randomized and opaque")
	}
	got, err := s.Decrypt(a)
	if err != nil || !bytes.Equal(got, tok) {
		t.Fatalf("round trip: %q %v", got, err)
	}
	a[len(a)-1] ^= 1
	if _, err := s.Decrypt(a); err == nil {
		t.Fatal("tampered ciphertext must fail")
	}
	other, _ := NewSecrets([]byte(strings.Repeat("t", 32)))
	if _, err := other.Decrypt(b); err == nil {
		t.Fatal("other key must fail")
	}
	if bytes.Equal(s.CookieKey, other.CookieKey) || len(s.CookieKey) != 32 {
		t.Fatal("cookie key must be derived per secret")
	}
}
