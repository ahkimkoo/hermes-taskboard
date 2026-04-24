package fsstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/ahkimkoo/hermes-taskboard/internal/store"
)

// FS wraps the data directory layout.
type FS struct {
	Root string
	// seq generators per attempt (monotonic, cross-run)
	seqMu sync.Mutex
	seqs  map[string]*atomic.Uint64
}

func New(root string) *FS {
	return &FS{Root: root, seqs: map[string]*atomic.Uint64{}}
}

// TaskDoc is the richer markdown-backed task document.
type TaskDoc struct {
	ID                 string   `json:"id"`
	Description        string   `json:"description"`
	AttachmentsMeta    []any    `json:"attachments_meta,omitempty"`
	CustomPromptPrefix string   `json:"custom_prompt_prefix,omitempty"`
	Tags               []string `json:"tags,omitempty"` // denormalized for convenience
}

func (f *FS) taskPath(id string) string    { return filepath.Join(f.Root, "task", id+".json") }
func (f *FS) attemptDir(id string) string  { return filepath.Join(f.Root, "attempt", id) }
func (f *FS) metaPath(id string) string    { return filepath.Join(f.attemptDir(id), "meta.json") }
func (f *FS) eventsPath(id string) string  { return filepath.Join(f.attemptDir(id), "events.ndjson") }

// LoadTaskDoc reads data/task/{id}.json; returns empty doc (no error) when missing.
func (f *FS) LoadTaskDoc(id string) (*TaskDoc, error) {
	b, err := os.ReadFile(f.taskPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &TaskDoc{ID: id}, nil
		}
		return nil, err
	}
	d := &TaskDoc{}
	if err := json.Unmarshal(b, d); err != nil {
		return nil, err
	}
	if d.ID == "" {
		d.ID = id
	}
	return d, nil
}

