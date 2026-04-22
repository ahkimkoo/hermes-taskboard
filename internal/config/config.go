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
	"time"

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
	OSS            OSS            `yaml:"oss"`
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

type Auth struct {
	Enabled         bool   `yaml:"enabled"`
	Username        string `yaml:"username"`
	PasswordHash    string `yaml:"password_hash"`
	SessionSecret   string `yaml:"session_secret"`
	SessionTTLHours int    `yaml:"session_ttl_hours"`
}

type Server struct {
	Listen      string   `yaml:"listen" json:"listen"`
	CorsOrigins []string `yaml:"cors_origins" json:"cors_origins"`
}

type HermesServer struct {
	ID            string        `yaml:"id"`
	Name          string        `yaml:"name"`
	// Transport selects how taskboard reaches this Hermes.
	//   "" or "http" — dial BaseURL + APIKey, current behavior
	//   "plugin"     — a plugin with hermes_id==ID dials into taskboard's
	//                  /api/plugin/ws. BaseURL / APIKey are ignored.
	Transport     string        `yaml:"transport,omitempty"`
	BaseURL       string        `yaml:"base_url,omitempty"`
	APIKey        string        `yaml:"api_key,omitempty"`      // plaintext convenience — auto-encrypted on save
	APIKeyEnc     string        `yaml:"api_key_enc,omitempty"`  // encrypted storage form
	IsDefault     bool          `yaml:"is_default"`
	MaxConcurrent int           `yaml:"max_concurrent"`
	Models        []HermesModel `yaml:"models"`
}

// TransportKind returns the normalized transport string ("http" or "plugin").
func (h *HermesServer) TransportKind() string {
	switch strings.ToLower(strings.TrimSpace(h.Transport)) {
	case "plugin":
		return "plugin"
	default:
		return "http"
	}
}

type HermesModel struct {
	Name          string `yaml:"name"`
	IsDefault     bool   `yaml:"is_default"`
	MaxConcurrent int    `yaml:"max_concurrent"`
}

type Scheduler struct {
	ScanIntervalSeconds int `yaml:"scan_interval_seconds" json:"scan_interval_seconds"`
	GlobalMaxConcurrent int `yaml:"global_max_concurrent" json:"global_max_concurrent"`
}

type Archive struct {
	AutoPurgeDays int `yaml:"auto_purge_days" json:"auto_purge_days"`
}

type Preferences struct {
	Language string `yaml:"language" json:"language"`
	Theme    string `yaml:"theme" json:"theme"` // "dark" | "light" | "" (auto)
	Sound    Sound  `yaml:"sound" json:"sound"`
}

type Sound struct {
	Enabled bool        `yaml:"enabled" json:"enabled"`
	Volume  float64     `yaml:"volume" json:"volume"`
	Events  SoundEvents `yaml:"events" json:"events"`
}

