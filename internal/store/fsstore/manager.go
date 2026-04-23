package fsstore

import (
	"path/filepath"
	"sync"
)

// Manager returns a per-user FS rooted at data/{username}/. Instances
// are cached so event-stream writers don't reinitialise the sequence
// counters on every append.
type Manager struct {
	dataDir string

	mu sync.Mutex
	fs map[string]*FS
}

func NewManager(dataDir string) *Manager {
	return &Manager{dataDir: dataDir, fs: map[string]*FS{}}
}

// Get returns the FS for username. Never fails — directories are
// created lazily by Save*.
func (m *Manager) Get(username string) *FS {
	m.mu.Lock()
	defer m.mu.Unlock()
	if f, ok := m.fs[username]; ok {
		return f
	}
	f := New(filepath.Join(m.dataDir, username))
	m.fs[username] = f
	return f
}

// Evict forgets the FS for username (after the user dir has been
// removed).
func (m *Manager) Evict(username string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.fs, username)
}

// AttemptDir returns the absolute path to an attempt dir for the given
// user (without touching the filesystem).
func (m *Manager) AttemptDir(username, attemptID string) string {
	return filepath.Join(m.dataDir, username, "attempt", attemptID)
}

// UserRoot returns the absolute path to the user's data directory.
func (m *Manager) UserRoot(username string) string {
	return filepath.Join(m.dataDir, username)
}
