// Hermes Task Board — single-binary Go backend serving a Vue 3 kanban UI,
// integrating with Hermes API server (gateway) for task execution.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ahkimkoo/hermes-taskboard/internal/attempt"
	"github.com/ahkimkoo/hermes-taskboard/internal/auth"
	"github.com/ahkimkoo/hermes-taskboard/internal/board"
	"github.com/ahkimkoo/hermes-taskboard/internal/config"
	cronpkg "github.com/ahkimkoo/hermes-taskboard/internal/cron"
	"github.com/ahkimkoo/hermes-taskboard/internal/hermes"
	"github.com/ahkimkoo/hermes-taskboard/internal/reaper"
	"github.com/ahkimkoo/hermes-taskboard/internal/scheduler"
	"github.com/ahkimkoo/hermes-taskboard/internal/server"
	"github.com/ahkimkoo/hermes-taskboard/internal/sse"
	"github.com/ahkimkoo/hermes-taskboard/internal/store"
	"github.com/ahkimkoo/hermes-taskboard/internal/store/fsstore"
	sqlitex "github.com/ahkimkoo/hermes-taskboard/internal/store/sqlite"
	"github.com/ahkimkoo/hermes-taskboard/internal/webfs"
)

// Version is injected at build time via -ldflags "-X main.Version=…".
var Version = "dev"

func main() {
	dataDir := flag.String("data", "data", "path to data directory")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		logger.Error("mkdir data", "err", err)
		os.Exit(1)
	}
	cfgPath := filepath.Join(*dataDir, "config.yaml")
	secretPath := filepath.Join(*dataDir, "db", ".secret")

	cfgStore, err := config.NewStore(cfgPath, secretPath)
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}

	db, err := sqlitex.Open(filepath.Join(*dataDir, "db", "taskboard.db"))
	if err != nil {
		logger.Error("open sqlite", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	st := store.New(db)
	fs := fsstore.New(*dataDir)

	pool := hermes.NewPool()
	reloadPool := func(c *config.Config) {
		entries := make([]hermes.PoolEntry, 0, len(c.HermesServers))
		for _, sv := range c.HermesServers {
			entries = append(entries, hermes.PoolEntry{
				ID: sv.ID, BaseURL: sv.BaseURL,
				APIKey:    sv.DecryptedAPIKey(cfgStore.Secret()),
				IsDefault: sv.IsDefault,
			})
		}
		pool.Reload(entries)
	}
	reloadPool(cfgStore.Snapshot())
	cfgStore.AddHook(func(_, new *config.Config) { reloadPool(new) })

	hub := sse.NewHub()
	boardSvc := board.New(st, hub)
	runner := attempt.New(cfgStore, st, fs, pool, hub, boardSvc)
	authSvc := auth.New(cfgStore)
	sched := scheduler.New(cfgStore, st, runner, logger)

	// Hermes keeps the agent conversation and run alive independently of
	// taskboard. On restart we just lost our SSE subscription, so for each
	// in-flight attempt we reopen /v1/runs/{runID}/events and let the
	// event-handling pipeline carry it the rest of the way. Attempts whose
	// Hermes run is genuinely gone (no run_id recorded, or stream rejects
	// the request) fall back to `failed` with a specific reason.
	// Runs before the scheduler / cron start so nothing new is dispatched
	// while we're still rehydrating.
	if resumed, failed, err := runner.ResumeOrphans(context.Background()); err != nil {
		logger.Warn("resume orphan attempts", "err", err)
	} else if resumed+failed > 0 {
		logger.Info("resumed orphan attempts", "resumed", resumed, "failed", failed)
	}

	srv := server.New(cfgStore, st, fs, pool, hub, boardSvc, runner, authSvc, logger, webfs.FS, *dataDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go sched.Start(ctx)

	cronW := &cronpkg.Worker{
		Store:  st,
		Runner: runner,
		Logger: logger.With("component", "cron"),
	}
	cronW.Start(ctx)

	rpr := &reaper.Reaper{
		DB:         db,
		AttemptDir: filepath.Join(*dataDir, "attempt"),
		Retention:  90 * 24 * time.Hour,
		Logger:     logger.With("component", "reaper"),
	}
	go rpr.Loop(ctx)

	// Auto-resume attempts whose SSE dropped abnormally (network blip,
	// taskboard restart mid-stream, etc.) by sending a synthetic
	// `continue` message. Bounded to 3 retries per attempt with a 60s
	// cooldown so a pathologically wedged Hermes can't loop forever.
	(&attempt.Resumer{Runner: runner, Log: logger.With("component", "resumer")}).Start(ctx)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.Info("shutting down")
		cancel()
	}()

	logger.Info("hermes-taskboard starting",
		"version", Version,
		"listen", cfgStore.Snapshot().Server.Listen,
		"data", *dataDir,
	)
	if err := srv.Run(ctx); err != nil {
		logger.Error("http server", "err", err)
		os.Exit(1)
	}
	sched.Stop()
	logger.Info("bye")
}
