// Package cron is the task scheduler. It maintains next_run_at timestamps on
// task_schedules rows and, on each tick, fires due schedules by starting a
// fresh Attempt on the owning task (bypassing the Plan-column requirement
// used by the auto-trigger scheduler).
//
// Two schedule kinds are supported:
//   interval — time.ParseDuration-compatible string ("15m", "2h30m"). After
//              a run finishes, next_run_at = now + duration.
//   cron     — 5-field cron ("min hour dom month dow") parsed by robfig/cron.
//
// Multiple schedules per task are allowed; each lives in its own row and ticks
// independently.
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
	switch s.Kind {
	case store.ScheduleInterval:
		d, err := time.ParseDuration(strings.TrimSpace(s.Spec))
		if err != nil {
			return fmt.Errorf("invalid interval %q: %w", s.Spec, err)
		}
		if d < 10*time.Second {
			return fmt.Errorf("interval must be ≥10s, got %s", d)
		}
		next := from.Add(d)
		s.NextRunAt = &next
	case store.ScheduleCron:
		schedule, err := rfcron.ParseStandard(strings.TrimSpace(s.Spec))
		if err != nil {
			return fmt.Errorf("invalid cron %q: %w", s.Spec, err)
		}
		next := schedule.Next(from)
		s.NextRunAt = &next
	default:
		return fmt.Errorf("unknown schedule kind %q", s.Kind)
	}
	return nil
}

// Validate checks whether a spec parses and passes minimum-interval rules.
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
	go w.loop(ctx, interval)
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
