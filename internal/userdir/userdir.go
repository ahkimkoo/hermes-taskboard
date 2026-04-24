// Package userdir manages the on-disk user registry.
//
// Layout (rooted at the global data/ directory):
//
//	data/
//	  config.yaml                 # global knobs (server / scheduler / archive / oss / session)
//	  {username}/
//	    config.yaml               # per-user: id, hash, is_admin, preferences, hermes_servers, tags
//	    disabled                  # sentinel file, presence = disabled
//	    db/taskboard.db           # this user's tasks / attempts / deps / schedules
//	    task/{task-id}.json       # task descriptions
//	    attempt/{attempt-id}/     # attempt event logs
//
// The Manager owns an in-memory cache keyed by username. Every mutating
// method atomically rewrites the matching YAML (and updates the cache);
// Reload() rescans the whole data dir so admins can hand-edit
// config.yaml and have the board pick it up without a restart.
//
// Users are identified by directory name. Renaming is not supported —
// to rename you delete + recreate.
package userdir

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// UserConfig is the serialised shape of data/{username}/config.yaml.
// Username (= directory name) is the sole identifier; no separate UUID
// is stored. If you need to rename, delete + recreate.
type UserConfig struct {
	Username      string         `yaml:"username" json:"username"`
	PasswordHash  string         `yaml:"password_hash" json:"-"`
	IsAdmin       bool           `yaml:"is_admin" json:"is_admin"`
	CreatedAt     time.Time      `yaml:"created_at" json:"created_at"`
	Preferences   Preferences    `yaml:"preferences" json:"preferences"`
	HermesServers []HermesServer `yaml:"hermes_servers" json:"hermes_servers"`
	Tags          []Tag          `yaml:"tags" json:"tags"`
}

// Preferences: per-user display / sound / language choices.
type Preferences struct {
	Language string `yaml:"language" json:"language"`
	Theme    string `yaml:"theme" json:"theme"`
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

// HermesServer is owned by a user. `Shared=true` means other users can
// see it in their server list and use it to dispatch tasks, but cannot
// edit or delete it.
type HermesServer struct {
	ID            string        `yaml:"id" json:"id"`
	Name          string        `yaml:"name" json:"name"`
	BaseURL       string        `yaml:"base_url" json:"base_url"`
	APIKey        string        `yaml:"api_key,omitempty" json:"-"`         // plaintext convenience — auto-encrypted on save
	APIKeyEnc     string        `yaml:"api_key_enc,omitempty" json:"-"`     // stored form
	IsDefault     bool          `yaml:"is_default" json:"is_default"`
	MaxConcurrent int           `yaml:"max_concurrent" json:"max_concurrent"`
	Models        []HermesModel `yaml:"models" json:"models"`
	Shared        bool          `yaml:"shared,omitempty" json:"shared"`
}

type HermesModel struct {
	Name          string `yaml:"name" json:"name"`
	IsDefault     bool   `yaml:"is_default" json:"is_default"`
	MaxConcurrent int    `yaml:"max_concurrent" json:"max_concurrent"`
}

// Tag is a user-owned label with an optional system prompt. Shared
// tags appear in other users' tag lists as read-only.
type Tag struct {
	Name         string `yaml:"name" json:"name"`
	Color        string `yaml:"color,omitempty" json:"color"`
	SystemPrompt string `yaml:"system_prompt,omitempty" json:"system_prompt"`
	Shared       bool   `yaml:"shared,omitempty" json:"shared"`
}

// Manager caches every user config in memory and guards the on-disk
// writes. Instances are safe for concurrent use.
type Manager struct {
	Root   string
	secret []byte

	mu    sync.RWMutex
	users map[string]*UserConfig // keyed by Username (= directory name)
}

// New constructs the manager but does not load anything yet — call
// LoadAll(). secret is the 32-byte AEAD key used to encrypt api keys
// at rest (shared with the global config's secret).
func New(root string, secret []byte) *Manager {
	return &Manager{Root: root, secret: secret, users: map[string]*UserConfig{}}
}

// DataDir returns the root data directory. Useful for subsystems that
// need to derive per-user paths (db, task, attempt).
func (m *Manager) DataDir() string { return m.Root }

// UserDir returns the directory that contains the user's config.yaml,
// disabled sentinel, db, task, attempt folders.
func (m *Manager) UserDir(username string) string {
	return filepath.Join(m.Root, username)
}

// LoadAll scans data/*/config.yaml and populates the cache. Missing
// directories, YAML errors on a single file, and unreadable files are
// logged to stderr but don't block other users from loading.
func (m *Manager) LoadAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.MkdirAll(m.Root, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(m.Root)
	if err != nil {
		return err
	}
	fresh := map[string]*UserConfig{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if isReservedDir(name) {
			continue
		}
		cfgPath := filepath.Join(m.Root, name, "config.yaml")
		b, err := os.ReadFile(cfgPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			fmt.Fprintf(os.Stderr, "userdir: skip %s: %v\n", name, err)
			continue
		}
		var u UserConfig
		if err := yaml.Unmarshal(b, &u); err != nil {
			fmt.Fprintf(os.Stderr, "userdir: parse %s/config.yaml: %v\n", name, err)
			continue
		}
		// Directory name is authoritative for username — if the yaml
		// disagrees (e.g. user hand-renamed the folder) we trust the
		// directory to keep the cookie → user lookup consistent.
		u.Username = name
		normalizeUser(&u)
		fresh[name] = &u
	}
	m.users = fresh
	return nil
}

// Reload is an alias for LoadAll intended for admin-triggered refreshes.
func (m *Manager) Reload() error { return m.LoadAll() }

// Get returns a copy of the user config. Nil + false when not found.
// Returns a deep copy so callers can't mutate the cache directly.
func (m *Manager) Get(username string) (*UserConfig, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[username]
	if !ok {
		return nil, false
	}
	return deepCopy(u), true
}

// GetRaw returns the cache entry WITHOUT copying. Read-only — callers
// MUST NOT mutate the returned struct. Primarily for hot paths like
// login + listing where allocation pressure matters.
func (m *Manager) GetRaw(username string) (*UserConfig, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[username]
	return u, ok
}

// List returns copies of every user config, sorted by username.
func (m *Manager) List() []*UserConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*UserConfig, 0, len(m.users))
	for _, u := range m.users {
		out = append(out, deepCopy(u))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

// Count returns the number of cached users.
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.users)
}

