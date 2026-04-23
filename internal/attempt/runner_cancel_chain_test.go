package attempt

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ahkimkoo/hermes-taskboard/internal/board"
	"github.com/ahkimkoo/hermes-taskboard/internal/config"
	"github.com/ahkimkoo/hermes-taskboard/internal/hermes"
	"github.com/ahkimkoo/hermes-taskboard/internal/sse"
	"github.com/ahkimkoo/hermes-taskboard/internal/store"
	"github.com/ahkimkoo/hermes-taskboard/internal/store/fsstore"
	"github.com/ahkimkoo/hermes-taskboard/internal/userdir"
)

// fakeHermes simulates the subset of the Hermes /v1/responses endpoint that
// matters for the previous_response_id chaining bug:
//   - tracks which response ids have "completed" on the server (i.e. would be
//     retained and accepted as a future previous_response_id)
//   - returns 404 "Previous response not found" for ids that were never
//     completed (e.g. cancelled mid-stream), mirroring real Hermes behavior
type fakeHermes struct {
	mu           sync.Mutex
	completed    map[string]bool // ids Hermes would keep as chain anchors
	seenPrevIDs  []string        // previous_response_id received on each call
	nextID       atomic.Int32
	// mode controls the next stream's outcome: "complete" or "cancel".
	// In "cancel" mode the handler sends response.created then hangs until
	// the HTTP client (runner) disconnects — simulating the user hitting Stop.
	mode string
	// createdSent fires after each response.created has been flushed, so
	// tests can deterministically wait for the client to have processed
	// that event before cancelling.
	createdSent chan string
}

func newFakeHermes() *fakeHermes {
	return &fakeHermes{
		completed:   map[string]bool{},
		mode:        "complete",
		createdSent: make(chan string, 8),
	}
}

func (f *fakeHermes) setMode(m string) {
	f.mu.Lock()
	f.mode = m
	f.mu.Unlock()
}

func (f *fakeHermes) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seenPrevIDs...)
}

func (f *fakeHermes) markCompleted(id string) {
	f.mu.Lock()
	f.completed[id] = true
	f.mu.Unlock()
}

