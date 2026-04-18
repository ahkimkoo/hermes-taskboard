// Package scheduler scans Plan-queued tasks with trigger=auto and creates
// Attempts when dependencies + concurrency gates allow.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/ahkimkoo/hermes-taskboard/internal/attempt"
	"github.com/ahkimkoo/hermes-taskboard/internal/config"
	"github.com/ahkimkoo/hermes-taskboard/internal/store"
)

type Scheduler struct {
	Cfg    *config.Store
	Store  *store.Store
	Runner *attempt.Runner
	Logger *slog.Logger
	stop   chan struct{}
}

func New(cfg *config.Store, s *store.Store, r *attempt.Runner, logger *slog.Logger) *Scheduler {
	return &Scheduler{Cfg: cfg, Store: s, Runner: r, Logger: logger, stop: make(chan struct{})}
}

func (s *Scheduler) Start(ctx context.Context) {
	go s.loop(ctx)
}

func (s *Scheduler) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
}

func (s *Scheduler) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-time.After(s.interval()):
		}
		s.tick(ctx)
	}
}

func (s *Scheduler) interval() time.Duration {
	cfg := s.Cfg.Snapshot()
	n := cfg.Scheduler.ScanIntervalSeconds
	if n <= 0 {
		n = 5
	}
	return time.Duration(n) * time.Second
}

func (s *Scheduler) tick(ctx context.Context) {
	ids, err := s.Store.TaskIDs(ctx, store.StatusPlan)
	if err != nil {
		s.Logger.Warn("scheduler list plan", "err", err)
		return
	}
	for _, id := range ids {
		t, err := s.Store.GetTask(ctx, id)
		if err != nil {
			continue
		}
		if t.TriggerMode != store.TriggerAuto {
			continue
		}
		ok, err := s.Store.AllDependenciesDone(ctx, id)
		if err != nil || !ok {
			continue
		}
		// try to start; concurrency checked inside Runner.Start
		_, err = s.Runner.Start(ctx, id, "", "")
		if err != nil {
			// concurrency errors are fine; log others quietly
			if _, ok := err.(*attempt.ConcurrencyErr); !ok {
				s.Logger.Info("scheduler skip", "task", id, "reason", err)
			}
			continue
		}
	}
}
