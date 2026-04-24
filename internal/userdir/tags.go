package userdir

// Per-user tag storage: `data/{username}/tags/{sanitized}.{private|public}`.
//
//   Filename encodes (a) the sanitised display name (spaces → '-') and
//   (b) whether the tag is visible to other users.
//   File content is a tiny YAML holding the authoritative display name
//   (so we can round-trip names that had spaces) + the system_prompt.
//
// Example:
//
//   data/admin/tags/
//     企微通知.public
//     浏览器.private
//     Browser-Skill.public
//
// `.public` tags must be globally unique across all users — flipping
// a private tag to public reads every other user's tags/ dir to look
// for a collision.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// tagFile is the on-disk representation.
type tagFile struct {
	Name         string `yaml:"name"`
	Color        string `yaml:"color,omitempty"`
	SystemPrompt string `yaml:"system_prompt,omitempty"`
}

// tagsDir is the per-user tags/ directory.
func (m *Manager) tagsDir(username string) string {
	return filepath.Join(m.Root, username, "tags")
}

// sanitizeTagFilename turns a display name into a filesystem-safe basename.
// Current rules:
//   - whitespace runs collapse into a single '-'
//   - leading/trailing whitespace trimmed
//   - control chars / path separators rejected by ValidateTagName first,
//     so we don't have to strip them here
func sanitizeTagFilename(name string) string {
	name = strings.TrimSpace(name)
	// Replace any whitespace with '-'. Using Fields+Join collapses runs.
	parts := strings.Fields(name)
	return strings.Join(parts, "-")
}

// ValidateTagName rejects names that would be confusing or unsafe on disk.
// Returns a nil error when name is OK to use.
func ValidateTagName(name string) error {
	n := strings.TrimSpace(name)
	if n == "" {
		return errors.New("tag name required")
	}
	if len(n) > 120 {
		return errors.New("tag name too long (max 120 chars)")
	}
	for _, r := range n {
		switch r {
		case '/', '\\', 0:
			return fmt.Errorf("tag name may not contain %q", string(r))
		}
		if r < 0x20 {
			return errors.New("tag name may not contain control characters")
		}
	}
	if strings.HasPrefix(n, ".") {
		return errors.New("tag name may not start with '.'")
	}
	return nil
}

// tagPath returns the absolute file path for a (name, shared) pair.
func (m *Manager) tagPath(username, name string, shared bool) string {
	suffix := "private"
	if shared {
		suffix = "public"
	}
	return filepath.Join(m.tagsDir(username), sanitizeTagFilename(name)+"."+suffix)
}

