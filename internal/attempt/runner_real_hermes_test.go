//go:build integration_real_hermes

// These tests drive the real local Hermes gateway (http://127.0.0.1:8642)
// reachable via the credentials in the repo's data/config.yaml. They do
// NOT touch the running taskboard — we allocate fresh SQLite + fsstore
// roots in temp dirs so nothing clobbers the user's board state.
//
// Run with:
//
//   go test -tags integration_real_hermes -count=1 -v \
//       -run TestRealHermes ./internal/attempt/...
//
// Required assumptions:
//   - Hermes gateway is listening at 127.0.0.1:8642 (Authorization: Bearer …).
//   - The model "hermes-agent" exists (that's the only one the local
//     gateway advertises today).
package attempt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ahkimkoo/hermes-taskboard/internal/board"
	"github.com/ahkimkoo/hermes-taskboard/internal/config"
	"github.com/ahkimkoo/hermes-taskboard/internal/hermes"
	"github.com/ahkimkoo/hermes-taskboard/internal/sse"
	"github.com/ahkimkoo/hermes-taskboard/internal/store"
	"github.com/ahkimkoo/hermes-taskboard/internal/store/fsstore"
	sqlitestore "github.com/ahkimkoo/hermes-taskboard/internal/store/sqlite"
)

// repoConfigStore loads the real config.yaml + .secret from the repo so we
// get the decrypted API key for the locally-running Hermes gateway.
func repoConfigStore(t *testing.T) (*config.Store, string, string, string) {
	t.Helper()
	repo, err := os.Getwd()
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	// When go test runs in internal/attempt, cwd is that dir; the repo
	// root is two levels up.
	root := filepath.Clean(filepath.Join(repo, "..", ".."))
	cfgPath := filepath.Join(root, "data", "config.yaml")
	secretPath := filepath.Join(root, "data", "db", ".secret")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Skipf("config.yaml not readable at %s: %v", cfgPath, err)
	}
	cfg, err := config.NewStore(cfgPath, secretPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	// Find the local Hermes server entry.
	c := cfg.Snapshot()
	var baseURL, apiKey, id string
	for _, sv := range c.HermesServers {
		if sv.ID == "local" {
			baseURL = sv.BaseURL
			apiKey = sv.DecryptedAPIKey(cfg.Secret())
			id = sv.ID
			break
		}
	}
	if baseURL == "" || apiKey == "" {
		t.Skip("local Hermes server not configured in data/config.yaml")
	}
	return cfg, id, baseURL, apiKey
}

// realHarness mirrors newHarness but points at the real Hermes gateway.
func realHarness(t *testing.T) (*Runner, *store.Store, *fsstore.FS, string) {
	t.Helper()
	cfg, serverID, baseURL, apiKey := repoConfigStore(t)

	dir := t.TempDir()
	db, err := sqlitestore.Open(filepath.Join(dir, "db", "taskboard.db"))
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.New(db)
	fs := fsstore.New(dir)

	pool := hermes.NewPool()
	pool.Reload([]hermes.PoolEntry{{ID: serverID, BaseURL: baseURL, APIKey: apiKey, IsDefault: true}})

	hub := sse.NewHub()
	boardSvc := board.New(st, hub)
	runner := New(cfg, st, fs, pool, hub, boardSvc)

	// Sanity check: if the gateway isn't up, skip rather than fail.
	client, err := pool.Get(serverID)
	if err != nil {
		t.Fatalf("pool get: %v", err)
	}
	if _, err := client.Models(context.Background()); err != nil {
		t.Skipf("Hermes gateway not reachable at %s: %v", baseURL, err)
	}
	return runner, st, fs, serverID
}

const realModel = "hermes-agent"

