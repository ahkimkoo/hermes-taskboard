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
// Ownership is implicit — tasks live in a per-user DB, so every row in the
// local DB is owned by the same user. No owner_id column.
type Task struct {
	ID                 string      `json:"id"`
	Title              string      `json:"title"`
	Description        string      `json:"description,omitempty"` // loaded from fsstore
	Status             TaskStatus  `json:"status"`
	Priority           int         `json:"priority"`
	TriggerMode        TriggerMode `json:"trigger_mode"`
	PreferredServer    string      `json:"preferred_server,omitempty"`
	PreferredModel     string      `json:"preferred_model,omitempty"`
	Position           int64       `json:"position"`
	CreatedAt          time.Time   `json:"created_at"`
	UpdatedAt          time.Time   `json:"updated_at"`
	DescriptionExcerpt string      `json:"description_excerpt,omitempty"`
	Tags                []string  `json:"tags"`
	Dependencies        []TaskDep `json:"dependencies"`
	AttemptCount        int       `json:"attempt_count"`
	ActiveAttempts      int       `json:"active_attempts"`       // queued + running + needs_input
	NeedsInputAttempts  int       `json:"needs_input_attempts"`  // subset of ActiveAttempts
}

// ScheduleKind is retained for DB column symmetry but only one value is
// accepted now: "cron" (standard 5-field min hour dom month dow, local time).
// Legacy "interval" rows are rewritten to cron at migration time.
type ScheduleKind string

const (
	ScheduleCron ScheduleKind = "cron"
)

type Schedule struct {
	ID        string       `json:"id"`
	TaskID    string       `json:"task_id"`
	Kind      ScheduleKind `json:"kind"`
	Spec      string       `json:"spec"`
	Note      string       `json:"note,omitempty"`
	Enabled   bool         `json:"enabled"`
	LastRunAt *time.Time   `json:"last_run_at,omitempty"`
	NextRunAt *time.Time   `json:"next_run_at,omitempty"`
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

// AttemptMeta is the JSON stored at data/{username}/attempt/{id}/meta.json.
type AttemptMeta struct {
	ID       string          `json:"id"`
	TaskID   string          `json:"task_id"`
	ServerID string          `json:"server_id"`
	Model    string          `json:"model"`
	Session  AttemptSession  `json:"session"`
	Summary  string          `json:"summary,omitempty"`
}

// DisconnectReason classifies why a Hermes SSE stream stopped. Stored
// on meta.session so the auto-resumer can decide whether to retry.
type DisconnectReason string

const (
	DisconnectCompleted DisconnectReason = "completed"  // clean end after response.completed
	DisconnectCancelled DisconnectReason = "cancelled"  // user hit Stop (ctx cancelled)
	DisconnectAbnormal  DisconnectReason = "abnormal"   // network drop, 5xx, taskboard crash, …
)

type AttemptSession struct {
	ConversationID   string   `json:"conversation_id"`
	Runs             []string `json:"runs"`
	CurrentRunID     string   `json:"current_run_id,omitempty"`
	LatestResponseID string   `json:"latest_response_id,omitempty"`
	// LastDisconnectReason records how the most recent turn's SSE
	// stream ended. Abnormal + attempt still running means the auto-
	// resumer should send a `continue` message (with rate-limiting).
	LastDisconnectReason DisconnectReason `json:"last_disconnect_reason,omitempty"`
	LastDisconnectAt     int64            `json:"last_disconnect_at,omitempty"`
	// ContinueResumeCount is the number of synthetic "continue" messages
	// the auto-resumer has sent to this attempt since the last clean
	// response.completed. Capped (see scheduler/reaper) so a wedged
	// attempt can't loop forever. Reset to 0 when (a) we see
	// response.completed, (b) the user sends a message via the UI.
	ContinueResumeCount int   `json:"continue_resume_count,omitempty"`
	LastContinueAt      int64 `json:"last_continue_at,omitempty"`
}
