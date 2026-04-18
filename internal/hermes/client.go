// Package hermes is a thin wrapper around the Hermes OpenAI-compatible API server.
// Endpoints referenced (all HTTP):
//   - POST /v1/responses                              stateful chat with `conversation` (SSE stream)
//   - POST /v1/runs                                   async run (OpenAI Runs compat)
//   - GET  /v1/runs/{run_id}/events                   SSE stream of run events
//   - GET  /v1/models                                 list configured model profiles
//   - GET  /health, /health/detailed                  health check
// The client deliberately stays schema-light: Hermes event fields are forwarded as opaque maps.
package hermes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ResponseRequest is the input to CreateResponse.
type ResponseRequest struct {
	Conversation string
	Model        string
	Input        string
	SystemPrompt string
	Stream       bool
}

// ResponseCreateResult is the minimal info we need from a response creation.
type ResponseCreateResult struct {
	ResponseID string
	RunID      string
	// RawStream carries the SSE stream body; nil if Stream was false.
	RawStream io.ReadCloser
}

// Event is a single SSE event forwarded upstream.
type Event struct {
	Data map[string]any
}

type HealthStatus struct {
	OK      bool           `json:"ok"`
	Details map[string]any `json:"details,omitempty"`
}

type Model struct {
	ID      string         `json:"id"`
	Object  string         `json:"object,omitempty"`
	Raw     map[string]any `json:"-"`
}

// Client talks to one Hermes API server.
type Client struct {
	ServerID string
	BaseURL  string
	APIKey   string
	HTTP     *http.Client
}

func NewClient(serverID, baseURL, apiKey string) *Client {
	return &Client{
		ServerID: serverID,
		BaseURL:  strings.TrimRight(baseURL, "/"),
		APIKey:   apiKey,
		HTTP: &http.Client{
			Timeout: 0, // streams have their own ctx
			Transport: &http.Transport{
				MaxIdleConns:        32,
				MaxConnsPerHost:     16,
				IdleConnTimeout:     90 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
	}
}

func (c *Client) newReq(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	r, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if c.APIKey != "" {
		r.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	return r, nil
}

// Health queries /health/detailed.
func (c *Client) Health(ctx context.Context) (HealthStatus, error) {
	r, err := c.newReq(ctx, "GET", "/health/detailed", nil)
	if err != nil {
		return HealthStatus{}, err
	}
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return HealthStatus{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return HealthStatus{OK: false}, fmt.Errorf("hermes health: %s: %s", resp.Status, string(body))
	}
	out := HealthStatus{OK: true}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err == nil {
		out.Details = raw
	}
	return out, nil
}

// Models queries /v1/models.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	r, err := c.newReq(ctx, "GET", "/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("hermes models: %s: %s", resp.Status, string(body))
	}
	var wrap struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return nil, err
	}
	out := []Model{}
	for _, m := range wrap.Data {
		id, _ := m["id"].(string)
		obj, _ := m["object"].(string)
		out = append(out, Model{ID: id, Object: obj, Raw: m})
	}
	return out, nil
}

