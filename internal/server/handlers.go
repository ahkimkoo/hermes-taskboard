package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ahkimkoo/hermes-taskboard/internal/attempt"
	"github.com/ahkimkoo/hermes-taskboard/internal/auth"
	"github.com/ahkimkoo/hermes-taskboard/internal/board"
	"github.com/ahkimkoo/hermes-taskboard/internal/config"
	cronpkg "github.com/ahkimkoo/hermes-taskboard/internal/cron"
	"github.com/ahkimkoo/hermes-taskboard/internal/sse"
	"github.com/ahkimkoo/hermes-taskboard/internal/store"
	"github.com/ahkimkoo/hermes-taskboard/internal/store/fsstore"
)

func contextWithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}

// ---------------- tasks ----------------

func (s *Server) hListTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.TaskFilter{
		Status: q.Get("status"),
		Tag:    q.Get("tag"),
		Query:  q.Get("q"),
		Server: q.Get("server"),
		Model:  q.Get("model"),
	}
	list, err := s.Store.ListTasks(r.Context(), f)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"tasks": list})
}

type createTaskReq struct {
	Title           string            `json:"title"`
	Description     string            `json:"description,omitempty"`
	Priority        int               `json:"priority,omitempty"`
	Status          string            `json:"status,omitempty"`
	TriggerMode     string            `json:"trigger_mode,omitempty"`
	PreferredServer string            `json:"preferred_server,omitempty"`
	PreferredModel  string            `json:"preferred_model,omitempty"`
	Tags            []string          `json:"tags,omitempty"`
	Dependencies    []json.RawMessage `json:"dependencies,omitempty"` // []string or []{task_id,required_state}
}

// normalizeDeps converts mixed string / object entries into the canonical TaskDep form.
// Legacy payloads that pass a bare string are assumed to mean required_state="done".
func normalizeDeps(raws []json.RawMessage) []store.TaskDep {
	out := make([]store.TaskDep, 0, len(raws))
	for _, raw := range raws {
		if len(raw) == 0 {
			continue
		}
		// Try object first.
		var obj struct {
			TaskID        string `json:"task_id"`
			RequiredState string `json:"required_state"`
		}
		if err := json.Unmarshal(raw, &obj); err == nil && obj.TaskID != "" {
			if obj.RequiredState == "" {
				obj.RequiredState = "done"
			}
			out = append(out, store.TaskDep{TaskID: obj.TaskID, RequiredState: obj.RequiredState})
			continue
		}
		// Fall back to string.
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			out = append(out, store.TaskDep{TaskID: s, RequiredState: "done"})
		}
	}
	return out
}

func (s *Server) hCreateTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		writeErr(w, 400, errors.New("title required"))
		return
	}
	priority := req.Priority
	if priority == 0 {
		priority = 3
	}
	trig := store.TriggerMode(req.TriggerMode)
	if trig == "" {
		trig = store.TriggerAuto
	}
	status := store.TaskStatus(req.Status)
	if status == "" {
		status = store.StatusDraft
	}
	t := &store.Task{
		ID:                 uuid.NewString(),
		Title:              req.Title,
		Status:             status,
		Priority:           priority,
		TriggerMode:        trig,
		PreferredServer:    req.PreferredServer,
		PreferredModel:     req.PreferredModel,
		Tags:               req.Tags,
		Dependencies:       normalizeDeps(req.Dependencies),
		DescriptionExcerpt: truncExcerpt(req.Description, 200),
	}
	if err := s.Store.CreateTask(r.Context(), t); err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := s.FS.SaveTaskDoc(&fsstore.TaskDoc{ID: t.ID, Description: req.Description, Tags: req.Tags}); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.Hub.Publish("board", toEvent("task.created", map[string]any{"task_id": t.ID}))
	t.Description = req.Description
	writeJSON(w, 201, map[string]any{"task": t})
}

