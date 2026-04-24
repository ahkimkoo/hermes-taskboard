package attempt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahkimkoo/hermes-taskboard/internal/config"
	"github.com/ahkimkoo/hermes-taskboard/internal/userdir"
)

// v0.3.17 coverage: one Hermes API server = one profile.
// resolveServerModel must return the server's Profile field, falling
// back to the "hermes-agent" default when the field is empty — the
// legacy `preferred_model` task override is ignored entirely.

func TestResolveServerModelUsesProfile(t *testing.T) {
	h := newHarness(t)
	err := h.runner.Users.Mutate(h.username, func(u *userdir.UserConfig) error {
		u.HermesServers = []userdir.HermesServer{{
			ID:      "srv-alice",
			Name:    "Alice's Hermes",
			BaseURL: "http://localhost:8643",
			Profile: "alice",
		}}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	sv, model := h.runner.resolveServerModel(h.username, "srv-alice")
	if sv == nil || sv.ID != "srv-alice" {
		t.Fatalf("server not found: %#v", sv)
	}
	if model != "alice" {
		t.Fatalf("expected profile 'alice', got %q", model)
	}
}

func TestResolveServerModelFallsBackToHermesAgent(t *testing.T) {
	h := newHarness(t)
	err := h.runner.Users.Mutate(h.username, func(u *userdir.UserConfig) error {
		u.HermesServers = []userdir.HermesServer{{
			ID:      "srv-default",
			Name:    "Default",
			BaseURL: "http://localhost:8642",
			// Profile left empty — represents a default Hermes install.
		}}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	sv, model := h.runner.resolveServerModel(h.username, "srv-default")
	if sv == nil {
		t.Fatal("server not found")
	}
	if model != config.HermesDefaultAgent {
		t.Fatalf("empty Profile must fall back to %q, got %q",
			config.HermesDefaultAgent, model)
	}
}

// Default-server lookup: with no explicit preferred server in the
// call, resolveServerModel must pick the IsDefault server and return
// its Profile — not any other.
func TestResolveServerModelUsesDefaultWhenNoPreference(t *testing.T) {
	h := newHarness(t)
	err := h.runner.Users.Mutate(h.username, func(u *userdir.UserConfig) error {
		u.HermesServers = []userdir.HermesServer{
			{ID: "srv-a", BaseURL: "http://a", Profile: "alpha"},
			{ID: "srv-b", BaseURL: "http://b", Profile: "beta", IsDefault: true},
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	sv, model := h.runner.resolveServerModel(h.username, "")
	if sv == nil || sv.ID != "srv-b" {
		t.Fatalf("expected srv-b, got %#v", sv)
	}
	if model != "beta" {
		t.Fatalf("expected 'beta' profile, got %q", model)
	}
}

// TestLegacyModelsYAMLCollapse round-trips a legacy v0.3.16-shape
// config.yaml through Manager.LoadAll and confirms:
//  1. Profile is populated by collapsing the IsDefault model (or
//     first if no default is flagged)
//  2. The `models:` key is gone from the re-written YAML on disk
//  3. Unrelated server fields (MaxConcurrent, IsDefault, Shared) pass
//     through unchanged
func TestLegacyModelsYAMLCollapse(t *testing.T) {
	dir := t.TempDir()
	legacyYAML := []byte(`username: bob
password_hash: x
is_admin: false
hermes_servers:
  - id: srv1
    name: Bob Hermes
    base_url: http://localhost:8644
    max_concurrent: 8
    is_default: true
    models:
      - name: bob-chat
        is_default: false
        max_concurrent: 4
      - name: bob-default
        is_default: true
        max_concurrent: 5
    shared: true
`)
	if err := os.MkdirAll(filepath.Join(dir, "bob"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bob", "config.yaml"), legacyYAML, 0o600); err != nil {
		t.Fatalf("seed yaml: %v", err)
	}

	users := userdir.New(dir, make([]byte, 32))
	if err := users.LoadAll(); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	u, ok := users.Get("bob")
	if !ok {
		t.Fatal("bob not loaded")
	}
	if len(u.HermesServers) != 1 {
		t.Fatalf("want 1 server, got %d", len(u.HermesServers))
	}
	sv := u.HermesServers[0]
	if sv.Profile != "bob-default" {
		t.Fatalf("Profile collapse picked wrong profile: %q want 'bob-default'", sv.Profile)
	}
	if sv.MaxConcurrent != 8 {
		t.Fatalf("MaxConcurrent perturbed: %d", sv.MaxConcurrent)
	}
	if !sv.IsDefault {
		t.Fatal("IsDefault flag lost during migration")
	}
	if !sv.Shared {
		t.Fatal("Shared flag lost during migration")
	}
	if len(sv.LegacyModels) != 0 {
		t.Fatalf("LegacyModels should clear post-normalize, got %d", len(sv.LegacyModels))
	}

	// On-disk file: models: key must be gone, profile: must be set.
	b, err := os.ReadFile(filepath.Join(dir, "bob", "config.yaml"))
	if err != nil {
		t.Fatalf("read migrated file: %v", err)
	}
	text := string(b)
	if strings.Contains(text, "\n    models:") || strings.Contains(text, "- name:") {
		t.Fatalf("migrated YAML still carries legacy models key:\n%s", text)
	}
	if !strings.Contains(text, "profile: bob-default") {
		t.Fatalf("migrated YAML missing profile field:\n%s", text)
	}
}

// First-non-default-model fallback: when legacy YAML has a models
// slice but none is flagged as default, collapse picks the first
// named entry (matches the resolveServerModel pre-migration priority
// chain which also fell back to Models[0]).
func TestLegacyModelsYAMLCollapseFirstWhenNoDefault(t *testing.T) {
	dir := t.TempDir()
	legacyYAML := []byte(`username: carol
password_hash: x
is_admin: false
hermes_servers:
  - id: srv1
    base_url: http://c
    max_concurrent: 5
    models:
      - name: first
      - name: second
`)
	_ = os.MkdirAll(filepath.Join(dir, "carol"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "carol", "config.yaml"), legacyYAML, 0o600)

	users := userdir.New(dir, make([]byte, 32))
	if err := users.LoadAll(); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	u, _ := users.Get("carol")
	if u.HermesServers[0].Profile != "first" {
		t.Fatalf("no-default fallback should pick first named, got %q",
			u.HermesServers[0].Profile)
	}
}

// Runner.Start end-to-end path keeps working with the new single-
// arg signature — regression coverage via runner_cancel_chain_test.go
// and runner_restart_resume_test.go, which both drive Start and
// inspect attempt state.
