// Package scheduler scans Plan-queued tasks with trigger=auto and
// creates Attempts when dependencies + concurrency gates allow. In the
// multi-user world every tick iterates every known user's DB.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/ahkimkoo/hermes-taskboard/internal/attempt"
	"github.com/ahkimkoo/hermes-taskboard/internal/config"
	"github.com/ahkimkoo/hermes-taskboard/internal/store"
	"github.com/ahkimkoo/hermes-taskboard/internal/userdir"
)

type Scheduler struct {
	Cfg    *config.Store
	Stores *store.Manager
	Users  *userdir.Manager
	Runner *attempt.Runner
	Logger *slog.Logger
	stop   chan struct{}
}

func New(cfg *config.Store, stores *store.Manager, users *userdir.Manager, r *attempt.Runner, logger *slog.Logger) *Scheduler {
	return &Scheduler{Cfg: cfg, Stores: stores, Users: users, Runner: r, Logger: logger, stop: make(chan struct{})}
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
	for _, u := range s.Users.List() {
		st, err := s.Stores.Get(u.Username)
		if err != nil {
			s.Logger.Warn("scheduler store open", "user", u.Username, "err", err)
			continue
		}
		// Skip disabled users — their tasks sit idle until the admin
		// re-enables the account.
		if s.Users.IsDisabled(u.Username) {
			continue
		}
		s.tickUser(ctx, u.Username, st)
	}
}

func (s *Scheduler) tickUser(ctx context.Context, username string, st *store.Store) {
	ids, err := st.TaskIDs(ctx, store.StatusPlan)
	if err != nil {
		s.Logger.Warn("scheduler list plan", "user", username, "err", err)
		return
	}
	for _, id := range ids {
		t, err := st.GetTask(ctx, id)
		if err != nil {
			continue
		}
		if t.TriggerMode != store.TriggerAuto {
			continue
		}
		ok, err := st.AllDependenciesDone(ctx, id)
		if err != nil || !ok {
			continue
		}
		_, err = s.Runner.Start(ctx, username, id, "", "")
		if err != nil {
			if _, ok := err.(*attempt.ConcurrencyErr); !ok {
				s.Logger.Info("scheduler skip", "user", username, "task", id, "reason", err)
			}
			continue
		}
	}
}
