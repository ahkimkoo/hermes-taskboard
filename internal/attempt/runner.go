// Package attempt owns the lifecycle of a single Attempt: drives a Hermes
// conversation, streams events to disk + SSE, and coordinates state transitions.
package attempt

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ahkimkoo/hermes-taskboard/internal/board"
	"github.com/ahkimkoo/hermes-taskboard/internal/config"
	"github.com/ahkimkoo/hermes-taskboard/internal/hermes"
	"github.com/ahkimkoo/hermes-taskboard/internal/sse"
	"github.com/ahkimkoo/hermes-taskboard/internal/store"
	"github.com/ahkimkoo/hermes-taskboard/internal/store/fsstore"
)

type Runner struct {
	Cfg   *config.Store
	Store *store.Store
	FS    *fsstore.FS
	Pool  *hermes.Pool
	Hub   *sse.Hub
	Board *board.Service

	mu       sync.Mutex
	active   map[string]*runCtx // attempt_id → live context
}

type runCtx struct {
	cancel    context.CancelFunc
	queue     chan string // pending user inputs
	runningMu sync.Mutex
	running   bool
}

func New(cfg *config.Store, s *store.Store, fs *fsstore.FS, pool *hermes.Pool, hub *sse.Hub, b *board.Service) *Runner {
	return &Runner{Cfg: cfg, Store: s, FS: fs, Pool: pool, Hub: hub, Board: b, active: map[string]*runCtx{}}
}

