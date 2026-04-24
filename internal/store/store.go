package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Store is the per-user repository facade. A Store wraps the SQLite
// database rooted at data/{username}/db/taskboard.db — all rows it
// returns belong to that user by construction. See store.Manager for
// the multi-user lookup layer.
type Store struct {
	DB *sql.DB
	tc *taskCache // LRU snapshot of fully-populated Task rows
}

func New(db *sql.DB) *Store {
	return &Store{DB: db, tc: newTaskCache(defaultTaskCacheMax)}
}

var ErrNotFound = errors.New("not found")

// Close releases the underlying DB handle. Safe to call nil.
func (s *Store) Close() error {
	if s == nil || s.DB == nil {
		return nil
	}
	return s.DB.Close()
}

// ----- tasks -----

func (s *Store) CreateTask(ctx context.Context, t *Task) error {
	if t.ID == "" {
		return errors.New("task id required")
	}
	if t.Status == "" {
		t.Status = StatusDraft
	}
	if t.Priority == 0 {
		t.Priority = 3
	}
	if t.TriggerMode == "" {
		t.TriggerMode = TriggerAuto
	}
	now := time.Now()
	t.CreatedAt, t.UpdatedAt = now, now
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var maxPos sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(position) FROM tasks WHERE status=?`, string(t.Status)).Scan(&maxPos); err != nil {
		return err
	}
	t.Position = maxPos.Int64 + 1024
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tasks(id,title,status,priority,trigger_mode,preferred_server,preferred_model,created_at,updated_at,description_excerpt,position)
         VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Title, string(t.Status), t.Priority, string(t.TriggerMode),
		nullStr(t.PreferredServer), nullStr(t.PreferredModel),
		now.UnixMilli(), now.UnixMilli(), t.DescriptionExcerpt, t.Position,
	); err != nil {
		return err
	}
	if err := writeTags(ctx, tx, t.ID, t.Tags); err != nil {
		return err
	}
	if err := writeDeps(ctx, tx, t.ID, t.Dependencies); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateTask(ctx context.Context, t *Task) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now()
	t.UpdatedAt = now
	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET title=?,priority=?,trigger_mode=?,preferred_server=?,preferred_model=?,updated_at=?,description_excerpt=? WHERE id=?`,
		t.Title, t.Priority, string(t.TriggerMode),
		nullStr(t.PreferredServer), nullStr(t.PreferredModel),
		now.UnixMilli(), t.DescriptionExcerpt, t.ID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_tags WHERE task_id=?`, t.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_deps WHERE task_id=?`, t.ID); err != nil {
		return err
	}
	if err := writeTags(ctx, tx, t.ID, t.Tags); err != nil {
		return err
	}
	if err := writeDeps(ctx, tx, t.ID, t.Dependencies); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.tc.invalidate(t.ID)
	return nil
}

func (s *Store) SetTaskStatus(ctx context.Context, id string, to TaskStatus) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var maxPos sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(position) FROM tasks WHERE status=?`, string(to)).Scan(&maxPos); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status=?, updated_at=?, position=? WHERE id=?`,
		string(to), time.Now().UnixMilli(), maxPos.Int64+1024, id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.tc.invalidate(id)
	return nil
}

