package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

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
  description_excerpt TEXT
);
CREATE INDEX IF NOT EXISTS idx_tasks_status_priority
  ON tasks(status, priority, updated_at DESC);

CREATE TABLE IF NOT EXISTS task_deps (
  task_id    TEXT NOT NULL,
  depends_on TEXT NOT NULL,
  PRIMARY KEY (task_id, depends_on)
);

CREATE TABLE IF NOT EXISTS tags (
  name  TEXT PRIMARY KEY,
  color TEXT
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
	return db, nil
}
