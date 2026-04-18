package store

import "time"

type TaskStatus string

const (
	StatusDraft   TaskStatus = "draft"
	StatusPlan    TaskStatus = "plan"
	StatusExecute TaskStatus = "execute"
	StatusVerify  TaskStatus = "verify"
	StatusDone    TaskStatus = "done"
	StatusArchive TaskStatus = "archive"
)

type TriggerMode string

const (
	TriggerAuto   TriggerMode = "auto"
	TriggerManual TriggerMode = "manual"
)

type AttemptState string

const (
	AttemptQueued     AttemptState = "queued"
	AttemptRunning    AttemptState = "running"
	AttemptNeedsInput AttemptState = "needs_input"
	AttemptCompleted  AttemptState = "completed"
	AttemptFailed     AttemptState = "failed"
	AttemptCancelled  AttemptState = "cancelled"
)

// TaskDep is a single dependency edge: the current task won't be dispatched
// until the target task reaches at least the given state.
//
//   required_state="verify" — satisfied once target enters verify (i.e. its
//                             attempts finished; user hasn't accepted yet).
//                             Dependents can start already.
//   required_state="done"   — target must be Done (human-accepted) or Archive.
type TaskDep struct {
	TaskID        string `json:"task_id"`
	RequiredState string `json:"required_state"` // "verify" | "done" (default "done")
}

// Task is the SQL row + enriched tags/deps (description loaded separately via fsstore).
type Task struct {
	ID                 string      `json:"id"`
	Title              string      `json:"title"`
	Description        string      `json:"description,omitempty"` // loaded from fsstore
	Status             TaskStatus  `json:"status"`
	Priority           int         `json:"priority"`
	TriggerMode        TriggerMode `json:"trigger_mode"`
	PreferredServer    string      `json:"preferred_server,omitempty"`
	PreferredModel     string      `json:"preferred_model,omitempty"`
	Position           int64       `json:"position"` // user-controlled order within a status column
	CreatedAt          time.Time   `json:"created_at"`
	UpdatedAt          time.Time   `json:"updated_at"`
	DescriptionExcerpt string      `json:"description_excerpt,omitempty"`
	Tags                []string  `json:"tags"`
	Dependencies        []TaskDep `json:"dependencies"`
	AttemptCount        int       `json:"attempt_count"`
	ActiveAttempts      int       `json:"active_attempts"`       // queued + running + needs_input
	NeedsInputAttempts  int       `json:"needs_input_attempts"`  // subset of ActiveAttempts
}

type Tag struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Attempt mirrors the DB row.
type Attempt struct {
	ID        string       `json:"id"`
	TaskID    string       `json:"task_id"`
	ServerID  string       `json:"server_id"`
	Model     string       `json:"model"`
	State     AttemptState `json:"state"`
	StartedAt *time.Time   `json:"started_at,omitempty"`
	EndedAt   *time.Time   `json:"ended_at,omitempty"`
}

// AttemptMeta is the JSON stored at data/attempt/{id}/meta.json.
type AttemptMeta struct {
	ID       string          `json:"id"`
	TaskID   string          `json:"task_id"`
	ServerID string          `json:"server_id"`
	Model    string          `json:"model"`
	Session  AttemptSession  `json:"session"`
	Summary  string          `json:"summary,omitempty"`
}

type AttemptSession struct {
	ConversationID   string   `json:"conversation_id"`
	Runs             []string `json:"runs"`
	CurrentRunID     string   `json:"current_run_id,omitempty"`
	LatestResponseID string   `json:"latest_response_id,omitempty"`
}