type SoundEvents struct {
	ExecuteStart bool `yaml:"execute_start" json:"execute_start"`
	NeedsInput   bool `yaml:"needs_input" json:"needs_input"`
	Done         bool `yaml:"done" json:"done"`
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
	// Encrypt OSS access_key_secret.
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
	// Sync the in-memory cfg too (strip plaintext, install enc).
	for i := range cfg.HermesServers {
		sv := &cfg.HermesServers[i]
		if sv.APIKey != "" {
			sv.APIKeyEnc = cpy.HermesServers[i].APIKeyEnc
			sv.APIKey = ""
		}
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
			Theme:    "dark",
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

// HermesDefaultAgent is the profile name Hermes Agent itself ships as the
// built-in default (see docs/requirements.md and the upstream API-server
// reference). We use it as the last-resort fallback when a server entry has
// no Models configured, so a task targeting such a server still dispatches
// instead of silently sitting in Plan forever.
const HermesDefaultAgent = "hermes-agent"

// DefaultModel returns the server's default model name. Priority:
//
//  1. the model explicitly marked IsDefault
//  2. the first model listed
//  3. HermesDefaultAgent — the built-in Hermes default profile
//
// (3) exists because an operator can accidentally save a server with no
// models (e.g. by clearing the UI prefill), and that used to leave every
// auto-triggered task stuck in Plan with `no model resolvable`. Falling
// back to the documented Hermes default turns that silent-skip failure
// mode into a visible dispatch that either succeeds or fails loudly at
// the Hermes side.
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
	return HermesDefaultAgent
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

// ResolveServerModelWithReachability is the dispatch-time variant of
// ResolveServerModel. It consults an `isReachable` predicate so plugin-
// transport servers whose plugin hasn't connected (yet / any more) are
// filtered out of the auto-pick pool. When the caller didn't specify a
// preferred server and multiple eligible servers remain, one is picked
// uniformly at random — letting tasks fan out across connected Hermes
// hosts without a sticky default.
//
// `virtualPluginIDs` are plugin hermes_id values the caller has observed
// connected but which aren't in config.HermesServers. They're synthesised
// as transient plugin-transport servers (max_concurrent 5, hermes-agent
// default model) so installing the plugin on a new Hermes host is
// plug-and-play — no config edit required.
//
// If preferredServer is non-empty it's honoured when it resolves to
// either a config server or a virtual plugin id; otherwise the caller
// must surface the "plugin not connected" error itself.
func (c *Config) ResolveServerModelWithReachability(
	preferredServer, preferredModel string,
	isReachable func(serverID string) bool,
	virtualPluginIDs []string,
) (*HermesServer, string) {
	if preferredServer != "" {
		if sv := c.FindServer(preferredServer); sv != nil {
			return sv, c.resolveModelFor(sv, preferredModel)
		}
		for _, vid := range virtualPluginIDs {
			if vid == preferredServer {
				sv := virtualPluginServer(vid)
				return sv, sv.DefaultModel()
			}
		}
	}

	reachable := c.reachableServers(isReachable)
	// Merge virtual plugin servers (not already in config) into the pool.
	for _, vid := range virtualPluginIDs {
		if c.FindServer(vid) == nil && (isReachable == nil || isReachable(vid)) {
			reachable = append(reachable, virtualPluginServer(vid))
		}
	}
	if len(reachable) == 0 {
		return c.DefaultServer(), ""
	}
	// Prefer the configured default if it's in the reachable set.
	if def := c.DefaultServer(); def != nil {
		for _, sv := range reachable {
			if sv.ID == def.ID {
				return sv, c.resolveModelFor(sv, preferredModel)
			}
		}
	}
	// Uniform random across reachable servers (both configured + virtual).
	pick := reachable[randomInt(len(reachable))]
	return pick, c.resolveModelFor(pick, preferredModel)
}

// virtualPluginServer returns an in-memory HermesServer for a plugin
// that announced itself on WS but isn't in config. Sensible defaults
// that can be overridden later by adding a real entry with the same id.
func virtualPluginServer(id string) *HermesServer {
	return &HermesServer{
		ID:            id,
		Name:          id, // display name defaults to the hermes_id
		Transport:     "plugin",
		MaxConcurrent: 5,
		Models:        nil, // falls back to HermesDefaultAgent
	}
}

func (c *Config) resolveModelFor(sv *HermesServer, preferredModel string) string {
	if preferredModel != "" {
		for _, m := range sv.Models {
			if strings.EqualFold(m.Name, preferredModel) {
				return m.Name
			}
		}
	}
	return sv.DefaultModel()
}

func (c *Config) reachableServers(isReachable func(string) bool) []*HermesServer {
	if isReachable == nil {
		// No probe → treat every HTTP server as reachable, skip plugin ones.
		isReachable = func(id string) bool {
			for _, sv := range c.HermesServers {
				if sv.ID == id {
					return sv.TransportKind() == "http"
				}
			}
			return false
		}
	}
	out := make([]*HermesServer, 0, len(c.HermesServers))
	for i := range c.HermesServers {
		sv := &c.HermesServers[i]
		if isReachable(sv.ID) {
			out = append(out, sv)
		}
	}
	return out
}

// randomInt: tiny indirection so tests can stub. crypto/rand is overkill;
// math/rand's default source is seeded at process start from the clock,
// good enough for round-robin fan-out.
func randomInt(n int) int {
	if n <= 1 {
		return 0
	}
	return int(time.Now().UnixNano()%int64(n))
}
