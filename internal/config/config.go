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
	"strings"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

// Config is the full app configuration persisted in data/config.yaml.
type Config struct {
	Auth           Auth           `yaml:"auth"`
	Server         Server         `yaml:"server"`
	HermesServers  []HermesServer `yaml:"hermes_servers"`
	Scheduler      Scheduler      `yaml:"scheduler"`
	Archive        Archive        `yaml:"archive"`
	Preferences    Preferences    `yaml:"preferences"`
}

type Auth struct {
	Enabled         bool   `yaml:"enabled"`
	Username        string `yaml:"username"`
	PasswordHash    string `yaml:"password_hash"`
	SessionSecret   string `yaml:"session_secret"`
	SessionTTLHours int    `yaml:"session_ttl_hours"`
}

type Server struct {
	Listen      string   `yaml:"listen"`
	CorsOrigins []string `yaml:"cors_origins"`
}

type HermesServer struct {
	ID            string        `yaml:"id"`
	Name          string        `yaml:"name"`
	BaseURL       string        `yaml:"base_url"`
	APIKey        string        `yaml:"api_key,omitempty"`      // plaintext convenience — auto-encrypted on save
	APIKeyEnc     string        `yaml:"api_key_enc,omitempty"`  // encrypted storage form
	IsDefault     bool          `yaml:"is_default"`
	MaxConcurrent int           `yaml:"max_concurrent"`
	Models        []HermesModel `yaml:"models"`
}

type HermesModel struct {
	Name          string `yaml:"name"`
	IsDefault     bool   `yaml:"is_default"`
	MaxConcurrent int    `yaml:"max_concurrent"`
}

type Scheduler struct {
	ScanIntervalSeconds int `yaml:"scan_interval_seconds"`
	GlobalMaxConcurrent int `yaml:"global_max_concurrent"`
}

type Archive struct {
	AutoPurgeDays int `yaml:"auto_purge_days"`
}

type Preferences struct {
	Language string `yaml:"language"`
	Sound    Sound  `yaml:"sound"`
}

type Sound struct {
	Enabled bool        `yaml:"enabled"`
	Volume  float64     `yaml:"volume"`
	Events  SoundEvents `yaml:"events"`
}

type SoundEvents struct {
	ExecuteStart bool `yaml:"execute_start"`
	NeedsInput   bool `yaml:"needs_input"`
	Done         bool `yaml:"done"`
}

// DecryptedAPIKey returns the plaintext key for a server; empty if none.
func (h *HermesServer) DecryptedAPIKey(secret []byte) string {
	if h.APIKey != "" {
		return h.APIKey
	}
	if h.APIKeyEnc == "" {
		return ""
	}
	plain, err := aesGCMDecrypt(secret, h.APIKeyEnc)
	if err != nil {
		return ""
	}
	return plain
}

// Store wraps an atomic *Config pointer with persistence.
type Store struct {
	path       string
	secret     []byte
	cur        atomic.Pointer[Config]
	mu         sync.Mutex // serialize writes
	hooks      []func(old, new *Config)
	hooksMu    sync.RWMutex
}

