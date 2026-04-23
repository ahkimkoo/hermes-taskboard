// Package board holds the task state machine: which transitions are
// legal and which are automatically enacted by the backend vs.
// triggered by user drag. Stateless — every method takes the per-user
// *store.Store so the board module doesn't need to know about the
// multi-user routing layer.
package board

import (
	"context"
	"errors"
	"fmt"

	"github.com/ahkimkoo/hermes-taskboard/internal/sse"
	"github.com/ahkimkoo/hermes-taskboard/internal/store"
)

var validTransitions = map[store.TaskStatus]map[store.TaskStatus]bool{
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

// Service is a stateless façade over the state machine + SSE hub.
type Service struct {
	Hub *sse.Hub
}

func New(h *sse.Hub) *Service {
	return &Service{Hub: h}
}

// Transition moves a task to `to` inside the caller's per-user store.
func (s *Service) Transition(ctx context.Context, st *store.Store, taskID string, to store.TaskStatus, kind Kind, reason string) error {
	t, err := st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if t.Status == to {
		return nil
	}
	if !validTransitions[t.Status][to] {
		return fmt.Errorf("illegal transition %s → %s", t.Status, to)
	}
	if err := st.SetTaskStatus(ctx, taskID, to); err != nil {
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

// MaybeAdvanceAfterAttempt bumps a task from Execute → Verify when
// every attempt has reached a terminal state.
func (s *Service) MaybeAdvanceAfterAttempt(ctx context.Context, st *store.Store, taskID string) error {
	t, err := st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if t.Status != store.StatusExecute {
		return nil
	}
	done, total, err := st.AllAttemptsTerminal(ctx, taskID)
	if err != nil {
		return err
	}
	if total == 0 {
		return nil
	}
	if done {
		return s.Transition(ctx, st, taskID, store.StatusVerify, KindAuto, "all_attempts_terminal")
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