// routeTasks handles /api/tasks/{id}[/subpath]
func (s *Server) routeTasks(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	switch {
	case len(parts) == 1:
		switch r.Method {
		case "GET":
			s.hGetTask(w, r, id)
		case "PATCH":
			s.hPatchTask(w, r, id)
		case "DELETE":
			s.hDeleteTask(w, r, id)
		default:
			http.Error(w, "method not allowed", 405)
		}
	case len(parts) == 2 && parts[1] == "transition":
		s.hTransition(w, r, id)
	case len(parts) == 2 && parts[1] == "attempts":
		if r.Method == "GET" {
			s.hListTaskAttempts(w, r, id)
		} else {
			s.hStartAttempt(w, r, id)
		}
	case len(parts) == 2 && parts[1] == "schedules":
		switch r.Method {
		case "GET":
			s.hListTaskSchedules(w, r, id)
		case "POST":
			s.hCreateSchedule(w, r, id)
		default:
			http.Error(w, "method not allowed", 405)
		}
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) hGetTask(w http.ResponseWriter, r *http.Request, id string) {
	t, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, 404, err)
		} else {
			writeErr(w, 500, err)
		}
		return
	}
	if doc, _ := s.FS.LoadTaskDoc(id); doc != nil {
		t.Description = doc.Description
	}
	writeJSON(w, 200, map[string]any{"task": t})
}

type patchTaskReq struct {
	Title           *string            `json:"title,omitempty"`
	Description     *string            `json:"description,omitempty"`
	Priority        *int               `json:"priority,omitempty"`
	TriggerMode     *string            `json:"trigger_mode,omitempty"`
	PreferredServer *string            `json:"preferred_server,omitempty"`
	PreferredModel  *string            `json:"preferred_model,omitempty"`
	Tags            *[]string          `json:"tags,omitempty"`
	Dependencies    *[]json.RawMessage `json:"dependencies,omitempty"`
}

func (s *Server) hPatchTask(w http.ResponseWriter, r *http.Request, id string) {
	var req patchTaskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	t, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if req.Title != nil {
		t.Title = *req.Title
	}
	if req.Priority != nil {
		t.Priority = *req.Priority
	}
	if req.TriggerMode != nil {
		t.TriggerMode = store.TriggerMode(*req.TriggerMode)
	}
	if req.PreferredServer != nil {
		t.PreferredServer = *req.PreferredServer
	}
	if req.PreferredModel != nil {
		t.PreferredModel = *req.PreferredModel
	}
	if req.Tags != nil {
		t.Tags = *req.Tags
	}
	if req.Dependencies != nil {
		t.Dependencies = normalizeDeps(*req.Dependencies)
	}
	if req.Description != nil {
		t.DescriptionExcerpt = truncExcerpt(*req.Description, 200)
	}
	if err := s.Store.UpdateTask(r.Context(), t); err != nil {
		writeErr(w, 500, err)
		return
	}
	if req.Description != nil {
		doc, _ := s.FS.LoadTaskDoc(id)
		if doc == nil {
			doc = &fsstore.TaskDoc{ID: id}
		}
		doc.Description = *req.Description
		_ = s.FS.SaveTaskDoc(doc)
		t.Description = *req.Description
	}
	s.Hub.Publish("board", toEvent("task.updated", map[string]any{"task_id": id}))
	writeJSON(w, 200, map[string]any{"task": t})
}

func (s *Server) hDeleteTask(w http.ResponseWriter, r *http.Request, id string) {
	ids, err := s.Store.DeleteTask(r.Context(), id)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	_ = s.FS.DeleteTask(id)
	for _, aid := range ids {
		_ = s.FS.DeleteAttempt(aid)
	}
	s.Hub.Publish("board", toEvent("task.deleted", map[string]any{"task_id": id}))
	writeJSON(w, 200, map[string]any{"ok": true})
}

type transitionReq struct {
	To       string `json:"to"`
	Reason   string `json:"reason,omitempty"`
	AfterID  string `json:"after_id,omitempty"`  // drop target: place right after this card
	BeforeID string `json:"before_id,omitempty"` // drop target: place right before this card
}

