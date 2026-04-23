package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	sqlitex "github.com/ahkimkoo/hermes-taskboard/internal/store/sqlite"
)

// Manager routes a username to its per-user *Store. Stores are opened
// lazily on first access and cached for the lifetime of the process.
//
// The DB file lives at data/{username}/db/taskboard.db. Removing a user
// via userdir.Manager.Delete() blows the directory away — callers MUST
// also call Manager.Evict(username) so we drop the now-invalid handle.
type Manager struct {
	dataDir string

	mu     sync.Mutex
	stores map[string]*Store
}

// NewManager constructs an empty manager. Nothing is opened until
// Get() is called with a username.
func NewManager(dataDir string) *Manager {
	return &Manager{dataDir: dataDir, stores: map[string]*Store{}}
}

// Get returns the Store for username, opening the DB if needed. The
// directory structure (data/{username}/db/) is created on demand.
func (m *Manager) Get(username string) (*Store, error) {
	if username == "" {
		return nil, errors.New("username required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.stores[username]; ok {
		return s, nil
	}
	path := m.DBPath(username)
	db, err := sqlitex.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open user db %q: %w", username, err)
	}
	s := New(db)
	m.stores[username] = s
	return s, nil
}

// DBPath returns the absolute DB path for a user.
func (m *Manager) DBPath(username string) string {
	return filepath.Join(m.dataDir, username, "db", "taskboard.db")
}

// Evict closes and forgets a user's DB handle. Safe to call for a
// username we never opened.
func (m *Manager) Evict(username string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.stores[username]; ok {
		_ = s.Close()
		delete(m.stores, username)
	}
}

// Close shuts every cached handle. Call on shutdown.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.stores {
		_ = s.Close()
	}
	m.stores = map[string]*Store{}
}

// ForEach runs fn against every username passed in. fn receives the
// store (opening if needed) and the name. Stops on first error. Used
// by schedulers / cron / reaper to iterate across users.
func (m *Manager) ForEach(usernames []string, fn func(username string, s *Store) error) error {
	for _, u := range usernames {
		s, err := m.Get(u)
		if err != nil {
			return err
		}
		if err := fn(u, s); err != nil {
			return err
		}
	}
	return nil
}

// ----- cross-user aggregations -----

// ActiveCounts sums the (queued|running|needs_input) attempt counts
// across every supplied user. Used for global + server-level +
// server+model-level concurrency checks.
func (m *Manager) ActiveCounts(ctx context.Context, usernames []string, serverID, model string) (global, byServer, byPair int, err error) {
	for _, u := range usernames {
		s, e := m.Get(u)
		if e != nil {
			err = e
			return
		}
		g, bs, bp, e := s.CountActive(ctx, serverID, model)
		if e != nil {
			err = e
			return
		}
		global += g
		byServer += bs
		byPair += bp
	}
	return
}

// ListAllActiveAttempts aggregates ListActiveAttempts across users,
// tagging each Attempt with its owning username. Used by the
// ResumeOrphans logic on boot.
type OwnedAttempt struct {
	Username string
	Attempt  *Attempt
}

func (m *Manager) ListAllActiveAttempts(ctx context.Context, usernames []string) ([]OwnedAttempt, error) {
	out := []OwnedAttempt{}
	for _, u := range usernames {
		s, err := m.Get(u)
		if err != nil {
			return nil, err
		}
		atts, err := s.ListActiveAttempts(ctx)
		if err != nil {
			return nil, err
		}
		for _, a := range atts {
			out = append(out, OwnedAttempt{Username: u, Attempt: a})
		}
	}
	return out, nil
}

// FindAttempt searches every user's DB for an attempt id. Returns the
// owning username + attempt, or ("", nil) when not found. O(n) over
// users — fine for the admin impersonation / scheduler path.
func (m *Manager) FindAttempt(ctx context.Context, usernames []string, id string) (string, *Attempt, error) {
	for _, u := range usernames {
		s, err := m.Get(u)
		if err != nil {
			return "", nil, err
		}
		a, err := s.GetAttempt(ctx, id)
		if err == nil {
			return u, a, nil
		}
		if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, ErrNotFound) {
			return "", nil, err
		}
	}
	return "", nil, nil
}