// NewStore loads the config file (creating defaults if missing) and returns a Store.
func NewStore(path string, secretPath string) (*Store, error) {
	secret, err := loadOrCreateSecret(secretPath)
	if err != nil {
		return nil, fmt.Errorf("load secret: %w", err)
	}
	s := &Store{path: path, secret: secret}
	if err := s.Reload(); err != nil {
		// file missing → write defaults
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

// Secret exposes the encryption secret (for decrypting api keys).
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
	// If user hand-wrote plaintext api_key, re-persist so it encrypts.
	needsPersist := false
	for i := range cfg.HermesServers {
		if cfg.HermesServers[i].APIKey != "" {
			needsPersist = true
		}
	}
	if needsPersist {
		if err := s.persist(cfg); err != nil {
			return err
		}
	}
	old := s.cur.Load()
	s.cur.Store(cfg)
	s.fireHooks(old, cfg)
	return nil
}

// persist writes the config to disk atomically, encrypting any plaintext api_key.
func (s *Store) persist(cfg *Config) error {
	cpy := deepCopy(cfg)
	// Encrypt plaintext api_key fields.
	for i := range cpy.HermesServers {
		sv := &cpy.HermesServers[i]
		if sv.APIKey != "" {
			enc, err := aesGCMEncrypt(s.secret, sv.APIKey)
			if err != nil {
				return fmt.Errorf("encrypt api key: %w", err)
			}
			sv.APIKeyEnc = enc
			sv.APIKey = ""
		}
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
	// Sync the in-memory cfg too (strip plaintext, install enc).
	for i := range cfg.HermesServers {
		sv := &cfg.HermesServers[i]
		if sv.APIKey != "" {
			sv.APIKeyEnc = cpy.HermesServers[i].APIKeyEnc
			sv.APIKey = ""
		}
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
	// Ensure at most one default server; if none, mark first as default.
	defaultCount := 0
	for i := range c.HermesServers {
		sv := &c.HermesServers[i]
		if sv.ID == "" {
			return fmt.Errorf("hermes_servers[%d]: id required", i)
		}
		if sv.MaxConcurrent <= 0 {
			sv.MaxConcurrent = 10
		}
		if sv.IsDefault {
			defaultCount++
		}
		modelDefault := 0
		for j := range sv.Models {
			m := &sv.Models[j]
			if m.Name == "" {
				return fmt.Errorf("hermes_servers[%s].models[%d]: name required", sv.ID, j)
			}
			if m.MaxConcurrent <= 0 {
				m.MaxConcurrent = 5
			}
			if m.IsDefault {
				modelDefault++
			}
		}
		if modelDefault > 1 {
			return fmt.Errorf("hermes_servers[%s]: only one default model allowed", sv.ID)
		}
		if modelDefault == 0 && len(sv.Models) > 0 {
			sv.Models[0].IsDefault = true
		}
	}
	if defaultCount > 1 {
		return errors.New("only one hermes_server may be is_default: true")
	}
	if defaultCount == 0 && len(c.HermesServers) > 0 {
		c.HermesServers[0].IsDefault = true
	}
	return nil
}

// DefaultConfig returns a sensible empty config.
func DefaultConfig() *Config {
	return &Config{
		Auth:   Auth{Enabled: false, SessionTTLHours: 168},
		Server: Server{Listen: "0.0.0.0:1900"},
		Scheduler: Scheduler{
			ScanIntervalSeconds: 5,
			GlobalMaxConcurrent: 50,
		},
		Archive: Archive{AutoPurgeDays: 30},
		Preferences: Preferences{
			Language: "",
			Sound: Sound{
				Enabled: true,
				Volume:  0.7,
				Events:  SoundEvents{ExecuteStart: true, NeedsInput: true, Done: true},
			},
		},
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
		// hex or base64; accept any 32+ byte decode
		if b, err := base64.StdEncoding.DecodeString(env); err == nil && len(b) >= 32 {
			return b[:32], nil
		}
		// fall back to raw bytes (padded/truncated)
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

// DefaultServer returns the hermes_servers entry marked is_default (or first).
func (c *Config) DefaultServer() *HermesServer {
	if c == nil {
		return nil
	}
	for i := range c.HermesServers {
		if c.HermesServers[i].IsDefault {
			return &c.HermesServers[i]
		}
	}
	if len(c.HermesServers) > 0 {
		return &c.HermesServers[0]
	}
	return nil
}

// FindServer looks up by id.
func (c *Config) FindServer(id string) *HermesServer {
	if c == nil {
		return nil
	}
	for i := range c.HermesServers {
		if c.HermesServers[i].ID == id {
			return &c.HermesServers[i]
		}
	}
	return nil
}

// DefaultModel returns the server's default model name (or first or empty).
func (h *HermesServer) DefaultModel() string {
	if h == nil {
		return ""
	}
	for _, m := range h.Models {
		if m.IsDefault {
			return m.Name
		}
	}
	if len(h.Models) > 0 {
		return h.Models[0].Name
	}
	return ""
}

// ResolveServerModel returns the effective server and model for a task.
// If preferred is empty / missing, falls back to default.
func (c *Config) ResolveServerModel(preferredServer, preferredModel string) (*HermesServer, string) {
	var sv *HermesServer
	if preferredServer != "" {
		sv = c.FindServer(preferredServer)
	}
	if sv == nil {
		sv = c.DefaultServer()
	}
	if sv == nil {
		return nil, ""
	}
	if preferredModel != "" {
		for _, m := range sv.Models {
			if strings.EqualFold(m.Name, preferredModel) {
				return sv, m.Name
			}
		}
	}
	return sv, sv.DefaultModel()
}
