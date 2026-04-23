// Package config holds the GLOBAL taskboard configuration — knobs that
// apply to the board as a whole and are admin-only. Per-user state
// (passwords, preferences, hermes servers, tags) lives under
// internal/userdir. HTTP-listen + session secret + scheduler +
// archive + OSS integration are the things that stay here.
package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

// Config is the full GLOBAL configuration persisted in data/config.yaml.
type Config struct {
	Auth      Auth      `yaml:"auth"`
	Server    Server    `yaml:"server"`
	Scheduler Scheduler `yaml:"scheduler"`
	Archive   Archive   `yaml:"archive"`
	OSS       OSS       `yaml:"oss"`
}

// Auth holds only the bits that are global: cookie signing + TTL.
// Per-user credentials live in data/{username}/config.yaml.
type Auth struct {
	SessionSecret   string `yaml:"session_secret"`
	SessionTTLHours int    `yaml:"session_ttl_hours"`
}

type Server struct {
	Listen      string   `yaml:"listen" json:"listen"`
	CorsOrigins []string `yaml:"cors_origins" json:"cors_origins"`
}

type Scheduler struct {
	ScanIntervalSeconds int `yaml:"scan_interval_seconds" json:"scan_interval_seconds"`
	GlobalMaxConcurrent int `yaml:"global_max_concurrent" json:"global_max_concurrent"`
}

type Archive struct {
	AutoPurgeDays int `yaml:"auto_purge_days" json:"auto_purge_days"`
}

// OSS is the optional Aliyun OSS image-hosting config.
type OSS struct {
	Enabled            bool   `yaml:"enabled" json:"enabled"`
	Endpoint           string `yaml:"endpoint" json:"endpoint"`
	Bucket             string `yaml:"bucket" json:"bucket"`
	AccessKeyID        string `yaml:"access_key_id" json:"access_key_id"`
	AccessKeySecret    string `yaml:"access_key_secret,omitempty" json:"access_key_secret,omitempty"`
	AccessKeySecretEnc string `yaml:"access_key_secret_enc,omitempty" json:"-"`
	PathPrefix         string `yaml:"path_prefix" json:"path_prefix"`
	PublicBase         string `yaml:"public_base" json:"public_base"`
}

// DecryptedAccessKeySecret returns the plaintext secret (empty if none).
func (o *OSS) DecryptedAccessKeySecret(secret []byte) string {
	if o.AccessKeySecret != "" {
		return o.AccessKeySecret
	}
	if o.AccessKeySecretEnc == "" {
		return ""
	}
	plain, err := aesGCMDecrypt(secret, o.AccessKeySecretEnc)
	if err != nil {
		return ""
	}
	return plain
}

// Store wraps an atomic *Config pointer with persistence.
type Store struct {
	path    string
	secret  []byte
	cur     atomic.Pointer[Config]
	mu      sync.Mutex // serialize writes
	hooks   []func(old, new *Config)
	hooksMu sync.RWMutex
}

// NewStore loads the config file (creating defaults if missing) and returns a Store.
func NewStore(path string, secretPath string) (*Store, error) {
	secret, err := loadOrCreateSecret(secretPath)
	if err != nil {
		return nil, fmt.Errorf("load secret: %w", err)
	}
	s := &Store{path: path, secret: secret}
	if err := s.Reload(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			def := DefaultConfig()
			s.cur.Store(def)
			if err := s.persist(def); err != nil {
				return nil, err
			}
			return s, nil
		}
		return nil, err
	}
	return s, nil
}

// Snapshot returns an immutable reference to the current config.
func (s *Store) Snapshot() *Config { return s.cur.Load() }

// Secret exposes the encryption secret (used by userdir for api-key AEAD).
func (s *Store) Secret() []byte { return s.secret }

// AddHook registers a callback fired after every successful swap with (old, new).
func (s *Store) AddHook(fn func(old, new *Config)) {
	s.hooksMu.Lock()
	defer s.hooksMu.Unlock()
	s.hooks = append(s.hooks, fn)
}