// ListTagsOfUser scans data/{username}/tags/ and returns the tags it
// finds. Returns a non-nil empty slice when the directory is missing
// (fresh user, never added a tag yet).
func (m *Manager) ListTagsOfUser(username string) ([]Tag, error) {
	dir := m.tagsDir(username)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Tag{}, nil
		}
		return nil, err
	}
	out := make([]Tag, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		var shared bool
		switch {
		case strings.HasSuffix(n, ".public"):
			shared = true
		case strings.HasSuffix(n, ".private"):
			shared = false
		default:
			continue // ignore stray files
		}
		t, err := m.readTagFile(filepath.Join(dir, n), shared)
		if err != nil {
			// Don't nuke the whole list because one file is corrupt;
			// skip + keep going so the rest still load.
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Manager) readTagFile(path string, shared bool) (Tag, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Tag{}, err
	}
	var tf tagFile
	if err := yaml.Unmarshal(b, &tf); err != nil {
		return Tag{}, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	if tf.Name == "" {
		// Fallback: derive from filename (strip .private/.public).
		base := filepath.Base(path)
		base = strings.TrimSuffix(base, ".private")
		base = strings.TrimSuffix(base, ".public")
		tf.Name = base
	}
	return Tag{
		Name:         tf.Name,
		Color:        tf.Color,
		SystemPrompt: tf.SystemPrompt,
		Shared:       shared,
	}, nil
}

// GetTagOfUser reads one tag by name out of a user's tags/ directory.
// Returns (Tag{}, false, nil) when not found. The name lookup is
// authoritative — we scan files instead of trusting the sanitised
// filename — so display names that differ only in whitespace layout
// (e.g. "Browser Skill" vs "Browser  Skill") are resolved correctly.
func (m *Manager) GetTagOfUser(username, name string) (Tag, bool, error) {
	tags, err := m.ListTagsOfUser(username)
	if err != nil {
		return Tag{}, false, err
	}
	for _, t := range tags {
		if t.Name == name {
			return t, true, nil
		}
	}
	return Tag{}, false, nil
}

// IsPublicTagNameTaken returns true when any user OTHER than excludeOwner
// owns a `.public` tag with the exact display name. Used to gate both
// "create a public tag" and "flip a private tag to public" — both must
// fail when another user has already claimed the name globally.
func (m *Manager) IsPublicTagNameTaken(name, excludeOwner string) (bool, string, error) {
	m.mu.RLock()
	owners := make([]string, 0, len(m.users))
	for u := range m.users {
		if u == excludeOwner {
			continue
		}
		owners = append(owners, u)
	}
	m.mu.RUnlock()
	for _, u := range owners {
		tags, err := m.ListTagsOfUser(u)
		if err != nil {
			return false, "", err
		}
		for _, t := range tags {
			if t.Shared && t.Name == name {
				return true, u, nil
			}
		}
	}
	return false, "", nil
}

// TagUpsertRequest describes a create-or-update operation. If oldName
// is non-empty and differs from tag.Name, the old file is removed as
// part of the upsert (rename).
type TagUpsertRequest struct {
	OldName string // empty when creating new; the current display name when editing
	Tag     Tag
}

// UpsertTagOfUser validates + writes a tag file. Enforces global name
// uniqueness for `.public` tags. When a tag changes name or sharing
// status, the stale file is removed so the new filename replaces it
// cleanly.
func (m *Manager) UpsertTagOfUser(ownerUsername string, req TagUpsertRequest) error {
	if err := ValidateTagName(req.Tag.Name); err != nil {
		return err
	}
	if req.Tag.Shared {
		if taken, otherOwner, err := m.IsPublicTagNameTaken(req.Tag.Name, ownerUsername); err != nil {
			return err
		} else if taken {
			return fmt.Errorf("a shared tag named %q already exists (owner: %s) — rename yours before making it public", req.Tag.Name, otherOwner)
		}
	}
	dir := m.tagsDir(ownerUsername)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	// Compute old + new paths so we can remove the stale file on
	// rename / shared-flag flip. On a pure content edit (same name +
	// same shared) newPath == oldPath and the rename is a no-op.
	var oldPath string
	if req.OldName != "" {
		// Look up the old file on disk because we don't know the old
		// shared state otherwise.
		oldTag, found, err := m.GetTagOfUser(ownerUsername, req.OldName)
		if err != nil {
			return err
		}
		if found {
			oldPath = m.tagPath(ownerUsername, oldTag.Name, oldTag.Shared)
		}
	}
	newPath := m.tagPath(ownerUsername, req.Tag.Name, req.Tag.Shared)

	// If renaming INTO a slot already occupied by a different tag,
	// refuse. Same-path write (pure edit) is fine.
	if _, err := os.Stat(newPath); err == nil && newPath != oldPath {
		return fmt.Errorf("another tag already lives at %s — pick a different name", filepath.Base(newPath))
	}

	body, err := yaml.Marshal(tagFile{
		Name:         req.Tag.Name,
		Color:        req.Tag.Color,
		SystemPrompt: req.Tag.SystemPrompt,
	})
	if err != nil {
		return err
	}
	if err := atomicWrite(newPath, body, 0o600); err != nil {
		return err
	}
	if oldPath != "" && oldPath != newPath {
		_ = os.Remove(oldPath)
	}
	return nil
}

// DeleteTagOfUser removes the tag file matching `name`. Ignores a
// missing file (returns nil) so re-delete is safe.
func (m *Manager) DeleteTagOfUser(ownerUsername, name string) error {
	t, found, err := m.GetTagOfUser(ownerUsername, name)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	return os.Remove(m.tagPath(ownerUsername, t.Name, t.Shared))
}

// migrateInlineTags pulls any `tags:` list embedded in a legacy
// config.yaml into the new per-file layout. Idempotent: writes only
// for tag names that don't already have a file on disk, then clears
// the cached slice and rewrites config.yaml so the next save drops
// the key. Returns the count of tags materialised.
func (m *Manager) migrateInlineTags(username string) (int, error) {
	m.mu.Lock()
	u, ok := m.users[username]
	if !ok {
		m.mu.Unlock()
		return 0, nil
	}
	legacy := append([]Tag{}, u.Tags...)
	if len(legacy) == 0 {
		m.mu.Unlock()
		return 0, nil
	}
	m.mu.Unlock()

	dir := m.tagsDir(username)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	written := 0
	for _, t := range legacy {
		if t.Name == "" {
			continue
		}
		if err := ValidateTagName(t.Name); err != nil {
			// Skip bogus names rather than abort the whole migration.
			continue
		}
		p := m.tagPath(username, t.Name, t.Shared)
		if _, err := os.Stat(p); err == nil {
			// File-backed tag already there — don't stomp the disk
			// copy on re-run.
			continue
		}
		body, err := yaml.Marshal(tagFile{
			Name:         t.Name,
			Color:        t.Color,
			SystemPrompt: t.SystemPrompt,
		})
		if err != nil {
			return written, err
		}
		if err := atomicWrite(p, body, 0o600); err != nil {
			return written, err
		}
		written++
	}
	// Clear the inline list + rewrite config.yaml so the legacy key
	// drops out.
	if err := m.Mutate(username, func(uc *UserConfig) error {
		uc.Tags = nil
		return nil
	}); err != nil {
		return written, err
	}
	return written, nil
}

// MigrateAllInlineTags runs migrateInlineTags for every cached user.
// Call once on boot after LoadAll to pull legacy `tags:` yaml keys
// into per-file storage.
func (m *Manager) MigrateAllInlineTags() (int, error) {
	m.mu.RLock()
	names := make([]string, 0, len(m.users))
	for n := range m.users {
		names = append(names, n)
	}
	m.mu.RUnlock()
	total := 0
	for _, n := range names {
		c, err := m.migrateInlineTags(n)
		if err != nil {
			return total, fmt.Errorf("migrate tags for %s: %w", n, err)
		}
		total += c
	}
	return total, nil
}
