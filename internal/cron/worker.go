// Package cron is the cron worker. It maintains next_run_at timestamps
// on task_schedules rows and, on each tick, fires due schedules by
// starting a fresh Attempt on the owning task. Schedules live in each
// user's per-user DB; the worker iterates every registered user.
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
	"github.com/ahkimkoo/hermes-taskboard/internal/userdir"
)

type Worker struct {
	Stores *store.Manager
	Users  *userdir.Manager
	Runner *attempt.Runner
	Logger *slog.Logger
	// TickInterval defaults to 5 s when zero.
	TickInterval time.Duration
}

// Compute computes the NextRunAt field for a schedule relative to `from`.
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

func Validate(kind store.ScheduleKind, spec string) error {
	stub := store.Schedule{Kind: kind, Spec: spec, Enabled: true}
	return Compute(&stub, time.Now())
}

func (w *Worker) Start(ctx context.Context) {
	interval := w.TickInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	w.rehydrate(ctx)
	go w.loop(ctx, interval)
}

func (w *Worker) rehydrate(ctx context.Context) {
	for _, u := range w.Users.List() {
		st, err := w.Stores.Get(u.Username)
		if err != nil {
			continue
		}
		rows, err := st.ListEnabledNullNextSchedules(ctx)
		if err != nil {
			w.Logger.Warn("rehydrate list", "user", u.Username, "err", err)
			continue
		}
		now := time.Now()
		for i := range rows {
			s := rows[i]
			if err := Compute(&s, now); err != nil {
				w.Logger.Warn("rehydrate compute", "user", u.Username, "id", s.ID, "spec", s.Spec, "err", err)
				s.Enabled = false
				s.NextRunAt = nil
			}
			if err := st.UpdateSchedule(ctx, &s); err != nil {
				w.Logger.Warn("rehydrate update", "user", u.Username, "id", s.ID, "err", err)
			}
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
	for _, u := range w.Users.List() {
		if w.Users.IsDisabled(u.Username) {
			continue
		}
		st, err := w.Stores.Get(u.Username)
		if err != nil {
			continue
		}
		due, err := st.ListDueSchedules(ctx, now)
		if err != nil {
			w.Logger.Warn("list due schedules", "user", u.Username, "err", err)
			continue
		}
		for i := range due {
			s := due[i]
			last := now
			s.LastRunAt = &last
			if err := Compute(&s, now); err != nil {
				w.Logger.Warn("schedule compute next", "user", u.Username, "id", s.ID, "err", err)
				s.Enabled = false
				s.NextRunAt = nil
			}
			if err := st.UpdateSchedule(ctx, &s); err != nil {
				w.Logger.Warn("schedule update", "user", u.Username, "id", s.ID, "err", err)
				continue
			}
			if _, err := w.Runner.Start(ctx, u.Username, s.TaskID, "", ""); err != nil {
				if _, ok := err.(*attempt.ConcurrencyErr); !ok {
					w.Logger.Warn("schedule fire", "user", u.Username, "id", s.ID, "task", s.TaskID, "err", err)
				}
			}
		}
	}
}