// Create writes a fresh user directory + config.yaml + the complete
// subdirectory skeleton (db/, task/, attempt/). Doing the skeleton
// eagerly instead of letting each of them land lazily on first use
// keeps `ls data/{username}/` looking consistent for operators and
// removes a class of "why doesn't my new user have a db/ yet" head-
// scratching. The per-user SQLite file is still opened lazily by
// store.Manager — we only lay down the empty directories here.
func (m *Manager) Create(u *UserConfig) error {
	if u == nil || u.Username == "" {
		return errors.New("username required")
	}
	if err := validateUsername(u.Username); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.users[u.Username]; exists {
		return fmt.Errorf("user %q already exists", u.Username)
	}
	// Guard against clobbering a real user whose config.yaml failed to
	// parse (LoadAll logs and skips those rather than erroring). Without
	// this check, a corrupted admin/config.yaml would silently be
	// overwritten by ensureDefaultAdmin on the next boot, destroying
	// the real password hash, hermes_servers, and tags. Refuse loudly
	// so the operator sees the problem instead.
	existing := filepath.Join(m.Root, u.Username, "config.yaml")
	if _, err := os.Stat(existing); err == nil {
		return fmt.Errorf("user %q has an existing config.yaml at %s — refusing to overwrite. Fix or remove the file first.", u.Username, existing)
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	normalizeUser(u)
	dir := filepath.Join(m.Root, u.Username)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, sub := range []string{"db", "task", "attempt"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return fmt.Errorf("mkdir %s/%s: %w", u.Username, sub, err)
		}
	}
	if err := m.persist(u); err != nil {
		return err
	}
	m.users[u.Username] = deepCopy(u)
	return nil
}

