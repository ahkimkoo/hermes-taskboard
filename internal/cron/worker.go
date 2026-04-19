// Package cron is the task scheduler. It maintains next_run_at timestamps on
// task_schedules rows and, on each tick, fires due schedules by starting a
// fresh Attempt on the owning task (bypassing the Plan-column requirement
// used by the auto-trigger scheduler).
//
// Schedules use standard 5-field cron ("min hour dom month dow") parsed by
// robfig/cron. Multiple schedules per task are allowed; each lives in its own
// row and ticks independently.
package cron

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	rfcron "github.com/robfig/cron/v3"

	"github.com/ahkimkoo/hermes-taskboard/internal/attempt"
	"github.com/ahkimkoo/hermes-taskboard/internal/store"
)

type Worker struct {
	Store  *store.Store
	Runner *attempt.Runner
	Logger *slog.Logger
	// TickInterval defaults to 5 s when zero.
	TickInterval time.Duration
}

// Compute computes the NextRunAt field for a schedule relative to `from`.
// Updates s in place. Returns an error if the spec is malformed.
func Compute(s *store.Schedule, from time.Time) error {
	if !s.Enabled {
		s.NextRunAt = nil
		return nil
	}
	schedule, err := rfcron.ParseStandard(strings.TrimSpace(s.Spec))
	if err != nil {
		return fmt.Errorf("invalid cron %q: %w", s.Spec, err)
	}
	next := schedule.Next(from)
	s.NextRunAt = &next
	return nil
}

// Validate checks whether a spec parses as a 5-field cron expression.
func Validate(kind store.ScheduleKind, spec string) error {
	stub := store.Schedule{Kind: kind, Spec: spec, Enabled: true}
	return Compute(&stub, time.Now())
}

// Start runs the scheduler goroutine until ctx is cancelled.
func (w *Worker) Start(ctx context.Context) {
	interval := w.TickInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	w.rehydrate(ctx)
	go w.loop(ctx, interval)
}

// rehydrate recomputes next_run_at for any enabled schedule that lost it
// (NULL in DB) — e.g. rows freshly migrated from interval → cron. Without
// this, ListDueSchedules would skip them forever.
func (w *Worker) rehydrate(ctx context.Context) {
	rows, err := w.Store.ListEnabledNullNextSchedules(ctx)
	if err != nil {
		w.Logger.Warn("rehydrate list", "err", err)
		return
	}
	now := time.Now()
	for i := range rows {
		s := rows[i]
		if err := Compute(&s, now); err != nil {
			w.Logger.Warn("rehydrate compute", "id", s.ID, "spec", s.Spec, "err", err)
			s.Enabled = false
			s.NextRunAt = nil
		}
		if err := w.Store.UpdateSchedule(ctx, &s); err != nil {
			w.Logger.Warn("rehydrate update", "id", s.ID, "err", err)
		}
	}
}

func (w *Worker) loop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

func (w *Worker) tick(ctx context.Context) {
	now := time.Now()
	due, err := w.Store.ListDueSchedules(ctx, now)
	if err != nil {
		w.Logger.Warn("list due schedules", "err", err)
		return
	}
	for i := range due {
		s := due[i]
		// Always bump next_run_at BEFORE firing so a Hermes failure doesn't
		// trap us into an infinite retry loop within one tick window.
		last := now
		s.LastRunAt = &last
		if err := Compute(&s, now); err != nil {
			w.Logger.Warn("schedule compute next", "id", s.ID, "err", err)
			// Disable the schedule so it doesn't re-fire instantly.
			s.Enabled = false
			s.NextRunAt = nil
		}
		if err := w.Store.UpdateSchedule(ctx, &s); err != nil {
			w.Logger.Warn("schedule update", "id", s.ID, "err", err)
			continue
		}
		if _, err := w.Runner.Start(ctx, s.TaskID, "", ""); err != nil {
			// Concurrency limit is a soft failure — we'll catch it next tick.
			if _, ok := err.(*attempt.ConcurrencyErr); !ok {
				w.Logger.Warn("schedule fire", "id", s.ID, "task", s.TaskID, "err", err)
			}
		}
	}
}
