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

// ReapOrphans flips any attempt stuck in `queued` or `running` to `failed`.
// Call once at process startup, before the scheduler / cron worker fire, so
// that ghost attempts from a prior crash or kill don't hold concurrency slots
// and the UI doesn't show a spinner forever. `needs_input` attempts are left
// alone — they're legitimately waiting for the user, and SendMessage will
// restart their loop when input arrives.
func (r *Runner) ReapOrphans(ctx context.Context) (int, error) {
	active, err := r.Store.ListActiveAttempts(ctx)
	if err != nil {
		return 0, err
	}
	var reaped int
	for _, a := range active {
		if a.State != store.AttemptRunning && a.State != store.AttemptQueued {
			continue
		}
		r.logSystemEvent(a.ID, "error", map[string]any{
			"msg":         "process restart — attempt reaped as failed (no active runner)",
			"prior_state": string(a.State),
		})
		if err := r.Store.UpdateAttemptState(ctx, a.ID, store.AttemptFailed); err != nil {
			continue
		}
		r.broadcastStateChange(a.ID, store.AttemptFailed)
		_ = r.Board.MaybeAdvanceAfterAttempt(ctx, a.TaskID)
		reaped++
	}
	return reaped, nil
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

	// Compose initial prompt.
	doc, _ := r.FS.LoadTaskDoc(taskID)
	var desc string
	if doc != nil {
		desc = doc.Description
	}
	initialInput := fmt.Sprintf("# Task\n%s\n\n%s", task.Title, desc)

	// Move card into Execute (auto) if it isn't already.
	if task.Status == store.StatusPlan || task.Status == store.StatusDraft {
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
	r.startLoop(attemptID, initialInput)
	return att, nil
}

// SendMessage enqueues a user message on an existing attempt; starts a new run when idle.
func (r *Runner) SendMessage(ctx context.Context, attemptID, text string) error {
	att, err := r.Store.GetAttempt(ctx, attemptID)
	if err != nil {
		return err
	}
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
	if first {
		req.SystemPrompt = r.buildSystemPrompt(ctx, att.TaskID)
	}
	// Record user-side message as a system event for transcript continuity.
	r.logSystemEvent(attemptID, "user_message", map[string]any{"input": input})
	res, err := client.CreateResponse(ctx, req)
	if err != nil {
		return err
	}
	runID := res.RunID
	// Update meta with run_id.
	if meta, _ := r.FS.LoadAttemptMeta(attemptID); meta != nil {
		if runID != "" {
			meta.Session.CurrentRunID = runID
			meta.Session.Runs = append(meta.Session.Runs, runID)
		}
		meta.Session.LatestResponseID = res.ResponseID
		_ = r.FS.SaveAttemptMeta(meta)
	}
	r.logSystemEvent(attemptID, "run_start", map[string]any{"run_id": runID, "response_id": res.ResponseID})

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

	// consume until stream closes
	for e := range events {
		r.handleEvent(attemptID, e)
	}
	err = <-streamErr
	r.logSystemEvent(attemptID, "run_end", map[string]any{"run_id": runID, "err": errStr(err)})

	// finalize meta
	if meta, _ := r.FS.LoadAttemptMeta(attemptID); meta != nil {
		meta.Session.CurrentRunID = ""
		_ = r.FS.SaveAttemptMeta(meta)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// handleEvent is the single routing point for a Hermes SSE event.
func (r *Runner) handleEvent(attemptID string, e hermes.Event) {
	// Persist raw event (with seq) and publish to SSE topic.
	evt := map[string]any{"kind": "hermes", "data": e.Data, "ts": time.Now().Unix()}
	seq, _ := r.FS.AppendEvent(attemptID, evt)
	r.Hub.Publish("attempt:"+attemptID, sse.Event{Seq: seq, Event: "event", Data: evt})
	// Detect needs_input via conservative heuristic: tool_call with approval_required.
	if typ, _ := e.Data["type"].(string); typ == "input_required" || typ == "approval_required" {
		_ = r.Store.UpdateAttemptState(context.Background(), attemptID, store.AttemptNeedsInput)
		r.broadcastStateChange(attemptID, store.AttemptNeedsInput)
	}
	// Capture final assistant message summary.
	if typ, _ := e.Data["type"].(string); typ == "response.completed" {
		if out, ok := e.Data["output_text"].(string); ok {
			if meta, _ := r.FS.LoadAttemptMeta(attemptID); meta != nil {
				meta.Summary = out
				_ = r.FS.SaveAttemptMeta(meta)
			}
		}
	}
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
