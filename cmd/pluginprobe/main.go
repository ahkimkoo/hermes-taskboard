// Probe binary — simulates the taskboard WS server so we can validate the
// Hermes plugin in isolation before wiring the real taskboard runner.
//
// Usage:
//
//	go run ./cmd/pluginprobe -listen 127.0.0.1:1900
//
// Then restart Hermes and the plugin should dial us. Drive a round-trip with:
//
//	curl -N http://127.0.0.1:1900/test/send -X POST \
//	     -d '{"attempt_id":"t1","text":"hello hermes"}'
//
// The probe prints every WS frame it sends/receives so you can see the
// Hermes plugin connect, the agent event stream come back, cancel work, etc.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

type probe struct {
	mu     sync.Mutex
	conn   *websocket.Conn // currently-connected plugin, if any
	events chan map[string]any
}

func newProbe() *probe {
	return &probe{events: make(chan map[string]any, 256)}
}

func (p *probe) setConn(c *websocket.Conn) {
	p.mu.Lock()
	if p.conn != nil {
		_ = p.conn.Close(websocket.StatusGoingAway, "new plugin connection")
	}
	p.conn = c
	p.mu.Unlock()
}

func (p *probe) getConn() *websocket.Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn
}

func (p *probe) hWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // localhost only
	})
	if err != nil {
		log.Printf("ws accept: %v", err)
		return
	}
	log.Printf("✓ plugin connected from %s", r.RemoteAddr)
	p.setConn(c)

	// Send hello.
	hello := map[string]any{
		"type":               "hello",
		"client_id":          "probe-1",
		"taskboard_version":  "probe-0.1",
		"ts":                 time.Now().Unix(),
	}
	if err := wsjsonWrite(r.Context(), c, hello); err != nil {
		log.Printf("write hello: %v", err)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer c.Close(websocket.StatusNormalClosure, "")

	// Reader loop.
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			log.Printf("ws read closed: %v", err)
			p.setConn(nil)
			return
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("bad frame: %v", err)
			continue
		}
		log.Printf("← plugin: %s", summarize(msg))
		p.events <- msg
	}
}

func (p *probe) hSend(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var in struct {
		AttemptID string `json:"attempt_id"`
		Text      string `json:"text"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c := p.getConn()
	if c == nil {
		http.Error(w, "no plugin connected", http.StatusServiceUnavailable)
		return
	}
	frame := map[string]any{
		"type":       "send_message",
		"attempt_id": in.AttemptID,
		"text":       in.Text,
		"ts":         time.Now().Unix(),
	}
	if err := wsjsonWrite(r.Context(), c, frame); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("→ plugin: send_message attempt=%s text=%q", in.AttemptID, truncate(in.Text, 80))
	fmt.Fprintln(w, "sent")
}

func (p *probe) hCancel(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var in struct{ AttemptID string `json:"attempt_id"` }
	_ = json.Unmarshal(body, &in)
	c := p.getConn()
	if c == nil {
		http.Error(w, "no plugin connected", http.StatusServiceUnavailable)
		return
	}
	if err := wsjsonWrite(r.Context(), c, map[string]any{
		"type":       "cancel",
		"attempt_id": in.AttemptID,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Fprintln(w, "sent cancel")
}

func (p *probe) hStatus(w http.ResponseWriter, r *http.Request) {
	connected := p.getConn() != nil
	fmt.Fprintf(w, `{"connected":%t}`+"\n", connected)
}

func wsjsonWrite(ctx context.Context, c *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, data)
}

func summarize(m map[string]any) string {
	t, _ := m["type"].(string)
	aid, _ := m["attempt_id"].(string)
	if t == "agent_event" {
		ev, _ := m["event"].(map[string]any)
		kind, _ := ev["kind"].(string)
		content, _ := ev["content"].(string)
		return fmt.Sprintf("agent_event[%s] attempt=%s content=%q", kind, aid, truncate(content, 80))
	}
	if t == "agent_done" {
		sum, _ := m["summary"].(string)
		return fmt.Sprintf("agent_done attempt=%s summary=%q", aid, truncate(sum, 80))
	}
	if aid != "" {
		return fmt.Sprintf("%s attempt=%s", t, aid)
	}
	return t
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func main() {
	listen := flag.String("listen", "127.0.0.1:1900", "listen addr")
	flag.Parse()

	p := newProbe()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/plugin/ws", p.hWS)
	mux.HandleFunc("/test/send", p.hSend)
	mux.HandleFunc("/test/cancel", p.hCancel)
	mux.HandleFunc("/test/status", p.hStatus)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"ok":true}`)
	})

	srv := &http.Server{
		Addr:    *listen,
		Handler: mux,
	}
	go func() {
		log.Printf("pluginprobe listening on %s", *listen)
		log.Printf("  ws endpoint: ws://%s/api/plugin/ws", *listen)
		log.Printf("  send: POST http://%s/test/send '{\"attempt_id\":\"…\",\"text\":\"…\"}'", *listen)
		log.Printf("  cancel: POST http://%s/test/cancel '{\"attempt_id\":\"…\"}'", *listen)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Printf("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