func (s *Server) hTransition(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req transitionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	to := store.TaskStatus(req.To)
	// Special-cases: Plan → Execute via manual drag = same as hitting Start.
	if to == store.StatusExecute {
		t, err := s.Store.GetTask(r.Context(), id)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		if t.Status == store.StatusPlan || t.Status == store.StatusDraft {
			if _, err := s.Runner.Start(r.Context(), id, "", ""); err != nil {
				if ce, ok := err.(*attempt.ConcurrencyErr); ok {
					writeJSON(w, 409, map[string]any{"code": "concurrency_limit", "level": ce.Level})
					return
				}
				writeErr(w, 500, err)
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
	// If caller passed after_id/before_id (drag-within-or-across columns with a
	// specific drop slot), use MoveTask so ordering is preserved exactly.
	if req.AfterID != "" || req.BeforeID != "" {
		if err := s.Store.MoveTask(r.Context(), id, to, req.AfterID, req.BeforeID); err != nil {
			writeErr(w, 400, err)
			return
		}
		s.Hub.Publish("board", toEvent("task.moved", map[string]any{"task_id": id, "to": req.To, "kind": "manual"}))
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	if err := s.Board.Transition(r.Context(), id, to, board.KindManual, req.Reason); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

type startAttemptReq struct {
	ServerID string `json:"server_id,omitempty"`
	Model    string `json:"model,omitempty"`
}

func (s *Server) hListTaskAttempts(w http.ResponseWriter, r *http.Request, id string) {
	atts, err := s.Store.ListAttemptsForTask(r.Context(), id)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"attempts": atts})
}

func (s *Server) hStartAttempt(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req startAttemptReq
	_ = json.NewDecoder(r.Body).Decode(&req)
	att, err := s.Runner.Start(r.Context(), id, req.ServerID, req.Model)
	if err != nil {
		if ce, ok := err.(*attempt.ConcurrencyErr); ok {
			writeJSON(w, 409, map[string]any{"code": "concurrency_limit", "level": ce.Level})
			return
		}
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 201, map[string]any{"attempt": att})
}

// ---------------- attempts ----------------

func (s *Server) routeAttempts(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/attempts/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	switch {
	case len(parts) == 1:
		if r.Method != "GET" {
			http.Error(w, "method not allowed", 405)
			return
		}
		s.hGetAttempt(w, r, id)
	case parts[1] == "messages":
		s.hAttemptMessages(w, r, id)
	case parts[1] == "cancel":
		s.hAttemptCancel(w, r, id)
	case parts[1] == "events":
		s.hAttemptEvents(w, r, id)
	case parts[1] == "reconnect":
		s.hAttemptReconnect(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

// hAttemptReconnect is invoked by the UI when the user scrolls the task
// modal to the bottom. It asks the Runner to open a fresh SSE subscription
// to Hermes for any recorded run — useful for stale / terminal attempts
// we might have transitioned to failed prematurely. If the attempt is
// already owned by a live runCtx this is a no-op (returns `already_live`).
func (s *Server) hAttemptReconnect(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	status, err := s.Runner.TryReconnect(r.Context(), id)
	if err != nil {
		writeJSON(w, 200, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"status": status})
}

func (s *Server) hGetAttempt(w http.ResponseWriter, r *http.Request, id string) {
	att, err := s.Store.GetAttempt(r.Context(), id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	meta, _ := s.FS.LoadAttemptMeta(id)
	writeJSON(w, 200, map[string]any{"attempt": att, "meta": meta})
}

func (s *Server) hAttemptMessages(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case "POST":
		var req struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
			writeErr(w, 400, errors.New("text required"))
			return
		}
		if err := s.Runner.SendMessage(r.Context(), id, req.Text); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 202, map[string]any{"ok": true})
	case "GET":
		q := r.URL.Query()
		// tail=N or before_seq=X & limit=
		if bs := q.Get("before_seq"); bs != "" {
			before, _ := strconv.ParseUint(bs, 10, 64)
			limit, _ := strconv.Atoi(q.Get("limit"))
			if limit <= 0 {
				limit = 20
			}
			events, err := s.FS.ReadEventsBefore(id, before, limit)
			if err != nil {
				writeErr(w, 500, err)
				return
			}
			writeJSON(w, 200, map[string]any{"events": events})
			return
		}
		tail := 5
		if t := q.Get("tail"); t != "" {
			tail, _ = strconv.Atoi(t)
		}
		events, err := s.FS.ReadEventsTail(id, tail*10) // 10 events per "message" approx
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"events": events})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) hAttemptCancel(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if err := s.Runner.Cancel(r.Context(), id); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) hAttemptEvents(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	q := r.URL.Query()
	// `tail=N` returns the last N events (ascending). When combined with
	// `before_seq=S`, returns the last N events whose seq is strictly below
	// S — that's the "load earlier" pagination the UI uses to page back
	// through a long event log without shipping the whole thing on open.
	if t := q.Get("tail"); t != "" {
		n, _ := strconv.Atoi(t)
		if n <= 0 {
			n = 500
		}
		if bs := q.Get("before_seq"); bs != "" {
			before, _ := strconv.ParseUint(bs, 10, 64)
			events, err := s.FS.ReadEventsBefore(id, before, n)
			if err != nil {
				writeErr(w, 500, err)
				return
			}
			writeJSON(w, 200, map[string]any{"events": events})
			return
		}
		events, err := s.FS.ReadEventsTail(id, n)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"events": events})
		return
	}
	since, _ := strconv.ParseUint(q.Get("since_seq"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	events, err := s.FS.ReadEventsRange(id, since, limit)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"events": events})
}

// ---------------- SSE streams ----------------

func (s *Server) hStreamBoard(w http.ResponseWriter, r *http.Request) {
	s.streamTopic(w, r, "board", 0)
}

func (s *Server) hStreamAttempt(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/stream/attempt/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	var since uint64
	if v := r.URL.Query().Get("since_seq"); v != "" {
		since, _ = strconv.ParseUint(v, 10, 64)
	}
	if leid := r.Header.Get("Last-Event-ID"); leid != "" {
		if v, err := strconv.ParseUint(leid, 10, 64); err == nil {
			since = v
		}
	}
	s.streamTopic(w, r, "attempt:"+id, since)
}

func (s *Server) streamTopic(w http.ResponseWriter, r *http.Request, topic string, since uint64) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}

	// For attempt topics with `since`, replay history from disk first.
	if strings.HasPrefix(topic, "attempt:") && since > 0 {
		attID := strings.TrimPrefix(topic, "attempt:")
		events, _ := s.FS.ReadEventsRange(attID, since, 2000)
		for _, e := range events {
			seq := uint64(0)
			if v, ok := e["seq"].(float64); ok {
				seq = uint64(v)
			}
			writeSSE(w, seq, "event", e)
		}
		flusher.Flush()
	}

	ch, unsub := s.Hub.Subscribe(topic)
	defer unsub()

	// keepalive
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	// initial comment to establish connection
	fmt.Fprintln(w, ":ok")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, ev.Seq, ev.Event, ev.Data)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprintln(w, ":ping")
			flusher.Flush()
		}
	}
}

