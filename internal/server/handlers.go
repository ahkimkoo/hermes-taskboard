package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ahkimkoo/hermes-taskboard/internal/attempt"
	"github.com/ahkimkoo/hermes-taskboard/internal/auth"
	"github.com/ahkimkoo/hermes-taskboard/internal/board"
	"github.com/ahkimkoo/hermes-taskboard/internal/config"
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
	Title           string   `json:"title"`
	Description     string   `json:"description,omitempty"`
	Priority        int      `json:"priority,omitempty"`
	Status          string   `json:"status,omitempty"`
	TriggerMode     string   `json:"trigger_mode,omitempty"`
	PreferredServer string   `json:"preferred_server,omitempty"`
	PreferredModel  string   `json:"preferred_model,omitempty"`
	Tags            []string `json:"tags,omitempty"`
	Dependencies    []string `json:"dependencies,omitempty"`
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
		Dependencies:       req.Dependencies,
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
	Title           *string   `json:"title,omitempty"`
	Description     *string   `json:"description,omitempty"`
	Priority        *int      `json:"priority,omitempty"`
	TriggerMode     *string   `json:"trigger_mode,omitempty"`
	PreferredServer *string   `json:"preferred_server,omitempty"`
	PreferredModel  *string   `json:"preferred_model,omitempty"`
	Tags            *[]string `json:"tags,omitempty"`
	Dependencies    *[]string `json:"dependencies,omitempty"`
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
		t.Dependencies = *req.Dependencies
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
	To     string `json:"to"`
	Reason string `json:"reason,omitempty"`
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
	default:
		http.NotFound(w, r)
	}
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

func writeSSE(w http.ResponseWriter, seq uint64, event string, data map[string]any) {
	if seq > 0 {
		fmt.Fprintf(w, "id: %d\n", seq)
	}
	if event != "" {
		fmt.Fprintf(w, "event: %s\n", event)
	}
	b, _ := json.Marshal(data)
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
	writeJSON(w, 200, map[string]any{
		"scheduler": c.Scheduler,
		"archive":   c.Archive,
		"server":    c.Server,
	})
}

type settingsReq struct {
	Scheduler *config.Scheduler `json:"scheduler,omitempty"`
	Archive   *config.Archive   `json:"archive,omitempty"`
	Server    *config.Server    `json:"server,omitempty"`
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
				copyTo(w, fi)
				return
			}
		}
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	setContentType(w, p)
	copyTo(w, f)
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
	case strings.HasSuffix(path, ".json") || strings.HasSuffix(path, ".webmanifest"):
		w.Header().Set("Content-Type", "application/json")
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
