// Package reaper sweeps each user's attempt directories older than a
// retention threshold whose attempt ID no longer exists in that user's
// DB, and removes them.
package reaper

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/ahkimkoo/hermes-taskboard/internal/store"
	"github.com/ahkimkoo/hermes-taskboard/internal/userdir"
)

type Reaper struct {
	Stores    *store.Manager
	Users     *userdir.Manager
	DataDir   string
	Retention time.Duration
	Logger    *slog.Logger
}

// Sweep scans every user's attempt directory. Returns the total count
// of removed directories.
func (r *Reaper) Sweep(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-r.Retention)
	total := 0
	for _, u := range r.Users.List() {
		n, err := r.sweepUser(ctx, u.Username, cutoff)
		if err != nil {
			r.Logger.Warn("reaper user sweep", "user", u.Username, "err", err)
			continue
		}
		total += n
	}
	if total > 0 {
		r.Logger.Info("sweep complete", "removed", total, "cutoff", cutoff.Format(time.RFC3339))
	}
	return total, nil
}

func (r *Reaper) sweepUser(ctx context.Context, username string, cutoff time.Time) (int, error) {
	dir := filepath.Join(r.DataDir, username, "attempt")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	st, err := r.Stores.Get(username)
	if err != nil {
		return 0, err
	}
	rows, err := st.DB.QueryContext(ctx, `SELECT id FROM attempts`)
	if err != nil {
		return 0, err
	}
	valid := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		valid[id] = struct{}{}
	}
	rows.Close()

	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		info, err := e.Info()
		if err != nil {
			r.Logger.Warn("stat failed", "user", username, "id", id, "err", err)
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if _, ok := valid[id]; ok {
			continue
		}
		path := filepath.Join(dir, id)
		if err := os.RemoveAll(path); err != nil {
			r.Logger.Error("failed to remove", "user", username, "id", id, "err", err)
			continue
		}
		r.Logger.Info("removed orphan", "user", username, "id", id, "mtime", info.ModTime().Format(time.RFC3339))
		removed++
	}
	return removed, nil
}

// Loop runs Sweep every 24 hours until the context is cancelled.
func (r *Reaper) Loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			r.Logger.Info("reaper stopped")
			return
		default:
			if _, err := r.Sweep(ctx); err != nil {
				r.Logger.Error("sweep failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(24 * time.Hour):
		}
	}
}