func (f *fakeHermes) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/responses" {
		http.NotFound(w, r)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	prev, _ := body["previous_response_id"].(string)

	f.mu.Lock()
	f.seenPrevIDs = append(f.seenPrevIDs, prev)
	mode := f.mode
	validPrev := prev == "" || f.completed[prev]
	f.mu.Unlock()

	if !validPrev {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"error":{"message":"Previous response not found: %s","type":"invalid_request_error","param":null,"code":null}}`, prev)
		return
	}

	id := fmt.Sprintf("resp_%d", f.nextID.Add(1))
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeEvent := func(obj any) {
		b, _ := json.Marshal(obj)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	writeEvent(map[string]any{
		"type":     "response.created",
		"response": map[string]any{"id": id},
	})
	// Non-blocking signal so multiple concurrent callers don't stall if the
	// test isn't draining.
	select {
	case f.createdSent <- id:
	default:
	}

	if mode == "cancel" {
		// Hang until the runner cancels its HTTP request context. This
		// mirrors a Hermes run that was in-flight when the user clicked
		// Stop — the server never finishes the response, so the id won't
		// enter `completed` and is invalid as a future chain anchor.
		<-r.Context().Done()
		return
	}

	// Complete the response.
	writeEvent(map[string]any{
		"type":        "response.completed",
		"response":    map[string]any{"id": id},
		"output_text": "ok",
	})
	f.markCompleted(id)
}

// testHarness bundles the pieces a test drives runOnce through.
type testHarness struct {
	t        *testing.T
	runner   *Runner
	store    *store.Store
	fs       *fsstore.FS
	fake     *fakeHermes
	httpSrv  *httptest.Server
	username string
}

const testUsername = "testuser"

func newHarness(t *testing.T) *testHarness {
	t.Helper()
	dir := t.TempDir()

	cfg, err := config.NewStore(filepath.Join(dir, "config.yaml"), filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	// Seed userdir with a single test user so the Runner's tag lookup
	// and other userdir-backed paths have something to find.
	users := userdir.New(dir, cfg.Secret())
	if err := users.Create(&userdir.UserConfig{
		Username: testUsername, PasswordHash: "x", IsAdmin: true,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	stores := store.NewManager(dir)
	t.Cleanup(func() { stores.Close() })
	st, err := stores.Get(testUsername)
	if err != nil {
		t.Fatalf("open user store: %v", err)
	}
	fsMgr := fsstore.NewManager(dir)
	fs := fsMgr.Get(testUsername)

	fake := newFakeHermes()
	srv := httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(srv.Close)

	pool := hermes.NewPool()
	pool.Reload([]hermes.PoolEntry{{ID: "test-srv", BaseURL: srv.URL, APIKey: "k", IsDefault: true}})

	hub := sse.NewHub()
	boardSvc := board.New(hub)

	runner := New(cfg, stores, fsMgr, users, pool, hub, boardSvc)
	return &testHarness{t: t, runner: runner, store: st, fs: fs, fake: fake, httpSrv: srv, username: testUsername}
}

// newAttempt inserts a bare attempt row + empty meta ready for runOnce.
func (h *testHarness) newAttempt() string {
	h.t.Helper()
	ctx := context.Background()
	// A task row isn't strictly required — buildSystemPrompt gracefully
	// falls back to the default persona when GetTask errors. Still, add
	// one for realism.
	task := &store.Task{ID: "task-" + randSuffix(), Title: "test", Status: store.StatusExecute}
	if err := h.store.CreateTask(ctx, task); err != nil {
		h.t.Fatalf("create task: %v", err)
	}
	att := &store.Attempt{
		ID:       "att-" + randSuffix(),
		TaskID:   task.ID,
		ServerID: "test-srv",
		Model:    "gpt-4o",
		State:    store.AttemptQueued,
	}
	if err := h.store.CreateAttempt(ctx, att); err != nil {
		h.t.Fatalf("create attempt: %v", err)
	}
	if err := h.fs.SaveAttemptMeta(&store.AttemptMeta{
		ID:       att.ID,
		TaskID:   att.TaskID,
		ServerID: att.ServerID,
		Model:    att.Model,
		Session:  store.AttemptSession{ConversationID: att.ID},
	}); err != nil {
		h.t.Fatalf("save meta: %v", err)
	}
	return att.ID
}

func randSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// runCancelledTurn drives one runOnce that will be interrupted mid-stream
// (after response.created has been *processed by the runner*, before
// response.completed) via ctx cancellation.
func (h *testHarness) runCancelledTurn(attemptID, input string) {
	h.t.Helper()
	h.fake.setMode("cancel")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.runner.runOnce(ctx, h.username, attemptID, input, true) }()

	// Wait for the runner to have processed response.created — evidenced
	// by CurrentRunID being written into meta by handleEvent. Polling this
	// is more reliable than racing on the server-side flush signal,
	// because CreateResponse on the client side still needs to return and
	// the SSE consumer goroutine needs to dispatch the event.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if meta, _ := h.fs.LoadAttemptMeta(attemptID); meta != nil && meta.Session.CurrentRunID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			h.t.Fatalf("runOnce returned unexpected err after cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		h.t.Fatalf("runOnce did not return after cancel")
	}
	// Runner clears CurrentRunID in the runOnce finalize block; give that
	// write a moment to land before the caller inspects meta.
	time.Sleep(50 * time.Millisecond)
}

func (h *testHarness) runCompletedTurn(attemptID, input string) {
	h.t.Helper()
	h.fake.setMode("complete")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.runner.runOnce(ctx, h.username, attemptID, input, false); err != nil {
		h.t.Fatalf("runOnce completed turn: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
}

// -----------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------

// TestCancelDoesNotPersistStaleResponseID verifies the fix: when a turn is
// cancelled before response.completed fires, LatestResponseID must NOT be
// updated, so the next turn can't chain off a discarded response.
//
// Before the fix, LatestResponseID was written on response.created — the
// reproduction of the user's 404 bug is: (a) start a turn, (b) cancel
// mid-stream, (c) send another turn — Hermes rejects because the id from
// step (a) was never actually retained.
func TestCancelDoesNotPersistStaleResponseID(t *testing.T) {
	h := newHarness(t)
	attID := h.newAttempt()

	h.runCancelledTurn(attID, "first input")

	meta, err := h.fs.LoadAttemptMeta(attID)
	if err != nil || meta == nil {
		t.Fatalf("load meta: %v", err)
	}
	if meta.Session.LatestResponseID != "" {
		t.Fatalf("LatestResponseID leaked after cancel: got %q, want empty "+
			"(this is the exact bug — next turn would 404 with "+
			"'Previous response not found')", meta.Session.LatestResponseID)
	}
	// CurrentRunID may be set (ResumeOrphans relies on it) — that's fine.
	// What matters is LatestResponseID.
}

// TestCancelThenResumeNoPreviousResponseID end-to-end: cancel a turn, then
// run a second turn on the same attempt, and confirm the runner sends no
// previous_response_id on that follow-up. If the bug were present, the
// second call would be rejected with 404 by the fake server.
func TestCancelThenResumeNoPreviousResponseID(t *testing.T) {
	h := newHarness(t)
	attID := h.newAttempt()

	h.runCancelledTurn(attID, "first")
	h.runCompletedTurn(attID, "second")

	seen := h.fake.seen()
	if len(seen) != 2 {
		t.Fatalf("expected 2 requests to fake hermes, got %d (%v)", len(seen), seen)
	}
	if seen[0] != "" {
		t.Fatalf("turn 1 should have no previous_response_id, got %q", seen[0])
	}
	if seen[1] != "" {
		t.Fatalf("turn 2 leaked previous_response_id %q from cancelled turn 1 "+
			"— this would 404 against real Hermes", seen[1])
	}

	// And after the successful second turn, LatestResponseID should pin
	// to that turn's completed id.
	meta, _ := h.fs.LoadAttemptMeta(attID)
	if meta.Session.LatestResponseID == "" {
		t.Fatalf("LatestResponseID not set after completed turn")
	}
}

// TestStaleResponseIDSelfHeals covers users who were already on a buggy
// build: their meta.json already carries a stale LatestResponseID pointing
// at a cancelled response. The first turn after upgrading should hit 404,
// the runner should clear the stale id and transparently retry as a cold
// start, and the turn should succeed.
func TestStaleResponseIDSelfHeals(t *testing.T) {
	h := newHarness(t)
	attID := h.newAttempt()

	// Seed a stale id that the fake server will reject.
	meta, _ := h.fs.LoadAttemptMeta(attID)
	meta.Session.LatestResponseID = "resp_stale_deadbeef"
	if err := h.fs.SaveAttemptMeta(meta); err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	h.runCompletedTurn(attID, "hello")

	seen := h.fake.seen()
	if len(seen) != 2 {
		t.Fatalf("expected 2 requests (reject + retry), got %d (%v)", len(seen), seen)
	}
	if seen[0] != "resp_stale_deadbeef" {
		t.Fatalf("first call should have tried the stale id, got %q", seen[0])
	}
	if seen[1] != "" {
		t.Fatalf("retry should drop previous_response_id, got %q", seen[1])
	}

	meta2, _ := h.fs.LoadAttemptMeta(attID)
	if meta2.Session.LatestResponseID == "resp_stale_deadbeef" {
		t.Fatalf("stale id never cleared")
	}
	if meta2.Session.LatestResponseID == "" {
		t.Fatalf("LatestResponseID should be set to the completed retry's id")
	}
}

// TestChainAfterCompletedTurn sanity-checks the happy path: two turns that
// both complete should result in turn 2 sending turn 1's completed id as
// previous_response_id — i.e. the fix doesn't break normal chaining.
func TestChainAfterCompletedTurn(t *testing.T) {
	h := newHarness(t)
	attID := h.newAttempt()

	h.runCompletedTurn(attID, "turn1")
	firstID := ""
	if meta, _ := h.fs.LoadAttemptMeta(attID); meta != nil {
		firstID = meta.Session.LatestResponseID
	}
	if firstID == "" {
		t.Fatalf("no LatestResponseID captured from completed turn 1")
	}

	h.runCompletedTurn(attID, "turn2")

	seen := h.fake.seen()
	if len(seen) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(seen))
	}
	if seen[0] != "" {
		t.Fatalf("turn 1 should cold-start, got prev=%q", seen[0])
	}
	if seen[1] != firstID {
		t.Fatalf("turn 2 should chain off %q, got %q", firstID, seen[1])
	}
}
