// Package auth implements always-on username/password login backed by
// the userdir registry. Session cookies are HMAC-signed with a secret
// kept in the global config.yaml and carry the authenticated user's
// username — which is also the directory name under data/, making
// the cookie → user dir lookup trivial.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/ahkimkoo/hermes-taskboard/internal/config"
	"github.com/ahkimkoo/hermes-taskboard/internal/userdir"
)

const CookieName = "hermes_taskboard_session"

// Service validates credentials against userdir.Manager and signs
// session cookies.
type Service struct {
	Cfg *config.Store
	Dir *userdir.Manager
}

func New(cfg *config.Store, dir *userdir.Manager) *Service {
	return &Service{Cfg: cfg, Dir: dir}
}

// ChangePassword verifies old password for the given user, then
// writes a new hash into data/{username}/config.yaml.
func (s *Service) ChangePassword(ctx context.Context, username, oldPw, newPw string) error {
	u, ok := s.Dir.GetRaw(username)
	if !ok {
		return errors.New("user not found")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(oldPw)); err != nil {
		return errors.New("bad password")
	}
	if len(newPw) < 8 {
		return errors.New("new password must be ≥8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPw), 12)
	if err != nil {
		return err
	}
	return s.Dir.Mutate(username, func(uc *userdir.UserConfig) error {
		uc.PasswordHash = string(hash)
		return nil
	})
}

// AdminSetPassword overwrites a user's password without the old one.
// Caller is responsible for enforcing admin privilege.
func (s *Service) AdminSetPassword(ctx context.Context, username, newPw string) error {
	if len(newPw) < 8 {
		return errors.New("password must be ≥8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPw), 12)
	if err != nil {
		return err
	}
	return s.Dir.Mutate(username, func(uc *userdir.UserConfig) error {
		uc.PasswordHash = string(hash)
		return nil
	})
}

// Login verifies credentials and returns a signed cookie value.
// Disabled users (presence of the `disabled` sentinel) fail the
// check with "account disabled".
func (s *Service) Login(ctx context.Context, username, password string) (string, time.Time, *userdir.UserConfig, error) {
	u, ok := s.Dir.GetRaw(username)
	if !ok {
		return "", time.Time{}, nil, errors.New("invalid credentials")
	}
	if s.Dir.IsDisabled(username) {
		return "", time.Time{}, nil, errors.New("account disabled")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return "", time.Time{}, nil, errors.New("invalid credentials")
	}
	cur := s.Cfg.Snapshot()
	ttl := time.Duration(cur.Auth.SessionTTLHours) * time.Hour
	if ttl == 0 {
		ttl = 168 * time.Hour
	}
	secret, err := s.ensureSessionSecret()
	if err != nil {
		return "", time.Time{}, nil, err
	}
	exp := time.Now().Add(ttl)
	token, err := signToken(secret, username, exp)
	if err != nil {
		return "", time.Time{}, nil, err
	}
	// Return a safe copy (no hash).
	cpy := *u
	cpy.PasswordHash = ""
	return token, exp, &cpy, nil
}

// ensureSessionSecret returns the current session secret, generating
// and persisting one if none is set yet.
func (s *Service) ensureSessionSecret() (string, error) {
	cur := s.Cfg.Snapshot()
	if cur.Auth.SessionSecret != "" {
		return cur.Auth.SessionSecret, nil
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	encoded := base64.StdEncoding.EncodeToString(secret)
	err := s.Cfg.Mutate(func(c *config.Config) error {
		if c.Auth.SessionSecret == "" {
			c.Auth.SessionSecret = encoded
		} else {
			encoded = c.Auth.SessionSecret
		}
		return nil
	})
	return encoded, err
}

// ----- request context -----

type contextKey int

const userKey contextKey = 1

// UserFromContext returns the authenticated user, or nil when the
// request is unauthenticated.
func UserFromContext(ctx context.Context) *userdir.UserConfig {
	u, _ := ctx.Value(userKey).(*userdir.UserConfig)
	return u
}

// WithUser installs the user on the context.
func WithUser(ctx context.Context, u *userdir.UserConfig) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// Middleware enforces login on non-public paths and attaches the
// authenticated user to the request context.
func (s *Service) Middleware(isPublic func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cur := s.Cfg.Snapshot()
			var resolved *userdir.UserConfig
			if cookie, err := r.Cookie(CookieName); err == nil {
				if uname, ok := validateToken(cur.Auth.SessionSecret, cookie.Value); ok {
					if u, ok := s.Dir.GetRaw(uname); ok && !s.Dir.IsDisabled(uname) {
						cp := *u
						cp.PasswordHash = ""
						resolved = &cp
					}
				}
			}
			if resolved != nil {
				r = r.WithContext(WithUser(r.Context(), resolved))
			}
			if isPublic(r) {
				next.ServeHTTP(w, r)
				return
			}
			if resolved == nil {
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

// RequireAdmin gates an http.Handler so only admins reach it.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := UserFromContext(r.Context())
		if u == nil {
			http.Error(w, `{"code":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if !u.IsAdmin {
			http.Error(w, `{"code":"forbidden"}`, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
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
	User string `json:"u"` // username
	Exp  int64  `json:"e"`
}

func signToken(secretB64, username string, exp time.Time) (string, error) {
	key, err := base64.StdEncoding.DecodeString(secretB64)
	if err != nil {
		return "", err
	}
	p := tokenPayload{User: username, Exp: exp.Unix()}
	body, _ := json.Marshal(p)
	bodyB64 := base64.RawURLEncoding.EncodeToString(body)
	h := hmac.New(sha256.New, key)
	h.Write([]byte(bodyB64))
	sig := base64.RawURLEncoding.EncodeToString(h.Sum(nil))
	return bodyB64 + "." + sig, nil
}

func validateToken(secretB64, token string) (string, bool) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	key, err := base64.StdEncoding.DecodeString(secretB64)
	if err != nil {
		return "", false
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte(parts[0]))
	wantSig := base64.RawURLEncoding.EncodeToString(h.Sum(nil))
	if !hmac.Equal([]byte(wantSig), []byte(parts[1])) {
		return "", false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	var p tokenPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return "", false
	}
	if time.Unix(p.Exp, 0).Before(time.Now()) {
		return "", false
	}
	return p.User, true
}