// writeSSE emits a Server-Sent Events frame. We intentionally DO NOT emit the
// `event:` header: a named event is only delivered to matching
// addEventListener(name) handlers and never to onmessage, which was silently
// starving our generic board subscriber. Instead we inline the event name into
// the JSON payload (key "event") so the frontend's onmessage handler can
// discriminate from a single stream.
//
// IMPORTANT: attempt-stream frames already carry their own `event` discriminator
// inside data (e.g. "user_message", "run_start", emitted by AttemptRunner).
// We must not overwrite that — only fill in `event` when data didn't set it.
func writeSSE(w http.ResponseWriter, seq uint64, event string, data map[string]any) {
	if seq > 0 {
		fmt.Fprintf(w, "id: %d\n", seq)
	}
	payload := make(map[string]any, len(data)+1)
	for k, v := range data {
		payload[k] = v
	}
	if event != "" {
		if _, alreadySet := payload["event"]; !alreadySet {
			payload["event"] = event
		}
	}
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

// ---------------- servers (hermes) ----------------

type serverDTO struct {
	ID            string                `json:"id"`
	Name          string                `json:"name"`
	BaseURL       string                `json:"base_url"`
	HasAPIKey     bool                  `json:"has_api_key"`
	IsDefault     bool                  `json:"is_default"`
	MaxConcurrent int                   `json:"max_concurrent"`
	Models        []config.HermesModel  `json:"models"`
}

func (s *Server) hListServers(w http.ResponseWriter, r *http.Request) {
	cur := s.Cfg.Snapshot()
	out := []serverDTO{}
	for _, sv := range cur.HermesServers {
		out = append(out, serverDTO{
			ID: sv.ID, Name: sv.Name, BaseURL: sv.BaseURL,
			HasAPIKey:  sv.APIKey != "" || sv.APIKeyEnc != "",
			IsDefault:  sv.IsDefault,
			MaxConcurrent: sv.MaxConcurrent,
			Models:     sv.Models,
		})
	}
	writeJSON(w, 200, map[string]any{"servers": out})
}

type serverUpsertReq struct {
	ID            string               `json:"id"`
	Name          string               `json:"name"`
	BaseURL       string               `json:"base_url"`
	APIKey        string               `json:"api_key,omitempty"`
	IsDefault     bool                 `json:"is_default"`
	MaxConcurrent int                  `json:"max_concurrent,omitempty"`
	Models        []config.HermesModel `json:"models,omitempty"`
}

func (s *Server) hCreateServer(w http.ResponseWriter, r *http.Request) {
	var req serverUpsertReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if req.ID == "" || req.BaseURL == "" {
		writeErr(w, 400, errors.New("id and base_url required"))
		return
	}
	err := s.Cfg.Mutate(func(c *config.Config) error {
		for i := range c.HermesServers {
			if c.HermesServers[i].ID == req.ID {
				return errors.New("id already exists")
			}
		}
		if req.IsDefault {
			for i := range c.HermesServers {
				c.HermesServers[i].IsDefault = false
			}
		}
		ns := config.HermesServer{
			ID: req.ID, Name: req.Name, BaseURL: req.BaseURL,
			APIKey: req.APIKey, IsDefault: req.IsDefault,
			MaxConcurrent: req.MaxConcurrent,
			Models: req.Models,
		}
		c.HermesServers = append(c.HermesServers, ns)
		return nil
	})
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, map[string]any{"ok": true})
}