// ResumeOrphans reattaches to Hermes runs that were in-flight when the
// previous process died. Hermes keeps the conversation and run alive
// independently of taskboard — we just lost the SSE subscription. For each
// active attempt we reopen `/v1/runs/{runID}/events` and let the existing
// handleEvent pipeline carry it to completion as if nothing happened.
//
// Flow per orphan:
//
//   meta.session.current_run_id present → try to reconnect.
//   - stream succeeds and delivers response.completed → attempt goes
//     Completed via the same idle-settle path as a normal run.
//   - stream errors immediately (run expired / unknown) → mark Failed with a
//     clear reason.
//   - no run_id recorded at all (rare: crash between meta write and Hermes
//     call) → mark Failed with "no run to resume".
//
// Call once at startup, before the scheduler / cron worker fire.
func (r *Runner) ResumeOrphans(ctx context.Context) (resumed, failed int, err error) {
	active, err := r.Store.ListActiveAttempts(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, a := range active {
		if a.State != store.AttemptRunning && a.State != store.AttemptQueued {
			continue
		}
		meta, _ := r.FS.LoadAttemptMeta(a.ID)
		var runID string
		if meta != nil {
			runID = meta.Session.CurrentRunID
		}
		if runID == "" {
			r.logSystemEvent(a.ID, "error", map[string]any{
				"msg":         "process restart — no run_id recorded, cannot resume",
				"prior_state": string(a.State),
			})
			_ = r.Store.UpdateAttemptState(ctx, a.ID, store.AttemptFailed)
			r.broadcastStateChange(a.ID, store.AttemptFailed)
			_ = r.Board.MaybeAdvanceAfterAttempt(ctx, a.TaskID)
			failed++
			continue
		}
		if err := r.resumeAttempt(a.ID, a.ServerID, runID); err != nil {
			r.logSystemEvent(a.ID, "error", map[string]any{
				"msg":         "resume failed: " + err.Error(),
				"run_id":      runID,
				"prior_state": string(a.State),
			})
			_ = r.Store.UpdateAttemptState(ctx, a.ID, store.AttemptFailed)
			r.broadcastStateChange(a.ID, store.AttemptFailed)
			_ = r.Board.MaybeAdvanceAfterAttempt(ctx, a.TaskID)
			failed++
			continue
		}
		r.logSystemEvent(a.ID, "resumed", map[string]any{"run_id": runID})
		resumed++
	}
	return resumed, failed, nil
}

// TryReconnect opens a fresh SSE subscription to the Hermes run recorded
// in this attempt's meta, unless a live runCtx already owns it. It's the
// backend for the "catch me up" flow the UI invokes when the user scrolls
// to the input area on a card: we want fresh events for stale / terminal
// attempts but must not duplicate an active stream.
//
// Returned statuses:
//   - "already_live" — r.active[attemptID] exists; nothing to do.
//   - "no_run_id"    — meta has no CurrentRunID, nothing to reconnect to.
//   - "reconnected"  — a fresh stream was opened; events will flow via SSE.
func (r *Runner) TryReconnect(ctx context.Context, attemptID string) (string, error) {
	r.mu.Lock()
	_, live := r.active[attemptID]
	r.mu.Unlock()
	if live {
		return "already_live", nil
	}
	att, err := r.Store.GetAttempt(ctx, attemptID)
	if err != nil {
		return "", err
	}
	meta, _ := r.FS.LoadAttemptMeta(attemptID)
	var runID string
	if meta != nil {
		runID = meta.Session.CurrentRunID
	}
	if runID == "" {
		return "no_run_id", nil
	}
	if err := r.resumeAttempt(attemptID, att.ServerID, runID); err != nil {
		return "", err
	}
	r.logSystemEvent(attemptID, "resumed", map[string]any{"run_id": runID, "trigger": "user_scroll"})
	return "reconnected", nil
}

// resumeAttempt spawns a goroutine that tails `/v1/runs/{runID}/events`
// from Hermes and funnels events through the same handleEvent pipeline the
// live flow uses. A fresh runCtx is registered in r.active so SendMessage /
// Cancel can interact with the attempt exactly like a fresh one.
func (r *Runner) resumeAttempt(attemptID, serverID, runID string) error {
	client, err := r.Pool.Get(serverID)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if _, exists := r.active[attemptID]; exists {
		r.mu.Unlock()
		return errors.New("already active")
	}
	ctx, cancel := context.WithCancel(context.Background())
	rc := &runCtx{cancel: cancel, queue: make(chan string, 32)}
	r.active[attemptID] = rc
	r.mu.Unlock()
	go r.resumeLoop(ctx, attemptID, runID, client, rc)
	return nil
}

func (r *Runner) resumeLoop(ctx context.Context, attemptID, runID string, client *hermes.Client, rc *runCtx) {
	defer func() {
		r.mu.Lock()
		delete(r.active, attemptID)
		r.mu.Unlock()
	}()
	events := make(chan hermes.Event, 64)
	streamErr := make(chan error, 1)
	go func() {
		streamErr <- client.StreamRunEvents(ctx, runID, events)
		close(events)
	}()
	for e := range events {
		r.handleEvent(attemptID, e)
	}
	err := <-streamErr
	r.logSystemEvent(attemptID, "run_end", map[string]any{"run_id": runID, "err": errStr(err), "resumed": true})
	// Settle the attempt state the same way loop() does: if nothing queued
	// and state is still running/queued, treat as completed.
	cctx := context.Background()
	att, gerr := r.Store.GetAttempt(cctx, attemptID)
	if gerr != nil || att == nil {
		return
	}
	if att.State == store.AttemptRunning || att.State == store.AttemptQueued {
		final := store.AttemptCompleted
		if err != nil && !errors.Is(err, context.Canceled) {
			final = store.AttemptFailed
		}
		_ = r.Store.UpdateAttemptState(cctx, attemptID, final)
		r.broadcastStateChange(attemptID, final)
		_ = r.Board.MaybeAdvanceAfterAttempt(cctx, att.TaskID)
	}
}

// Start creates a new Attempt with initial system+user prompt (task title+description) and
// drives it. Returns the attempt id.
func (r *Runner) Start(ctx context.Context, taskID, serverID, model string) (*store.Attempt, error) {
	task, err := r.Store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	cfg := r.Cfg.Snapshot()
	sv, effModel := cfg.ResolveServerModel(
		firstNonEmpty(serverID, task.PreferredServer),
		firstNonEmpty(model, task.PreferredModel),
	)
	if sv == nil {
		return nil, errors.New("no hermes server configured")
	}
	if effModel == "" {
		return nil, errors.New("no model resolvable")
	}

	// Check gating.
	global, byServer, byPair, err := r.Store.CountActive(ctx, sv.ID, effModel)
	if err != nil {
		return nil, err
	}
	if global >= cfg.Scheduler.GlobalMaxConcurrent {
		return nil, Concurrency("global")
	}
	if byServer >= sv.MaxConcurrent {
		return nil, Concurrency("server")
	}
	// find profile max
	var profMax = 5
	for _, m := range sv.Models {
		if strings.EqualFold(m.Name, effModel) {
			profMax = m.MaxConcurrent
		}
	}
	if byPair >= profMax {
		return nil, Concurrency("profile")
	}

	attemptID := uuid.NewString()
	att := &store.Attempt{
		ID:       attemptID,
		TaskID:   taskID,
		ServerID: sv.ID,
		Model:    effModel,
		State:    store.AttemptQueued,
	}
	if err := r.Store.CreateAttempt(ctx, att); err != nil {
		return nil, err
	}
	meta := &store.AttemptMeta{
		ID:       attemptID,
		TaskID:   taskID,
		ServerID: sv.ID,
		Model:    effModel,
		Session: store.AttemptSession{
			ConversationID: attemptID,
			Runs:           []string{},
		},
	}
	if err := r.FS.SaveAttemptMeta(meta); err != nil {
		return nil, err
	}

	// Compose initial prompt. Prefix each taskboard-originated turn with
	// a `tb-<8-hex>` tag so `hermes sessions list` on the Hermes host
	// shows the taskboard origin in its Preview column — easier than
	// trying to wire api_server sessions to the CLI session store via
	// /title (api_server responses land in Hermes's response DB, not
	// the CLI's session DB, so /title creates an orphan session not
	// tied to ours). The prefix is short + stable + human-readable.
	doc, _ := r.FS.LoadTaskDoc(taskID)
	var desc string
	if doc != nil {
		desc = doc.Description
	}
	idPrefix := attemptID
	if len(idPrefix) > 8 {
		idPrefix = idPrefix[:8]
	}
	initialInput := fmt.Sprintf("[tb-%s] # Task\n%s\n\n%s", idPrefix, task.Title, desc)

	// Move card into Execute (auto) if it isn't already. Verify is included
	// so that "Run again" on a reviewed task pulls the card back out of the
	// Verify column — any task with a live Attempt belongs under Execute.
	if task.Status == store.StatusPlan || task.Status == store.StatusDraft || task.Status == store.StatusVerify {
		if err := r.Board.Transition(ctx, taskID, store.StatusExecute, board.KindAuto, "attempt_started"); err != nil {
			// best-effort
		}
	}
	r.Hub.Publish("board", sse.Event{Event: "attempt.created", Data: map[string]any{
		"task_id":    taskID,
		"attempt_id": attemptID,
		"server_id":  sv.ID,
		"model":      effModel,
	}})
	// Record the initial task prompt as a user_message right away so the
	// event stream always shows what was sent first (rather than waiting
	// for runOnce to log it after dispatch).
	r.logSystemEvent(attemptID, "user_message", map[string]any{"input": initialInput})
	r.startLoop(attemptID, initialInput)
	return att, nil
}

// SendMessage enqueues a user message on an existing attempt; starts a new run when idle.
func (r *Runner) SendMessage(ctx context.Context, attemptID, text string) error {
	att, err := r.Store.GetAttempt(ctx, attemptID)
	if err != nil {
		return err
	}
	// A real user message (as opposed to the auto-resumer's synthetic
	// "continue") means the user took over, so reset the auto-continue
	// counter — otherwise we'd hold stale abnormal-disconnect retry
	// state and refuse to auto-recover on a future network blip.
	if meta, _ := r.FS.LoadAttemptMeta(attemptID); meta != nil && meta.Session.ContinueResumeCount > 0 {
		meta.Session.ContinueResumeCount = 0
		_ = r.FS.SaveAttemptMeta(meta)
	}
	// Record the user's bubble immediately — regardless of whether we start a
	// new run, append to an active queue, or reopen from a terminal state.
	// If we defer the log to dispatch time (runOnce), a user typing while an
	// earlier run is still streaming would see their message vanish for
	// seconds-to-minutes until the current turn finishes.
	r.logSystemEvent(attemptID, "user_message", map[string]any{"input": text})

	if att.State == store.AttemptCompleted || att.State == store.AttemptFailed || att.State == store.AttemptCancelled {
		// Sending to a terminal attempt is Verify → Execute auto-transition.
		if err := r.Store.UpdateAttemptState(ctx, attemptID, store.AttemptQueued); err != nil {
			return err
		}
		// transition task back to execute
		_ = r.Board.Transition(ctx, att.TaskID, store.StatusExecute, board.KindAuto, "verify_followup")
		r.startLoop(attemptID, text)
		return nil
	}
	r.mu.Lock()
	rc, ok := r.active[attemptID]
	r.mu.Unlock()
	if ok {
		select {
		case rc.queue <- text:
		default:
			return errors.New("message queue full")
		}
		return nil
	}
	// No active ctx but attempt is non-terminal → kick a new loop.
	r.startLoop(attemptID, text)
	return nil
}

// Cancel stops the active run and marks the attempt cancelled (if running).
func (r *Runner) Cancel(ctx context.Context, attemptID string) error {
	r.mu.Lock()
	rc, ok := r.active[attemptID]
	r.mu.Unlock()
	if ok && rc.cancel != nil {
		rc.cancel()
	}
	_ = r.Store.UpdateAttemptState(ctx, attemptID, store.AttemptCancelled)
	r.broadcastStateChange(attemptID, store.AttemptCancelled)
	att, err := r.Store.GetAttempt(ctx, attemptID)
	if err == nil {
		_ = r.Board.MaybeAdvanceAfterAttempt(ctx, att.TaskID)
	}
	return nil
}

// startLoop kicks off a background goroutine that processes initial + queued inputs.
func (r *Runner) startLoop(attemptID, firstInput string) {
	r.mu.Lock()
	if _, ok := r.active[attemptID]; ok {
		// already running; push to its queue
		rc := r.active[attemptID]
		r.mu.Unlock()
		select {
		case rc.queue <- firstInput:
		default:
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	rc := &runCtx{cancel: cancel, queue: make(chan string, 32)}
	r.active[attemptID] = rc
	r.mu.Unlock()

	go r.loop(ctx, attemptID, firstInput, rc)
}

func (r *Runner) loop(ctx context.Context, attemptID, firstInput string, rc *runCtx) {
	defer func() {
		r.mu.Lock()
		delete(r.active, attemptID)
		r.mu.Unlock()
	}()
	pending := firstInput
	isFirst := true
	for {
		if pending != "" {
			if err := r.runOnce(ctx, attemptID, pending, isFirst); err != nil {
				// Abnormal disconnect is not a permanent failure — the
				// attempt stays in its current running state so the
				// Resumer can pick it up and send a synthetic continue.
				// runOnce already logged `abnormal_disconnect` for the UI.
				if errors.Is(err, errAbnormalDisconnect) {
					return
				}
				r.logSystemEvent(attemptID, "error", map[string]any{"msg": err.Error()})
				_ = r.Store.UpdateAttemptState(ctx, attemptID, store.AttemptFailed)
				r.broadcastStateChange(attemptID, store.AttemptFailed)
				r.maybeAdvance(ctx, attemptID)
				return
			}
			pending = ""
			isFirst = false
		}
		// wait for next user input or terminate
		select {
		case <-ctx.Done():
			return
		case nxt, ok := <-rc.queue:
			if !ok {
				return
			}
			pending = nxt
		case <-time.After(50 * time.Millisecond):
			// short idle — promote attempt to completed if nothing else to do
			att, err := r.Store.GetAttempt(ctx, attemptID)
			if err == nil && att.State == store.AttemptRunning {
				// nothing queued → mark completed
				_ = r.Store.UpdateAttemptState(ctx, attemptID, store.AttemptCompleted)
				r.broadcastStateChange(attemptID, store.AttemptCompleted)
				r.maybeAdvance(ctx, attemptID)
				return
			}
			if err == nil && att.State == store.AttemptQueued {
				_ = r.Store.UpdateAttemptState(ctx, attemptID, store.AttemptCompleted)
				r.broadcastStateChange(attemptID, store.AttemptCompleted)
				r.maybeAdvance(ctx, attemptID)
				return
			}
			if err == nil && (att.State == store.AttemptCompleted || att.State == store.AttemptFailed || att.State == store.AttemptCancelled) {
				return
			}
		}
	}
}

// runOnce performs one Hermes round and streams events to disk + SSE.
func (r *Runner) runOnce(ctx context.Context, attemptID, input string, first bool) error {
	_ = r.Store.UpdateAttemptState(ctx, attemptID, store.AttemptRunning)
	r.broadcastStateChange(attemptID, store.AttemptRunning)

	att, err := r.Store.GetAttempt(ctx, attemptID)
	if err != nil {
		return err
	}
	client, err := r.Pool.Get(att.ServerID)
	if err != nil {
		return err
	}
	req := hermes.ResponseRequest{
		Conversation: att.ID,
		Model:        att.Model,
		Input:        input,
		Stream:       true,
	}
	// Chain off the last response this attempt got back so Hermes treats
	// this call as a continuation of the same conversation rather than a
	// cold start. Without this, a user who types "continue" into an
	// existing Attempt just opens a brand-new session with no memory of
	// the earlier tool calls. The latest id is captured in handleEvent
	// on `response.completed` — not `response.created` — because Hermes
	// discards responses that never complete (e.g. user cancellation),
	// and chaining from a discarded id returns 404 on the next turn.
	if meta, _ := r.FS.LoadAttemptMeta(attemptID); meta != nil && meta.Session.LatestResponseID != "" {
		req.PreviousResponseID = meta.Session.LatestResponseID
	}
	// Always re-send the combined system prompt on every turn — the docs say
	// `instructions` on /v1/responses is the equivalent of a role=system
	// message in /v1/chat/completions, and chat-completions re-delivers the
	// system message on every request because the whole messages[] array is
	// resent. Mirror that here so follow-up user messages can't drop the
	// task's tag prompts (e.g. the "notify on finish" instruction).
	req.SystemPrompt = r.buildSystemPrompt(ctx, att.TaskID)
	if req.SystemPrompt != "" {
		// Audit the exact `instructions` payload we're about to send so
		// operators can confirm post-hoc that tag System Prompts were
		// delivered — previously invisible without sniffing outgoing HTTP.
		r.logSystemEvent(attemptID, "system_prompt_sent", map[string]any{
			"instructions": req.SystemPrompt,
			"length":       len(req.SystemPrompt),
			"first_turn":   first,
		})
	}
	// Note: user_message events are emitted at acceptance time in Start() /
	// SendMessage() so the user's bubble is visible the moment they click
	// send. Logging again here would duplicate.
	res, err := client.CreateResponse(ctx, req)
	if err != nil {
		// If Hermes rejects a stale previous_response_id (e.g. a previous
		// turn was cancelled before `response.completed`, or the Hermes
		// server dropped the response), clear the id and retry as a fresh
		// turn. We still keep `conversation` continuity via the fallback
		// in client.CreateResponse.
		if req.PreviousResponseID != "" && isPreviousResponseNotFound(err) {
			r.logSystemEvent(attemptID, "previous_response_id_stale", map[string]any{
				"previous_response_id": req.PreviousResponseID,
				"err":                  err.Error(),
			})
			if meta, _ := r.FS.LoadAttemptMeta(attemptID); meta != nil {
				meta.Session.LatestResponseID = ""
				_ = r.FS.SaveAttemptMeta(meta)
			}
			req.PreviousResponseID = ""
			res, err = client.CreateResponse(ctx, req)
		}
		if err != nil {
			return err
		}
	}
	runID := res.RunID
	// Update meta with run_id. We only persist LatestResponseID here when
	// CreateResponse returned a concrete id (non-streaming path where the
	// response has already completed by the time the HTTP call returns).
	// In the streaming path res.ResponseID is empty and the real id
	// arrives later via the response.completed SSE event — writing the
	// empty string here would wipe the prior turn's chain anchor.
	if meta, _ := r.FS.LoadAttemptMeta(attemptID); meta != nil {
		dirty := false
		if runID != "" {
			meta.Session.CurrentRunID = runID
			meta.Session.Runs = append(meta.Session.Runs, runID)
			dirty = true
		}
		if res.ResponseID != "" {
			meta.Session.LatestResponseID = res.ResponseID
			dirty = true
		}
		if dirty {
			_ = r.FS.SaveAttemptMeta(meta)
		}
	}
	r.logSystemEvent(attemptID, "run_start", map[string]any{
		"run_id":               runID,
		"response_id":          res.ResponseID,
		"previous_response_id": req.PreviousResponseID,
	})

	events := make(chan hermes.Event, 64)
	streamErr := make(chan error, 1)
	go func() {
		if res.RawStream != nil {
			streamErr <- hermes.StreamResponseBody(ctx, res.RawStream, events)
		} else if runID != "" {
			streamErr <- client.StreamRunEvents(ctx, runID, events)
		} else {
			streamErr <- errors.New("no stream available")
		}
		close(events)
	}()

	// consume until stream closes — flag whether the stream reached
	// `response.completed` before closing. That, plus ctx.Err(), lets us
	// classify the turn's exit as completed / cancelled / abnormal.
	var sawCompleted bool
	for e := range events {
		r.handleEvent(attemptID, e)
		if typ, _ := e.Data["type"].(string); typ == "response.completed" {
			sawCompleted = true
		}
	}
	err = <-streamErr
	r.logSystemEvent(attemptID, "run_end", map[string]any{"run_id": runID, "err": errStr(err)})

	// Classify the disconnect + record it on meta for the auto-resumer.
	// `ctx` cancellation maps to user-cancel (Runner.Cancel calls rc.cancel).
	// Stream ended cleanly after response.completed → completed.
	// Anything else (net error, premature close, 5xx, timeout) → abnormal.
	reason := store.DisconnectAbnormal
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		reason = store.DisconnectCancelled
	} else if err == nil && sawCompleted {
		reason = store.DisconnectCompleted
	}
	if meta, _ := r.FS.LoadAttemptMeta(attemptID); meta != nil {
		meta.Session.CurrentRunID = ""
		meta.Session.LastDisconnectReason = reason
		meta.Session.LastDisconnectAt = time.Now().Unix()
		// A clean completion resets the auto-resume counter — if the
		// agent just delivered a normal turn, any prior abnormal-streak
		// is clearly over.
		if reason == store.DisconnectCompleted {
			meta.Session.ContinueResumeCount = 0
		}
		_ = r.FS.SaveAttemptMeta(meta)
	}
	// Mark abnormal for the UI so the user sees a clear badge.
	if reason == store.DisconnectAbnormal {
		r.logSystemEvent(attemptID, "abnormal_disconnect", map[string]any{
			"run_id":   runID,
			"err":      errStr(err),
			"ctx_err":  errStr(ctx.Err()),
		})
		// Signal to the loop() caller that this is an abnormal disconnect,
		// NOT a permanent failure — the attempt should stay in running
		// state so the Resumer can retry it. Caller must treat this
		// sentinel specially (don't flip to failed, don't auto-complete).
		return errAbnormalDisconnect
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// errAbnormalDisconnect is the signal runOnce returns when the SSE stream
// dropped unexpectedly (network, Hermes crash, taskboard killed mid-stream).
// The loop() goroutine treats this differently from a hard error: the
// attempt stays in its current state so the background Resumer can later
// send a synthetic `continue` to pick up where the agent left off.
var errAbnormalDisconnect = errors.New("abnormal SSE disconnect; awaiting auto-resume")

// handleEvent is the single routing point for a Hermes SSE event.
func (r *Runner) handleEvent(attemptID string, e hermes.Event) {
	// Persist raw event (with seq) and publish to SSE topic.
	evt := map[string]any{"kind": "hermes", "data": e.Data, "ts": time.Now().Unix()}
	seq, _ := r.FS.AppendEvent(attemptID, evt)
	r.Hub.Publish("attempt:"+attemptID, sse.Event{Seq: seq, Event: "event", Data: evt})

	typ, _ := e.Data["type"].(string)

	// Capture the run / response id from the first SSE event of a stream.
	// When we POST /v1/responses with stream=true the HTTP body IS the SSE
	// stream, so client.CreateResponse returns an empty RunID/ResponseID
	// — the only place the id appears is in the response.created event
	// that arrives first on the wire. Record it as CurrentRunID so
	// ResumeOrphans has something to reconnect with after a taskboard
	// restart. LatestResponseID is *not* written here: we only want to
	// chain off responses Hermes will actually retain, which is decided
	// at completion time (cancelled / aborted responses get discarded and
	// would produce 404 on the next turn's previous_response_id).
	if typ == "response.created" {
		if resp, ok := e.Data["response"].(map[string]any); ok {
			if rid, ok := resp["id"].(string); ok && rid != "" {
				if meta, _ := r.FS.LoadAttemptMeta(attemptID); meta != nil {
					meta.Session.CurrentRunID = rid
					// Dedupe so repeat receipts don't balloon the Runs slice.
					if len(meta.Session.Runs) == 0 || meta.Session.Runs[len(meta.Session.Runs)-1] != rid {
						meta.Session.Runs = append(meta.Session.Runs, rid)
					}
					_ = r.FS.SaveAttemptMeta(meta)
				}
			}
		}
	}

	// Detect needs_input via conservative heuristic: tool_call with approval_required.
	if typ == "input_required" || typ == "approval_required" {
		_ = r.Store.UpdateAttemptState(context.Background(), attemptID, store.AttemptNeedsInput)
		r.broadcastStateChange(attemptID, store.AttemptNeedsInput)
	}
	// Capture final assistant message summary + pin LatestResponseID to
	// the id of the *completed* response. Only completed responses can be
	// used as `previous_response_id` on the next turn — Hermes drops the
	// record for any response that was cancelled or errored mid-stream.
	if typ == "response.completed" {
		if meta, _ := r.FS.LoadAttemptMeta(attemptID); meta != nil {
			changed := false
			if out, ok := e.Data["output_text"].(string); ok {
				meta.Summary = out
				changed = true
			}
			if resp, ok := e.Data["response"].(map[string]any); ok {
				if rid, ok := resp["id"].(string); ok && rid != "" {
					meta.Session.LatestResponseID = rid
					changed = true
				}
			}
			if changed {
				_ = r.FS.SaveAttemptMeta(meta)
			}
		}
	}
}

// isPreviousResponseNotFound reports whether err is the specific Hermes
// rejection that the previous_response_id we supplied does not exist — e.g.
// after the user cancels mid-stream or Hermes evicts the response.
func isPreviousResponseNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "404") && strings.Contains(s, "Previous response not found")
}

func (r *Runner) logSystemEvent(attemptID, event string, extra map[string]any) {
	evt := map[string]any{"kind": "system", "event": event, "ts": time.Now().Unix()}
	for k, v := range extra {
		evt[k] = v
	}
	seq, _ := r.FS.AppendEvent(attemptID, evt)
	r.Hub.Publish("attempt:"+attemptID, sse.Event{Seq: seq, Event: "event", Data: evt})
}

// buildSystemPrompt composes the first-turn system prompt:
//   - the board's base persona,
//   - plus any `system_prompt` attached to the task's tags.
//
// Tag prompts let users express cross-cutting instructions once (e.g. "when
// finished, notify via QQ") and have them automatically injected into every
// task tagged that way. Multiple tags concatenate in the order they appear.
func (r *Runner) buildSystemPrompt(ctx context.Context, taskID string) string {
	base := "You are Hermes, acting as an autonomous task agent within the Hermes Task Board. Execute the task end-to-end and describe your reasoning as you go."
	task, err := r.Store.GetTask(ctx, taskID)
	if err != nil || task == nil || len(task.Tags) == 0 {
		return base
	}
	tags, err := r.Store.TagsByNames(ctx, task.Tags)
	if err != nil {
		return base
	}
	var extras []string
	for _, t := range tags {
		if s := strings.TrimSpace(t.SystemPrompt); s != "" {
			extras = append(extras, s)
		}
	}
	if len(extras) == 0 {
		return base
	}
	return base + "\n\n" + strings.Join(extras, "\n\n")
}

func (r *Runner) broadcastStateChange(attemptID string, state store.AttemptState) {
	r.Hub.Publish("board", sse.Event{Event: "attempt.state_changed", Data: map[string]any{
		"attempt_id": attemptID,
		"state":      string(state),
	}})
}

func (r *Runner) maybeAdvance(ctx context.Context, attemptID string) {
	att, err := r.Store.GetAttempt(ctx, attemptID)
	if err != nil {
		return
	}
	_ = r.Board.MaybeAdvanceAfterAttempt(ctx, att.TaskID)
}

// ------- helpers -------

type ConcurrencyErr struct{ Level string }

func (c *ConcurrencyErr) Error() string { return "concurrency_limit:" + c.Level }
func Concurrency(level string) error    { return &ConcurrencyErr{Level: level} }

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

func errStr(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}
