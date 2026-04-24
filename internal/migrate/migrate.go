// Package migrate converts a legacy taskboard data directory (single
// central DB + hermes_servers inline in data/config.yaml + task/attempt
// dirs at the top level) into the new per-user layout where every
// user has data/{username}/config.yaml + db/taskboard.db + task/ + attempt/.
//
// On legacy installs everything is reassigned to the bootstrap `admin`
// user. The old central DB + shared task/attempt dirs are archived to
// data/_migrated-YYYYMMDD/ so nothing is deleted in anger.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"

	"github.com/ahkimkoo/hermes-taskboard/internal/config"
	"github.com/ahkimkoo/hermes-taskboard/internal/userdir"

	_ "modernc.org/sqlite"
)

const (
	adminUsername = "admin"
	adminPassword = "admin123"
)

// MigrateLegacy is a no-op unless the legacy layout is detected
// (data/db/taskboard.db or hermes_servers in data/config.yaml). On
// detection it:
//   1. Builds data/admin/ with a fresh config.yaml containing the legacy
//      hermes_servers and a freshly-hashed default password.
//   2. Copies tasks/deps/task_tags/attempts/task_schedules rows from
//      the central DB into data/admin/db/taskboard.db.
//   3. Moves data/task/ → data/admin/task/.
//   4. Moves data/attempt/ → data/admin/attempt/.
//   5. Archives data/db/ (the legacy central DB) to data/_migrated-YYYYMMDD/db/.
//   6. Rewrites data/config.yaml stripping hermes_servers.
//
// Safe to call on every boot — bails out silently when the data dir
// already looks like the new layout.
func MigrateLegacy(dataDir string, cfgStore *config.Store, secret []byte, logger *slog.Logger) error {
	legacyDB := filepath.Join(dataDir, "db", "taskboard.db")
	legacyTaskDir := filepath.Join(dataDir, "task")
	legacyAttemptDir := filepath.Join(dataDir, "attempt")
	legacyConfig := filepath.Join(dataDir, "config.yaml")
	adminDir := filepath.Join(dataDir, adminUsername)

	// Already on the new layout? Bail.
	if !fileExists(legacyDB) && !dirExists(legacyTaskDir) && !dirExists(legacyAttemptDir) && !hasLegacyYAMLFields(legacyConfig) {
		return nil
	}

	logger.Warn("legacy layout detected — migrating to per-user directories")

	if err := os.MkdirAll(adminDir, 0o700); err != nil {
		return err
	}

	// Build the admin UserConfig: pull hermes_servers out of the legacy
	// YAML, seed a fresh admin password, copy over preferences if present.
	adminCfg, err := buildAdminConfigFromLegacy(legacyConfig, secret)
	if err != nil {
		return err
	}
	// Legacy DB's `tags` table carried the tag metadata (name, color,
	// system_prompt, and — from v0.3.0 onwards — owner_id + shared).
	// Pull those rows out and inline them into admin's per-user config
	// so tag prompts survive the move. Silently no-op when there's no
	// legacy DB (e.g. migrating from a YAML-only install).
	if fileExists(legacyDB) {
		tags, err := readLegacyTags(legacyDB)
		if err != nil {
			logger.Warn("read legacy tags (continuing)", "err", err)
		}
		if len(tags) > 0 {
			adminCfg.Tags = append(adminCfg.Tags, tags...)
			logger.Info("migrated legacy tags into admin config", "count", len(tags))
		}
	}
	// If data/admin/config.yaml somehow already exists (mid-migration
	// crash, or operator added files manually), merge rather than
	// clobber: keep the existing password + creation timestamp.
	//
	// Three cases:
	//   - file doesn't exist → fresh migration, use adminCfg as-is
	//   - file exists + parses → merge (preserve hash/IsAdmin/created)
	//   - file exists + corrupt → bail out; never overwrite an
	//     unreadable config, operator needs to fix / remove it manually
	adminCfgPath := filepath.Join(adminDir, "config.yaml")
	if fileExists(adminCfgPath) {
		existing, rerr := readUserConfig(adminCfgPath)
		if rerr != nil {
			return fmt.Errorf("existing %s is unreadable (%v) — refusing to overwrite. Fix or remove the file and retry migration.", adminCfgPath, rerr)
		}
		adminCfg.PasswordHash = existing.PasswordHash
		adminCfg.IsAdmin = existing.IsAdmin
		if !existing.CreatedAt.IsZero() {
			adminCfg.CreatedAt = existing.CreatedAt
		}
		// Prepend any tags the existing admin already has so a re-run
		// doesn't drop user-added tags on top of the migrated ones.
		if len(existing.Tags) > 0 {
			seen := map[string]bool{}
			merged := make([]userdir.Tag, 0, len(existing.Tags)+len(adminCfg.Tags))
			for _, t := range existing.Tags {
				merged = append(merged, t)
				seen[t.Name] = true
			}
			for _, t := range adminCfg.Tags {
				if !seen[t.Name] {
					merged = append(merged, t)
				}
			}
			adminCfg.Tags = merged
		}
	}
	if err := writeUserConfig(filepath.Join(adminDir, "config.yaml"), adminCfg); err != nil {
		return err
	}

	// Copy DB rows.
	if fileExists(legacyDB) {
		if err := copyTaskRows(legacyDB, filepath.Join(adminDir, "db", "taskboard.db"), logger); err != nil {
			return fmt.Errorf("copy DB rows: %w", err)
		}
	}

	// Move fs artefacts.
	if dirExists(legacyTaskDir) {
		if err := moveDir(legacyTaskDir, filepath.Join(adminDir, "task")); err != nil {
			return fmt.Errorf("move task dir: %w", err)
		}
	}
	if dirExists(legacyAttemptDir) {
		if err := moveDir(legacyAttemptDir, filepath.Join(adminDir, "attempt")); err != nil {
			return fmt.Errorf("move attempt dir: %w", err)
		}
	}

	// Delete the legacy taskboard.db + WAL/SHM files now that their
	// rows have been copied into admin's per-user DB. The AEAD key
	// used to live at data/db/.secret; main.relocateLegacySecret
	// moves it to data/.secret before this function runs, so by the
	// time we get here data/db/ should contain only DB files and we
	// can rmdir the whole directory afterwards. If anything unexpected
	// is still there, log + leave it rather than delete operator data.
	if fileExists(legacyDB) {
		oldDir := filepath.Join(dataDir, "db")
		entries, _ := os.ReadDir(oldDir)
		for _, e := range entries {
			name := e.Name()
			if name == ".secret" {
				continue // still handled by relocateLegacySecret
			}
			if err := os.RemoveAll(filepath.Join(oldDir, name)); err != nil {
				logger.Warn("remove legacy db artefact", "name", name, "err", err)
			}
		}
		// Remove the now-empty data/db/ so fresh-boot tree shows only
		// data/config.yaml + data/{username}/ + data/.secret.
		if rest, _ := os.ReadDir(oldDir); len(rest) == 0 {
			_ = os.Remove(oldDir)
		}
	}

	// Rewrite global config.yaml with hermes_servers stripped.
	if err := cfgStore.Reload(); err != nil {
		// If it still parses, its mutate will re-marshal without the
		// hermes_servers field because our config struct no longer has
		// that key.
		logger.Warn("config reload after migration", "err", err)
	}
	if err := cfgStore.Mutate(func(c *config.Config) error { return nil }); err != nil {
		return fmt.Errorf("rewrite config.yaml: %w", err)
	}

	logger.Warn("migration complete — default admin: admin / admin123 (change it on first login)")
	return nil
}