// MoveTask relocates a task into `to` column at a specific slot.
// See the original implementation for the full contract — unchanged in
// the per-user layout since there's never cross-user drag.
func (s *Store) MoveTask(ctx context.Context, id string, to TaskStatus, afterID, beforeID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	type row struct {
		id  string
		pos int64
	}
	var neighbors []row
	rows, err := tx.QueryContext(ctx, `SELECT id, position FROM tasks WHERE status=? AND id != ? ORDER BY position ASC`, string(to), id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.pos); err != nil {
			rows.Close()
			return err
		}
		neighbors = append(neighbors, r)
	}
	rows.Close()

	findIdx := func(tid string) int {
		for i, n := range neighbors {
			if n.id == tid {
				return i
			}
		}
		return -1
	}

	var lo, hi int64
	var haveLo, haveHi bool
	if afterID != "" {
		if i := findIdx(afterID); i >= 0 {
			lo = neighbors[i].pos
			haveLo = true
			if i+1 < len(neighbors) {
				hi = neighbors[i+1].pos
				haveHi = true
			}
		}
	} else if beforeID != "" {
		if i := findIdx(beforeID); i >= 0 {
			hi = neighbors[i].pos
			haveHi = true
			if i > 0 {
				lo = neighbors[i-1].pos
				haveLo = true
			}
		}
	} else {
		if len(neighbors) > 0 {
			lo = neighbors[len(neighbors)-1].pos
			haveLo = true
		}
	}

	var newPos int64
	switch {
	case haveLo && haveHi:
		if hi-lo <= 1 {
			for i, n := range neighbors {
				if _, err := tx.ExecContext(ctx, `UPDATE tasks SET position=? WHERE id=?`, int64(i+1)*1024, n.id); err != nil {
					return err
				}
			}
			for i := range neighbors {
				neighbors[i].pos = int64(i+1) * 1024
			}
			if afterID != "" {
				i := findIdx(afterID)
				newPos = neighbors[i].pos + 512
			} else if beforeID != "" {
				i := findIdx(beforeID)
				if i > 0 {
					newPos = (neighbors[i-1].pos + neighbors[i].pos) / 2
				} else {
					newPos = neighbors[i].pos - 512
				}
			} else {
				newPos = neighbors[len(neighbors)-1].pos + 1024
			}
		} else {
			newPos = (lo + hi) / 2
		}
	case haveLo:
		newPos = lo + 1024
	case haveHi:
		newPos = hi - 1024
	default:
		newPos = 1024
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status=?, position=?, updated_at=? WHERE id=?`,
		string(to), newPos, time.Now().UnixMilli(), id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.tc.invalidate(id)
	return nil
}

func (s *Store) DeleteTask(ctx context.Context, id string) ([]string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM attempts WHERE task_id=?`, id)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var aid string
		if err := rows.Scan(&aid); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, aid)
	}
	rows.Close()

	for _, q := range []string{
		`DELETE FROM task_tags WHERE task_id=?`,
		`DELETE FROM task_deps WHERE task_id=?`,
		`DELETE FROM task_deps WHERE depends_on=?`,
		`DELETE FROM attempts WHERE task_id=?`,
		`DELETE FROM task_schedules WHERE task_id=?`,
		`DELETE FROM tasks WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.tc.invalidate(id)
	return ids, nil
}

// GetTask loads a task + tags + deps (description still in fsstore).
// Snapshots are cached per-Store in a 200-entry LRU so the common
// "operator clicks the same card again" path avoids 4 round-trips.
// Writes invalidate by id (see SetTaskStatus / UpdateTask / ...).
func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
	if t := s.tc.get(id); t != nil {
		return t, nil
	}
	row := s.DB.QueryRowContext(ctx,
		`SELECT id,title,status,priority,trigger_mode,preferred_server,preferred_model,created_at,updated_at,description_excerpt,position
         FROM tasks WHERE id=?`, id)
	t, err := scanTask(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := s.loadTaskTagsDeps(ctx, t); err != nil {
		return nil, err
	}
	cnt, active, ni, err := s.attemptCounts(ctx, id)
	if err != nil {
		return nil, err
	}
	t.AttemptCount, t.ActiveAttempts, t.NeedsInputAttempts = cnt, active, ni
	s.tc.put(id, t)
	// Return a fresh copy (cache entry should stay immutable to callers).
	return cloneTask(t), nil
}

// DeleteAttempt removes a single attempt row by id. Invalidates the
// owning task's cache entry so the next GetTask sees fresh counts.
func (s *Store) DeleteAttempt(ctx context.Context, id string) error {
	var taskID string
	_ = s.DB.QueryRowContext(ctx, `SELECT task_id FROM attempts WHERE id=?`, id).Scan(&taskID)
	res, err := s.DB.ExecContext(ctx, `DELETE FROM attempts WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	s.tc.invalidate(taskID)
	return nil
}

type TaskFilter struct {
	Status string
	Tag    string
	Query  string
	Limit  int
	Offset int
}

