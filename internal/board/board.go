// Package board holds the task state machine: which transitions are legal
// and which are automatically enacted by the backend vs. triggered by user drag.
package board

import (
	"context"
	"errors"
	"fmt"

	"github.com/ahkimkoo/hermes-taskboard/internal/sse"
	"github.com/ahkimkoo/hermes-taskboard/internal/store"
)

// Legal transitions (graph below). Manual (user-drag) and Auto (backend) are both
// accepted through the same Transition method; kind is recorded for SSE consumers.
//
//   draft   → plan
//   plan    → execute  (auto: scheduler creates Attempt; manual: user drags / clicks Start)
//   execute → verify   (auto: all attempts reached terminal)
//   verify  → execute  (auto: user sent a follow-up in Verify view)
//   verify  → done     (manual)
//   done    → archive  (manual)
//   any     → archive  (manual)
//   archive → delete   (explicit delete)

var validTransitions = map[store.TaskStatus]map[store.TaskStatus]bool{
	// Draft → Execute is permitted because the task-modal "立即执行 /
	// Start now" button skips the Plan column entirely when the user is
	// ready to run immediately. Without it, Runner.Start silently
	// dropped the column move (best-effort), leaving the card stranded
	// in Draft while an attempt was already streaming.
	store.StatusDraft:   {store.StatusPlan: true, store.StatusExecute: true, store.StatusArchive: true},
	store.StatusPlan:    {store.StatusExecute: true, store.StatusDraft: true, store.StatusArchive: true},
	store.StatusExecute: {store.StatusVerify: true, store.StatusArchive: true},
	store.StatusVerify:  {store.StatusDone: true, store.StatusExecute: true, store.StatusArchive: true},
	store.StatusDone:    {store.StatusArchive: true, store.StatusVerify: true},
	store.StatusArchive: {store.StatusDraft: true, store.StatusPlan: true, store.StatusExecute: true, store.StatusVerify: true, store.StatusDone: true},
}

type Kind string

const (
	KindManual Kind = "manual"
	KindAuto   Kind = "auto"
)

// Service encapsulates task state transitions + publishes board events.
type Service struct {
	Store *store.Store
	Hub   *sse.Hub
}

func New(s *store.Store, h *sse.Hub) *Service {
	return &Service{Store: s, Hub: h}
}

// Transition moves a task to `to`; returns error if illegal.
func (s *Service) Transition(ctx context.Context, taskID string, to store.TaskStatus, kind Kind, reason string) error {
	t, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if t.Status == to {
		return nil
	}
	if !validTransitions[t.Status][to] {
		return fmt.Errorf("illegal transition %s → %s", t.Status, to)
	}
	if err := s.Store.SetTaskStatus(ctx, taskID, to); err != nil {
		return err
	}
	s.Hub.Publish("board", sse.Event{Event: "task.moved", Data: map[string]any{
		"task_id": taskID,
		"from":    string(t.Status),
		"to":      string(to),
		"kind":    string(kind),
		"reason":  reason,
	}})
	return nil
}

// MaybeAdvanceAfterAttempt checks whether the task should auto-advance from
// Execute → Verify (all attempts terminal) and does so if appropriate.
func (s *Service) MaybeAdvanceAfterAttempt(ctx context.Context, taskID string) error {
	t, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if t.Status != store.StatusExecute {
		return nil
	}
	done, total, err := s.Store.AllAttemptsTerminal(ctx, taskID)
	if err != nil {
		return err
	}
	if total == 0 {
		return nil
	}
	if done {
		return s.Transition(ctx, taskID, store.StatusVerify, KindAuto, "all_attempts_terminal")
	}
	return nil
}

// ValidateNewTransition exposes legality check without performing the transition.
func ValidateNewTransition(from, to store.TaskStatus) error {
	if from == to {
		return nil
	}
	if !validTransitions[from][to] {
		return errors.New("illegal transition")
	}
	return nil
}
