package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill creates a minimal .claude/skills/<name>/SKILL.md in dir. [128.1]
func writeSkill(t *testing.T, dir, name, frontmatter, body string) {
	t.Helper()
	skillDir := filepath.Join(dir, ".claude", "skills", name)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "---\n" + frontmatter + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// TestFolderAuditAllOK verifies that a valid, referenced skill reports no issues. [128.1]
func TestFolderAuditAllOK(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "my-skill",
		"name: my-skill\ndescription: A valid skill for testing.", "")
	// CLAUDE.md references the skill.
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("see my-skill skill"), 0600); err != nil {
		t.Fatal(err)
	}

	rows, err := auditClaudeFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	r := rows[0]
	if !r.exists {
		t.Error("exists should be true")
	}
	if !r.inCLAUDE {
		t.Error("inCLAUDE should be true")
	}
	if !r.pathsValid {
		t.Error("pathsValid should be true when no paths: field")
	}
	if len(r.brokenXrefs) > 0 {
		t.Errorf("expected no broken xrefs, got %v", r.brokenXrefs)
	}
}

// TestFolderAuditMissingSkill verifies that a skill referenced in CLAUDE.md
// but not present in .claude/skills/ does not appear as a row (we audit existing
// skill files, not hypothetical ones). This tests the inverse: a skill dir without
// CLAUDE.md reference shows inCLAUDE=false. [128.1]
func TestFolderAuditMissingSkillRef(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "orphan-skill",
		"name: orphan-skill\ndescription: Not referenced anywhere.", "")
	// CLAUDE.md does NOT reference this skill.
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("no references here"), 0600); err != nil {
		t.Fatal(err)
	}

	rows, err := auditClaudeFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].inCLAUDE {
		t.Error("inCLAUDE should be false for orphaned skill")
	}
}

// TestFolderAuditBrokenPathGlob verifies that a paths: glob matching no files
// sets pathsValid=false. [128.1]
func TestFolderAuditBrokenPathGlob(t *testing.T) {
	dir := t.TempDir()
	fm := "name: path-skill\ndescription: Skill with broken paths.\npaths:\n  - \"nonexistent/**/*.go\""
	writeSkill(t, dir, "path-skill", fm, "")

	rows, err := auditClaudeFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].pathsValid {
		t.Error("pathsValid should be false when glob matches nothing")
	}
}

// TestFolderAuditDoublestarValid verifies that paths globs containing `**`
// (which Go's filepath.Glob does NOT natively support) are validated by
// checking the prefix-directory existence. This is the canonical pattern for
// path-scoped skills (e.g. pkg/dba/**, cmd/**/*.go).
func TestFolderAuditDoublestarValid(t *testing.T) {
	dir := t.TempDir()
	// Create the prefix directory that the doublestar glob should match.
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "scope"), 0755); err != nil {
		t.Fatal(err)
	}
	fm := "name: scope-skill\ndescription: Doublestar paths test.\npaths:\n  - \"pkg/scope/**\"\n  - \"pkg/**/*.go\""
	writeSkill(t, dir, "scope-skill", fm, "")

	rows, err := auditClaudeFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if !rows[0].pathsValid {
		t.Error("pathsValid should be true when ** prefix directory exists")
	}
}

// TestFolderAuditInventoryAtNewPath verifies that inventory references are
// detected at the post-2026-05-13 canonical path (docs/plugins/) — not just
// the legacy docs/ root. [128.3]
func TestFolderAuditInventoryAtNewPath(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "doc-skill",
		"name: doc-skill\ndescription: Skill listed in plugins inventory.", "")
	// Place inventory in the post-refactor location.
	pluginsDir := filepath.Join(dir, "docs", "plugins")
	if err := os.MkdirAll(pluginsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginsDir, "claude-folder-inventory.md"),
		[]byte("contains doc-skill reference"), 0600); err != nil {
		t.Fatal(err)
	}

	rows, err := auditClaudeFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if !rows[0].inInventory {
		t.Error("inInventory should be true when inventory is at docs/plugins/")
	}
}

// TestFolderAuditBrokenXref verifies that a markdown link pointing to a
// non-existent file is reported as a broken xref. [128.1]
func TestFolderAuditBrokenXref(t *testing.T) {
	dir := t.TempDir()
	// Body contains a relative link that does not exist.
	body := "See [missing file](../rules/does-not-exist.md) for details."
	writeSkill(t, dir, "xref-skill",
		"name: xref-skill\ndescription: Skill with a broken cross-reference.", body)

	rows, err := auditClaudeFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if len(rows[0].brokenXrefs) == 0 {
		t.Error("expected at least one broken xref")
	}
	if !strings.Contains(rows[0].brokenXrefs[0], "does-not-exist.md") {
		t.Errorf("broken xref should mention does-not-exist.md, got %v", rows[0].brokenXrefs)
	}
}