// Mutate applies fn to a deep copy, validates, persists, and swaps.
func (s *Store) Mutate(fn func(*Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.cur.Load()
	cpy := deepCopy(cur)
	if err := fn(cpy); err != nil {
		return err
	}
	if err := s.normalizeAndValidate(cpy); err != nil {
		return err
	}
	if err := s.persist(cpy); err != nil {
		return err
	}
	old := cur
	s.cur.Store(cpy)
	s.fireHooks(old, cpy)
	return nil
}

// Reload re-reads the file and swaps.
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("parse yaml: %w", err)
	}
	if err := s.normalizeAndValidate(cfg); err != nil {
		return err
	}
	old := s.cur.Load()
	s.cur.Store(cfg)
	s.fireHooks(old, cfg)
	return nil
}

// persist writes the config to disk atomically, encrypting any plaintext OSS secret.
func (s *Store) persist(cfg *Config) error {
	cpy := deepCopy(cfg)
	if cpy.OSS.AccessKeySecret != "" {
		enc, err := aesGCMEncrypt(s.secret, cpy.OSS.AccessKeySecret)
		if err != nil {
			return fmt.Errorf("encrypt oss secret: %w", err)
		}
		cpy.OSS.AccessKeySecretEnc = enc
		cpy.OSS.AccessKeySecret = ""
	}
	data, err := yaml.Marshal(cpy)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	if cfg.OSS.AccessKeySecret != "" {
		cfg.OSS.AccessKeySecretEnc = cpy.OSS.AccessKeySecretEnc
		cfg.OSS.AccessKeySecret = ""
	}
	return nil
}

func (s *Store) fireHooks(old, new *Config) {
	s.hooksMu.RLock()
	hooks := append([]func(old, new *Config){}, s.hooks...)
	s.hooksMu.RUnlock()
	for _, h := range hooks {
		h(old, new)
	}
}

// normalizeAndValidate fills defaults + enforces invariants.
func (s *Store) normalizeAndValidate(c *Config) error {
	if c.Scheduler.ScanIntervalSeconds <= 0 {
		c.Scheduler.ScanIntervalSeconds = 5
	}
	if c.Scheduler.GlobalMaxConcurrent <= 0 {
		c.Scheduler.GlobalMaxConcurrent = 50
	}
	if c.Archive.AutoPurgeDays < 0 {
		c.Archive.AutoPurgeDays = 0
	}
	if c.Server.Listen == "" {
		c.Server.Listen = "0.0.0.0:1900"
	}
	if c.Auth.SessionTTLHours <= 0 {
		c.Auth.SessionTTLHours = 168
	}
	return nil
}

// DefaultConfig returns a sensible empty config.
func DefaultConfig() *Config {
	return &Config{
		Auth:   Auth{SessionTTLHours: 168},
		Server: Server{Listen: "0.0.0.0:1900"},
		Scheduler: Scheduler{
			ScanIntervalSeconds: 5,
			GlobalMaxConcurrent: 50,
		},
		Archive: Archive{AutoPurgeDays: 30},
	}
}

// ---------- helpers ----------

func deepCopy(c *Config) *Config {
	data, _ := yaml.Marshal(c)
	out := &Config{}
	_ = yaml.Unmarshal(data, out)
	return out
}

// loadOrCreateSecret loads the 32-byte AES key from secretPath, creating it if missing.
func loadOrCreateSecret(path string) ([]byte, error) {
	if env := os.Getenv("APP_SECRET"); env != "" {
		if b, err := base64.StdEncoding.DecodeString(env); err == nil && len(b) >= 32 {
			return b[:32], nil
		}
		b := []byte(env)
		if len(b) < 32 {
			pad := make([]byte, 32-len(b))
			b = append(b, pad...)
		}
		return b[:32], nil
	}
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b[:32], nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	return b, nil
}

func aesGCMEncrypt(key []byte, plain string) (string, error) {
	if len(key) < 32 {
		return "", errors.New("bad key")
	}
	blk, err := aes.NewCipher(key[:32])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

func aesGCMDecrypt(key []byte, enc string) (string, error) {
	if len(key) < 32 {
		return "", errors.New("bad key")
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	blk, err := aes.NewCipher(key[:32])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// HermesDefaultAgent is the profile name Hermes Agent itself ships as
// the built-in default. We keep it in this package to avoid introducing
// a cycle with userdir: both packages may fall back to this name when
// a user-defined server has no explicit models listed.
const HermesDefaultAgent = "hermes-agent"