// ListTasks returns the user's tasks with tags/deps/attempt-counts
// already populated. To avoid the old N+1 shape (one SELECT per task
// × 3 facets), we pull all four datasets in at most four queries and
// zip them together in Go:
//
//   1. the task rows that match the filter
//   2. task_tags rows for those task ids
//   3. task_deps rows for those task ids
//   4. attempt counts GROUP BY task_id
//
// For 20 tasks this cuts ~60 queries down to ~4. The board SSE stream
// fires this path on every state change, so the saving is real.
func (s *Store) ListTasks(ctx context.Context, f TaskFilter) ([]*Task, error) {
	sb := &strings.Builder{}
	sb.WriteString(`SELECT t.id,t.title,t.status,t.priority,t.trigger_mode,t.preferred_server,t.preferred_model,t.created_at,t.updated_at,t.description_excerpt,t.position FROM tasks t`)
	args := []any{}
	where := []string{}
	if f.Tag != "" {
		sb.WriteString(` JOIN task_tags tt ON tt.task_id=t.id`)
		where = append(where, "tt.tag=?")
		args = append(args, f.Tag)
	}
	if f.Status != "" {
		where = append(where, "t.status=?")
		args = append(args, f.Status)
	}
	if f.Query != "" {
		where = append(where, "(t.title LIKE ? OR COALESCE(t.description_excerpt,'') LIKE ?)")
		q := "%" + f.Query + "%"
		args = append(args, q, q)
	}
	if len(where) > 0 {
		sb.WriteString(" WHERE " + strings.Join(where, " AND "))
	}
	sb.WriteString(" ORDER BY t.status, t.position ASC")
	if f.Limit > 0 {
		sb.WriteString(fmt.Sprintf(" LIMIT %d OFFSET %d", f.Limit, f.Offset))
	}
	rows, err := s.DB.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*Task
	byID := map[string]*Task{}
	for rows.Next() {
		t, err := scanTask(rows.Scan)
		if err != nil {
			return nil, err
		}
		list = append(list, t)
		byID[t.ID] = t
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return list, nil
	}

	ids := make([]string, 0, len(list))
	idArgs := make([]any, 0, len(list))
	for _, t := range list {
		ids = append(ids, t.ID)
		idArgs = append(idArgs, t.ID)
	}
	placeholders := "?" + strings.Repeat(",?", len(ids)-1)

	// 2. tags
	tagRows, err := s.DB.QueryContext(ctx,
		`SELECT task_id, tag FROM task_tags WHERE task_id IN (`+placeholders+`) ORDER BY task_id, tag`,
		idArgs...)
	if err != nil {
		return nil, err
	}
	for tagRows.Next() {
		var tid, tag string
		if err := tagRows.Scan(&tid, &tag); err != nil {
			tagRows.Close()
			return nil, err
		}
		if t, ok := byID[tid]; ok {
			t.Tags = append(t.Tags, tag)
		}
	}
	tagRows.Close()

	// 3. deps
	depRows, err := s.DB.QueryContext(ctx,
		`SELECT task_id, depends_on, required_state FROM task_deps WHERE task_id IN (`+placeholders+`)`,
		idArgs...)
	if err != nil {
		return nil, err
	}
	for depRows.Next() {
		var tid string
		var d TaskDep
		if err := depRows.Scan(&tid, &d.TaskID, &d.RequiredState); err != nil {
			depRows.Close()
			return nil, err
		}
		if t, ok := byID[tid]; ok {
			t.Dependencies = append(t.Dependencies, d)
		}
	}
	depRows.Close()

	// 4. attempt counts
	cntRows, err := s.DB.QueryContext(ctx, `
		SELECT task_id,
		       COUNT(*),
		       SUM(CASE WHEN state IN ('queued','running','needs_input') THEN 1 ELSE 0 END),
		       SUM(CASE WHEN state = 'needs_input' THEN 1 ELSE 0 END)
		FROM attempts WHERE task_id IN (`+placeholders+`) GROUP BY task_id`,
		idArgs...)
	if err != nil {
		return nil, err
	}
	for cntRows.Next() {
		var tid string
		var total, active, ni sql.NullInt64
		if err := cntRows.Scan(&tid, &total, &active, &ni); err != nil {
			cntRows.Close()
			return nil, err
		}
		if t, ok := byID[tid]; ok {
			t.AttemptCount = int(total.Int64)
			t.ActiveAttempts = int(active.Int64)
			t.NeedsInputAttempts = int(ni.Int64)
		}
	}
	cntRows.Close()

	// Prime the per-task cache — every row we returned is a
	// fully-populated snapshot, the same shape GetTask produces. On
	// unfiltered list reloads this pre-warms the cache for
	// card-open, turning typical clicks into hits.
	for _, t := range list {
		s.tc.put(t.ID, t)
	}
	return list, nil
}

