package attempt

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ahkimkoo/hermes-taskboard/internal/store"
)

// v0.3.16 coverage: user-initiated Cancel lands the attempt in
// AttemptCompleted (not AttemptCancelled). The semantic intent is that
// a deliberate stop is a positive terminal — the user declared the turn
// finished.
func TestCancelMarksCompletedNotCancelled(t *testing.T) {
	h := newHarness(t)
	attID := h.newAttempt()
	// Pre-set to Running so Cancel's branch-ahead state machine has a
	// non-terminal value to flip.
	if err := h.store.UpdateAttemptState(context.Background(), attID, store.AttemptRunning); err != nil {
		t.Fatalf("seed running state: %v", err)
	}

	if err := h.runner.Cancel(context.Background(), h.username, attID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	att, err := h.store.GetAttempt(context.Background(), attID)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	if att.State != store.AttemptCompleted {
		t.Fatalf("cancel should produce AttemptCompleted, got %q", att.State)
	}
	if att.State == store.AttemptCancelled {
		t.Fatalf("AttemptCancelled must no longer be written by Cancel (legacy state)")
	}
}

// When a mid-turn cancel interrupts an active runLoop, the DB flips
// Completed but the runLoop's own DisconnectCancelled reason still
// lands on the meta — so the background Resumer won't mistake this
// for an abnormal disconnect and auto-retry the turn.
func TestCancelDoesNotTriggerAutoResume(t *testing.T) {
	h := newHarness(t)
	attID := h.newAttempt()
	h.runCancelledTurn(attID, "turn")

	// Simulate the Cancel() call landing after the runLoop's finalize
	// block, which writes meta.Session.LastDisconnectReason. (In the
	// real cancel flow these happen concurrently; we drive them
	// sequentially here because the assertion is about the
	// meta-level marker, not the race.)
	if err := h.runner.Cancel(context.Background(), h.username, attID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	att, _ := h.store.GetAttempt(context.Background(), attID)
	if att.State != store.AttemptCompleted {
		t.Fatalf("expected Completed after cancel, got %q", att.State)
	}
	meta, _ := h.fs.LoadAttemptMeta(attID)
	if meta == nil {
		t.Fatal("meta missing after cancelled turn")
	}
	if meta.Session.LastDisconnectReason == store.DisconnectAbnormal {
		t.Fatalf("cancelled turn must not record DisconnectAbnormal "+
			"(got %q) — would trigger auto-resume retries",
			meta.Session.LastDisconnectReason)
	}
}

// Cancel on an attempt with no live runCtx (runner goroutine already
// exited, e.g. ran to completion, then user clicks Stop on a stale
// UI) should still flip DB state without panicking.
func TestCancelOnNoLiveRunCtx(t *testing.T) {
	h := newHarness(t)
	attID := h.newAttempt()
	_ = h.store.UpdateAttemptState(context.Background(), attID, store.AttemptRunning)
	// Do NOT seed r.active — exercise the "no runCtx" path.
	if err := h.runner.Cancel(context.Background(), h.username, attID); err != nil {
		t.Fatalf("cancel without live runCtx: %v", err)
	}
	att, _ := h.store.GetAttempt(context.Background(), attID)
	if att.State != store.AttemptCompleted {
		t.Fatalf("state = %q, want completed", att.State)
	}
}

// ResumeOrphans — when the stored run_id can't be reattached AND the
// attempt has a LatestResponseID from a prior completed turn, the
// runner kicks an auto-continue that chains off that response id.
// Verifies:
//   - attempt stays in an active-ish state (startLoop fired runOnce,
//     which flips it to AttemptRunning)
//   - ContinueResumeCount incremented
//   - the fake Hermes saw a request using the prior response id as
//     previous_response_id (i.e. kind=continue, not retry_initial)
func TestResumeOrphansAutoContinueWithPriorResponse(t *testing.T) {
	h := newHarness(t)
	attID := h.newAttempt()

	// Seed the scenario: attempt was mid-session at taskboard restart —
	// had a completed first turn (LatestResponseID set), run_id was
	// cleared because the live SSE run was long-gone. State stays at
	// Running because that's what the orphan scanner looks for.
	if err := h.store.UpdateAttemptState(context.Background(), attID, store.AttemptRunning); err != nil {
		t.Fatalf("seed running: %v", err)
	}
	meta, _ := h.fs.LoadAttemptMeta(attID)
	meta.Session.LatestResponseID = "resp_prior_completed"
	meta.Session.CurrentRunID = ""
	if err := h.fs.SaveAttemptMeta(meta); err != nil {
		t.Fatalf("seed meta: %v", err)
	}
	// The fake server must accept this id as a valid chain anchor —
	// real Hermes keeps completed responses; the fake needs the same.
	h.fake.markCompleted("resp_prior_completed")

	resumed, failed, err := h.runner.ResumeOrphans(context.Background(), []string{h.username})
	if err != nil {
		t.Fatalf("ResumeOrphans: %v", err)
	}
	if resumed != 1 || failed != 0 {
		t.Fatalf("expected resumed=1 failed=0, got resumed=%d failed=%d", resumed, failed)
	}

	// ContinueResumeCount records the auto-continue attempt — check
	// immediately because a successful recovery turn resets it back to
	// 0 via runLoop's DisconnectCompleted finalize branch.
	meta2, _ := h.fs.LoadAttemptMeta(attID)
	if meta2.Session.ContinueResumeCount != 1 {
		t.Fatalf("ContinueResumeCount = %d, want 1", meta2.Session.ContinueResumeCount)
	}
	if meta2.Session.LastDisconnectReason != store.DisconnectAbnormal {
		t.Fatalf("LastDisconnectReason = %q, want abnormal",
			meta2.Session.LastDisconnectReason)
	}

	// Wait for startLoop → runOnce → fake server request to land, then
	// inspect what was sent.
	waitForSeen(t, h, 1, 3*time.Second)

	// Fake server received a continue, chained off the prior response id.
	seen := h.fake.seen()
	if len(seen) != 1 {
		t.Fatalf("fake saw %d requests, want 1 (%v)", len(seen), seen)
	}
	if seen[0] != "resp_prior_completed" {
		t.Fatalf("auto-continue must chain off prior completed id, "+
			"got previous_response_id=%q want resp_prior_completed", seen[0])
	}
}

// ResumeOrphans — when there's no LatestResponseID (the attempt died
// before any turn completed), auto-continue falls back to rebuilding
// the original task prompt and resending it. "continue" alone would
// leave Hermes without context.
func TestResumeOrphansRebuildsInitialWhenNoPriorResponse(t *testing.T) {
	h := newHarness(t)
	attID := h.newAttempt()
	// Load the attempt's task to get a known title for the assertion.
	att, _ := h.store.GetAttempt(context.Background(), attID)
	task, _ := h.store.GetTask(context.Background(), att.TaskID)

	if err := h.store.UpdateAttemptState(context.Background(), attID, store.AttemptRunning); err != nil {
		t.Fatalf("seed running: %v", err)
	}
	// Meta has no LatestResponseID, no CurrentRunID — fresh attempt
	// that never finished a turn.
	meta, _ := h.fs.LoadAttemptMeta(attID)
	meta.Session.LatestResponseID = ""
	meta.Session.CurrentRunID = ""
	_ = h.fs.SaveAttemptMeta(meta)

	resumed, failed, err := h.runner.ResumeOrphans(context.Background(), []string{h.username})
	if err != nil {
		t.Fatalf("ResumeOrphans: %v", err)
	}
	if resumed != 1 || failed != 0 {
		t.Fatalf("expected resumed=1 failed=0, got %d/%d", resumed, failed)
	}

	// Check ContinueResumeCount synchronously, before the kicked
	// goroutine can complete its recovery turn (DisconnectCompleted
	// resets the counter back to 0).
	meta2, _ := h.fs.LoadAttemptMeta(attID)
	if meta2.Session.ContinueResumeCount != 1 {
		t.Fatalf("ContinueResumeCount = %d, want 1", meta2.Session.ContinueResumeCount)
	}

	waitForSeen(t, h, 1, 3*time.Second)

	// The fake server doesn't surface the input body — we have to
	// sniff it through fakeHermes. We didn't record input text in the
	// existing fake, so instead assert: (a) previous_response_id is
	// empty (first turn, retry_initial path), and (b) the runner's
	// initial-input format is what Start() uses — a quick sanity on
	// the rebuild helper.
	seen := h.fake.seen()
	if len(seen) != 1 || seen[0] != "" {
		t.Fatalf("retry_initial path should send no previous_response_id, "+
			"got %v", seen)
	}
	rebuilt, ok := h.runner.rebuildInitialInput(context.Background(), h.store, h.fs, att.TaskID, att.ID)
	if !ok {
		t.Fatal("rebuildInitialInput returned !ok for valid task")
	}
	if !strings.Contains(rebuilt, task.Title) {
		t.Fatalf("rebuilt input missing task title %q: %q", task.Title, rebuilt)
	}
	if !strings.HasPrefix(rebuilt, "[tb-") {
		t.Fatalf("rebuilt input missing tb- prefix: %q", rebuilt)
	}
}

// ResumeOrphans — once an attempt has burned AutoResumeMaxRetries of
// auto-continues across successive restarts, give up and mark it
// Failed. Prevents a permanently broken conversation from looping
// forever every time the process cycles.
func TestResumeOrphansFailsWhenRetriesExhausted(t *testing.T) {
	h := newHarness(t)
	attID := h.newAttempt()

	_ = h.store.UpdateAttemptState(context.Background(), attID, store.AttemptRunning)
	meta, _ := h.fs.LoadAttemptMeta(attID)
	meta.Session.LatestResponseID = "resp_something"
	meta.Session.CurrentRunID = ""
	meta.Session.ContinueResumeCount = AutoResumeMaxRetries
	_ = h.fs.SaveAttemptMeta(meta)

	resumed, failed, err := h.runner.ResumeOrphans(context.Background(), []string{h.username})
	if err != nil {
		t.Fatalf("ResumeOrphans: %v", err)
	}
	if resumed != 0 || failed != 1 {
		t.Fatalf("expected resumed=0 failed=1, got %d/%d", resumed, failed)
	}
	att, _ := h.store.GetAttempt(context.Background(), attID)
	if att.State != store.AttemptFailed {
		t.Fatalf("exhausted attempt must be Failed, got %q", att.State)
	}
	// And critically — no Hermes request fired.
	time.Sleep(50 * time.Millisecond)
	if len(h.fake.seen()) != 0 {
		t.Fatalf("exhausted retry must not re-hit Hermes, saw %v", h.fake.seen())
	}
}

// ResumeOrphans — attempts already in a terminal state (Completed /
// Failed / Cancelled) are skipped outright. Guards against the
// orphan scanner mistakenly re-running finished work.
func TestResumeOrphansSkipsTerminalAttempts(t *testing.T) {
	h := newHarness(t)
	attID := h.newAttempt()
	// Drive to a terminal state.
	_ = h.store.UpdateAttemptState(context.Background(), attID, store.AttemptCompleted)

	resumed, failed, err := h.runner.ResumeOrphans(context.Background(), []string{h.username})
	if err != nil {
		t.Fatalf("ResumeOrphans: %v", err)
	}
	if resumed+failed != 0 {
		t.Fatalf("terminal attempts must be skipped, got resumed=%d failed=%d", resumed, failed)
	}
	// Terminal attempts never appear in ListActiveAttempts so the fake
	// server should see nothing.
	time.Sleep(50 * time.Millisecond)
	if len(h.fake.seen()) != 0 {
		t.Fatalf("terminal attempt must not be re-run, saw %v", h.fake.seen())
	}
	// State unchanged.
	att, _ := h.store.GetAttempt(context.Background(), attID)
	if att.State != store.AttemptCompleted {
		t.Fatalf("terminal attempt state perturbed: %q", att.State)
	}
}

// waitForSeen polls the fake server's seen-list until at least n
// requests have landed, or the timeout fires. Used to synchronise
// with the startLoop goroutine that ResumeOrphans dispatches.
func waitForSeen(t *testing.T, h *testHarness, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(h.fake.seen()) >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d fake requests, saw %d (%v)",
		n, len(h.fake.seen()), h.fake.seen())
}
