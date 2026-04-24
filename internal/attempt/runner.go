// Package attempt owns the lifecycle of a single Attempt: drives a Hermes
// conversation, streams events to disk + SSE, and coordinates state
// transitions. Every API takes a `username` so the runner can route
// store + filesystem reads to the correct per-user directory.
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
	"github.com/ahkimkoo/hermes-taskboard/internal/userdir"
)

type Runner struct {
	Cfg    *config.Store
	Stores *store.Manager
	FS     *fsstore.Manager
	Users  *userdir.Manager
	Pool   *hermes.Pool
	Hub    *sse.Hub
	Board  *board.Service

	mu     sync.Mutex
	active map[string]*runCtx // attempt_id → live context
}

type runCtx struct {
	username  string
	cancel    context.CancelFunc
	queue     chan string // pending user inputs
	runningMu sync.Mutex
	running   bool
}

func New(cfg *config.Store, stores *store.Manager, fs *fsstore.Manager, users *userdir.Manager, pool *hermes.Pool, hub *sse.Hub, b *board.Service) *Runner {
	return &Runner{
		Cfg: cfg, Stores: stores, FS: fs, Users: users,
		Pool: pool, Hub: hub, Board: b,
		active: map[string]*runCtx{},
	}
}

// ResumeOrphans reattaches to Hermes runs that were in-flight when the
// previous process died. Iterates every user's DB.
//
// Recovery strategy, in priority order:
//  1. If the stored run_id is still streaming on Hermes, reattach to
//     it. Pure recovery — no extra prompt, no turn cost.
//  2. Otherwise (run_id missing, or Hermes forgot the run), fall back
//     to kicking an auto-continue. The Hermes conversation — keyed by
//     attempt.ID — is preserved even when the SSE run is gone, so a
//     fresh turn with previous_response_id picks up where the prior
//     completed turn left off.
//  3. If even that isn't possible (no prior completed turn + task
//     details unreadable), or if the attempt has already burned
//     AutoResumeMaxRetries of auto-continues, only then mark Failed.
func (r *Runner) ResumeOrphans(ctx context.Context, usernames []string) (resumed, failed int, err error) {
	owned, err := r.Stores.ListAllActiveAttempts(ctx, usernames)
	if err != nil {
		return 0, 0, err
	}
	for _, oa := range owned {
		a := oa.Attempt
		if a.State != store.AttemptRunning && a.State != store.AttemptQueued {
			continue
		}
		fs := r.FS.Get(oa.Username)
		meta, _ := fs.LoadAttemptMeta(a.ID)
		var runID string
		if meta != nil {
			runID = meta.Session.CurrentRunID
		}
		st, serr := r.Stores.Get(oa.Username)
		if serr != nil {
			failed++
			continue
		}
		if runID != "" {
			if rerr := r.resumeAttempt(oa.Username, a.ID, a.ServerID, runID); rerr == nil {
				r.logSystemEvent(oa.Username, a.ID, "resumed", map[string]any{"run_id": runID})
				resumed++
				continue
			} else {
				r.logSystemEvent(oa.Username, a.ID, "resume_failed", map[string]any{
					"run_id":      runID,
					"prior_state": string(a.State),
					"err":         rerr.Error(),
				})
			}
		} else {
			r.logSystemEvent(oa.Username, a.ID, "resume_no_run_id", map[string]any{
				"prior_state": string(a.State),
			})
		}
		if r.kickRestartAutoContinue(ctx, st, fs, oa.Username, a, meta) {
			resumed++
		} else {
			failed++
		}
	}
	return resumed, failed, nil
}

