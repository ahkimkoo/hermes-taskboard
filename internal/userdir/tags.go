package userdir

// Per-user tag storage: `data/{username}/tags/{filename}.{private|public}`.
//
// The filename IS the tag — no metadata wrapper inside. Spaces in a
// display name get stored as '-' in the filename (so "Browser Skill"
// becomes "Browser-Skill.private"). File contents are the raw
// system_prompt text, operator-friendly for `cat` / `grep` / manual
// edits. Missing file or empty contents means a tag with no prompt.
//
// Example:
//
//   data/admin/tags/
//     企微通知.public       ← content: raw markdown system prompt
//     浏览器.private
//     Browser-Skill.public
//
// `.public` tag names are globally unique across all users — the
// flip-to-public and rename paths both check against every other
// user's tags/ directory.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// tagsDir is the per-user tags/ directory.
func (m *Manager) tagsDir(username string) string {
	return filepath.Join(m.Root, username, "tags")
}

// sanitizeTagFilename turns a display name into a filesystem-safe basename.
// Whitespace runs collapse to a single '-'; other chars pass through.
// ValidateTagName has already rejected control chars / path separators
// by the time we call this.
func sanitizeTagFilename(name string) string {
	parts := strings.Fields(strings.TrimSpace(name))
	return strings.Join(parts, "-")
}

// ValidateTagName rejects names that would be confusing or unsafe on disk.
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

// displayNameFromFilename strips the .private/.public suffix and returns
// the name as stored on disk. Hyphens coming from sanitisation stay as
// hyphens — we don't try to reverse the spaces→'-' mapping because it
// would corrupt names that legitimately contain hyphens.
func displayNameFromFilename(basename string) (name string, shared bool, ok bool) {
	switch {
	case strings.HasSuffix(basename, ".public"):
		return strings.TrimSuffix(basename, ".public"), true, true
	case strings.HasSuffix(basename, ".private"):
		return strings.TrimSuffix(basename, ".private"), false, true
	}
	return "", false, false
}

// ListTagsOfUser scans data/{username}/tags/ and returns the tags it
// finds. Returns a non-nil empty slice when the directory is missing.
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
		name, shared, ok := displayNameFromFilename(e.Name())
		if !ok {
			continue // ignore stray files without our suffix
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			// Don't tank the whole list because one file is unreadable.
			continue
		}
		out = append(out, Tag{
			Name:         name,
			SystemPrompt: string(body),
			Shared:       shared,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GetTagOfUser reads one tag by display name. Lookup compares against
// the on-disk name (derived from filename), so it correctly returns
// hits for both the sanitised form ("Browser-Skill") and the raw form
// if the user stored spaces (though the standard path rewrites spaces).
func (m *Manager) GetTagOfUser(username, name string) (Tag, bool, error) {
	tags, err := m.ListTagsOfUser(username)
	if err != nil {
		return Tag{}, false, err
	}
	sanitised := sanitizeTagFilename(name)
	for _, t := range tags {
		if t.Name == name || t.Name == sanitised {
			return t, true, nil
		}
	}
	return Tag{}, false, nil
}

// IsPublicTagNameTaken returns true when any user OTHER than excludeOwner
// owns a `.public` tag with the exact sanitised filename. Gates create +
// rename + flip-to-public so two users can't claim the same public slot.
func (m *Manager) IsPublicTagNameTaken(name, excludeOwner string) (bool, string, error) {
	want := sanitizeTagFilename(name)
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
			if t.Shared && sanitizeTagFilename(t.Name) == want {
				return true, u, nil
			}
		}
	}
	return false, "", nil
}

// TagUpsertRequest describes a create-or-update operation. If OldName
// is non-empty and differs from Tag.Name (after sanitisation) OR the
// shared flag flipped, the stale file gets removed as part of the
// write so the new filename replaces it cleanly.
type TagUpsertRequest struct {
	OldName string
	Tag     Tag
}

// UpsertTagOfUser writes the tag's system_prompt as the file body at
// data/{owner}/tags/{name-with-spaces-as-hyphens}.{private|public}.
// Public-name uniqueness is checked first — collision errors out
// before any disk write happens.
func (m *Manager) UpsertTagOfUser(ownerUsername string, req TagUpsertRequest) error {
	if err := ValidateTagName(req.Tag.Name); err != nil {
		return err
	}
	if req.Tag.Shared {
		if taken, otherOwner, err := m.IsPublicTagNameTaken(req.Tag.Name, ownerUsername); err != nil {
			return err
		} else if taken {
			return fmt.Errorf("a shared tag named %q already exists (owner: %s) — rename yours before making it public", sanitizeTagFilename(req.Tag.Name), otherOwner)
		}
	}
	dir := m.tagsDir(ownerUsername)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	var oldPath string
	if req.OldName != "" {
		if oldTag, found, err := m.GetTagOfUser(ownerUsername, req.OldName); err == nil && found {
			oldPath = m.tagPath(ownerUsername, oldTag.Name, oldTag.Shared)
		}
	}
	newPath := m.tagPath(ownerUsername, req.Tag.Name, req.Tag.Shared)

	// Refuse to write if another distinct tag already occupies the
	// destination — protects against a rename silently stomping an
	// unrelated file.
	if newPath != oldPath {
		if _, err := os.Stat(newPath); err == nil {
			return fmt.Errorf("another tag already lives at %s — pick a different name", filepath.Base(newPath))
		}
	}

	if err := atomicWrite(newPath, []byte(req.Tag.SystemPrompt), 0o600); err != nil {
		return err
	}
	if oldPath != "" && oldPath != newPath {
		_ = os.Remove(oldPath)
	}
	return nil
}

// DeleteTagOfUser removes the tag file for a given display name.
// Missing file is a no-op (redelete safe).
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

// migrateInlineTags pulls any legacy `tags:` list embedded in a user's
// config.yaml into the new per-file layout. Idempotent: writes only
// for tag names that don't already have a file on disk, then clears
// the cached slice so the next Mutate drops the key from the yaml.
// Returns the count of tags materialised.
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
			continue
		}
		p := m.tagPath(username, t.Name, t.Shared)
		if _, err := os.Stat(p); err == nil {
			continue // already migrated; don't stomp disk copy on re-run
		}
		if err := atomicWrite(p, []byte(t.SystemPrompt), 0o600); err != nil {
			return written, err
		}
		written++
	}
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