func (s *Server) routeServers(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/servers/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	switch {
	case len(parts) == 1:
		switch r.Method {
		case "PATCH":
			s.hUpdateServer(w, r, id)
		case "DELETE":
			s.hDeleteServer(w, r, id)
		default:
			http.Error(w, "method not allowed", 405)
		}
	case parts[1] == "test":
		s.hTestServer(w, r, id)
	case parts[1] == "models":
		s.hServerModels(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) hUpdateServer(w http.ResponseWriter, r *http.Request, id string) {
	var req serverUpsertReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	err := s.Cfg.Mutate(func(c *config.Config) error {
		found := false
		for i := range c.HermesServers {
			if c.HermesServers[i].ID != id {
				continue
			}
			found = true
			sv := &c.HermesServers[i]
			if req.Name != "" {
				sv.Name = req.Name
			}
			if req.BaseURL != "" {
				sv.BaseURL = req.BaseURL
			}
			if req.APIKey != "" {
				sv.APIKey = req.APIKey
				sv.APIKeyEnc = ""
			}
			if req.MaxConcurrent != 0 {
				sv.MaxConcurrent = req.MaxConcurrent
			}
			if req.Models != nil {
				sv.Models = req.Models
			}
			if req.IsDefault {
				for j := range c.HermesServers {
					c.HermesServers[j].IsDefault = false
				}
				sv.IsDefault = true
			}
		}
		if !found {
			return errors.New("not found")
		}
		return nil
	})
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) hDeleteServer(w http.ResponseWriter, r *http.Request, id string) {
	err := s.Cfg.Mutate(func(c *config.Config) error {
		idx := -1
		for i := range c.HermesServers {
			if c.HermesServers[i].ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return errors.New("not found")
		}
		c.HermesServers = append(c.HermesServers[:idx], c.HermesServers[idx+1:]...)
		return nil
	})
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) hTestServer(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	client, err := s.Pool.Get(id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	ctx, cancel := contextWithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	h, err := client.Health(ctx)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	models, err := client.Models(ctx)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": true, "health": h, "models_error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": h.OK, "health": h, "models": models})
}

func (s *Server) hServerModels(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	client, err := s.Pool.Get(id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	ctx, cancel := contextWithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	ms, err := client.Models(ctx)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	writeJSON(w, 200, map[string]any{"models": ms})
}

// ---------------- tags ----------------

func (s *Server) hListTags(w http.ResponseWriter, r *http.Request) {
	tags, err := s.Store.ListTags(r.Context())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"tags": tags})
}

func (s *Server) hUpsertTag(w http.ResponseWriter, r *http.Request) {
	var t store.Tag
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		writeErr(w, 400, err)
		return
	}
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		writeErr(w, 400, errors.New("tag name required"))
		return
	}
	if err := s.Store.UpsertTag(r.Context(), t); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) hDeleteTag(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" {
		http.Error(w, "method not allowed", 405)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/tags/")
	if err := s.Store.DeleteTag(r.Context(), name); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------- settings / preferences ----------------

func (s *Server) hGetSettings(w http.ResponseWriter, r *http.Request) {
	c := s.Cfg.Snapshot()
	ossOut := c.OSS
	ossOut.AccessKeySecret = ""       // never leak
	ossOut.AccessKeySecretEnc = ""    // never leak
	hasSecret := c.OSS.AccessKeySecret != "" || c.OSS.AccessKeySecretEnc != ""
	writeJSON(w, 200, map[string]any{
		"scheduler":        c.Scheduler,
		"archive":          c.Archive,
		"server":           c.Server,
		"oss":              ossOut,
		"oss_has_secret":   hasSecret,
	})
}

type settingsReq struct {
	Scheduler *config.Scheduler `json:"scheduler,omitempty"`
	Archive   *config.Archive   `json:"archive,omitempty"`
	Server    *config.Server    `json:"server,omitempty"`
	OSS       *config.OSS       `json:"oss,omitempty"`
}

func (s *Server) hUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	err := s.Cfg.Mutate(func(c *config.Config) error {
		if req.Scheduler != nil {
			c.Scheduler = *req.Scheduler
		}
		if req.Archive != nil {
			c.Archive = *req.Archive
		}
		if req.Server != nil {
			c.Server = *req.Server
		}
		if req.OSS != nil {
			// Preserve existing encrypted secret if caller didn't send a new plaintext.
			prev := c.OSS
			c.OSS = *req.OSS
			if req.OSS.AccessKeySecret == "" {
				c.OSS.AccessKeySecret = ""
				c.OSS.AccessKeySecretEnc = prev.AccessKeySecretEnc
			}
		}
		return nil
	})
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) hGetPreferences(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"preferences": s.Cfg.Snapshot().Preferences})
}

