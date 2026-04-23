// Background auto-resumer for abnormal SSE disconnects.
//
// When Hermes's stream closes for any reason other than (a) response.completed
// or (b) user cancel, the turn is marked "abnormal" and the Hermes agent may
// still be waiting for a prompt to continue. Rather than requiring the user
// to manually type something, this watcher periodically scans for attempts
// that are *stuck* in this state and sends a synthetic "continue" message.
//
// Guardrails to prevent runaway loops (user's explicit concern):
//   - max retries per attempt: AutoResumeMaxRetries (3)
//   - cooldown between retries: AutoResumeCooldown (60s)
//   - reset count to 0 on any clean response.completed (runner.runOnce)
//   - reset count to 0 on any real user message (runner.SendMessage)
//   - skip attempts already in a terminal state
//   - skip attempts with an active runCtx (something is streaming right now)

package attempt

import (
	"context"
	"log/slog"
	"time"

	"github.com/ahkimkoo/hermes-taskboard/internal/store"
)

// Resumer runs a loop that auto-recovers attempts wedged on abnormal
// SSE disconnect. One per Runner.
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

// Start launches the scan loop; returns immediately. Stops when ctx
// cancels (taskboard shutdown).
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
	// Enumerate active attempts (queued/running/needs_input). Terminal
	// ones (completed/failed/cancelled) never need auto-resume.
	active, err := rz.Runner.Store.ListActiveAttempts(ctx)
	if err != nil {
		rz.log().Warn("resumer list failed", "err", err)
		return
	}
	now := time.Now().Unix()
	for _, a := range active {
		// Skip attempts with a live runCtx — their loop goroutine is
		// already owning them; a fresh "continue" would collide with
		// the in-flight stream.
		rz.Runner.mu.Lock()
		_, live := rz.Runner.active[a.ID]
		rz.Runner.mu.Unlock()
		if live {
			continue
		}
		meta, err := rz.Runner.FS.LoadAttemptMeta(a.ID)
		if err != nil || meta == nil {
			continue
		}
		if meta.Session.LastDisconnectReason != store.DisconnectAbnormal {
			continue
		}
		// Idempotency: enforce cooldown + max-retries so a wedged
		// Hermes that keeps dropping after every continue can't get us
		// into a send-loop.
		if meta.Session.ContinueResumeCount >= AutoResumeMaxRetries {
			continue
		}
		if now-meta.Session.LastContinueAt < int64(AutoResumeCooldown.Seconds()) {
			continue
		}

		// Record the attempt + fire.
		meta.Session.ContinueResumeCount++
		meta.Session.LastContinueAt = now
		_ = rz.Runner.FS.SaveAttemptMeta(meta)

		rz.Runner.logSystemEvent(a.ID, "auto_resume", map[string]any{
			"retry_count": meta.Session.ContinueResumeCount,
			"max_retries": AutoResumeMaxRetries,
			"reason":      "abnormal_disconnect",
		})
		rz.log().Info("auto-resume",
			"attempt", a.ID,
			"retry", meta.Session.ContinueResumeCount,
			"max", AutoResumeMaxRetries,
		)
		// Use a short-lived ctx derived from the scan ctx so shutdown
		// doesn't leave a send in flight. We deliberately do NOT log a
		// user_message for the synthetic prompt — we don't want it to
		// pollute the user-visible chat bubbles, and we don't want it
		// to reset ContinueResumeCount (which SendMessage does for
		// real user messages).
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := rz.autoSend(sctx, a.ID, AutoResumeMessage); err != nil {
			rz.log().Warn("auto-resume send failed", "attempt", a.ID, "err", err)
		}
		cancel()
	}
}

// autoSend mirrors Runner.SendMessage's spawn path but without the
// ContinueResumeCount reset and without logging user_message — this is
// taskboard talking to itself, not a user action.
func (rz *Resumer) autoSend(ctx context.Context, attemptID, text string) error {
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
	rz.Runner.startLoop(attemptID, text)
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