func (f *FS) SaveTaskDoc(d *TaskDoc) error {
	if d.ID == "" {
		return errors.New("task doc id required")
	}
	if err := os.MkdirAll(filepath.Dir(f.taskPath(d.ID)), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(f.taskPath(d.ID), b, 0o600)
}

func (f *FS) DeleteTask(id string) error {
	_ = os.Remove(f.taskPath(id))
	return nil
}

// LoadAttemptMeta returns meta.json; nil (no error) if missing.
func (f *FS) LoadAttemptMeta(id string) (*store.AttemptMeta, error) {
	b, err := os.ReadFile(f.metaPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	m := &store.AttemptMeta{}
	if err := json.Unmarshal(b, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (f *FS) SaveAttemptMeta(m *store.AttemptMeta) error {
	if m.ID == "" {
		return errors.New("attempt id required")
	}
	if err := os.MkdirAll(f.attemptDir(m.ID), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(f.metaPath(m.ID), b, 0o600)
}

func (f *FS) DeleteAttempt(id string) error {
	return os.RemoveAll(f.attemptDir(id))
}

// EventsPath exposes raw path for streaming readers.
func (f *FS) EventsPath(id string) string { return f.eventsPath(id) }

// nextSeq returns the next sequence number for an attempt. Lazily initializes from file.
func (f *FS) nextSeq(attemptID string) uint64 {
	f.seqMu.Lock()
	c, ok := f.seqs[attemptID]
	if !ok {
		c = &atomic.Uint64{}
		// seed from existing file
		s, _ := f.loadMaxSeq(attemptID)
		c.Store(s)
		f.seqs[attemptID] = c
	}
	f.seqMu.Unlock()
	return c.Add(1)
}

func (f *FS) loadMaxSeq(attemptID string) (uint64, error) {
	file, err := os.Open(f.eventsPath(attemptID))
	if err != nil {
		return 0, err
	}
	defer file.Close()
	// Read in chunks from the end to find last seq; for simplicity, scan whole file.
	// Files should be bounded by archive policy.
	dec := json.NewDecoder(file)
	var max uint64
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				break
			}
			// ignore malformed lines
			break
		}
		if s, ok := m["seq"].(float64); ok {
			if uint64(s) > max {
				max = uint64(s)
			}
		}
	}
	return max, nil
}

// AppendEvent appends a JSON event line. It attaches a monotonically increasing `seq` if absent.
func (f *FS) AppendEvent(attemptID string, evt map[string]any) (uint64, error) {
	if err := os.MkdirAll(f.attemptDir(attemptID), 0o700); err != nil {
		return 0, err
	}
	seq := f.nextSeq(attemptID)
	evt["seq"] = seq
	line, err := json.Marshal(evt)
	if err != nil {
		return 0, err
	}
	file, err := os.OpenFile(f.eventsPath(attemptID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	if _, err := file.Write(append(line, '\n')); err != nil {
		return 0, err
	}
	return seq, nil
}

// ReadEventsRange returns events with seq > sinceSeq, up to limit (0 = all).
func (f *FS) ReadEventsRange(attemptID string, sinceSeq uint64, limit int) ([]map[string]any, error) {
	file, err := os.Open(f.eventsPath(attemptID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()
	dec := json.NewDecoder(file)
	var out []map[string]any
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				break
			}
			return out, fmt.Errorf("decode: %w", err)
		}
		if s, ok := m["seq"].(float64); ok {
			if uint64(s) <= sinceSeq {
				continue
			}
		}
		out = append(out, m)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ReadEventsTail returns the last `n` events (in ascending order).
// Chunk-reads backwards from EOF instead of loading the whole log:
// for a 50 MB events.ndjson asking for tail=30 the naïve approach
// parsed every line (hundreds of ms); this reads ~64 KB from the end,
// finds the last n line boundaries, and decodes just those.
func (f *FS) ReadEventsTail(attemptID string, n int) ([]map[string]any, error) {
	if n <= 0 {
		return nil, nil
	}
	file, err := os.Open(f.eventsPath(attemptID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size == 0 {
		return nil, nil
	}

	const chunk = 64 * 1024
	var buf []byte
	var offset = size
	for offset > 0 && countNewlines(buf) < n+1 {
		readSize := int64(chunk)
		if readSize > offset {
			readSize = offset
		}
		offset -= readSize
		segment := make([]byte, readSize)
		if _, err := file.ReadAt(segment, offset); err != nil && err != io.EOF {
			return nil, err
		}
		buf = append(segment, buf...)
	}

	// Drop a potentially-partial first line when we didn't read from 0.
	start := 0
	if offset > 0 {
		if nl := indexNewline(buf); nl >= 0 {
			start = nl + 1
		}
	}

	events := decodeNDJSON(buf[start:])
	if len(events) > n {
		events = events[len(events)-n:]
	}
	return events, nil
}

func countNewlines(b []byte) int {
	c := 0
	for _, x := range b {
		if x == '\n' {
			c++
		}
	}
	return c
}

func indexNewline(b []byte) int {
	for i, x := range b {
		if x == '\n' {
			return i
		}
	}
	return -1
}

func decodeNDJSON(b []byte) []map[string]any {
	dec := json.NewDecoder(bytes.NewReader(b))
	var out []map[string]any
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			break // EOF or a malformed trailing record — either way, stop
		}
		out = append(out, m)
	}
	return out
}

// ReadEventsBefore returns up to `limit` events with seq < beforeSeq (ascending).
func (f *FS) ReadEventsBefore(attemptID string, beforeSeq uint64, limit int) ([]map[string]any, error) {
	all, err := f.ReadEventsRange(attemptID, 0, 0)
	if err != nil {
		return nil, err
	}
	var filtered []map[string]any
	for _, m := range all {
		if s, ok := m["seq"].(float64); ok {
			if uint64(s) < beforeSeq {
				filtered = append(filtered, m)
			}
		}
	}
	if len(filtered) > limit && limit > 0 {
		return filtered[len(filtered)-limit:], nil
	}
	return filtered, nil
}

// ---------- helpers ----------

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
