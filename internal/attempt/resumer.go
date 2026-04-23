// Background auto-resumer for abnormal SSE disconnects. Scans every
// user's active attempts every AutoResumeScanInterval and fires a
// synthetic "continue" for ones stuck on an abnormal-disconnect
// (bounded by AutoResumeMaxRetries and AutoResumeCooldown).
package attempt

import (
	"context"
	"log/slog"
	"time"

	"github.com/ahkimkoo/hermes-taskboard/internal/store"
)

type Resumer struct {
	Runner *Runner
	Log    *slog.Logger
}

const (
	AutoResumeMaxRetries   = 3
	AutoResumeCooldown     = 60 * time.Second
	AutoResumeScanInterval = 30 * time.Second
	AutoResumeMessage      = "[auto-resume] taskboard detected the connection dropped before the previous turn finished. Please continue where you left off."
)

func (rz *Resumer) Start(ctx context.Context) {
	go rz.loop(ctx)
}

func (rz *Resumer) loop(ctx context.Context) {
	t := time.NewTicker(AutoResumeScanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rz.tick(ctx)
		}
	}
}

func (rz *Resumer) tick(ctx context.Context) {
	if rz.Runner == nil {
		return
	}
	owned, err := rz.Runner.Stores.ListAllActiveAttempts(ctx, rz.Runner.allUsernames())
	if err != nil {
		rz.log().Warn("resumer list failed", "err", err)
		return
	}
	now := time.Now().Unix()
	for _, oa := range owned {
		a := oa.Attempt
		// Skip attempts with a live runCtx.
		rz.Runner.mu.Lock()
		_, live := rz.Runner.active[a.ID]
		rz.Runner.mu.Unlock()
		if live {
			continue
		}
		fs := rz.Runner.FS.Get(oa.Username)
		meta, err := fs.LoadAttemptMeta(a.ID)
		if err != nil || meta == nil {
			continue
		}
		if meta.Session.LastDisconnectReason != store.DisconnectAbnormal {
			continue
		}
		if meta.Session.ContinueResumeCount >= AutoResumeMaxRetries {
			continue
		}
		if now-meta.Session.LastContinueAt < int64(AutoResumeCooldown.Seconds()) {
			continue
		}

		meta.Session.ContinueResumeCount++
		meta.Session.LastContinueAt = now
		_ = fs.SaveAttemptMeta(meta)

		rz.Runner.logSystemEvent(oa.Username, a.ID, "auto_resume", map[string]any{
			"retry_count": meta.Session.ContinueResumeCount,
			"max_retries": AutoResumeMaxRetries,
			"reason":      "abnormal_disconnect",
		})
		rz.log().Info("auto-resume",
			"user", oa.Username,
			"attempt", a.ID,
			"retry", meta.Session.ContinueResumeCount,
			"max", AutoResumeMaxRetries,
		)
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := rz.autoSend(sctx, oa.Username, a.ID, AutoResumeMessage); err != nil {
			rz.log().Warn("auto-resume send failed", "attempt", a.ID, "err", err)
		}
		cancel()
	}
}

func (rz *Resumer) autoSend(ctx context.Context, username, attemptID, text string) error {
	rz.Runner.mu.Lock()
	rc, live := rz.Runner.active[attemptID]
	rz.Runner.mu.Unlock()
	if live {
		select {
		case rc.queue <- text:
		default:
			return errContinueQueueFull
		}
		return nil
	}
	rz.Runner.startLoop(username, attemptID, text)
	return nil
}

func (rz *Resumer) log() *slog.Logger {
	if rz.Log != nil {
		return rz.Log
	}
	return slog.Default()
}

type sentinelErr string

func (e sentinelErr) Error() string { return string(e) }

const errContinueQueueFull sentinelErr = "continue queue full"
