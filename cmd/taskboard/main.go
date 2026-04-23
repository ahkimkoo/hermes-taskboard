// Hermes Task Board — single-binary Go backend serving a Vue 3 kanban UI,
// integrating with Hermes API server (gateway) for task execution.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/ahkimkoo/hermes-taskboard/internal/attempt"
	"github.com/ahkimkoo/hermes-taskboard/internal/auth"
	"github.com/ahkimkoo/hermes-taskboard/internal/board"
	"github.com/ahkimkoo/hermes-taskboard/internal/config"
	cronpkg "github.com/ahkimkoo/hermes-taskboard/internal/cron"
	"github.com/ahkimkoo/hermes-taskboard/internal/hermes"
	"github.com/ahkimkoo/hermes-taskboard/internal/migrate"
	"github.com/ahkimkoo/hermes-taskboard/internal/reaper"
	"github.com/ahkimkoo/hermes-taskboard/internal/scheduler"
	"github.com/ahkimkoo/hermes-taskboard/internal/server"
	"github.com/ahkimkoo/hermes-taskboard/internal/sse"
	"github.com/ahkimkoo/hermes-taskboard/internal/store"
	"github.com/ahkimkoo/hermes-taskboard/internal/store/fsstore"
	"github.com/ahkimkoo/hermes-taskboard/internal/userdir"
	"github.com/ahkimkoo/hermes-taskboard/internal/webfs"
)

// Version is injected at build time via -ldflags "-X main.Version=…".
var Version = "dev"

const (
	defaultAdminUsername = "admin"
	defaultAdminPassword = "admin123"
)

func main() {
	dataDir := flag.String("data", "data", "path to data directory")
	resetAdmin := flag.Bool("reset-admin", false, "reset the 'admin' account password to admin123 and exit")
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

	// Migrate the old single-DB / hermes_servers-in-config layout into
	// per-user directories owned by admin. Must run BEFORE userdir.LoadAll
	// so the scan picks up the freshly-created admin/config.yaml.
	if err := migrate.MigrateLegacy(*dataDir, cfgStore, cfgStore.Secret(), logger); err != nil {
		logger.Error("migrate legacy layout", "err", err)
		os.Exit(1)
	}

	users := userdir.New(*dataDir, cfgStore.Secret())
	if err := users.LoadAll(); err != nil {
		logger.Error("load userdir", "err", err)
		os.Exit(1)
	}

	if *resetAdmin {
		if err := resetAdminPassword(users, logger); err != nil {
			logger.Error("reset-admin", "err", err)
			os.Exit(1)
		}
		logger.Info("admin password reset to default (change it at next login)")
		return
	}

	// Ensure at least one admin exists. The default admin user is also
	// what the one-shot migration writes to.
	if err := ensureDefaultAdmin(users, logger); err != nil {
		logger.Error("bootstrap admin", "err", err)
		os.Exit(1)
	}

	stores := store.NewManager(*dataDir)
	fsMgr := fsstore.NewManager(*dataDir)
	defer stores.Close()

	pool := hermes.NewPool()
	reloadPool := func() {
		entries := poolEntries(users)
		pool.Reload(entries)
	}
	reloadPool()

	hub := sse.NewHub()
	boardSvc := board.New(hub)
	runner := attempt.New(cfgStore, stores, fsMgr, users, pool, hub, boardSvc)
	authSvc := auth.New(cfgStore, users)
	sched := scheduler.New(cfgStore, stores, users, runner, logger)

	// Hermes keeps agent conversation + run alive across our restart.
	// ResumeOrphans reopens /v1/runs/{runID}/events for every user's
	// in-flight attempts so the event pipeline carries them to completion.
	if resumed, failed, err := runner.ResumeOrphans(context.Background(), usernamesFrom(users)); err != nil {
		logger.Warn("resume orphan attempts", "err", err)
	} else if resumed+failed > 0 {
		logger.Info("resumed orphan attempts", "resumed", resumed, "failed", failed)
	}

	srv := server.New(cfgStore, stores, fsMgr, users, pool, reloadPool, hub, boardSvc, runner, authSvc, logger, webfs.FS, *dataDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go sched.Start(ctx)

	cronW := &cronpkg.Worker{
		Stores: stores,
		Users:  users,
		Runner: runner,
		Logger: logger.With("component", "cron"),
	}
	cronW.Start(ctx)

	rpr := &reaper.Reaper{
		Stores:    stores,
		Users:     users,
		DataDir:   *dataDir,
		Retention: 90 * 24 * time.Hour,
		Logger:    logger.With("component", "reaper"),
	}
	go rpr.Loop(ctx)

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
		"users", users.Count(),
	)
	if err := srv.Run(ctx); err != nil {
		logger.Error("http server", "err", err)
		os.Exit(1)
	}
	sched.Stop()
	logger.Info("bye")
}

// ensureDefaultAdmin creates data/admin/ with admin/admin123 when the
// registry is empty or no admin user exists.
func ensureDefaultAdmin(users *userdir.Manager, logger *slog.Logger) error {
	for _, u := range users.List() {
		if u.IsAdmin {
			return nil
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(defaultAdminPassword), 12)
	if err != nil {
		return err
	}
	u := &userdir.UserConfig{
		Username:     defaultAdminUsername,
		PasswordHash: string(hash),
		IsAdmin:      true,
	}
	if err := users.Create(u); err != nil {
		return err
	}
	logger.Warn("created default admin user; change the password immediately",
		"username", defaultAdminUsername, "password", defaultAdminPassword)
	return nil
}

// resetAdminPassword is the --reset-admin flow: ensure data/admin/
// exists with admin privileges, reset its password to admin123, and
// clear the disabled sentinel.
func resetAdminPassword(users *userdir.Manager, logger *slog.Logger) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(defaultAdminPassword), 12)
	if err != nil {
		return err
	}
	if _, ok := users.Get(defaultAdminUsername); !ok {
		u := &userdir.UserConfig{
			Username:     defaultAdminUsername,
			PasswordHash: string(hash),
			IsAdmin:      true,
		}
		return users.Create(u)
	}
	_ = users.SetDisabled(defaultAdminUsername, false)
	return users.Mutate(defaultAdminUsername, func(uc *userdir.UserConfig) error {
		uc.PasswordHash = string(hash)
		if !uc.IsAdmin {
			// refuse to silently flip non-admin → admin; caller should
			// fix the config.yaml or delete + recreate instead.
			return fmt.Errorf("user %q exists but is_admin=false — refusing to reset", defaultAdminUsername)
		}
		return nil
	})
}

// poolEntries flattens every user's hermes_servers into pool entries.
// Server IDs must be globally unique — userdir.Manager enforces this
// at Create/Mutate time; last-writer-wins on any stale collision.
func poolEntries(users *userdir.Manager) []hermes.PoolEntry {
	out := []hermes.PoolEntry{}
	for _, u := range users.List() {
		for _, sv := range u.HermesServers {
			out = append(out, hermes.PoolEntry{
				ID:        sv.ID,
				BaseURL:   sv.BaseURL,
				APIKey:    users.DecryptedAPIKey(&sv),
				IsDefault: sv.IsDefault,
			})
		}
	}
	return out
}

func usernamesFrom(users *userdir.Manager) []string {
	list := users.List()
	out := make([]string, 0, len(list))
	for _, u := range list {
		out = append(out, u.Username)
	}
	return out
}