// CreateResponse posts to /v1/responses. If Stream=true, the response body is a
// text/event-stream and returned via RawStream for the caller to consume.
func (c *Client) CreateResponse(ctx context.Context, req ResponseRequest) (*ResponseCreateResult, error) {
	body := map[string]any{
		"model":        req.Model,
		"input":        req.Input,
		"stream":       req.Stream,
	}
	if req.Conversation != "" {
		body["conversation"] = req.Conversation
	}
	if req.SystemPrompt != "" {
		body["instructions"] = req.SystemPrompt
	}

	r, err := c.newReq(ctx, "POST", "/v1/responses", body)
	if err != nil {
		return nil, err
	}
	if req.Stream {
		r.Header.Set("Accept", "text/event-stream")
	}
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("hermes responses: %s: %s", resp.Status, string(b))
	}
	if !req.Stream {
		defer resp.Body.Close()
		var data struct {
			ID   string `json:"id"`
			RunID string `json:"run_id,omitempty"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			return nil, err
		}
		return &ResponseCreateResult{ResponseID: data.ID, RunID: data.RunID}, nil
	}
	return &ResponseCreateResult{RawStream: resp.Body}, nil
}

// StreamRunEvents subscribes to /v1/runs/{runID}/events as SSE and forwards to `out`.
// The function returns when the stream is closed or ctx is done.
func (c *Client) StreamRunEvents(ctx context.Context, runID string, out chan<- Event) error {
	r, err := c.newReq(ctx, "GET", "/v1/runs/"+runID+"/events", nil)
	if err != nil {
		return err
	}
	r.Header.Set("Accept", "text/event-stream")
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("hermes run events: %s: %s", resp.Status, string(b))
	}
	return consumeSSE(ctx, resp.Body, out)
}

// StreamResponseBody reads an SSE body directly (when CreateResponse Stream=true returned RawStream).
func StreamResponseBody(ctx context.Context, body io.ReadCloser, out chan<- Event) error {
	defer body.Close()
	return consumeSSE(ctx, body, out)
}

// CancelRun calls POST /v1/runs/{id}/cancel.
func (c *Client) CancelRun(ctx context.Context, runID string) error {
	r, err := c.newReq(ctx, "POST", "/v1/runs/"+runID+"/cancel", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode != 404 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("hermes cancel: %s: %s", resp.Status, string(b))
	}
	return nil
}

// consumeSSE parses `data:` lines and pushes JSON objects to out.
func consumeSSE(ctx context.Context, body io.Reader, out chan<- Event) error {
	br := bufio.NewReader(body)
	var buf strings.Builder
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := br.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case trimmed == "":
			// dispatch accumulated event
			if buf.Len() > 0 {
				raw := buf.String()
				buf.Reset()
				if raw == "[DONE]" {
					return nil
				}
				var obj map[string]any
				if jerr := json.Unmarshal([]byte(raw), &obj); jerr == nil {
					select {
					case out <- Event{Data: obj}:
					case <-ctx.Done():
						return ctx.Err()
					}
				} else {
					// non-JSON data: wrap as a plain event
					select {
					case out <- Event{Data: map[string]any{"raw": raw}}:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
		case strings.HasPrefix(trimmed, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if buf.Len() > 0 {
				buf.WriteString("\n")
			}
			buf.WriteString(data)
		case strings.HasPrefix(trimmed, ":"):
			// comment / keepalive
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

// ---------------- Pool ----------------

// Pool routes requests by server_id; rebuilds on config reload.
type Pool struct {
	mu      sync.RWMutex
	clients map[string]*Client
	defID   string
}

func NewPool() *Pool { return &Pool{clients: map[string]*Client{}} }

type PoolEntry struct {
	ID        string
	BaseURL   string
	APIKey    string
	IsDefault bool
}

// Reload replaces the pool atomically with the provided entries.
func (p *Pool) Reload(entries []PoolEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	next := map[string]*Client{}
	for _, e := range entries {
		// reuse existing client to keep conn pool warm when URL+key match
		if cur, ok := p.clients[e.ID]; ok && cur.BaseURL == strings.TrimRight(e.BaseURL, "/") && cur.APIKey == e.APIKey {
			next[e.ID] = cur
		} else {
			next[e.ID] = NewClient(e.ID, e.BaseURL, e.APIKey)
		}
		if e.IsDefault {
			p.defID = e.ID
		}
	}
	p.clients = next
}

func (p *Pool) Get(id string) (*Client, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	c, ok := p.clients[id]
	if !ok {
		return nil, fmt.Errorf("hermes server %q not registered", id)
	}
	return c, nil
}

func (p *Pool) Default() (*Client, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.defID == "" {
		for id, c := range p.clients {
			p.defID = id
			return c, nil
		}
		return nil, errors.New("no hermes servers configured")
	}
	c, ok := p.clients[p.defID]
	if !ok {
		return nil, fmt.Errorf("default server %q missing", p.defID)
	}
	return c, nil
}

func (p *Pool) DefaultID() string { p.mu.RLock(); defer p.mu.RUnlock(); return p.defID }
