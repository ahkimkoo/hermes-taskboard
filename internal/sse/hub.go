// Package sse is a minimal fan-out pub/sub for Server-Sent Events.
// Topics:
//   - "board"                          task status/card-level events
//   - "attempt:<id>"                   per-attempt event stream
package sse

import (
	"encoding/json"
	"sync"
)

type Event struct {
	Seq   uint64         `json:"seq,omitempty"`
	Event string         `json:"event"`
	Data  map[string]any `json:"data,omitempty"`
}

type subscriber struct {
	topic string
	ch    chan Event
}

// Hub fans out events by topic.
type Hub struct {
	mu   sync.RWMutex
	subs map[string]map[*subscriber]struct{}
}

func NewHub() *Hub {
	return &Hub{subs: map[string]map[*subscriber]struct{}{}}
}

// Subscribe returns a channel receiving events for `topic`. Caller must call Unsubscribe.
func (h *Hub) Subscribe(topic string) (<-chan Event, func()) {
	s := &subscriber{topic: topic, ch: make(chan Event, 256)}
	h.mu.Lock()
	if h.subs[topic] == nil {
		h.subs[topic] = map[*subscriber]struct{}{}
	}
	h.subs[topic][s] = struct{}{}
	h.mu.Unlock()
	return s.ch, func() {
		h.mu.Lock()
		delete(h.subs[topic], s)
		h.mu.Unlock()
		close(s.ch)
	}
}

// Publish sends an event to all subscribers on topic (non-blocking; drops if subscriber is full).
func (h *Hub) Publish(topic string, ev Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs[topic] {
		select {
		case s.ch <- ev:
		default:
			// drop slow subscriber
		}
	}
}

// Encode serializes event data to JSON, ignoring errors (best-effort).
func (e Event) Encode() []byte {
	b, _ := json.Marshal(e.Data)
	return b
}
