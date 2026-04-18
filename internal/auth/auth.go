// Package auth implements optional username/password gate for the board.
// Storage is in data/config.yaml (auth.*); cookies are HMAC-signed with session_secret.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/ahkimkoo/hermes-taskboard/internal/config"
)

const CookieName = "hermes_taskboard_session"

type Service struct {
	Cfg *config.Store
}

func New(cfg *config.Store) *Service { return &Service{Cfg: cfg} }

// Enable creates a new auth record; allowed only when auth is currently disabled.
func (s *Service) Enable(username, password string) error {
	if len(password) < 8 {
		return errors.New("password must be ≥8 characters")
	}
	if strings.TrimSpace(username) == "" {
		return errors.New("username required")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	return s.Cfg.Mutate(func(c *config.Config) error {
		if c.Auth.Enabled {
			return errors.New("already enabled")
		}
		c.Auth.Enabled = true
		c.Auth.Username = username
		c.Auth.PasswordHash = string(hash)
		c.Auth.SessionSecret = base64.StdEncoding.EncodeToString(secret)
		if c.Auth.SessionTTLHours == 0 {
			c.Auth.SessionTTLHours = 168
		}
		return nil
	})
}

// Disable requires current password; clears credentials and rotates secret (invalidates cookies).
func (s *Service) Disable(password string) error {
	cur := s.Cfg.Snapshot()
	if !cur.Auth.Enabled {
		return errors.New("not enabled")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(cur.Auth.PasswordHash), []byte(password)); err != nil {
		return errors.New("bad password")
	}
	return s.Cfg.Mutate(func(c *config.Config) error {
		c.Auth.Enabled = false
		c.Auth.Username = ""
		c.Auth.PasswordHash = ""
		c.Auth.SessionSecret = ""
		return nil
	})
}

// ChangePassword verifies old password then updates hash; rotates secret to invalidate old cookies.
func (s *Service) ChangePassword(oldPw, newPw string) error {
	cur := s.Cfg.Snapshot()
	if !cur.Auth.Enabled {
		return errors.New("auth not enabled")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(cur.Auth.PasswordHash), []byte(oldPw)); err != nil {
		return errors.New("bad password")
	}
	if len(newPw) < 8 {
		return errors.New("new password must be ≥8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPw), 12)
	if err != nil {
		return err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	return s.Cfg.Mutate(func(c *config.Config) error {
		c.Auth.PasswordHash = string(hash)
		c.Auth.SessionSecret = base64.StdEncoding.EncodeToString(secret)
		return nil
	})
}

// Login verifies credentials, returns a signed cookie value.
func (s *Service) Login(username, password string) (string, time.Time, error) {
	cur := s.Cfg.Snapshot()
	if !cur.Auth.Enabled {
		return "", time.Time{}, errors.New("auth not enabled")
	}
	if cur.Auth.Username != username {
		return "", time.Time{}, errors.New("invalid credentials")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(cur.Auth.PasswordHash), []byte(password)); err != nil {
		return "", time.Time{}, errors.New("invalid credentials")
	}
	ttl := time.Duration(cur.Auth.SessionTTLHours) * time.Hour
	if ttl == 0 {
		ttl = 168 * time.Hour
	}
	exp := time.Now().Add(ttl)
	token, err := signToken(cur.Auth.SessionSecret, username, exp)
	if err != nil {
		return "", time.Time{}, err
	}
	return token, exp, nil
}

// Valid checks a cookie value against current secret.
func (s *Service) Valid(token string) bool {
	cur := s.Cfg.Snapshot()
	if !cur.Auth.Enabled {
		return true
	}
	return validateToken(cur.Auth.SessionSecret, token)
}

// Middleware enforces login on non-public API paths when auth is enabled.
func (s *Service) Middleware(isPublic func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cur := s.Cfg.Snapshot()
			if !cur.Auth.Enabled || isPublic(r) {
				next.ServeHTTP(w, r)
				return
			}
			cookie, err := r.Cookie(CookieName)
			if err != nil || !validateToken(cur.Auth.SessionSecret, cookie.Value) {
				if strings.HasPrefix(r.URL.Path, "/api/") {
					http.Error(w, `{"code":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// WriteCookie sets the session cookie.
func WriteCookie(w http.ResponseWriter, value string, expires time.Time, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

// IsSecureRequest detects HTTPS from the direct connection or proxy headers.
func IsSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto == "https" {
		return true
	}
	if proto := r.Header.Get("X-Forwarded-Ssl"); proto == "on" {
		return true
	}
	return false
}

func ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ----- token helpers -----

type tokenPayload struct {
	User string `json:"u"`
	Exp  int64  `json:"e"`
}

func signToken(secretB64, user string, exp time.Time) (string, error) {
	key, err := base64.StdEncoding.DecodeString(secretB64)
	if err != nil {
		return "", err
	}
	p := tokenPayload{User: user, Exp: exp.Unix()}
	body, _ := json.Marshal(p)
	bodyB64 := base64.RawURLEncoding.EncodeToString(body)
	h := hmac.New(sha256.New, key)
	h.Write([]byte(bodyB64))
	sig := base64.RawURLEncoding.EncodeToString(h.Sum(nil))
	return bodyB64 + "." + sig, nil
}

func validateToken(secretB64, token string) bool {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}
	key, err := base64.StdEncoding.DecodeString(secretB64)
	if err != nil {
		return false
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte(parts[0]))
	wantSig := base64.RawURLEncoding.EncodeToString(h.Sum(nil))
	if !hmac.Equal([]byte(wantSig), []byte(parts[1])) {
		return false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	var p tokenPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return false
	}
	if time.Unix(p.Exp, 0).Before(time.Now()) {
		return false
	}
	return true
}

// Ensure bcrypt import is not pruned when cross-compiling against CGO-less targets.
var _ = fmt.Sprintf
