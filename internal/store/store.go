package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Store is the repository facade. All SQL access goes through here.
type Store struct {
	DB *sql.DB
}

func New(db *sql.DB) *Store { return &Store{DB: db} }

var ErrNotFound = errors.New("not found")

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
	// Position: max + 1024 within target column so new cards land at the end.
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
	return tx.Commit()
}

func (s *Store) SetTaskStatus(ctx context.Context, id string, to TaskStatus) error {
	// When status changes, park at end of the target column (position = max+1024).
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
	return tx.Commit()
}

// MoveTask relocates a task into `to` column at a specific slot:
//   - afterID empty, beforeID empty → end of column
//   - afterID empty, beforeID set   → before given task
//   - afterID set,   beforeID empty → after given task
//   - afterID set,   beforeID set   → midpoint between the two
// New position is chosen between neighbors; if neighbors collide, renumbers
// the column with 1024-spaced positions to recover.
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
		// End of column.
		if len(neighbors) > 0 {
			lo = neighbors[len(neighbors)-1].pos
			haveLo = true
		}
	}

	var newPos int64
	switch {
	case haveLo && haveHi:
		if hi-lo <= 1 {
			// collision — renumber column with 1024 spacing to recover.
			for i, n := range neighbors {
				if _, err := tx.ExecContext(ctx, `UPDATE tasks SET position=? WHERE id=?`, int64(i+1)*1024, n.id); err != nil {
					return err
				}
			}
			// Recompute neighbors' positions.
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
	return tx.Commit()
}

func (s *Store) DeleteTask(ctx context.Context, id string) ([]string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Collect attempt ids first so caller can clean FS.
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
		`DELETE FROM tasks WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// GetTask loads a task + tags + deps (description still in fsstore).
func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
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
	return t, nil
}

// ListTasks applies filters and returns tasks sorted by priority asc, updated_at desc.
type TaskFilter struct {
	Status   string
	Tag      string
	Query    string
	Server   string
	Model    string
	Limit    int
	Offset   int
}

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
	if f.Server != "" {
		where = append(where, "t.preferred_server=?")
		args = append(args, f.Server)
	}
	if f.Model != "" {
		where = append(where, "t.preferred_model=?")
		args = append(args, f.Model)
	}
	if f.Query != "" {
		where = append(where, "(t.title LIKE ? OR COALESCE(t.description_excerpt,'') LIKE ?)")
		q := "%" + f.Query + "%"
		args = append(args, q, q)
	}
	if len(where) > 0 {
		sb.WriteString(" WHERE " + strings.Join(where, " AND "))
	}
	// Preserve the user's per-column drag order (position ASC). `status` is
	// the primary sort so the backend emits cards grouped by column even
	// though the frontend groups independently.
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
	for rows.Next() {
		t, err := scanTask(rows.Scan)
		if err != nil {
			return nil, err
		}
		list = append(list, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// populate tags/deps/attempt counts
	for _, t := range list {
		if err := s.loadTaskTagsDeps(ctx, t); err != nil {
			return nil, err
		}
		cnt, active, ni, err := s.attemptCounts(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		t.AttemptCount, t.ActiveAttempts, t.NeedsInputAttempts = cnt, active, ni
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
//
// Satisfaction semantics:
//   required_state='verify'  → target ∈ {verify, done, archive}
//   required_state='done'    → target ∈ {done, archive}
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
	return err
}

func (s *Store) GetAttempt(ctx context.Context, id string) (*Attempt, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT id,task_id,server_id,model,state,started_at,ended_at FROM attempts WHERE id=?`, id)
	return scanAttempt(row.Scan)
}

func (s *Store) ListAttemptsForTask(ctx context.Context, taskID string) ([]*Attempt, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id,task_id,server_id,model,state,started_at,ended_at FROM attempts WHERE task_id=? ORDER BY started_at ASC`, taskID)
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
// Returns false when there are no attempts yet.
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

// CountActiveByServerModel returns (global, server, server+model) active counts.
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

// ----- tags -----

func (s *Store) ListTags(ctx context.Context) ([]Tag, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT name, COALESCE(color,''), COALESCE(system_prompt,'') FROM tags ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.Name, &t.Color, &t.SystemPrompt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TagsByNames returns each requested tag (if present) in the same order.
// Missing tags are skipped silently.
func (s *Store) TagsByNames(ctx context.Context, names []string) ([]Tag, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]Tag, 0, len(names))
	for _, n := range names {
		var t Tag
		err := s.DB.QueryRowContext(ctx,
			`SELECT name, COALESCE(color,''), COALESCE(system_prompt,'') FROM tags WHERE name=?`, n,
		).Scan(&t.Name, &t.Color, &t.SystemPrompt)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (s *Store) UpsertTag(ctx context.Context, t Tag) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO tags(name, color, system_prompt) VALUES(?,?,?)
		 ON CONFLICT(name) DO UPDATE SET color=excluded.color, system_prompt=excluded.system_prompt`,
		t.Name, t.Color, t.SystemPrompt)
	return err
}

func (s *Store) DeleteTag(ctx context.Context, name string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_tags WHERE tag=?`, name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE name=?`, name); err != nil {
		return err
	}
	return tx.Commit()
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
		// INSERT OR IGNORE preserves any existing system_prompt — it only
		// adds a row for a brand-new tag.
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO tags(name, color, system_prompt) VALUES(?,'','')`, tag); err != nil {
			return err
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

// SetScheduleEnabled flips the enabled flag and clears next_run_at when
// disabling (so the worker stops picking it up until re-enabled).
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

// GetSchedule returns the schedule row with the given id.
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

// ListDueSchedules returns enabled schedules whose next_run_at ≤ `at`.
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
