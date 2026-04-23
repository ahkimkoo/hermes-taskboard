package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Per-user DB schema. Users themselves live in data/{username}/config.yaml
// (see internal/userdir) — this file tracks only the work artefacts:
// tasks, dep edges, tag links, attempts, schedules. owner_id is
// implicit — it's whichever user directory the file sits in.
const schema = `
CREATE TABLE IF NOT EXISTS tasks (
  id                  TEXT PRIMARY KEY,
  title               TEXT NOT NULL,
  status              TEXT NOT NULL,
  priority            INTEGER NOT NULL,
  trigger_mode        TEXT NOT NULL,
  preferred_server    TEXT,
  preferred_model     TEXT,
  created_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL,
  description_excerpt TEXT,
  position            INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS task_deps (
  task_id        TEXT NOT NULL,
  depends_on     TEXT NOT NULL,
  required_state TEXT NOT NULL DEFAULT 'done',
  PRIMARY KEY (task_id, depends_on)
);

CREATE TABLE IF NOT EXISTS task_tags (
  task_id TEXT NOT NULL,
  tag     TEXT NOT NULL,
  PRIMARY KEY (task_id, tag)
);

CREATE TABLE IF NOT EXISTS attempts (
  id         TEXT PRIMARY KEY,
  task_id    TEXT NOT NULL,
  server_id  TEXT NOT NULL,
  model      TEXT NOT NULL,
  state      TEXT NOT NULL,
  started_at INTEGER,
  ended_at   INTEGER
);
CREATE INDEX IF NOT EXISTS idx_attempts_task_state
  ON attempts(task_id, state);
CREATE INDEX IF NOT EXISTS idx_attempts_server_model_state
  ON attempts(server_id, model, state);
`

// Open opens / creates the SQLite DB at path and applies schema + pragmas.
func Open(path string) (*sql.DB, error) {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		return nil, err
	}
	return db, nil
}

// migrate applies idempotent ALTERs for column additions on pre-existing DBs.
// SQLite lacks IF NOT EXISTS on ADD COLUMN, so we inspect PRAGMA first.
func migrate(ctx context.Context, db *sql.DB) error {
	// tasks.position — older DBs don't have it.
	has, err := columnExists(ctx, db, "tasks", "position")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.ExecContext(ctx, `ALTER TABLE tasks ADD COLUMN position INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add tasks.position: %w", err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE tasks SET position = (created_at / 1000)`); err != nil {
			return fmt.Errorf("seed positions: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_tasks_status_position ON tasks(status, position)`); err != nil {
		return fmt.Errorf("create idx_tasks_status_position: %w", err)
	}
	// task_deps.required_state — default to 'done' for pre-existing rows.
	hasReqState, err := columnExists(ctx, db, "task_deps", "required_state")
	if err != nil {
		return err
	}
	if !hasReqState {
		if _, err := db.ExecContext(ctx, `ALTER TABLE task_deps ADD COLUMN required_state TEXT NOT NULL DEFAULT 'done'`); err != nil {
			return fmt.Errorf("add task_deps.required_state: %w", err)
		}
	}
	// task_schedules — added in round 7. `kind` column kept for forward
	// compat but only 'cron' is accepted now; 'interval' rows are rewritten
	// below.
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS task_schedules (
		  id          TEXT PRIMARY KEY,
		  task_id     TEXT NOT NULL,
		  kind        TEXT NOT NULL,     -- always 'cron' (legacy: 'interval')
		  spec        TEXT NOT NULL,     -- 5-field cron "0 9 * * *"
		  note        TEXT NOT NULL DEFAULT '',
		  enabled     INTEGER NOT NULL DEFAULT 1,
		  last_run_at INTEGER,
		  next_run_at INTEGER
		)`); err != nil {
		return fmt.Errorf("create task_schedules: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_schedules_next ON task_schedules(enabled, next_run_at)`); err != nil {
		return fmt.Errorf("create idx_schedules_next: %w", err)
	}
	if err := migrateIntervalSchedules(ctx, db); err != nil {
		return err
	}
	return nil
}

// migrateIntervalSchedules rewrites any legacy kind='interval' rows to
// kind='cron' using a best-effort duration → cron approximation. Clears
// next_run_at so the worker recomputes from the new spec on the next tick.
func migrateIntervalSchedules(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT id, spec FROM task_schedules WHERE kind='interval'`)
	if err != nil {
		return fmt.Errorf("list interval schedules: %w", err)
	}
	type row struct{ id, spec string }
	var legacy []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.spec); err != nil {
			rows.Close()
			return err
		}
		legacy = append(legacy, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range legacy {
		cronSpec := intervalToCron(r.spec)
		if _, err := db.ExecContext(ctx,
			`UPDATE task_schedules SET kind='cron', spec=?, next_run_at=NULL WHERE id=?`,
			cronSpec, r.id); err != nil {
			return fmt.Errorf("migrate interval schedule %s: %w", r.id, err)
		}
	}
	return nil
}

// intervalToCron approximates a Go duration spec (e.g. "15m", "1h30m") as a
// standard 5-field cron. Sub-minute intervals clamp to one minute; non-hour
// multiples over an hour round down to whole hours; anything past a day
// collapses to daily at midnight.
func intervalToCron(spec string) string {
	d, err := time.ParseDuration(spec)
	if err != nil || d <= 0 {
		return "*/5 * * * *"
	}
	total := int(d.Minutes())
	if total < 1 {
		total = 1
	}
	if total <= 59 {
		return fmt.Sprintf("*/%d * * * *", total)
	}
	hours := total / 60
	if hours >= 24 {
		return "0 0 * * *"
	}
	return fmt.Sprintf("0 */%d * * *", hours)
}

func columnExists(ctx context.Context, db *sql.DB, table, col string) (bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}