func (s *Store) TaskIDs(ctx context.Context, status TaskStatus) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM tasks WHERE status=?`, string(status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// AllDependenciesDone returns whether every dep of taskID is satisfied,
// per its recorded required_state.
func (s *Store) AllDependenciesDone(ctx context.Context, taskID string) (bool, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT COUNT(*) FROM task_deps d
		JOIN tasks t ON t.id = d.depends_on
		WHERE d.task_id = ?
		  AND NOT (
		    (d.required_state = 'verify' AND t.status IN ('verify','done','archive'))
		    OR
		    (d.required_state = 'done'   AND t.status IN ('done','archive'))
		  )
	`, taskID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return true, nil
	}
	var n int
	if err := rows.Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
}

// ----- attempts -----

func (s *Store) CreateAttempt(ctx context.Context, a *Attempt) error {
	now := time.Now()
	a.StartedAt = &now
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO attempts(id,task_id,server_id,model,state,started_at,ended_at) VALUES(?,?,?,?,?,?,NULL)`,
		a.ID, a.TaskID, a.ServerID, a.Model, string(a.State), now.UnixMilli(),
	)
	if err == nil {
		s.tc.invalidate(a.TaskID)
	}
	return err
}

func (s *Store) UpdateAttemptState(ctx context.Context, id string, state AttemptState) error {
	now := time.Now()
	var endedAt any
	if state == AttemptCompleted || state == AttemptFailed || state == AttemptCancelled {
		endedAt = now.UnixMilli()
	}
	_, err := s.DB.ExecContext(ctx,
		`UPDATE attempts SET state=?, ended_at=COALESCE(?, ended_at) WHERE id=?`,
		string(state), endedAt, id,
	)
	if err == nil {
		// Invalidate the owning task's cache so attempt-count changes
		// show up on the next card open. Cheap extra query.
		var taskID string
		if qerr := s.DB.QueryRowContext(ctx, `SELECT task_id FROM attempts WHERE id=?`, id).Scan(&taskID); qerr == nil {
			s.tc.invalidate(taskID)
		}
	}
	return err
}

func (s *Store) GetAttempt(ctx context.Context, id string) (*Attempt, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT id,task_id,server_id,model,state,started_at,ended_at FROM attempts WHERE id=?`, id)
	return scanAttempt(row.Scan)
}

// ListAttemptsForTask returns attempts in chronological order (oldest
// first). When limit > 0, returns the most recent `limit` attempts
// whose started_at is strictly less than `before` (or latest when
// before is zero). Use the returned hasMore flag to decide whether a
// "load earlier" UI needs to show.
//
// Pagination is keyed by started_at (millis). Clients page by setting
// before = oldest loaded attempt's started_at.
func (s *Store) ListAttemptsForTask(ctx context.Context, taskID string, limit int, beforeMs int64) (attempts []*Attempt, hasMore bool, err error) {
	q := `SELECT id,task_id,server_id,model,state,started_at,ended_at FROM attempts WHERE task_id=?`
	args := []any{taskID}
	if beforeMs > 0 {
		q += ` AND started_at < ?`
		args = append(args, beforeMs)
	}
	q += ` ORDER BY started_at DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit+1) // over-fetch by 1 to detect hasMore
	}
	rows, qerr := s.DB.QueryContext(ctx, q, args...)
	if qerr != nil {
		return nil, false, qerr
	}
	defer rows.Close()
	var out []*Attempt
	for rows.Next() {
		a, err := scanAttempt(rows.Scan)
		if err != nil {
			return nil, false, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
		hasMore = true
	}
	// Reverse to chronological (oldest → newest) for display.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, hasMore, nil
}

func (s *Store) ListActiveAttempts(ctx context.Context) ([]*Attempt, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id,task_id,server_id,model,state,started_at,ended_at FROM attempts WHERE state IN ('queued','running','needs_input')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Attempt
	for rows.Next() {
		a, err := scanAttempt(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AllAttemptsTerminal returns true if every attempt for task is in terminal state.
func (s *Store) AllAttemptsTerminal(ctx context.Context, taskID string) (bool, int, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT COUNT(*), SUM(CASE WHEN state IN ('completed','failed','cancelled') THEN 1 ELSE 0 END) FROM attempts WHERE task_id=?`, taskID)
	if err != nil {
		return false, 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		return false, 0, nil
	}
	var total, term sql.NullInt64
	if err := rows.Scan(&total, &term); err != nil {
		return false, 0, err
	}
	if total.Int64 == 0 {
		return false, 0, nil
	}
	return total.Int64 == term.Int64, int(total.Int64), nil
}