// Save persists the given user's config.yaml and refreshes the cache.
// Fails when the user doesn't exist on disk.
func (m *Manager) Save(u *UserConfig) error {
	if u == nil || u.Username == "" {
		return errors.New("username required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.users[u.Username]; !exists {
		return fmt.Errorf("user %q not found", u.Username)
	}
	normalizeUser(u)
	if err := m.persist(u); err != nil {
		return err
	}
	m.users[u.Username] = deepCopy(u)
	return nil
}

// Mutate applies fn to a copy of the user's config, persists it, and
// updates the cache atomically. Never use on admin deletion — use
// Delete + Create for creation/removal.
func (m *Manager) Mutate(username string, fn func(*UserConfig) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[username]
	if !ok {
		return fmt.Errorf("user %q not found", username)
	}
	cpy := deepCopy(u)
	if err := fn(cpy); err != nil {
		return err
	}
	normalizeUser(cpy)
	if err := m.persist(cpy); err != nil {
		return err
	}
	m.users[username] = cpy
	return nil
}

// Delete removes the user from the cache AND rm -rf's their whole
// directory — tasks, attempts, DB, config. Irreversible.
func (m *Manager) Delete(username string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[username]; !ok {
		return fmt.Errorf("user %q not found", username)
	}
	dir := filepath.Join(m.Root, username)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	delete(m.users, username)
	return nil
}

// SetDisabled toggles the disabled sentinel for username.
//   disabled=true  → creates data/{username}/disabled
//   disabled=false → removes it
// Does not affect cookies — disabled users fail every auth middleware
// check until the flag is cleared.
func (m *Manager) SetDisabled(username string, disabled bool) error {
	m.mu.RLock()
	_, exists := m.users[username]
	m.mu.RUnlock()
	if !exists {
		return fmt.Errorf("user %q not found", username)
	}
	p := filepath.Join(m.Root, username, "disabled")
	if disabled {
		return os.WriteFile(p, []byte("disabled by admin\n"), 0o600)
	}
	err := os.Remove(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// IsDisabled returns true if data/{username}/disabled exists.
func (m *Manager) IsDisabled(username string) bool {
	_, err := os.Stat(filepath.Join(m.Root, username, "disabled"))
	return err == nil
}

// persist writes u to disk, encrypting any plaintext hermes api_keys
// first. Caller holds m.mu.
func (m *Manager) persist(u *UserConfig) error {
	cpy := deepCopy(u)
	for i := range cpy.HermesServers {
		sv := &cpy.HermesServers[i]
		if sv.APIKey != "" {
			enc, err := aesGCMEncrypt(m.secret, sv.APIKey)
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
	dir := filepath.Join(m.Root, u.Username)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "config.yaml")
	if err := atomicWrite(path, data, 0o600); err != nil {
		return err
	}
	// Reflect encryption back into the caller's pointer so the cache
	// doesn't keep the plaintext around.
	for i := range u.HermesServers {
		if u.HermesServers[i].APIKey != "" {
			u.HermesServers[i].APIKeyEnc = cpy.HermesServers[i].APIKeyEnc
			u.HermesServers[i].APIKey = ""
		}
	}
	return nil
}

// DecryptedAPIKey decrypts a server's api key using the manager's
// secret. Returns the plaintext, or "" on any error / empty input.
func (m *Manager) DecryptedAPIKey(sv *HermesServer) string {
	if sv == nil {
		return ""
	}
	if sv.APIKey != "" {
		return sv.APIKey
	}
	if sv.APIKeyEnc == "" {
		return ""
	}
	p, err := aesGCMDecrypt(m.secret, sv.APIKeyEnc)
	if err != nil {
		return ""
	}
	return p
}

// ----- shared views -----

// ServerView is what non-owners see for a shared server — api key is
// stripped, ownership is indicated by the owner's username (which is
// also their data directory name).
type ServerView struct {
	HermesServer
	OwnerUsername string `json:"owner_username"`
	Mine          bool   `json:"mine"`
}

// VisibleServers returns the server list that `viewer` should see:
// their own servers plus any other user's servers marked shared.
// Ordered: viewer's servers first (alpha by id), then shared servers
// alpha by id. Always returns a non-nil slice.
func (m *Manager) VisibleServers(viewerUsername string) []ServerView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []ServerView{}
	me, ok := m.users[viewerUsername]
	if ok {
		for _, sv := range me.HermesServers {
			out = append(out, ServerView{
				HermesServer:  stripKey(sv),
				OwnerUsername: me.Username,
				Mine:          true,
			})
		}
	}
	for name, u := range m.users {
		if name == viewerUsername {
			continue
		}
		for _, sv := range u.HermesServers {
			if !sv.Shared {
				continue
			}
			out = append(out, ServerView{
				HermesServer:  stripKey(sv),
				OwnerUsername: u.Username,
				Mine:          false,
			})
		}
	}
	return out
}

// FindServer looks up a server by id across all users. Returns the
// owning username + a copy of the row, plus a flag indicating whether
// `viewer` may use it (owner or shared).
func (m *Manager) FindServer(viewer, id string) (owner string, sv HermesServer, usable, found bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for name, u := range m.users {
		for _, s := range u.HermesServers {
			if s.ID == id {
				owner = name
				sv = s
				found = true
				usable = name == viewer || s.Shared
				return
			}
		}
	}
	return
}

// TagView is the tag DTO with ownership tacked on.
type TagView struct {
	Tag
	OwnerUsername string `json:"owner_username"`
	Mine          bool   `json:"mine"`
}

// VisibleTags — viewer's tags + every other user's shared tags.
func (m *Manager) VisibleTags(viewerUsername string) []TagView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []TagView{}
	me, ok := m.users[viewerUsername]
	if ok {
		for _, t := range me.Tags {
			out = append(out, TagView{Tag: t, OwnerUsername: me.Username, Mine: true})
		}
	}
	for name, u := range m.users {
		if name == viewerUsername {
			continue
		}
		for _, t := range u.Tags {
			if !t.Shared {
				continue
			}
			out = append(out, TagView{Tag: t, OwnerUsername: u.Username, Mine: false})
		}
	}
	return out
}

// TagByName returns a tag (and its owner) visible to viewer. Returns
// found=false when the name doesn't exist in viewer's own list and in
// no shared list from other users.
func (m *Manager) TagByName(viewer, name string) (owner string, tag Tag, mine, found bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if u, ok := m.users[viewer]; ok {
		for _, t := range u.Tags {
			if t.Name == name {
				return viewer, t, true, true
			}
		}
	}
	for uname, u := range m.users {
		if uname == viewer {
			continue
		}
		for _, t := range u.Tags {
			if t.Name == name && t.Shared {
				return uname, t, false, true
			}
		}
	}
	return "", Tag{}, false, false
}

// DefaultServer picks the server the scheduler should dispatch against
// for `viewer` when nothing is specified. Priority:
//  1. viewer's own server marked is_default
//  2. viewer's first server
//  3. any shared server marked is_default from other users
//  4. any shared server
// Returns nil when no usable server exists yet.
func (m *Manager) DefaultServer(viewer string) *ServerView {
	views := m.VisibleServers(viewer)
	if len(views) == 0 {
		return nil
	}
	for _, v := range views {
		if v.Mine && v.IsDefault {
			return &v
		}
	}
	for _, v := range views {
		if v.Mine {
			return &v
		}
	}
	for _, v := range views {
		if v.IsDefault {
			return &v
		}
	}
	v := views[0]
	return &v
}

// ----- helpers -----

func stripKey(s HermesServer) HermesServer {
	cp := s
	cp.APIKey = ""
	cp.APIKeyEnc = ""
	return cp
}

func normalizeUser(u *UserConfig) {
	if u.Preferences.Theme == "" {
		u.Preferences.Theme = "dark"
	}
	if u.Preferences.Sound.Volume == 0 && !u.Preferences.Sound.Enabled {
		// zero-value sound block: set a sensible default
		u.Preferences.Sound = Sound{
			Enabled: true, Volume: 0.7,
			Events: SoundEvents{ExecuteStart: true, NeedsInput: true, Done: true},
		}
	}
	// Enforce at most one default server per user.
	seenDefault := false
	for i := range u.HermesServers {
		sv := &u.HermesServers[i]
		if sv.MaxConcurrent <= 0 {
			sv.MaxConcurrent = 10
		}
		if sv.IsDefault {
			if seenDefault {
				sv.IsDefault = false
			}
			seenDefault = true
		}
		modelDefault := 0
		for j := range sv.Models {
			m := &sv.Models[j]
			if m.MaxConcurrent <= 0 {
				m.MaxConcurrent = 5
			}
			if m.IsDefault {
				modelDefault++
			}
		}
		if modelDefault == 0 && len(sv.Models) > 0 {
			sv.Models[0].IsDefault = true
		}
	}
}

func deepCopy(u *UserConfig) *UserConfig {
	if u == nil {
		return nil
	}
	data, _ := yaml.Marshal(u)
	out := &UserConfig{}
	_ = yaml.Unmarshal(data, out)
	// yaml round-trip nukes the empty time.Time back to zero but we
	// want to keep whatever was already there.
	out.CreatedAt = u.CreatedAt
	return out
}

// validateUsername rejects names that would be confusing on disk
// (path separators, reserved names, dotfiles).
func validateUsername(name string) error {
	if len(name) == 0 || len(name) > 64 {
		return errors.New("username must be 1-64 chars")
	}
	if isReservedDir(name) {
		return fmt.Errorf("username %q is reserved", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return fmt.Errorf("username may only contain a-z, A-Z, 0-9, _, -, .: got %q", name)
		}
	}
	if name[0] == '.' || name[0] == '-' {
		return errors.New("username may not start with '.' or '-'")
	}
	return nil
}

// isReservedDir blocks dir names we use for purposes other than user
// data (the global db path for migration backups, uploads, tmp, etc.)
// from being interpreted as user accounts.
func isReservedDir(name string) bool {
	switch name {
	case "db", "task", "attempt", "uploads", "_migrated", "tmp":
		return true
	}
	// historical `data/_migrated-YYYYMMDD` style backup dirs
	if len(name) > 10 && name[:10] == "_migrated-" {
		return true
	}
	return false
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, mode)
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
	return os.Rename(tmp, path)
}

// These helpers duplicate config's AEAD primitives, but we want userdir
// to be self-contained and not import config.
func aesGCMEncrypt(key []byte, plain string) (string, error) {
	return aeadEncrypt(key, plain)
}
func aesGCMDecrypt(key []byte, enc string) (string, error) {
	return aeadDecrypt(key, enc)
}

// base64 wrapper kept as a tiny indirection so the crypto implementation
// can move without touching call sites.
var _ = base64.StdEncoding