func (s *Server) hUpdatePreferences(w http.ResponseWriter, r *http.Request) {
	var p config.Preferences
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, 400, err)
		return
	}
	err := s.Cfg.Mutate(func(c *config.Config) error {
		c.Preferences = p
		return nil
	})
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.Hub.Publish("board", toEvent("preferences_updated", map[string]any{}))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) hGetConfig(w http.ResponseWriter, r *http.Request) {
	c := s.Cfg.Snapshot()
	// sanitize: strip secrets
	sanitized := *c
	sanitized.Auth.PasswordHash = ""
	sanitized.Auth.SessionSecret = ""
	clean := make([]config.HermesServer, len(c.HermesServers))
	for i, sv := range c.HermesServers {
		clean[i] = sv
		clean[i].APIKey = ""
		clean[i].APIKeyEnc = ""
	}
	sanitized.HermesServers = clean
	writeJSON(w, 200, map[string]any{"config": sanitized})
}

func (s *Server) hReloadConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if err := s.Cfg.Reload(); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------- auth handlers ----------------

func (s *Server) hAuthStatus(w http.ResponseWriter, r *http.Request) {
	c := s.Cfg.Snapshot()
	logged := true
	if c.Auth.Enabled {
		cookie, err := r.Cookie(auth.CookieName)
		logged = err == nil && s.Auth.Valid(cookie.Value)
	}
	writeJSON(w, 200, map[string]any{"enabled": c.Auth.Enabled, "logged_in": logged, "username": c.Auth.Username})
}