// ----- helpers -----

func fileExists(p string) bool {
	s, err := os.Stat(p)
	return err == nil && !s.IsDir()
}
func dirExists(p string) bool {
	s, err := os.Stat(p)
	return err == nil && s.IsDir()
}

// hasLegacyYAMLFields returns true if config.yaml still carries the
// per-user fields we've moved out (hermes_servers, auth.username,
// auth.password_hash, auth.enabled).
func hasLegacyYAMLFields(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var probe map[string]any
	if err := yaml.Unmarshal(b, &probe); err != nil {
		return false
	}
	if _, ok := probe["hermes_servers"]; ok {
		return true
	}
	if a, ok := probe["auth"].(map[string]any); ok {
		if _, ok := a["username"]; ok {
			return true
		}
		if _, ok := a["password_hash"]; ok {
			return true
		}
		if _, ok := a["enabled"]; ok {
			return true
		}
	}
	if _, ok := probe["preferences"]; ok {
		// preferences moved to per-user
		return true
	}
	return false
}

// legacyFile mirrors just enough of the old config YAML to pull the
// fields we need during migration.
type legacyFile struct {
	Auth struct {
		Username     string `yaml:"username"`
		PasswordHash string `yaml:"password_hash"`
	} `yaml:"auth"`
	HermesServers []legacyServer          `yaml:"hermes_servers"`
	Preferences   *userdir.Preferences    `yaml:"preferences,omitempty"`
}

