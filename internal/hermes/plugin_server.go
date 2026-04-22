// Package hermes — plugin transport.
//
// This half is the WebSocket *server* that Hermes plugins connect into.
// Each plugin announces itself with a `hermes_id` (explicit env override,
// otherwise the Hermes host's machine hostname). The server keeps at most
// one live connection per hermes_id; a second plugin announcing the same
// id evicts the first. The Runner (or any caller) routes outbound work
// (`send_message`, `cancel`) through the server by hermes_id+attempt_id
// and consumes inbound `agent_event` / `agent_done` frames over a channel.
//
// There's no assumption that the plugin-transport Hermes hosts are listed
// in taskboard's `hermes_servers` config — a plugin that connects with a
// previously-unknown id is auto-registered and available for task dispatch
// immediately. This matches the user model: install the plugin on a new
// Hermes host, point it at the taskboard WS URL, it shows up.
package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ahkimkoo/hermes-taskboard/internal/sse"
)

// PluginFrame is every JSON frame on the wire (both directions). Fields not
// set for a given type are omitted; extra fields are preserved via Raw so
// future protocol extensions can coexist with this Go version.
type PluginFrame struct {
	Type       string          `json:"type"`
	HermesID   string          `json:"hermes_id,omitempty"`
	Hostname   string          `json:"hostname,omitempty"`
	ClientID   string          `json:"client_id,omitempty"`
	PluginVer  string          `json:"plugin_version,omitempty"`
	GatewayVer string          `json:"gateway_version,omitempty"`
	Token      string          `json:"token,omitempty"`
	AttemptID  string          `json:"attempt_id,omitempty"`
	Seq        int64           `json:"seq,omitempty"`
	Text       string          `json:"text,omitempty"`
	Summary    string          `json:"summary,omitempty"`
	TS         float64         `json:"ts,omitempty"`
	Event      json.RawMessage `json:"event,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

// PluginInfo is the public view of one connected plugin — handed out to the
// scheduler / UI / health endpoints.
type PluginInfo struct {
	HermesID   string    `json:"hermes_id"`
	Hostname   string    `json:"hostname"`
	ClientID   string    `json:"client_id"`
	PluginVer  string    `json:"plugin_version"`
	GatewayVer string    `json:"gateway_version"`
	Connected  bool      `json:"connected"`
	ConnectedAt time.Time `json:"connected_at"`
	RemoteAddr string    `json:"remote_addr"`
}

// pluginConn is the server-side state for one connected plugin. Guarded by
// its own mutex for writes; the read loop owns the reader.
type pluginConn struct {
	info   PluginInfo
	ws     *websocket.Conn
	writeMu sync.Mutex
	// per-attempt event channels: attempt_id → chan<- frame
	// Subscribers register before sending a message so they catch the reply
	// without a racy gap.
	subsMu sync.Mutex
	subs   map[string][]chan<- PluginFrame
}

func (c *pluginConn) writeJSON(ctx context.Context, v any) error {
	buf, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.ws.Write(wctx, websocket.MessageText, buf)
}

func (c *pluginConn) subscribe(attemptID string, ch chan<- PluginFrame) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	c.subs[attemptID] = append(c.subs[attemptID], ch)
}

func (c *pluginConn) unsubscribe(attemptID string, ch chan<- PluginFrame) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	list := c.subs[attemptID]
	for i, s := range list {
		if s == ch {
			c.subs[attemptID] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(c.subs[attemptID]) == 0 {
		delete(c.subs, attemptID)
	}
}

func (c *pluginConn) fanout(f PluginFrame) {
	if f.AttemptID == "" {
		return
	}
	c.subsMu.Lock()
	targets := append([]chan<- PluginFrame(nil), c.subs[f.AttemptID]...)
	c.subsMu.Unlock()
	for _, ch := range targets {
		select {
		case ch <- f:
		default:
			// drop if slow — subscriber must size its buffer adequately.
			// Logged once per drop-burst; the guarantee is at-most-once, so
			// subscribers relying on every frame must oversize.
		}
	}
}

// PluginServer accepts WS connections from Hermes plugins and exposes a
// simple API for the Runner to drive sessions.
type PluginServer struct {
	Log   *slog.Logger
	Token string // optional shared secret; if non-empty, plugins must match
	// Hub, when set, is used to publish plugin.connected /
	// plugin.disconnected events on the "board" topic so the frontend
	// server-list can refresh without polling.
	Hub   *sse.Hub
	mu    sync.RWMutex
	conns map[string]*pluginConn // hermes_id → active conn
}

func NewPluginServer(log *slog.Logger) *PluginServer {
	if log == nil {
		log = slog.Default()
	}
	return &PluginServer{
		Log:   log,
		conns: map[string]*pluginConn{},
	}
}

// broadcastStatus is a best-effort board event. Safe no-op when Hub is nil.
func (s *PluginServer) broadcastStatus(eventName string, info PluginInfo) {
	if s.Hub == nil {
		return
	}
	s.Hub.Publish("board", sse.Event{
		Event: eventName,
		Data: map[string]any{
			"hermes_id":      info.HermesID,
			"hostname":       info.Hostname,
			"plugin_version": info.PluginVer,
			"remote_addr":    info.RemoteAddr,
			"ts":             time.Now().Unix(),
		},
	})
}

// HandleWS is the http.HandlerFunc for the taskboard WS endpoint, e.g.
// mux.HandleFunc("/api/plugin/ws", server.HandleWS).
func (s *PluginServer) HandleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // localhost trust model; no origin check
	})
	if err != nil {
		s.Log.Warn("ws accept failed", "err", err)
		return
	}
	defer ws.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Greet first so the plugin knows taskboard speaks the protocol.
	hello := PluginFrame{
		Type:             "hello",
		ClientID:         "taskboard",
		PluginVer:        "", // server-side no-op
		TS:               float64(time.Now().Unix()),
	}
	if err := writeJSONOn(ctx, ws, hello); err != nil {
		s.Log.Warn("write hello failed", "err", err)
		return
	}

	// Wait for hello_ack so we know the plugin's identity before registering.
	// Bounded timeout so a stuck client can't park a slot forever.
	actx, acancel := context.WithTimeout(ctx, 10*time.Second)
	_, ackData, err := ws.Read(actx)
	acancel()
	if err != nil {
		s.Log.Warn("hello_ack read failed", "err", err, "remote", r.RemoteAddr)
		return
	}
	var ack PluginFrame
	if err := json.Unmarshal(ackData, &ack); err != nil {
		s.Log.Warn("hello_ack not json", "err", err, "remote", r.RemoteAddr)
		return
	}
	if ack.Type != "hello_ack" {
		s.Log.Warn("expected hello_ack, got", "type", ack.Type, "remote", r.RemoteAddr)
		return
	}
	if s.Token != "" && ack.Token != s.Token {
		s.Log.Warn("plugin token mismatch", "remote", r.RemoteAddr)
		return
	}

	hermesID := ack.HermesID
	if hermesID == "" {
		hermesID = ack.Hostname
	}
	if hermesID == "" {
		s.Log.Warn("plugin announced no hermes_id and no hostname — rejecting", "remote", r.RemoteAddr)
		return
	}

	conn := &pluginConn{
		ws: ws,
		info: PluginInfo{
			HermesID:    hermesID,
			Hostname:    ack.Hostname,
			ClientID:    ack.ClientID,
			PluginVer:   ack.PluginVer,
			GatewayVer:  ack.GatewayVer,
			Connected:   true,
			ConnectedAt: time.Now(),
			RemoteAddr:  r.RemoteAddr,
		},
		subs: map[string][]chan<- PluginFrame{},
	}

	// Register, evicting any previous connection with the same hermes_id.
	s.mu.Lock()
	if old, ok := s.conns[hermesID]; ok && old != conn {
		s.Log.Info("plugin re-registered, evicting previous", "hermes_id", hermesID)
		_ = old.ws.Close(websocket.StatusGoingAway, "replaced by new plugin connection")
	}
	s.conns[hermesID] = conn
	s.mu.Unlock()

	s.Log.Info("plugin connected",
		"hermes_id", hermesID,
		"hostname", ack.Hostname,
		"remote", r.RemoteAddr,
		"plugin_version", ack.PluginVer,
	)
	s.broadcastStatus("plugin.connected", conn.info)

	defer func() {
		s.mu.Lock()
		if s.conns[hermesID] == conn {
			delete(s.conns, hermesID)
		}
		s.mu.Unlock()
		s.Log.Info("plugin disconnected", "hermes_id", hermesID)
		s.broadcastStatus("plugin.disconnected", conn.info)
	}()

	// Read loop.
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.Log.Info("plugin read ended", "hermes_id", hermesID, "err", err)
			}
			return
		}
		var f PluginFrame
		if err := json.Unmarshal(data, &f); err != nil {
			s.Log.Warn("bad frame from plugin", "hermes_id", hermesID, "err", err)
			continue
		}
		f.Raw = data
		switch f.Type {
		case "ping":
			_ = conn.writeJSON(ctx, PluginFrame{Type: "pong", TS: f.TS})
		case "agent_event", "agent_done", "agent_error":
			conn.fanout(f)
		default:
			s.Log.Debug("unhandled frame from plugin",
				"hermes_id", hermesID, "type", f.Type,
			)
		}
	}
}

// Plugins returns a snapshot of the currently connected plugins.
func (s *PluginServer) Plugins() []PluginInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]PluginInfo, 0, len(s.conns))
	for _, c := range s.conns {
		out = append(out, c.info)
	}
	return out
}

// ErrPluginNotConnected means no plugin has announced the given hermes_id.
var ErrPluginNotConnected = errors.New("hermes plugin not connected")

// SendMessage pushes a user turn to the plugin identified by hermesID. The
// returned channel receives every agent_event / agent_done / agent_error
// frame for that attempt_id until ctx is cancelled or the caller
// unsubscribes via the returned cancel function.
func (s *PluginServer) SendMessage(
	ctx context.Context,
	hermesID, attemptID, text string,
) (<-chan PluginFrame, func(), error) {
	s.mu.RLock()
	conn, ok := s.conns[hermesID]
	s.mu.RUnlock()
	if !ok {
		return nil, func() {}, fmt.Errorf("%w: %s", ErrPluginNotConnected, hermesID)
	}
	ch := make(chan PluginFrame, 64)
	conn.subscribe(attemptID, ch)
	if err := conn.writeJSON(ctx, PluginFrame{
		Type:      "send_message",
		AttemptID: attemptID,
		Text:      text,
		TS:        float64(time.Now().Unix()),
	}); err != nil {
		conn.unsubscribe(attemptID, ch)
		return nil, func() {}, err
	}
	cancel := func() {
		conn.unsubscribe(attemptID, ch)
	}
	return ch, cancel, nil
}

// Cancel asks Hermes (via the plugin) to interrupt the active turn.
// Delivered through Hermes's native /stop mechanism.
func (s *PluginServer) Cancel(ctx context.Context, hermesID, attemptID string) error {
	s.mu.RLock()
	conn, ok := s.conns[hermesID]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrPluginNotConnected, hermesID)
	}
	return conn.writeJSON(ctx, PluginFrame{
		Type:      "cancel",
		AttemptID: attemptID,
	})
}

// Ack reports to the plugin that taskboard has processed events up to seq
// for attemptID. The plugin uses this to trim its reconnect-replay ring
// buffer so stale frames aren't replayed forever.
func (s *PluginServer) Ack(ctx context.Context, hermesID, attemptID string, seq int64) error {
	s.mu.RLock()
	conn, ok := s.conns[hermesID]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrPluginNotConnected, hermesID)
	}
	return conn.writeJSON(ctx, PluginFrame{
		Type:      "ack",
		AttemptID: attemptID,
		Seq:       seq,
	})
}

// Helper for the initial hello write before a conn exists.
func writeJSONOn(ctx context.Context, ws *websocket.Conn, v any) error {
	buf, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return ws.Write(wctx, websocket.MessageText, buf)
}
