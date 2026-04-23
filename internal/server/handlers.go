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
	"github.com/ahkimkoo/hermes-taskboard/internal/userdir"
)

func contextWithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}

// ---------------- tasks ----------------

func (s *Server) hListTasks(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	list, err := st.ListTasks(r.Context(), store.TaskFilter{
		Status: q.Get("status"),
		Tag:    q.Get("tag"),
		Query:  q.Get("q"),
		Server: q.Get("server"),
		Model:  q.Get("model"),
	})
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
	Dependencies    []json.RawMessage `json:"dependencies,omitempty"`
}

func normalizeDeps(raws []json.RawMessage) []store.TaskDep {
	out := make([]store.TaskDep, 0, len(raws))
	for _, raw := range raws {
		if len(raw) == 0 {
			continue
		}
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
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			out = append(out, store.TaskDep{TaskID: s, RequiredState: "done"})
		}
	}
	return out
}

func (s *Server) hCreateTask(w http.ResponseWriter, r *http.Request) {
	st, username, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	fs := s.FS.Get(username)
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
	if err := st.CreateTask(r.Context(), t); err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := fs.SaveTaskDoc(&fsstore.TaskDoc{ID: t.ID, Description: req.Description, Tags: req.Tags}); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.Hub.Publish("board", toEvent("task.created", map[string]any{"task_id": t.ID, "owner": username}))
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
	st, username, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	t, err := st.GetTask(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, 404, err)
		} else {
			writeErr(w, 500, err)
		}
		return
	}
	fs := s.FS.Get(username)
	if doc, _ := fs.LoadTaskDoc(id); doc != nil {
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
	st, username, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	var req patchTaskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	t, err := st.GetTask(r.Context(), id)
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
	if err := st.UpdateTask(r.Context(), t); err != nil {
		writeErr(w, 500, err)
		return
	}
	fs := s.FS.Get(username)
	if req.Description != nil {
		doc, _ := fs.LoadTaskDoc(id)
		if doc == nil {
			doc = &fsstore.TaskDoc{ID: id}
		}
		doc.Description = *req.Description
		_ = fs.SaveTaskDoc(doc)
		t.Description = *req.Description
	}
	s.Hub.Publish("board", toEvent("task.updated", map[string]any{"task_id": id, "owner": username}))
	writeJSON(w, 200, map[string]any{"task": t})
}

func (s *Server) hDeleteTask(w http.ResponseWriter, r *http.Request, id string) {
	st, username, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	ids, err := st.DeleteTask(r.Context(), id)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	fs := s.FS.Get(username)
	_ = fs.DeleteTask(id)
	for _, aid := range ids {
		_ = fs.DeleteAttempt(aid)
	}
	s.Hub.Publish("board", toEvent("task.deleted", map[string]any{"task_id": id, "owner": username}))
	writeJSON(w, 200, map[string]any{"ok": true})
}

type transitionReq struct {
	To       string `json:"to"`
	Reason   string `json:"reason,omitempty"`
	AfterID  string `json:"after_id,omitempty"`
	BeforeID string `json:"before_id,omitempty"`
}