func (s *Server) hAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct{ Username, Password string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	token, exp, err := s.Auth.Login(req.Username, req.Password)
	if err != nil {
		writeErr(w, 401, err)
		return
	}
	auth.WriteCookie(w, token, exp, auth.IsSecureRequest(r))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) hAuthLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	auth.ClearCookie(w)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) hAuthEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct{ Username, Password string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.Auth.Enable(req.Username, req.Password); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) hAuthDisable(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct{ Password string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.Auth.Disable(req.Password); err != nil {
		writeErr(w, 400, err)
		return
	}
	auth.ClearCookie(w)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) hAuthChange(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.Auth.ChangePassword(req.OldPassword, req.NewPassword); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------- schedules ----------------

type scheduleReq struct {
	Kind    string `json:"kind"`    // accepted: "cron" (empty defaults to "cron")
	Spec    string `json:"spec"`    // 5-field cron expression
	Note    string `json:"note,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

func (s *Server) hListTaskSchedules(w http.ResponseWriter, r *http.Request, taskID string) {
	list, err := s.Store.ListSchedulesForTask(r.Context(), taskID)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"schedules": list})
}

func (s *Server) hCreateSchedule(w http.ResponseWriter, r *http.Request, taskID string) {
	var req scheduleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	kind := store.ScheduleKind(strings.ToLower(strings.TrimSpace(req.Kind)))
	if kind == "" {
		kind = store.ScheduleCron
	}
	if kind != store.ScheduleCron {
		writeErr(w, 400, errors.New("only kind='cron' is supported"))
		return
	}
	sch := store.Schedule{
		ID:      uuid.NewString(),
		TaskID:  taskID,
		Kind:    kind,
		Spec:    strings.TrimSpace(req.Spec),
		Note:    req.Note,
		Enabled: true,
	}
	if req.Enabled != nil {
		sch.Enabled = *req.Enabled
	}
	if err := cronpkg.Compute(&sch, time.Now()); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.Store.CreateSchedule(r.Context(), &sch); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 201, map[string]any{"schedule": sch})
}

// routeSchedules handles /api/schedules/{id} — PATCH toggles the enabled
// flag; DELETE removes the row. Changing kind/spec is done via DELETE + POST
// to avoid needing to re-derive next_run_at relative to an unknown base.
func (s *Server) routeSchedules(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/schedules/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case "DELETE":
		if err := s.Store.DeleteSchedule(r.Context(), id); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case "PATCH":
		var req struct {
			Enabled *bool `json:"enabled,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		if req.Enabled == nil {
			writeErr(w, 400, errors.New("only enabled toggle is supported"))
			return
		}
		existing, err := s.Store.GetSchedule(r.Context(), id)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		if err := s.Store.SetScheduleEnabled(r.Context(), id, *req.Enabled); err != nil {
			writeErr(w, 500, err)
			return
		}
		// When re-enabling, recompute next_run_at from now so it actually fires.
		if *req.Enabled {
			existing.Enabled = true
			if err := cronpkg.Compute(existing, time.Now()); err == nil {
				_ = s.Store.UpdateSchedule(r.Context(), existing)
			}
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// ---------------- uploads ----------------

// hUploadFile accepts a multipart form file named "file" and returns {url}.
// Size limit: 50 MB. Accepted: images (image/*), audio/video (audio/*,
// video/*) — explicitly mp4/mov/avi/mp3/wav — and common documents (pdf,
// txt, md, doc/docx, xls/xlsx, ppt/pptx) verified by MIME prefix and
// filename extension fallback.
//
// We refuse uploads outright unless Aliyun OSS is configured: Hermes forwards
// the task description verbatim as text to its LLM provider, so a
// locally-hosted URL can't be fetched by the LLM. Without OSS there's
// literally no way for the file to reach the model — we fail loud instead
// of silently saving bytes nobody will see.
func (s *Server) hUploadFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	cur := s.Cfg.Snapshot()
	ossOK := cur.OSS.Enabled && cur.OSS.AccessKeyID != "" && (cur.OSS.AccessKeySecret != "" || cur.OSS.AccessKeySecretEnc != "")
	if !ossOK {
		writeJSON(w, 503, map[string]any{
			"code":    "upload_disabled",
			"message": "uploads require a reachable file host (Aliyun OSS) in Settings → Integrations",
		})
		return
	}
	const maxUpload = 50 << 20 // 50 MB
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		writeErr(w, 400, err)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	defer file.Close()
	ct := hdr.Header.Get("Content-Type")
	if !uploadTypeAllowed(ct, hdr.Filename) {
		writeErr(w, 400, errors.New("file type not allowed; accepted: image/audio/video and pdf/doc/docx/xls/xlsx/ppt/pptx/txt/md"))
		return
	}
	buf := make([]byte, 0, hdr.Size)
	tmp := make([]byte, 32*1024)
	for {
		n, err := file.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	svc := s.uploadsService()
	ctx, cancel := contextWithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	url, err := svc.Put(ctx, buf, ct, hdr.Filename)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	kind := "local"
	if svc.OSSEnabled {
		kind = "oss"
	}
	writeJSON(w, 200, map[string]any{"url": url, "size": len(buf), "storage": kind})
}

// uploadTypeAllowed checks the upload allowlist via MIME prefix first then
// filename extension as a fallback (browsers sometimes report empty MIME
// for office documents picked from older OSes). Mirrors the manual.md
// spec: images, audio, video, PDF, plain text, markdown, and the
// MS Office formats.
func uploadTypeAllowed(contentType, filename string) bool {
	ct := strings.ToLower(contentType)
	if strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "audio/") || strings.HasPrefix(ct, "video/") {
		return true
	}
	docMIMEs := map[string]bool{
		"application/pdf":  true,
		"text/plain":       true,
		"text/markdown":    true,
		"application/msword": true,
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": true,
		"application/vnd.ms-excel": true,
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": true,
		"application/vnd.ms-powerpoint": true,
		"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
	}
	// Strip ;charset=… style suffix.
	bare := ct
	if i := strings.IndexByte(bare, ';'); i >= 0 {
		bare = strings.TrimSpace(bare[:i])
	}
	if docMIMEs[bare] {
		return true
	}
	// Extension fallback for poorly-typed uploads.
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg",
		".mp3", ".wav", ".m4a", ".mp4", ".mov", ".avi", ".webm",
		".pdf", ".txt", ".md", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx":
		return true
	}
	return false
}

// hUploadServe serves files from data/uploads/{name}.
func (s *Server) hUploadServe(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/uploads/")
	s.uploadsService().ServeLocal(w, r, name)
}

// ---------------- static ----------------

func (s *Server) hStatic(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = "index.html"
	}
	// Fallback to index.html for SPA-style routes (/login, /settings, etc.)
	f, err := s.Web.Open(p)
	if err != nil {
		if p != "index.html" {
			fi, err2 := s.Web.Open("index.html")
			if err2 == nil {
				defer fi.Close()
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				setStaticCacheHeaders(w, "index.html")
				copyTo(w, fi)
				return
			}
		}
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	setContentType(w, p)
	setStaticCacheHeaders(w, p)
	copyTo(w, f)
}

// setStaticCacheHeaders writes Cache-Control so the browser doesn't pin the
// app's frontend to whatever it loaded the first time. Without this Go
// returns no Cache-Control at all and Chrome / Safari heuristically cache
// the JS / CSS for hours, which makes "I changed the code, why doesn't the
// fix show up?" the dominant failure mode during development.
//
//   - index.html and sw.js: must always re-validate against the server so
//     a fresh deploy is picked up on the next reload.
//   - The rest of the app shell (JS / CSS / locales): same — the file might
//     have changed since the cached copy, so revalidate every load. The
//     server-side service worker bumps its CACHE name on each release, so
//     the offline fallback eventually replaces too.
//   - Static assets that are content-addressable by name (icons, the
//     vendored vue.global.js): can cache forever; we keep them
//     conservative for now and treat them like the rest until we add
//     hashed filenames.
func setStaticCacheHeaders(w http.ResponseWriter, pathInside string) {
	if pathInside == "sw.js" || pathInside == "index.html" {
		// Service worker spec already says browsers must revalidate sw.js,
		// but explicit no-cache is belt-and-suspenders.
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		return
	}
	// Everything else: allow a cached copy but require revalidation each
	// time so a redeploy always wins on next reload.
	w.Header().Set("Cache-Control", "no-cache")
}

// ---------------- helpers ----------------

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

func toEvent(name string, data map[string]any) sse.Event {
	return sse.Event{Event: name, Data: data}
}

func truncExcerpt(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func copyTo(w http.ResponseWriter, r interface{ Read([]byte) (int, error) }) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func setContentType(w http.ResponseWriter, path string) {
	switch {
	case strings.HasSuffix(path, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(path, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case strings.HasSuffix(path, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(path, ".webmanifest"):
		// Strict MIME type from the W3C manifest spec. Some browser
		// installability checks are picky and reject a manifest served
		// as plain application/json.
		w.Header().Set("Content-Type", "application/manifest+json")
	case strings.HasSuffix(path, ".json"):
		w.Header().Set("Content-Type", "application/json")
	case strings.HasSuffix(path, ".md"):
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	case strings.HasSuffix(path, ".svg"):
		w.Header().Set("Content-Type", "image/svg+xml")
	case strings.HasSuffix(path, ".png"):
		w.Header().Set("Content-Type", "image/png")
	case strings.HasSuffix(path, ".ogg"):
		w.Header().Set("Content-Type", "audio/ogg")
	case strings.HasSuffix(path, ".mp3"):
		w.Header().Set("Content-Type", "audio/mpeg")
	}
}
