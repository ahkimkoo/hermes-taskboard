// Plugin-transport HTTP handlers. Runtime control surface for the WS
// plugin server — lists connected plugins, pushes a user turn, cancels a
// running turn. The list endpoint is UI-facing; the send/cancel endpoints
// are dev tooling the runner will eventually call in-process.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/ahkimkoo/hermes-taskboard/internal/hermes"
)

// GET /api/plugin/plugins
func (s *Server) hListPlugins(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	plugins := s.PluginServer.Plugins()
	writeJSON(w, http.StatusOK, map[string]any{
		"plugins": plugins,
		"count":   len(plugins),
		"ts":      time.Now().Unix(),
	})
}

// POST /api/plugin/send
//
//	{"hermes_id": "...", "attempt_id": "...", "text": "..."}
//
// Returns the agent's final assistant content once `agent_done` arrives,
// or an error. This is synchronous from the HTTP caller's perspective but
// the underlying WS conversation remains streaming — subscribers to the
// same attempt_id still receive every event. Mainly for probing the
// plugin path end-to-end before the Runner is wired in.
func (s *Server) hPluginSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		HermesID  string `json:"hermes_id"`
		AttemptID string `json:"attempt_id"`
		Text      string `json:"text"`
		TimeoutMs int    `json:"timeout_ms,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if in.HermesID == "" || in.AttemptID == "" || in.Text == "" {
		http.Error(w, "hermes_id, attempt_id, text required", http.StatusBadRequest)
		return
	}
	timeout := time.Duration(in.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	ctx := r.Context()
	events, unsub, err := s.PluginServer.SendMessage(ctx, in.HermesID, in.AttemptID, in.Text)
	if err != nil {
		if errors.Is(err, hermes.ErrPluginNotConnected) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer unsub()

	var summary string
	deadline := time.After(timeout)
	for {
		select {
		case f, ok := <-events:
			if !ok {
				http.Error(w, "plugin disconnected mid-turn", http.StatusServiceUnavailable)
				return
			}
			switch f.Type {
			case "agent_done":
				summary = f.Summary
				writeJSON(w, http.StatusOK, map[string]any{
					"hermes_id":  in.HermesID,
					"attempt_id": in.AttemptID,
					"summary":    summary,
				})
				return
			case "agent_error":
				writeJSON(w, http.StatusBadGateway, map[string]any{
					"hermes_id":  in.HermesID,
					"attempt_id": in.AttemptID,
					"error":      string(f.Raw),
				})
				return
			}
		case <-deadline:
			http.Error(w, "turn did not complete before timeout", http.StatusGatewayTimeout)
			return
		case <-ctx.Done():
			return
		}
	}
}

// POST /api/plugin/cancel
//
//	{"hermes_id": "...", "attempt_id": "..."}
//
// Injects Hermes's native /stop into the running turn via the plugin.
func (s *Server) hPluginCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		HermesID  string `json:"hermes_id"`
		AttemptID string `json:"attempt_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if in.HermesID == "" || in.AttemptID == "" {
		http.Error(w, "hermes_id, attempt_id required", http.StatusBadRequest)
		return
	}
	if err := s.PluginServer.Cancel(r.Context(), in.HermesID, in.AttemptID); err != nil {
		if errors.Is(err, hermes.ErrPluginNotConnected) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