func (s *Server) hTransition(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	st, username, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	var req transitionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	to := store.TaskStatus(req.To)
	if to == store.StatusExecute {
		t, err := st.GetTask(r.Context(), id)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		if t.Status == store.StatusPlan || t.Status == store.StatusDraft {
			if _, err := s.Runner.Start(r.Context(), username, id, "", ""); err != nil {
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
	if req.AfterID != "" || req.BeforeID != "" {
		if err := st.MoveTask(r.Context(), id, to, req.AfterID, req.BeforeID); err != nil {
			writeErr(w, 400, err)
			return
		}
		s.Hub.Publish("board", toEvent("task.moved", map[string]any{"task_id": id, "to": req.To, "kind": "manual"}))
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	if err := s.Board.Transition(r.Context(), st, id, to, board.KindManual, req.Reason); err != nil {
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
	st, _, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	atts, err := st.ListAttemptsForTask(r.Context(), id)
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
	_, username, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	var req startAttemptReq
	_ = json.NewDecoder(r.Body).Decode(&req)
	att, err := s.Runner.Start(r.Context(), username, id, req.ServerID, req.Model)
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
		switch r.Method {
		case "GET":
			s.hGetAttempt(w, r, id)
		case "DELETE":
			s.hDeleteAttempt(w, r, id)
		default:
			http.Error(w, "method not allowed", 405)
		}
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

func (s *Server) hAttemptReconnect(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	_, username, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	status, err := s.Runner.TryReconnect(r.Context(), username, id)
	if err != nil {
		writeJSON(w, 200, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"status": status})
}

func (s *Server) hGetAttempt(w http.ResponseWriter, r *http.Request, id string) {
	st, username, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	att, err := st.GetAttempt(r.Context(), id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	fs := s.FS.Get(username)
	meta, _ := fs.LoadAttemptMeta(id)
	writeJSON(w, 200, map[string]any{"attempt": att, "meta": meta})
}

// hDeleteAttempt removes a single attempt row + its filesystem payload
// from the caller's per-user store.
func (s *Server) hDeleteAttempt(w http.ResponseWriter, r *http.Request, id string) {
	st, username, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	att, err := st.GetAttempt(r.Context(), id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if att.State == store.AttemptQueued || att.State == store.AttemptRunning || att.State == store.AttemptNeedsInput {
		writeErr(w, 409, errors.New("cancel the attempt first"))
		return
	}
	if err := st.DeleteAttempt(r.Context(), id); err != nil {
		writeErr(w, 500, err)
		return
	}
	fs := s.FS.Get(username)
	_ = fs.DeleteAttempt(id)
	s.Hub.Publish("board", toEvent("attempt.deleted", map[string]any{"task_id": att.TaskID, "attempt_id": id, "owner": username}))
	s.Hub.Publish("attempt:"+id, toEvent("attempt.deleted", map[string]any{"task_id": att.TaskID, "attempt_id": id, "owner": username}))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) hAttemptMessages(w http.ResponseWriter, r *http.Request, id string) {
	_, username, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	fs := s.FS.Get(username)
	switch r.Method {
	case "POST":
		var req struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
			writeErr(w, 400, errors.New("text required"))
			return
		}
		if err := s.Runner.SendMessage(r.Context(), username, id, req.Text); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 202, map[string]any{"ok": true})
	case "GET":
		q := r.URL.Query()
		if bs := q.Get("before_seq"); bs != "" {
			before, _ := strconv.ParseUint(bs, 10, 64)
			limit, _ := strconv.Atoi(q.Get("limit"))
			if limit <= 0 {
				limit = 20
			}
			events, err := fs.ReadEventsBefore(id, before, limit)
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
		events, err := fs.ReadEventsTail(id, tail*10)
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
	_, username, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	if err := s.Runner.Cancel(r.Context(), username, id); err != nil {
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
	_, username, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	fs := s.FS.Get(username)
	q := r.URL.Query()
	if t := q.Get("tail"); t != "" {
		n, _ := strconv.Atoi(t)
		if n <= 0 {
			n = 500
		}
		if bs := q.Get("before_seq"); bs != "" {
			before, _ := strconv.ParseUint(bs, 10, 64)
			events, err := fs.ReadEventsBefore(id, before, n)
			if err != nil {
				writeErr(w, 500, err)
				return
			}
			writeJSON(w, 200, map[string]any{"events": events})
			return
		}
		events, err := fs.ReadEventsTail(id, n)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"events": events})
		return
	}
	since, _ := strconv.ParseUint(q.Get("since_seq"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	events, err := fs.ReadEventsRange(id, since, limit)
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
	_, username, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	// Access check: the attempt must exist in this user's store.
	st, _ := s.Stores.Get(username)
	if st != nil {
		if _, err := st.GetAttempt(r.Context(), id); err != nil {
			http.Error(w, `{"code":"not_found"}`, http.StatusNotFound)
			return
		}
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
	s.streamTopicForUser(w, r, "attempt:"+id, since, username, id)
}

func (s *Server) streamTopicForUser(w http.ResponseWriter, r *http.Request, topic string, since uint64, username, attemptID string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}

	if since > 0 {
		fs := s.FS.Get(username)
		events, _ := fs.ReadEventsRange(attemptID, since, 2000)
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

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

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

	ch, unsub := s.Hub.Subscribe(topic)
	defer unsub()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

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
	Models        []userdir.HermesModel `json:"models"`
	Shared        bool                  `json:"shared"`
	OwnerID       string                `json:"owner_id"`
	OwnerUsername string                `json:"owner_username"`
	Mine          bool                  `json:"mine"`
}

func (s *Server) hListServers(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	if u == nil {
		writeErr(w, 401, errors.New("unauthorized"))
		return
	}
	views := s.Users.VisibleServers(u.Username)
	out := make([]serverDTO, 0, len(views))
	for _, v := range views {
		out = append(out, serverDTO{
			ID: v.ID, Name: v.Name, BaseURL: v.BaseURL,
			HasAPIKey:     v.APIKey != "" || v.APIKeyEnc != "",
			IsDefault:     v.IsDefault,
			MaxConcurrent: v.MaxConcurrent,
			Models:        v.Models,
			Shared:        v.Shared,
			OwnerID:       v.OwnerID,
			OwnerUsername: v.OwnerUsername,
			Mine:          v.Mine,
		})
	}
	writeJSON(w, 200, map[string]any{"servers": out})
}

type serverUpsertReq struct {
	ID            string                `json:"id"`
	Name          string                `json:"name"`
	BaseURL       string                `json:"base_url"`
	APIKey        string                `json:"api_key,omitempty"`
	IsDefault     bool                  `json:"is_default"`
	MaxConcurrent int                   `json:"max_concurrent,omitempty"`
	Models        []userdir.HermesModel `json:"models,omitempty"`
	Shared        bool                  `json:"shared"`
}

func (s *Server) hCreateServer(w http.ResponseWriter, r *http.Request) {
	me := auth.UserFromContext(r.Context())
	if me == nil {
		writeErr(w, 401, errors.New("unauthorized"))
		return
	}
	var req serverUpsertReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" || req.BaseURL == "" {
		writeErr(w, 400, errors.New("id and base_url required"))
		return
	}
	// Server IDs must be globally unique across users so the Hermes pool
	// can key clients by ID alone.
	if owner, _, _, found := s.Users.FindServer(me.Username, req.ID); found {
		writeErr(w, 409, fmt.Errorf("server id %q already exists (owner: %s)", req.ID, owner))
		return
	}
	err := s.Users.Mutate(me.Username, func(uc *userdir.UserConfig) error {
		for _, sv := range uc.HermesServers {
			if strings.EqualFold(sv.BaseURL, req.BaseURL) {
				return fmt.Errorf("you already have a server with base_url %q", req.BaseURL)
			}
		}
		if req.IsDefault {
			for i := range uc.HermesServers {
				uc.HermesServers[i].IsDefault = false
			}
		}
		uc.HermesServers = append(uc.HermesServers, userdir.HermesServer{
			ID: req.ID, Name: req.Name, BaseURL: req.BaseURL,
			APIKey: req.APIKey, IsDefault: req.IsDefault,
			MaxConcurrent: req.MaxConcurrent,
			Models:        req.Models,
			Shared:        req.Shared,
		})
		return nil
	})
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.ReloadPool()
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
	me := auth.UserFromContext(r.Context())
	if me == nil {
		writeErr(w, 401, errors.New("unauthorized"))
		return
	}
	owner, _, _, found := s.Users.FindServer(me.Username, id)
	if !found || owner != me.Username {
		writeErr(w, 404, errors.New("not found"))
		return
	}
	var req serverUpsertReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	err := s.Users.Mutate(me.Username, func(uc *userdir.UserConfig) error {
		for i := range uc.HermesServers {
			if uc.HermesServers[i].ID != id {
				continue
			}
			sv := &uc.HermesServers[i]
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
			sv.Shared = req.Shared
			if req.IsDefault {
				for j := range uc.HermesServers {
					uc.HermesServers[j].IsDefault = false
				}
				sv.IsDefault = true
			}
			return nil
		}
		return errors.New("not found")
	})
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.ReloadPool()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) hDeleteServer(w http.ResponseWriter, r *http.Request, id string) {
	me := auth.UserFromContext(r.Context())
	if me == nil {
		writeErr(w, 401, errors.New("unauthorized"))
		return
	}
	owner, _, _, found := s.Users.FindServer(me.Username, id)
	if !found || owner != me.Username {
		writeErr(w, 404, errors.New("not found"))
		return
	}
	err := s.Users.Mutate(me.Username, func(uc *userdir.UserConfig) error {
		idx := -1
		for i := range uc.HermesServers {
			if uc.HermesServers[i].ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return errors.New("not found")
		}
		uc.HermesServers = append(uc.HermesServers[:idx], uc.HermesServers[idx+1:]...)
		return nil
	})
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.ReloadPool()
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
	u := auth.UserFromContext(r.Context())
	if u == nil {
		writeErr(w, 401, errors.New("unauthorized"))
		return
	}
	writeJSON(w, 200, map[string]any{"tags": s.Users.VisibleTags(u.Username)})
}

func (s *Server) hUpsertTag(w http.ResponseWriter, r *http.Request) {
	me := auth.UserFromContext(r.Context())
	if me == nil {
		writeErr(w, 401, errors.New("unauthorized"))
		return
	}
	var t userdir.Tag
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		writeErr(w, 400, err)
		return
	}
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		writeErr(w, 400, errors.New("tag name required"))
		return
	}
	// Only allowed to edit if no one else owns this tag name, OR it's mine.
	if owner, _, _, found := s.Users.TagByName(me.Username, t.Name); found && owner != me.Username {
		writeErr(w, 403, errors.New("tag owned by another user"))
		return
	}
	err := s.Users.Mutate(me.Username, func(uc *userdir.UserConfig) error {
		for i := range uc.Tags {
			if uc.Tags[i].Name == t.Name {
				uc.Tags[i].Color = t.Color
				uc.Tags[i].SystemPrompt = t.SystemPrompt
				uc.Tags[i].Shared = t.Shared
				return nil
			}
		}
		uc.Tags = append(uc.Tags, t)
		return nil
	})
	if err != nil {
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
	me := auth.UserFromContext(r.Context())
	if me == nil {
		writeErr(w, 401, errors.New("unauthorized"))
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/tags/")
	owner, _, _, found := s.Users.TagByName(me.Username, name)
	if !found || owner != me.Username {
		writeErr(w, 404, errors.New("not found"))
		return
	}
	err := s.Users.Mutate(me.Username, func(uc *userdir.UserConfig) error {
		idx := -1
		for i := range uc.Tags {
			if uc.Tags[i].Name == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			return errors.New("not found")
		}
		uc.Tags = append(uc.Tags[:idx], uc.Tags[idx+1:]...)
		return nil
	})
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------- settings / preferences ----------------

func (s *Server) hGetSettings(w http.ResponseWriter, r *http.Request) {
	c := s.Cfg.Snapshot()
	ossOut := c.OSS
	ossOut.AccessKeySecret = ""
	ossOut.AccessKeySecretEnc = ""
	hasSecret := c.OSS.AccessKeySecret != "" || c.OSS.AccessKeySecretEnc != ""
	writeJSON(w, 200, map[string]any{
		"scheduler":      c.Scheduler,
		"archive":        c.Archive,
		"server":         c.Server,
		"oss":            ossOut,
		"oss_has_secret": hasSecret,
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
	u := auth.UserFromContext(r.Context())
	if u == nil {
		writeErr(w, 401, errors.New("unauthorized"))
		return
	}
	writeJSON(w, 200, map[string]any{"preferences": u.Preferences})
}

func (s *Server) hUpdatePreferences(w http.ResponseWriter, r *http.Request) {
	me := auth.UserFromContext(r.Context())
	if me == nil {
		writeErr(w, 401, errors.New("unauthorized"))
		return
	}
	var p userdir.Preferences
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, 400, err)
		return
	}
	err := s.Users.Mutate(me.Username, func(uc *userdir.UserConfig) error {
		uc.Preferences = p
		return nil
	})
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.Hub.Publish("board", toEvent("preferences_updated", map[string]any{"owner": me.Username}))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) hGetConfig(w http.ResponseWriter, r *http.Request) {
	c := s.Cfg.Snapshot()
	sanitized := *c
	sanitized.Auth.SessionSecret = ""
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
	if err := s.Users.Reload(); err != nil {
		writeErr(w, 400, err)
		return
	}
	s.ReloadPool()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------- auth handlers ----------------

func (s *Server) hAuthStatus(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	if u == nil {
		writeJSON(w, 200, map[string]any{"logged_in": false})
		return
	}
	writeJSON(w, 200, map[string]any{
		"logged_in": true,
		"user": map[string]any{
			"id":       u.ID,
			"username": u.Username,
			"is_admin": u.IsAdmin,
		},
	})
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
	token, exp, u, err := s.Auth.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		writeErr(w, 401, err)
		return
	}
	auth.WriteCookie(w, token, exp, auth.IsSecureRequest(r))
	writeJSON(w, 200, map[string]any{
		"ok": true,
		"user": map[string]any{
			"id": u.ID, "username": u.Username, "is_admin": u.IsAdmin,
		},
	})
}

func (s *Server) hAuthLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
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
	u := auth.UserFromContext(r.Context())
	if u == nil {
		writeErr(w, 401, errors.New("unauthorized"))
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
	if err := s.Auth.ChangePassword(r.Context(), u.Username, req.OldPassword, req.NewPassword); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------- schedules ----------------

type scheduleReq struct {
	Kind    string `json:"kind"`
	Spec    string `json:"spec"`
	Note    string `json:"note,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

func (s *Server) hListTaskSchedules(w http.ResponseWriter, r *http.Request, taskID string) {
	st, _, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	list, err := st.ListSchedulesForTask(r.Context(), taskID)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"schedules": list})
}

func (s *Server) hCreateSchedule(w http.ResponseWriter, r *http.Request, taskID string) {
	st, _, ok := s.storeFor(w, r)
	if !ok {
		return
	}
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
	if err := st.CreateSchedule(r.Context(), &sch); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 201, map[string]any{"schedule": sch})
}

func (s *Server) routeSchedules(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/schedules/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	st, _, ok := s.storeFor(w, r)
	if !ok {
		return
	}
	sch, err := st.GetSchedule(r.Context(), id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	switch r.Method {
	case "DELETE":
		if err := st.DeleteSchedule(r.Context(), id); err != nil {
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
		if err := st.SetScheduleEnabled(r.Context(), id, *req.Enabled); err != nil {
			writeErr(w, 500, err)
			return
		}
		if *req.Enabled {
			sch.Enabled = true
			if err := cronpkg.Compute(sch, time.Now()); err == nil {
				_ = st.UpdateSchedule(r.Context(), sch)
			}
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// ---------------- uploads ----------------

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
	const maxUpload = 50 << 20
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
		writeErr(w, 400, errors.New("file type not allowed"))
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

func uploadTypeAllowed(contentType, filename string) bool {
	ct := strings.ToLower(contentType)
	if strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "audio/") || strings.HasPrefix(ct, "video/") {
		return true
	}
	docMIMEs := map[string]bool{
		"application/pdf":    true,
		"text/plain":         true,
		"text/markdown":      true,
		"application/msword": true,
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": true,
		"application/vnd.ms-excel": true,
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       true,
		"application/vnd.ms-powerpoint":                                            true,
		"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
	}
	bare := ct
	if i := strings.IndexByte(bare, ';'); i >= 0 {
		bare = strings.TrimSpace(bare[:i])
	}
	if docMIMEs[bare] {
		return true
	}
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg",
		".mp3", ".wav", ".m4a", ".mp4", ".mov", ".avi", ".webm",
		".pdf", ".txt", ".md", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx":
		return true
	}
	return false
}

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

func setStaticCacheHeaders(w http.ResponseWriter, pathInside string) {
	if pathInside == "sw.js" || pathInside == "index.html" {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		return
	}
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