// kickRestartAutoContinue rescues an orphan whose live Hermes run could
// not be reattached. Returns true when an auto-continue was launched,
// false when the attempt was marked Failed instead (no conversation
// state or retries exhausted).
func (r *Runner) kickRestartAutoContinue(ctx context.Context, st *store.Store, fs *fsstore.FS, username string, a *store.Attempt, meta *store.AttemptMeta) bool {
	markFailed := func(reason string) {
		r.logSystemEvent(username, a.ID, "auto_resume_restart_abandoned", map[string]any{"reason": reason})
		_ = st.UpdateAttemptState(ctx, a.ID, store.AttemptFailed)
		r.broadcastStateChange(a.ID, store.AttemptFailed)
		_ = r.Board.MaybeAdvanceAfterAttempt(ctx, st, a.TaskID)
	}
	if meta == nil {
		markFailed("no_meta")
		return false
	}
	if meta.Session.ContinueResumeCount >= AutoResumeMaxRetries {
		markFailed("retries_exhausted")
		return false
	}
	// Pick the recovery input: "continue" when prior turns are on record;
	// otherwise rebuild the original task prompt so Hermes has context.
	var input, kind string
	if meta.Session.LatestResponseID != "" {
		input = AutoResumeMessage
		kind = "continue"
	} else {
		rebuilt, ok := r.rebuildInitialInput(ctx, st, fs, a.TaskID, a.ID)
		if !ok {
			markFailed("rebuild_failed")
			return false
		}
		input = rebuilt
		kind = "retry_initial"
	}
	meta.Session.ContinueResumeCount++
	meta.Session.LastContinueAt = time.Now().Unix()
	meta.Session.LastDisconnectReason = store.DisconnectAbnormal
	meta.Session.LastDisconnectAt = time.Now().Unix()
	meta.Session.CurrentRunID = ""
	_ = fs.SaveAttemptMeta(meta)
	r.logSystemEvent(username, a.ID, "auto_resume_restart", map[string]any{
		"kind":        kind,
		"retry_count": meta.Session.ContinueResumeCount,
		"max_retries": AutoResumeMaxRetries,
	})
	r.startLoop(username, a.ID, input)
	return true
}

// rebuildInitialInput reproduces the first-turn user message that
// Start() sends. Used when a restart orphan has no LatestResponseID
// to continue from — we have to resend the task details so Hermes
// knows what to work on.
func (r *Runner) rebuildInitialInput(ctx context.Context, st *store.Store, fs *fsstore.FS, taskID, attemptID string) (string, bool) {
	task, err := st.GetTask(ctx, taskID)
	if err != nil || task == nil {
		return "", false
	}
	doc, _ := fs.LoadTaskDoc(taskID)
	var desc string
	if doc != nil {
		desc = doc.Description
	}
	idPrefix := attemptID
	if len(idPrefix) > 8 {
		idPrefix = idPrefix[:8]
	}
	return fmt.Sprintf("[tb-%s] # Task\n%s\n\n%s", idPrefix, task.Title, desc), true
}

// TryReconnect opens a fresh SSE subscription to the Hermes run for
// attemptID (must belong to username).
func (r *Runner) TryReconnect(ctx context.Context, username, attemptID string) (string, error) {
	r.mu.Lock()
	_, live := r.active[attemptID]
	r.mu.Unlock()
	if live {
		return "already_live", nil
	}
	st, err := r.Stores.Get(username)
	if err != nil {
		return "", err
	}
	att, err := st.GetAttempt(ctx, attemptID)
	if err != nil {
		return "", err
	}
	fs := r.FS.Get(username)
	meta, _ := fs.LoadAttemptMeta(attemptID)
	var runID string
	if meta != nil {
		runID = meta.Session.CurrentRunID
	}
	if runID == "" {
		return "no_run_id", nil
	}
	if err := r.resumeAttempt(username, attemptID, att.ServerID, runID); err != nil {
		return "", err
	}
	r.logSystemEvent(username, attemptID, "resumed", map[string]any{"run_id": runID, "trigger": "user_scroll"})
	return "reconnected", nil
}

// resumeAttempt spawns a goroutine that tails `/v1/runs/{runID}/events`
// and feeds handleEvent.
func (r *Runner) resumeAttempt(username, attemptID, serverID, runID string) error {
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
	rc := &runCtx{username: username, cancel: cancel, queue: make(chan string, 32)}
	r.active[attemptID] = rc
	r.mu.Unlock()
	go r.resumeLoop(ctx, username, attemptID, runID, client, rc)
	return nil
}

