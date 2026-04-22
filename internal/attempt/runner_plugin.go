// Plugin-transport runOnce: drives a turn over the WebSocket plugin
// instead of /v1/responses. Events published to the existing per-attempt
// SSE topic so the frontend pipeline is oblivious to transport.
package attempt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ahkimkoo/hermes-taskboard/internal/hermes"
	"github.com/ahkimkoo/hermes-taskboard/internal/sse"
	"github.com/ahkimkoo/hermes-taskboard/internal/store"
)

// isPluginTransport is the single source of truth for "should this
// attempt's server use the plugin path?". We check the Pool first
// (config-declared plugin transport) and fall back to "there's a live
// plugin announcing this hermes_id" so auto-registered plugins work
// without a config entry.
func (r *Runner) isPluginTransport(serverID string) bool {
	if r.Pool.Transport(serverID) == "plugin" {
		return true
	}
	if r.PluginServer == nil {
		return false
	}
	for _, p := range r.PluginServer.Plugins() {
		if p.HermesID == serverID {
			return true
		}
	}
	return false
}

// runOncePlugin is the plugin-transport equivalent of runOnce. It assumes
// the caller already verified the transport is plugin (via isPluginTransport).
// The attempt's `ServerID` must match a connected plugin's hermes_id; if
// it isn't currently connected we fail the turn cleanly with a system
// event so the UI tells the user why.
func (r *Runner) runOncePlugin(
	ctx context.Context,
	attemptID, input string,
	first bool,
	att *store.Attempt,
) error {
	if r.PluginServer == nil {
		return errors.New("plugin transport requested but no PluginServer wired")
	}

	hermesID := att.ServerID

	r.logSystemEvent(attemptID, "system_prompt_sent", map[string]any{
		"instructions": r.buildSystemPrompt(ctx, att.TaskID),
		"first_turn":   first,
		"transport":    "plugin",
		"hermes_id":    hermesID,
	})

	events, unsub, err := r.PluginServer.SendMessage(ctx, hermesID, attemptID, input)
	if err != nil {
		if errors.Is(err, hermes.ErrPluginNotConnected) {
			r.logSystemEvent(attemptID, "error", map[string]any{
				"msg": fmt.Sprintf(
					"hermes plugin %q is not currently connected; "+
						"make sure hermes-taskboard-bridge is running on that host",
					hermesID,
				),
				"hermes_id": hermesID,
			})
		}
		return err
	}
	defer unsub()

	r.logSystemEvent(attemptID, "run_start", map[string]any{
		"transport": "plugin",
		"hermes_id": hermesID,
	})

	// Drain frames until the turn completes, the stream closes, or the
	// outer ctx is cancelled (user pressed Stop).
	for {
		select {
		case <-ctx.Done():
			// Cancellation: push /stop through the plugin so Hermes's
			// native busy-session interrupt stops the run. Best-effort —
			// even if the cancel frame fails to write (e.g. plugin just
			// dropped), ctx.Err() still returns and the attempt lands in
			// Cancelled state via the loop caller.
			cctx, cancel := context.WithTimeout(
				context.Background(), 2*time.Second,
			)
			_ = r.PluginServer.Cancel(cctx, hermesID, attemptID)
			cancel()
			r.logSystemEvent(attemptID, "run_end", map[string]any{
				"transport": "plugin",
				"hermes_id": hermesID,
				"err":       ctx.Err().Error(),
			})
			return ctx.Err()
		case f, ok := <-events:
			if !ok {
				r.logSystemEvent(attemptID, "run_end", map[string]any{
					"transport": "plugin",
					"hermes_id": hermesID,
					"err":       "plugin channel closed",
				})
				return errors.New("plugin connection closed mid-turn")
			}
			r.recordPluginFrame(attemptID, f)
			switch f.Type {
			case "agent_done":
				r.logSystemEvent(attemptID, "run_end", map[string]any{
					"transport": "plugin",
					"hermes_id": hermesID,
					"summary":   f.Summary,
				})
				if meta, _ := r.FS.LoadAttemptMeta(attemptID); meta != nil {
					meta.Summary = f.Summary
					_ = r.FS.SaveAttemptMeta(meta)
				}
				// ACK back to the plugin so its reconnect-replay buffer
				// can drop anything up to and including this turn.
				_ = r.PluginServer.Ack(ctx, hermesID, attemptID, f.Seq)
				return nil
			case "agent_error":
				r.logSystemEvent(attemptID, "run_end", map[string]any{
					"transport": "plugin",
					"hermes_id": hermesID,
					"err":       "agent_error",
				})
				return fmt.Errorf("plugin agent_error: %s", string(f.Raw))
			}
		}
	}
}

// recordPluginFrame persists a plugin frame onto the attempt event log
// and republishes it on the SSE topic so the existing frontend pipeline
// surfaces it alongside HTTP-path events.
func (r *Runner) recordPluginFrame(attemptID string, f hermes.PluginFrame) {
	// Unmarshal the inner `event` blob so the UI can pattern-match on it
	// the same way it does for hermes SSE events.
	var inner map[string]any
	if len(f.Event) > 0 {
		_ = json.Unmarshal(f.Event, &inner)
	}
	evt := map[string]any{
		"kind":       "plugin",
		"type":       f.Type,
		"attempt_id": f.AttemptID,
		"seq":        f.Seq,
		"event":      inner,
		"summary":    f.Summary,
		"ts":         time.Now().Unix(),
	}
	seq, _ := r.FS.AppendEvent(attemptID, evt)
	r.Hub.Publish(
		"attempt:"+attemptID,
		sse.Event{Seq: seq, Event: "event", Data: evt},
	)
}
