package store

// Per-Store LRU cache for GetTask. Opening a task card used to cost
// 4 SQL round-trips (task row, tags, deps, attempt counts) + the
// fsstore JSON read every time. For a board that gets reopened
// dozens of times a session that's pure waste — most reads return
// the same bytes. This cache keeps the most-recently-touched 200
// fully-populated Task snapshots per user; writes invalidate by id.
//
// Design notes:
//   - cache lives inside Store, so per-user isolation comes for free
//     (each user's Store.Manager.Get() returns a distinct cache).
//   - we cache fully-populated Task (tags + deps + attempt counts),
//     not just the row — that's where the 4 queries went.
//   - `get` returns a deep-enough copy (clones Tags + Dependencies
//     slices) so mutating callers like hPatchTask don't stomp the
//     cached entry.
//   - Position is cached too; MoveTask invalidates on writes so stale
//     positions never leak into the single-task view.

import (
	"container/list"
	"sync"
)

const defaultTaskCacheMax = 200

type taskCacheEntry struct {
	id   string
	task *Task
}

type taskCache struct {
	mu   sync.Mutex
	max  int
	byID map[string]*list.Element // key → LRU node
	ord  *list.List               // most-recent at Front()
}

func newTaskCache(max int) *taskCache {
	if max <= 0 {
		max = defaultTaskCacheMax
	}
	return &taskCache{
		max:  max,
		byID: map[string]*list.Element{},
		ord:  list.New(),
	}
}

// get returns a copy of the cached Task or nil on miss. On hit the
// entry is moved to the front so the LRU ordering stays honest.
func (c *taskCache) get(id string) *Task {
	if c == nil || id == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byID[id]
	if !ok {
		return nil
	}
	c.ord.MoveToFront(e)
	return cloneTask(e.Value.(*taskCacheEntry).task)
}

// put stores a copy of t. Evicts the oldest entry when over capacity.
func (c *taskCache) put(id string, t *Task) {
	if c == nil || id == "" || t == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.byID[id]; ok {
		e.Value.(*taskCacheEntry).task = cloneTask(t)
		c.ord.MoveToFront(e)
		return
	}
	entry := &taskCacheEntry{id: id, task: cloneTask(t)}
	c.byID[id] = c.ord.PushFront(entry)
	for c.ord.Len() > c.max {
		tail := c.ord.Back()
		if tail == nil {
			break
		}
		c.ord.Remove(tail)
		delete(c.byID, tail.Value.(*taskCacheEntry).id)
	}
}

// invalidate drops the entry for id. Safe to call for ids never cached.
func (c *taskCache) invalidate(id string) {
	if c == nil || id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.byID[id]; ok {
		c.ord.Remove(e)
		delete(c.byID, id)
	}
}

func cloneTask(t *Task) *Task {
	if t == nil {
		return nil
	}
	cp := *t
	if t.Tags != nil {
		cp.Tags = append([]string(nil), t.Tags...)
	}
	if t.Dependencies != nil {
		cp.Dependencies = append([]TaskDep(nil), t.Dependencies...)
	}
	return &cp
}