func (r *Runner) resumeLoop(ctx context.Context, username, attemptID, runID string, client *hermes.Client, rc *runCtx) {
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
		r.handleEvent(username, attemptID, e)
	}
	err := <-streamErr
	r.logSystemEvent(username, attemptID, "run_end", map[string]any{"run_id": runID, "err": errStr(err), "resumed": true})
	cctx := context.Background()
	st, serr := r.Stores.Get(username)
	if serr != nil {
		return
	}
	att, gerr := st.GetAttempt(cctx, attemptID)
	if gerr != nil || att == nil {
		return
	}
	if att.State == store.AttemptRunning || att.State == store.AttemptQueued {
		final := store.AttemptCompleted
		if err != nil && !errors.Is(err, context.Canceled) {
			final = store.AttemptFailed
		}
		_ = st.UpdateAttemptState(cctx, attemptID, final)
		r.broadcastStateChange(attemptID, final)
		_ = r.Board.MaybeAdvanceAfterAttempt(cctx, st, att.TaskID)
	}
}

// Start creates a new Attempt for the given user's task and drives it.
// serverID is an optional override; the chosen server's Profile
// fully determines which Hermes profile the run targets (each API
// server is tied to exactly one profile per Hermes docs).
func (r *Runner) Start(ctx context.Context, username, taskID, serverID string) (*store.Attempt, error) {
	st, err := r.Stores.Get(username)
	if err != nil {
		return nil, err
	}
	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	sv, effModel := r.resolveServerModel(username, firstNonEmpty(serverID, task.PreferredServer))
	if sv == nil {
		return nil, errors.New("no hermes server configured")
	}
	if effModel == "" {
		return nil, errors.New("no model resolvable")
	}

	cfg := r.Cfg.Snapshot()

	// Global concurrency aggregates across ALL users' DBs.
	allUsers := r.allUsernames()
	global, byServer, _, err := r.Stores.ActiveCounts(ctx, allUsers, sv.ID, effModel)
	if err != nil {
		return nil, err
	}
	if global >= cfg.Scheduler.GlobalMaxConcurrent {
		return nil, Concurrency("global")
	}
	// Server cap applies across all users because the Hermes server
	// itself has a single concurrency budget. No separate per-profile
	// cap anymore: one server = one profile, so they're equivalent.
	if byServer >= sv.MaxConcurrent {
		return nil, Concurrency("server")
	}

	attemptID := uuid.NewString()
	att := &store.Attempt{
		ID: attemptID, TaskID: taskID,
		ServerID: sv.ID, Model: effModel,
		State: store.AttemptQueued,
	}
	if err := st.CreateAttempt(ctx, att); err != nil {
		return nil, err
	}
	fs := r.FS.Get(username)
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
	if err := fs.SaveAttemptMeta(meta); err != nil {
		return nil, err
	}

	doc, _ := fs.LoadTaskDoc(taskID)
	var desc string
	if doc != nil {
		desc = doc.Description
	}
	idPrefix := attemptID
	if len(idPrefix) > 8 {
		idPrefix = idPrefix[:8]
	}
	initialInput := fmt.Sprintf("[tb-%s] # Task\n%s\n\n%s", idPrefix, task.Title, desc)

	if task.Status == store.StatusPlan || task.Status == store.StatusDraft || task.Status == store.StatusVerify {
		_ = r.Board.Transition(ctx, st, taskID, store.StatusExecute, board.KindAuto, "attempt_started")
	}
	r.Hub.Publish("board", sse.Event{Event: "attempt.created", Data: map[string]any{
		"task_id":    taskID,
		"attempt_id": attemptID,
		"server_id":  sv.ID,
		"model":      effModel,
		"owner":      username,
	}})
	r.logSystemEvent(username, attemptID, "user_message", map[string]any{"input": initialInput})
	r.startLoop(username, attemptID, initialInput)
	return att, nil
}