func realNewAttempt(t *testing.T, r *Runner, st *store.Store, fs *fsstore.FS, serverID string) string {
	t.Helper()
	ctx := context.Background()
	task := &store.Task{
		ID:     fmt.Sprintf("task-real-%d", time.Now().UnixNano()),
		Title:  "integration-probe",
		Status: store.StatusExecute,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	att := &store.Attempt{
		ID:       fmt.Sprintf("att-real-%d", time.Now().UnixNano()),
		TaskID:   task.ID,
		ServerID: serverID,
		Model:    realModel,
		State:    store.AttemptQueued,
	}
	if err := st.CreateAttempt(ctx, att); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	if err := fs.SaveAttemptMeta(&store.AttemptMeta{
		ID:       att.ID,
		TaskID:   att.TaskID,
		ServerID: serverID,
		Model:    realModel,
		Session:  store.AttemptSession{ConversationID: att.ID},
	}); err != nil {
		t.Fatalf("save meta: %v", err)
	}
	return att.ID
}

// ---------------------------------------------------------------
// The actual scenarios
// ---------------------------------------------------------------

// TestRealHermes_CancelThenSendFollowUp is the real-world reproduction of
// the user's bug: send a prompt that will stream for a while, cancel
// mid-stream, then send a follow-up. With the fix, the follow-up must
// succeed. Without the fix, the follow-up hits `404: Previous response
// not found`.
func TestRealHermes_CancelThenSendFollowUp(t *testing.T) {
	runner, st, fs, serverID := realHarness(t)
	attID := realNewAttempt(t, runner, st, fs, serverID)

	// Turn 1: ask for a long structured answer so the stream runs for
	// several seconds. We cancel mid-stream.
	longPrompt := "Please slowly explain, step by step and in great detail, " +
		"how to implement a CRDT for collaborative text editing. Include " +
		"code examples for each merge rule. Take your time, be verbose."

	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	go func() { done1 <- runner.runOnce(ctx1, attID, longPrompt, true) }()

	// Wait for the runner to have received response.created from the
	// real gateway — evidenced by CurrentRunID being populated.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if m, _ := fs.LoadAttemptMeta(attID); m != nil && m.Session.CurrentRunID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if m, _ := fs.LoadAttemptMeta(attID); m == nil || m.Session.CurrentRunID == "" {
		cancel1()
		<-done1
		t.Fatalf("real Hermes never sent response.created within deadline")
	}
	midRunID := ""
	if m, _ := fs.LoadAttemptMeta(attID); m != nil {
		midRunID = m.Session.CurrentRunID
	}
	t.Logf("turn 1: response.created observed, id=%s — cancelling now", midRunID)

	cancel1()
	select {
	case err := <-done1:
		t.Logf("turn 1 runOnce returned: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatalf("runOnce didn't return after cancel")
	}
	// Let any trailing async meta writes land.
	time.Sleep(200 * time.Millisecond)

	meta, _ := fs.LoadAttemptMeta(attID)
	if meta == nil {
		t.Fatalf("meta gone after cancel")
	}
	t.Logf("turn 1 post-cancel meta: LatestResponseID=%q CurrentRunID=%q",
		meta.Session.LatestResponseID, meta.Session.CurrentRunID)

	// Turn 2: follow-up. Against the *buggy* build this would POST
	// previous_response_id=<cancelled id> and get 404. With the fix,
	// LatestResponseID is empty after a cancelled turn so the request
	// chain-starts cleanly.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()
	if err := runner.runOnce(ctx2, attID, "say hi in one short sentence", false); err != nil {
		t.Fatalf("follow-up turn failed against real Hermes: %v", err)
	}
	t.Log("turn 2 succeeded — no 404")

	// Sanity: after a completed turn, LatestResponseID should be set.
	if m, _ := fs.LoadAttemptMeta(attID); m == nil || m.Session.LatestResponseID == "" {
		t.Fatalf("expected LatestResponseID set after completed turn 2")
	}
}

// TestRealHermes_StaleResponseIDSelfHeals pre-seeds a meta with a known-
// bad previous id and checks the 404 retry path: the runner must detect
// "Previous response not found", clear the stale id, and retry cold.
func TestRealHermes_StaleResponseIDSelfHeals(t *testing.T) {
	runner, st, fs, serverID := realHarness(t)
	attID := realNewAttempt(t, runner, st, fs, serverID)

	meta, _ := fs.LoadAttemptMeta(attID)
	meta.Session.LatestResponseID = "resp_000000000000000000000000_fake"
	if err := fs.SaveAttemptMeta(meta); err != nil {
		t.Fatalf("seed stale id: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := runner.runOnce(ctx, attID, "reply with exactly the word: ok", true); err != nil {
		t.Fatalf("runOnce should have self-healed, got: %v", err)
	}
	m, _ := fs.LoadAttemptMeta(attID)
	if m.Session.LatestResponseID == "resp_000000000000000000000000_fake" {
		t.Fatalf("stale id never cleared")
	}
	if m.Session.LatestResponseID == "" {
		t.Fatalf("expected a real LatestResponseID after successful retry")
	}
	t.Logf("self-heal OK: new LatestResponseID=%s", m.Session.LatestResponseID)
}
