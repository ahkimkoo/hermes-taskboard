// Package reaper sweeps data/attempt directories older than a retention
// threshold whose attempt ID no longer exists in the DB, and removes them.
package reaper

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Reaper removes orphan attempt directories.
type Reaper struct {
	DB        *sql.DB
	AttemptDir string
	Retention  time.Duration
	Logger     *slog.Logger
}

// Sweep scans attempt directories, deletes those older than Retention
// with no matching row in the attempts table, and returns the count of
// removed directories.
func (r *Reaper) Sweep(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-r.Retention)
	log := r.Logger.With("cutoff", cutoff.Format(time.RFC3339))

	// Load all valid attempt IDs from DB.
	validIDs, err := r.listAttemptIDs(ctx)
	if err != nil {
		return 0, err
	}

	entries, err := os.ReadDir(r.AttemptDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()

		info, err := e.Info()
		if err != nil {
			log.Warn("stat failed", "id", id, "err", err)
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}

		if _, ok := validIDs[id]; ok {
			continue
		}

		dir := filepath.Join(r.AttemptDir, id)
		if err := os.RemoveAll(dir); err != nil {
			log.Error("failed to remove", "id", id, "err", err)
			continue
		}
		log.Info("removed orphan", "id", id, "mtime", info.ModTime().Format(time.RFC3339))
		removed++
	}

	if removed > 0 {
		log.Info("sweep complete", "removed", removed)
	}
	return removed, nil
}

// Loop runs Sweep every 24 hours until the context is cancelled.
// The first sweep runs immediately on startup.
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

func (r *Reaper) listAttemptIDs(ctx context.Context) (map[string]struct{}, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT id FROM attempts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	return ids, rows.Err()
}