// CountActive returns active attempt counts scoped to THIS store.
// Global counts (for global_max_concurrent) are aggregated by the
// Manager across all per-user stores.
func (s *Store) CountActive(ctx context.Context, serverID, model string) (global, byServer, byPair int, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts WHERE state IN ('queued','running','needs_input')`).Scan(&global)
	if err != nil {
		return
	}
	err = s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM attempts WHERE state IN ('queued','running','needs_input') AND server_id=?`, serverID).Scan(&byServer)
	if err != nil {
		return
	}
	err = s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM attempts WHERE state IN ('queued','running','needs_input') AND server_id=? AND model=?`,
		serverID, model).Scan(&byPair)
	return
}

// ----- helpers -----

func (s *Store) loadTaskTagsDeps(ctx context.Context, t *Task) error {
	rows, err := s.DB.QueryContext(ctx, `SELECT tag FROM task_tags WHERE task_id=? ORDER BY tag`, t.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			rows.Close()
			return err
		}
		t.Tags = append(t.Tags, tag)
	}
	rows.Close()

	rows, err = s.DB.QueryContext(ctx, `SELECT depends_on, required_state FROM task_deps WHERE task_id=?`, t.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var d TaskDep
		if err := rows.Scan(&d.TaskID, &d.RequiredState); err != nil {
			return err
		}
		t.Dependencies = append(t.Dependencies, d)
	}
	return rows.Err()
}

func (s *Store) attemptCounts(ctx context.Context, taskID string) (total, active, needsInput int, err error) {
	var totalN, activeN, niN sql.NullInt64
	err = s.DB.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       SUM(CASE WHEN state IN ('queued','running','needs_input') THEN 1 ELSE 0 END),
		       SUM(CASE WHEN state = 'needs_input' THEN 1 ELSE 0 END)
		FROM attempts WHERE task_id=?`,
		taskID,
	).Scan(&totalN, &activeN, &niN)
	if err != nil {
		return
	}
	total = int(totalN.Int64)
	active = int(activeN.Int64)
	needsInput = int(niN.Int64)
	return
}

func writeTags(ctx context.Context, tx *sql.Tx, taskID string, tags []string) error {
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO task_tags(task_id,tag) VALUES(?,?)`, taskID, tag); err != nil {
			return err
		}
	}
	return nil
}

func writeDeps(ctx context.Context, tx *sql.Tx, taskID string, deps []TaskDep) error {
	for _, d := range deps {
		if d.TaskID == "" || d.TaskID == taskID {
			continue
		}
		state := strings.ToLower(strings.TrimSpace(d.RequiredState))
		if state != "verify" && state != "done" {
			state = "done"
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO task_deps(task_id, depends_on, required_state) VALUES(?,?,?)`,
			taskID, d.TaskID, state); err != nil {
			return err
		}
	}
	return nil
}

type scanner func(dest ...any) error

func scanTask(scan scanner) (*Task, error) {
	var (
		t                Task
		createdAt, updAt int64
		prefSrv, prefMdl sql.NullString
		excerpt          sql.NullString
		position         int64
	)
	var status, trigger string
	if err := scan(&t.ID, &t.Title, &status, &t.Priority, &trigger, &prefSrv, &prefMdl, &createdAt, &updAt, &excerpt, &position); err != nil {
		return nil, err
	}
	t.Status = TaskStatus(status)
	t.TriggerMode = TriggerMode(trigger)
	t.PreferredServer = prefSrv.String
	t.PreferredModel = prefMdl.String
	t.DescriptionExcerpt = excerpt.String
	t.CreatedAt = time.UnixMilli(createdAt)
	t.UpdatedAt = time.UnixMilli(updAt)
	t.Position = position
	t.Tags = []string{}
	t.Dependencies = []TaskDep{}
	return &t, nil
}