// SendMessage enqueues a user message.
func (r *Runner) SendMessage(ctx context.Context, username, attemptID, text string) error {
	st, err := r.Stores.Get(username)
	if err != nil {
		return err
	}
	att, err := st.GetAttempt(ctx, attemptID)
	if err != nil {
		return err
	}
	fs := r.FS.Get(username)
	if meta, _ := fs.LoadAttemptMeta(attemptID); meta != nil && meta.Session.ContinueResumeCount > 0 {
		meta.Session.ContinueResumeCount = 0
		_ = fs.SaveAttemptMeta(meta)
	}
	r.logSystemEvent(username, attemptID, "user_message", map[string]any{"input": text})

	if att.State == store.AttemptCompleted || att.State == store.AttemptFailed || att.State == store.AttemptCancelled {
		if err := st.UpdateAttemptState(ctx, attemptID, store.AttemptQueued); err != nil {
			return err
		}
		_ = r.Board.Transition(ctx, st, att.TaskID, store.StatusExecute, board.KindAuto, "verify_followup")
		r.startLoop(username, attemptID, text)
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
	r.startLoop(username, attemptID, text)
	return nil
}

// Cancel stops the active run and marks the attempt as completed.
//
// Semantics note: a user-initiated stop is treated as a successful
// terminal state (AttemptCompleted), not AttemptCancelled. Rationale:
// the user chose to end the turn — from their perspective the work
// is finished. The DisconnectCancelled reason still gets written to
// attempt meta by the run loop, so the auto-resumer knows not to
// retry. AttemptCancelled remains a valid legacy state for existing
// rows; no new writes produce it.
func (r *Runner) Cancel(ctx context.Context, username, attemptID string) error {
	r.mu.Lock()
	rc, ok := r.active[attemptID]
	r.mu.Unlock()
	if ok && rc.cancel != nil {
		rc.cancel()
	}
	st, err := r.Stores.Get(username)
	if err != nil {
		return err
	}
	_ = st.UpdateAttemptState(ctx, attemptID, store.AttemptCompleted)
	r.broadcastStateChange(attemptID, store.AttemptCompleted)
	att, err := st.GetAttempt(ctx, attemptID)
	if err == nil {
		_ = r.Board.MaybeAdvanceAfterAttempt(ctx, st, att.TaskID)
	}
	return nil
}

// startLoop kicks off the per-attempt goroutine.
func (r *Runner) startLoop(username, attemptID, firstInput string) {
	r.mu.Lock()
	if _, ok := r.active[attemptID]; ok {
		rc := r.active[attemptID]
		r.mu.Unlock()
		select {
		case rc.queue <- firstInput:
		default:
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	rc := &runCtx{username: username, cancel: cancel, queue: make(chan string, 32)}
	r.active[attemptID] = rc
	r.mu.Unlock()

	go r.loop(ctx, username, attemptID, firstInput, rc)
}

func (r *Runner) loop(ctx context.Context, username, attemptID, firstInput string, rc *runCtx) {
	defer func() {
		r.mu.Lock()
		delete(r.active, attemptID)
		r.mu.Unlock()
	}()
	st, err := r.Stores.Get(username)
	if err != nil {
		return
	}
	pending := firstInput
	isFirst := true
	for {
		if pending != "" {
			if err := r.runOnce(ctx, username, attemptID, pending, isFirst); err != nil {
				if errors.Is(err, errAbnormalDisconnect) {
					return
				}
				r.logSystemEvent(username, attemptID, "error", map[string]any{"msg": err.Error()})
				_ = st.UpdateAttemptState(ctx, attemptID, store.AttemptFailed)
				r.broadcastStateChange(attemptID, store.AttemptFailed)
				r.maybeAdvance(ctx, username, attemptID)
				return
			}
			pending = ""
			isFirst = false
		}
		select {
		case <-ctx.Done():
			return
		case nxt, ok := <-rc.queue:
			if !ok {
				return
			}
			pending = nxt
		case <-time.After(50 * time.Millisecond):
			att, err := st.GetAttempt(ctx, attemptID)
			if err == nil && (att.State == store.AttemptRunning || att.State == store.AttemptQueued) {
				_ = st.UpdateAttemptState(ctx, attemptID, store.AttemptCompleted)
				r.broadcastStateChange(attemptID, store.AttemptCompleted)
				r.maybeAdvance(ctx, username, attemptID)
				return
			}
			if err == nil && (att.State == store.AttemptCompleted || att.State == store.AttemptFailed || att.State == store.AttemptCancelled) {
				return
			}
		}
	}
}

// runOnce performs one Hermes round and streams events to disk + SSE.
func (r *Runner) runOnce(ctx context.Context, username, attemptID, input string, first bool) error {
	st, err := r.Stores.Get(username)
	if err != nil {
		return err
	}
	fs := r.FS.Get(username)
	_ = st.UpdateAttemptState(ctx, attemptID, store.AttemptRunning)
	r.broadcastStateChange(attemptID, store.AttemptRunning)

	att, err := st.GetAttempt(ctx, attemptID)
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
	if meta, _ := fs.LoadAttemptMeta(attemptID); meta != nil && meta.Session.LatestResponseID != "" {
		req.PreviousResponseID = meta.Session.LatestResponseID
	}
	req.SystemPrompt = r.buildSystemPrompt(ctx, username, att.TaskID)
	if req.SystemPrompt != "" {
		r.logSystemEvent(username, attemptID, "system_prompt_sent", map[string]any{
			"instructions": req.SystemPrompt,
			"length":       len(req.SystemPrompt),
			"first_turn":   first,
		})
	}
	res, err := client.CreateResponse(ctx, req)
	if err != nil {
		if req.PreviousResponseID != "" && isPreviousResponseNotFound(err) {
			r.logSystemEvent(username, attemptID, "previous_response_id_stale", map[string]any{
				"previous_response_id": req.PreviousResponseID,
				"err":                  err.Error(),
			})
			if meta, _ := fs.LoadAttemptMeta(attemptID); meta != nil {
				meta.Session.LatestResponseID = ""
				_ = fs.SaveAttemptMeta(meta)
			}
			req.PreviousResponseID = ""
			res, err = client.CreateResponse(ctx, req)
		}
		if err != nil {
			return err
		}
	}
	runID := res.RunID
	if meta, _ := fs.LoadAttemptMeta(attemptID); meta != nil {
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
			_ = fs.SaveAttemptMeta(meta)
		}
	}
	r.logSystemEvent(username, attemptID, "run_start", map[string]any{
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

	var sawCompleted bool
	for e := range events {
		r.handleEvent(username, attemptID, e)
		if typ, _ := e.Data["type"].(string); typ == "response.completed" {
			sawCompleted = true
		}
	}
	err = <-streamErr
	r.logSystemEvent(username, attemptID, "run_end", map[string]any{"run_id": runID, "err": errStr(err)})

	reason := store.DisconnectAbnormal
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		reason = store.DisconnectCancelled
	} else if err == nil && sawCompleted {
		reason = store.DisconnectCompleted
	}
	if meta, _ := fs.LoadAttemptMeta(attemptID); meta != nil {
		meta.Session.CurrentRunID = ""
		meta.Session.LastDisconnectReason = reason
		meta.Session.LastDisconnectAt = time.Now().Unix()
		if reason == store.DisconnectCompleted {
			meta.Session.ContinueResumeCount = 0
		}
		_ = fs.SaveAttemptMeta(meta)
	}
	if reason == store.DisconnectAbnormal {
		r.logSystemEvent(username, attemptID, "abnormal_disconnect", map[string]any{
			"run_id":  runID,
			"err":     errStr(err),
			"ctx_err": errStr(ctx.Err()),
		})
		return errAbnormalDisconnect
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

var errAbnormalDisconnect = errors.New("abnormal SSE disconnect; awaiting auto-resume")

// handleEvent is the single routing point for a Hermes SSE event.
func (r *Runner) handleEvent(username, attemptID string, e hermes.Event) {
	fs := r.FS.Get(username)
	evt := map[string]any{"kind": "hermes", "data": e.Data, "ts": time.Now().Unix()}
	seq, _ := fs.AppendEvent(attemptID, evt)
	r.Hub.Publish("attempt:"+attemptID, sse.Event{Seq: seq, Event: "event", Data: evt})

	typ, _ := e.Data["type"].(string)

	if typ == "response.created" {
		if resp, ok := e.Data["response"].(map[string]any); ok {
			if rid, ok := resp["id"].(string); ok && rid != "" {
				if meta, _ := fs.LoadAttemptMeta(attemptID); meta != nil {
					meta.Session.CurrentRunID = rid
					if len(meta.Session.Runs) == 0 || meta.Session.Runs[len(meta.Session.Runs)-1] != rid {
						meta.Session.Runs = append(meta.Session.Runs, rid)
					}
					_ = fs.SaveAttemptMeta(meta)
				}
			}
		}
	}

	if typ == "input_required" || typ == "approval_required" {
		st, err := r.Stores.Get(username)
		if err == nil {
			_ = st.UpdateAttemptState(context.Background(), attemptID, store.AttemptNeedsInput)
		}
		r.broadcastStateChange(attemptID, store.AttemptNeedsInput)
	}
	if typ == "response.completed" {
		if meta, _ := fs.LoadAttemptMeta(attemptID); meta != nil {
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
				_ = fs.SaveAttemptMeta(meta)
			}
		}
	}
}

func isPreviousResponseNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "404") && strings.Contains(s, "Previous response not found")
}

func (r *Runner) logSystemEvent(username, attemptID, event string, extra map[string]any) {
	fs := r.FS.Get(username)
	evt := map[string]any{"kind": "system", "event": event, "ts": time.Now().Unix()}
	for k, v := range extra {
		evt[k] = v
	}
	seq, _ := fs.AppendEvent(attemptID, evt)
	r.Hub.Publish("attempt:"+attemptID, sse.Event{Seq: seq, Event: "event", Data: evt})
}

// LookupAttempt finds which user owns an attempt, searching across
// every currently-registered user. Used by SSE stream handlers and
// the resumer when only an attempt id is known.
func (r *Runner) LookupAttempt(ctx context.Context, attemptID string) (string, *store.Attempt, error) {
	// Fast path: active runCtx knows the username.
	r.mu.Lock()
	if rc, ok := r.active[attemptID]; ok {
		u := rc.username
		r.mu.Unlock()
		st, err := r.Stores.Get(u)
		if err != nil {
			return "", nil, err
		}
		a, err := st.GetAttempt(ctx, attemptID)
		return u, a, err
	}
	r.mu.Unlock()
	return r.Stores.FindAttempt(ctx, r.allUsernames(), attemptID)
}

// buildSystemPrompt composes the first-turn system prompt from board
// persona + tag system_prompts (looked up in the user's userdir tags).
func (r *Runner) buildSystemPrompt(ctx context.Context, username, taskID string) string {
	base := "You are Hermes, acting as an autonomous task agent within the Hermes Task Board. Execute the task end-to-end and describe your reasoning as you go."
	st, err := r.Stores.Get(username)
	if err != nil {
		return base
	}
	task, err := st.GetTask(ctx, taskID)
	if err != nil || task == nil || len(task.Tags) == 0 {
		return base
	}
	var extras []string
	for _, name := range task.Tags {
		_, t, _, ok := r.Users.TagByName(username, name)
		if !ok {
			continue
		}
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

func (r *Runner) maybeAdvance(ctx context.Context, username, attemptID string) {
	st, err := r.Stores.Get(username)
	if err != nil {
		return
	}
	att, err := st.GetAttempt(ctx, attemptID)
	if err != nil {
		return
	}
	_ = r.Board.MaybeAdvanceAfterAttempt(ctx, st, att.TaskID)
}

// resolveServerModel looks up a server across the user's own + shared
// configs and returns it along with the profile name to pass to Hermes
// as the `model` field. Since each Hermes API server is tied to a
// single profile, the second return value is simply the server's
// configured Profile, falling back to the "hermes-agent" default
// profile name advertised by bare Hermes installs.
func (r *Runner) resolveServerModel(username, preferredServer string) (*userdir.HermesServer, string) {
	var sv *userdir.HermesServer
	if preferredServer != "" {
		if _, s, usable, found := r.Users.FindServer(username, preferredServer); found && usable {
			sv = &s
		}
	}
	if sv == nil {
		if v := r.Users.DefaultServer(username); v != nil {
			s := v.HermesServer
			sv = &s
		}
	}
	if sv == nil {
		return nil, ""
	}
	profile := sv.Profile
	if profile == "" {
		profile = config.HermesDefaultAgent
	}
	return sv, profile
}

// allUsernames returns every cached username. Primarily used by
// cross-user aggregate queries (concurrency, orphan enumeration).
func (r *Runner) allUsernames() []string {
	list := r.Users.List()
	out := make([]string, 0, len(list))
	for _, u := range list {
		out = append(out, u.Username)
	}
	return out
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