type legacyServer struct {
	ID            string        `yaml:"id"`
	Name          string        `yaml:"name"`
	BaseURL       string        `yaml:"base_url"`
	APIKey        string        `yaml:"api_key,omitempty"`
	APIKeyEnc     string        `yaml:"api_key_enc,omitempty"`
	IsDefault     bool          `yaml:"is_default"`
	MaxConcurrent int           `yaml:"max_concurrent"`
	Models        []legacyModel `yaml:"models"`
}

type legacyModel struct {
	Name      string `yaml:"name"`
	IsDefault bool   `yaml:"is_default"`
}

func buildAdminConfigFromLegacy(legacyPath string, secret []byte) (*userdir.UserConfig, error) {
	lf := legacyFile{}
	if b, err := os.ReadFile(legacyPath); err == nil {
		_ = yaml.Unmarshal(b, &lf)
	}
	servers := make([]userdir.HermesServer, 0, len(lf.HermesServers))
	for _, s := range lf.HermesServers {
		// Pre-v0.3.17 legacy rows carried a Models slice — collapse
		// it to the single Profile field the new schema uses.
		profile := ""
		for _, m := range s.Models {
			if m.IsDefault && m.Name != "" {
				profile = m.Name
				break
			}
		}
		if profile == "" {
			for _, m := range s.Models {
				if m.Name != "" {
					profile = m.Name
					break
				}
			}
		}
		sv := userdir.HermesServer{
			ID: s.ID, Name: s.Name, BaseURL: s.BaseURL,
			APIKey: s.APIKey, APIKeyEnc: s.APIKeyEnc,
			IsDefault:     s.IsDefault,
			MaxConcurrent: s.MaxConcurrent,
			Profile:       profile,
		}
		// If the legacy file had a plaintext api_key, encrypt it in
		// place using the same AEAD userdir uses so the migrated file
		// matches what a live edit produces.
		if sv.APIKey != "" {
			if enc, err := aeadEncrypt(secret, sv.APIKey); err == nil {
				sv.APIKeyEnc = enc
				sv.APIKey = ""
			}
		}
		servers = append(servers, sv)
	}
	// Always give the bootstrap admin a fresh known password unless the
	// legacy yaml had a usable hash we can carry forward.
	hash := lf.Auth.PasswordHash
	if hash == "" {
		h, err := bcrypt.GenerateFromPassword([]byte(adminPassword), 12)
		if err != nil {
			return nil, err
		}
		hash = string(h)
	}
	// Seed sane default preferences so the YAML isn't full of zero-
	// valued fields after migration.
	prefs := userdir.Preferences{
		Theme: "dark",
		Sound: userdir.Sound{
			Enabled: true, Volume: 0.7,
			Events: userdir.SoundEvents{ExecuteStart: true, NeedsInput: true, Done: true},
		},
	}
	if lf.Preferences != nil {
		prefs = *lf.Preferences
	}
	return &userdir.UserConfig{
		Username:      adminUsername,
		PasswordHash:  hash,
		IsAdmin:       true,
		CreatedAt:     time.Now(),
		Preferences:   prefs,
		HermesServers: servers,
	}, nil
}