func scanAttempt(scan scanner) (*Attempt, error) {
	var a Attempt
	var start, end sql.NullInt64
	var state string
	if err := scan(&a.ID, &a.TaskID, &a.ServerID, &a.Model, &state, &start, &end); err != nil {
		return nil, err
	}
	a.State = AttemptState(state)
	if start.Valid {
		t := time.UnixMilli(start.Int64)
		a.StartedAt = &t
	}
	if end.Valid {
		t := time.UnixMilli(end.Int64)
		a.EndedAt = &t
	}
	return &a, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------------- task_schedules ----------------

func (s *Store) ListSchedulesForTask(ctx context.Context, taskID string) ([]Schedule, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, task_id, kind, spec, COALESCE(note,''), enabled, last_run_at, next_run_at
		 FROM task_schedules WHERE task_id=? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		s, err := scanSchedule(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (s *Store) CreateSchedule(ctx context.Context, sch *Schedule) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO task_schedules(id, task_id, kind, spec, note, enabled, last_run_at, next_run_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		sch.ID, sch.TaskID, string(sch.Kind), sch.Spec, sch.Note,
		boolInt(sch.Enabled),
		msOrNil(sch.LastRunAt), msOrNil(sch.NextRunAt),
	)
	return err
}

func (s *Store) UpdateSchedule(ctx context.Context, sch *Schedule) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE task_schedules SET kind=?, spec=?, note=?, enabled=?, last_run_at=?, next_run_at=? WHERE id=?`,
		string(sch.Kind), sch.Spec, sch.Note, boolInt(sch.Enabled),
		msOrNil(sch.LastRunAt), msOrNil(sch.NextRunAt),
		sch.ID,
	)
	return err
}

func (s *Store) SetScheduleEnabled(ctx context.Context, id string, enabled bool) error {
	var q string
	if enabled {
		q = `UPDATE task_schedules SET enabled=1 WHERE id=?`
	} else {
		q = `UPDATE task_schedules SET enabled=0, next_run_at=NULL WHERE id=?`
	}
	_, err := s.DB.ExecContext(ctx, q, id)
	return err
}

func (s *Store) GetSchedule(ctx context.Context, id string) (*Schedule, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT id, task_id, kind, spec, COALESCE(note,''), enabled, last_run_at, next_run_at
		 FROM task_schedules WHERE id=?`, id)
	sch, err := scanSchedule(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &sch, nil
}

func (s *Store) DeleteSchedule(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM task_schedules WHERE id=?`, id)
	return err
}

func (s *Store) ListEnabledNullNextSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, task_id, kind, spec, COALESCE(note,''), enabled, last_run_at, next_run_at
		 FROM task_schedules
		 WHERE enabled=1 AND next_run_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		sch, err := scanSchedule(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, sch)
	}
	return out, rows.Err()
}

func (s *Store) ListDueSchedules(ctx context.Context, at time.Time) ([]Schedule, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, task_id, kind, spec, COALESCE(note,''), enabled, last_run_at, next_run_at
		 FROM task_schedules
		 WHERE enabled=1 AND next_run_at IS NOT NULL AND next_run_at <= ?`,
		at.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		sch, err := scanSchedule(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, sch)
	}
	return out, rows.Err()
}

func scanSchedule(scan scanner) (Schedule, error) {
	var (
		s          Schedule
		last, next sql.NullInt64
		enabled    int
	)
	if err := scan(&s.ID, &s.TaskID, (*string)(&s.Kind), &s.Spec, &s.Note, &enabled, &last, &next); err != nil {
		return s, err
	}
	s.Enabled = enabled != 0
	if last.Valid {
		t := time.UnixMilli(last.Int64)
		s.LastRunAt = &t
	}
	if next.Valid {
		t := time.UnixMilli(next.Int64)
		s.NextRunAt = &t
	}
	return s, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
func msOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}
