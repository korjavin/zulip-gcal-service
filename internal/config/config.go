// Package config reads the service configuration from the environment
// (docs/design.md §2) and derives the secret subkeys from APP_SECRET.
package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config is the validated service configuration. Secret fields are never
// included in error messages or logs.
type Config struct {
	PublicURL          string
	GoogleClientID     string
	GoogleClientSecret string
	ZulipSite          string
	ZulipBotEmail      string
	ZulipBotAPIKey     string
	AllowedDomains     []string
	DataDir            string
	DefaultLeadMinutes int
	PollInterval       time.Duration
	Secrets            *Secrets
}

// Load validates the environment via getenv (os.Getenv in production) and
// reports every problem at once.
func Load(getenv func(string) string) (*Config, error) {
	var errs []error
	c := &Config{
		PublicURL:          strings.TrimRight(getenv("PUBLIC_URL"), "/"),
		GoogleClientID:     getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: getenv("GOOGLE_CLIENT_SECRET"),
		ZulipSite:          strings.TrimRight(getenv("ZULIP_SITE"), "/"),
		ZulipBotEmail:      getenv("ZULIP_BOT_EMAIL"),
		ZulipBotAPIKey:     getenv("ZULIP_BOT_API_KEY"),
		DataDir:            getenv("DATA_DIR"),
		DefaultLeadMinutes: 10,
		PollInterval:       3 * time.Minute,
	}
	for _, kv := range []struct{ name, val string }{
		{"GOOGLE_CLIENT_ID", c.GoogleClientID},
		{"GOOGLE_CLIENT_SECRET", c.GoogleClientSecret},
		{"ZULIP_BOT_EMAIL", c.ZulipBotEmail},
		{"ZULIP_BOT_API_KEY", c.ZulipBotAPIKey},
	} {
		if kv.val == "" {
			errs = append(errs, fmt.Errorf("%s is required", kv.name))
		}
	}
	for _, kv := range []struct{ name, val string }{
		{"PUBLIC_URL", c.PublicURL},
		{"ZULIP_SITE", c.ZulipSite},
	} {
		if err := checkURL(kv.val); err != nil {
			errs = append(errs, fmt.Errorf("%s %v", kv.name, err))
		}
	}

	if raw := getenv("APP_SECRET"); raw == "" {
		errs = append(errs, errors.New("APP_SECRET is required"))
	} else if key, err := base64.StdEncoding.DecodeString(raw); err != nil {
		errs = append(errs, errors.New("APP_SECRET must be base64"))
	} else if len(key) < 32 {
		errs = append(errs, errors.New("APP_SECRET must decode to at least 32 bytes"))
	} else if c.Secrets, err = NewSecrets(key); err != nil {
		errs = append(errs, err)
	}

	for _, d := range strings.Split(getenv("ALLOWED_DOMAINS"), ",") {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			c.AllowedDomains = append(c.AllowedDomains, d)
		}
	}
	if c.DataDir == "" {
		c.DataDir = "/data"
	}
	if v := getenv("DEFAULT_LEAD_MINUTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 60 {
			errs = append(errs, errors.New("DEFAULT_LEAD_MINUTES must be an integer 1-60"))
		}
		c.DefaultLeadMinutes = n
	}
	if v := getenv("POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			errs = append(errs, errors.New("POLL_INTERVAL must be a positive duration like 3m"))
		}
		c.PollInterval = d
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

func checkURL(v string) error {
	if v == "" {
		return errors.New("is required")
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("must be an absolute http(s) URL")
	}
	return nil
}

// Secrets holds the subkeys derived from APP_SECRET with HKDF-SHA256.
type Secrets struct {
	tokenAEAD cipher.AEAD
	// CookieKey is the HMAC key for signed session cookies.
	CookieKey []byte
}

// NewSecrets derives the "token-enc" (AES-256-GCM) and "cookie" (HMAC) subkeys.
func NewSecrets(appSecret []byte) (*Secrets, error) {
	encKey, err := hkdf.Key(sha256.New, appSecret, nil, "token-enc", 32)
	if err != nil {
		return nil, err
	}
	cookieKey, err := hkdf.Key(sha256.New, appSecret, nil, "cookie", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Secrets{tokenAEAD: aead, CookieKey: cookieKey}, nil
}

// Encrypt seals plaintext (a refresh token) as nonce||ciphertext.
func (s *Secrets) Encrypt(plaintext []byte) []byte {
	nonce := make([]byte, s.tokenAEAD.NonceSize())
	rand.Read(nonce) // never fails (crypto/rand panics instead)
	return s.tokenAEAD.Seal(nonce, nonce, plaintext, nil)
}

// Decrypt opens a value produced by Encrypt.
func (s *Secrets) Decrypt(sealed []byte) ([]byte, error) {
	n := s.tokenAEAD.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("ciphertext too short")
	}
	return s.tokenAEAD.Open(nil, sealed[:n], sealed[n:], nil)
}