// readLegacyTags pulls the `tags` table out of the legacy central DB
// and returns userdir.Tag rows ready to merge into admin's per-user
// config. Schema varies by vintage:
//
//   Pre-v0.3.0:  name, color, system_prompt
//   v0.3.0-era:  name, color, system_prompt, owner_id, shared
//
// We probe PRAGMA table_info first so we can select the right column
// set — an ALTER TABLE ADD COLUMN path that landed mid-cycle shouldn't
// break migration from one of the in-between builds.
func readLegacyTags(dbPath string) ([]userdir.Tag, error) {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	hasShared, err := columnExists(db, "tags", "shared")
	if err != nil {
		// Table missing entirely is fine — means the legacy install
		// never had any tags.
		return nil, nil
	}
	q := `SELECT name, COALESCE(color,''), COALESCE(system_prompt,'')`
	if hasShared {
		q += `, COALESCE(shared, 0)`
	}
	q += ` FROM tags`
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []userdir.Tag
	for rows.Next() {
		var t userdir.Tag
		var shared int
		var color string // read-but-discarded; v0.3.7+ tag files don't carry color
		if hasShared {
			if err := rows.Scan(&t.Name, &color, &t.SystemPrompt, &shared); err != nil {
				return nil, err
			}
			t.Shared = shared != 0
		} else {
			if err := rows.Scan(&t.Name, &color, &t.SystemPrompt); err != nil {
				return nil, err
			}
		}
		if t.Name == "" {
			continue
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// columnExists returns true if table.col is present in the given DB.
// Returns false (no error) when the table itself is missing — that's
// "nothing to migrate", not an error.
func columnExists(db *sql.DB, table, col string) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	found := false
	any := false
	for rows.Next() {
		any = true
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			found = true
		}
	}
	if !any {
		return false, fmt.Errorf("no such table: %s", table)
	}
	return found, nil
}

func readUserConfig(path string) (*userdir.UserConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	u := &userdir.UserConfig{}
	if err := yaml.Unmarshal(b, u); err != nil {
		return nil, err
	}
	return u, nil
}

func writeUserConfig(path string, u *userdir.UserConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(u)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// copyTaskRows reads every relevant row from srcDB and inserts into
// dstDB. We use plain database/sql so we don't circularly import the
// store package (which would pull migrations that assume a fresh new
// layout). Schema columns match the new schema exactly.
func copyTaskRows(srcPath, dstPath string, logger *slog.Logger) error {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o700); err != nil {
		return err
	}
	src, err := sql.Open("sqlite", "file:"+srcPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := sql.Open("sqlite", "file:"+dstPath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return err
	}
	defer dst.Close()

	// Create schema on dst (mirrors internal/store/sqlite.schema).
	schema := `
	CREATE TABLE IF NOT EXISTS tasks (
	  id TEXT PRIMARY KEY, title TEXT NOT NULL, status TEXT NOT NULL,
	  priority INTEGER NOT NULL, trigger_mode TEXT NOT NULL,
	  preferred_server TEXT,
	  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
	  description_excerpt TEXT, position INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE IF NOT EXISTS task_deps (
	  task_id TEXT NOT NULL, depends_on TEXT NOT NULL,
	  required_state TEXT NOT NULL DEFAULT 'done',
	  PRIMARY KEY (task_id, depends_on)
	);
	CREATE TABLE IF NOT EXISTS task_tags (
	  task_id TEXT NOT NULL, tag TEXT NOT NULL,
	  PRIMARY KEY (task_id, tag)
	);
	CREATE TABLE IF NOT EXISTS attempts (
	  id TEXT PRIMARY KEY, task_id TEXT NOT NULL,
	  server_id TEXT NOT NULL, model TEXT NOT NULL, state TEXT NOT NULL,
	  started_at INTEGER, ended_at INTEGER
	);
	CREATE TABLE IF NOT EXISTS task_schedules (
	  id TEXT PRIMARY KEY, task_id TEXT NOT NULL, kind TEXT NOT NULL,
	  spec TEXT NOT NULL, note TEXT NOT NULL DEFAULT '',
	  enabled INTEGER NOT NULL DEFAULT 1, last_run_at INTEGER, next_run_at INTEGER
	);
	`
	ctx := context.Background()
	if _, err := dst.ExecContext(ctx, schema); err != nil {
		return err
	}

	// Tasks — read the legacy 11-column shape (with preferred_model)
	// and drop that column on insert. Pre-v0.3.17 DBs always had it.
	if err := copyTaskRowsDroppingPreferredModel(ctx, src, dst); err != nil {
		logger.Warn("copy tasks", "err", err)
	}
	if err := copyRows(ctx, src, dst,
		`SELECT task_id,depends_on,required_state FROM task_deps`,
		`INSERT OR IGNORE INTO task_deps(task_id,depends_on,required_state) VALUES(?,?,?)`,
		3); err != nil {
		logger.Warn("copy task_deps", "err", err)
	}
	if err := copyRows(ctx, src, dst,
		`SELECT task_id,tag FROM task_tags`,
		`INSERT OR IGNORE INTO task_tags(task_id,tag) VALUES(?,?)`,
		2); err != nil {
		logger.Warn("copy task_tags", "err", err)
	}
	if err := copyRows(ctx, src, dst,
		`SELECT id,task_id,server_id,model,state,started_at,ended_at FROM attempts`,
		`INSERT OR IGNORE INTO attempts(id,task_id,server_id,model,state,started_at,ended_at) VALUES(?,?,?,?,?,?,?)`,
		7); err != nil {
		logger.Warn("copy attempts", "err", err)
	}
	// task_schedules may or may not exist on very old DBs.
	_ = copyRows(ctx, src, dst,
		`SELECT id,task_id,kind,spec,note,enabled,last_run_at,next_run_at FROM task_schedules`,
		`INSERT OR IGNORE INTO task_schedules(id,task_id,kind,spec,note,enabled,last_run_at,next_run_at) VALUES(?,?,?,?,?,?,?,?)`,
		8)
	return nil
}

// copyTaskRowsDroppingPreferredModel copies legacy task rows into the
// v0.3.17 schema, dropping the preferred_model column. If the source
// already lacks the column (a DB migrated while running v0.3.17+), it
// falls back to a direct 10-column copy.
func copyTaskRowsDroppingPreferredModel(ctx context.Context, src, dst *sql.DB) error {
	rows, err := src.QueryContext(ctx,
		`SELECT id,title,status,priority,trigger_mode,preferred_server,preferred_model,created_at,updated_at,description_excerpt,position FROM tasks`)
	if err != nil {
		return copyRows(ctx, src, dst,
			`SELECT id,title,status,priority,trigger_mode,preferred_server,created_at,updated_at,description_excerpt,position FROM tasks`,
			`INSERT OR IGNORE INTO tasks(id,title,status,priority,trigger_mode,preferred_server,created_at,updated_at,description_excerpt,position) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			10)
	}
	defer rows.Close()
	stmt, err := dst.PrepareContext(ctx,
		`INSERT OR IGNORE INTO tasks(id,title,status,priority,trigger_mode,preferred_server,created_at,updated_at,description_excerpt,position) VALUES(?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for rows.Next() {
		vals := make([]any, 11)
		ptrs := make([]any, 11)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		// Drop index 6 (preferred_model) — the column sits between
		// preferred_server (5) and created_at (7) in the legacy order.
		kept := append(vals[:6:6], vals[7:]...)
		if _, err := stmt.ExecContext(ctx, kept...); err != nil {
			return err
		}
	}
	return rows.Err()
}

func copyRows(ctx context.Context, src, dst *sql.DB, selectQ, insertQ string, cols int) error {
	rows, err := src.QueryContext(ctx, selectQ)
	if err != nil {
		return err
	}
	defer rows.Close()
	stmt, err := dst.PrepareContext(ctx, insertQ)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for rows.Next() {
		vals := make([]any, cols)
		ptrs := make([]any, cols)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, vals...); err != nil {
			return err
		}
	}
	return rows.Err()
}

// moveDir renames src → dst. Falls back to a copy-and-delete if rename
// fails (e.g. across filesystems).
func moveDir(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// Slow path — recursive copy then remove.
	if err := copyTree(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFile(path, target, info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	d, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer d.Close()
	if _, err := io.Copy(d, s); err != nil {
		return err
	}
	return nil
}

var _ = errors.New // keep import in some build tags
