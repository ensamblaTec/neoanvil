package federation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newOrgDirectivesDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	orgDir := filepath.Join(d, ".neo-org")
	if err := os.MkdirAll(orgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	return orgDir
}

// TestAppendOrgDirective_MonotonicID verifies IDs start at 1 and increment. [356.B]
func TestAppendOrgDirective_MonotonicID(t *testing.T) {
	dir := newOrgDirectivesDir(t)
	d1, _ := AppendOrgDirective(dir, "First rule", nil)
	d2, _ := AppendOrgDirective(dir, "Second rule", nil)
	d3, _ := AppendOrgDirective(dir, "Third rule", nil)
	if d1.ID != 1 || d2.ID != 2 || d3.ID != 3 {
		t.Errorf("ID sequence broken: %d, %d, %d", d1.ID, d2.ID, d3.ID)
	}
}

// TestAppendOrgDirective_SupersedesMarksOld verifies that `supersedes: [1, 2]`
// auto-deprecates IDs 1 and 2. [356.B]
func TestAppendOrgDirective_SupersedesMarksOld(t *testing.T) {
	dir := newOrgDirectivesDir(t)
	_, _ = AppendOrgDirective(dir, "Old A", nil)
	_, _ = AppendOrgDirective(dir, "Old B", nil)
	_, _ = AppendOrgDirective(dir, "Current", []int{1, 2})

	all, _ := ListOrgDirectives(dir)
	if !all[0].Deprecated || !all[1].Deprecated {
		t.Error("supersedes did not deprecate old IDs")
	}
	if all[2].Deprecated {
		t.Error("new directive should not be deprecated")
	}
}

// TestDeprecateOrgDirective_SoftDelete verifies soft-delete + ErrAlreadyDeprecated. [356.B]
func TestDeprecateOrgDirective_SoftDelete(t *testing.T) {
	dir := newOrgDirectivesDir(t)
	_, _ = AppendOrgDirective(dir, "Retire me", nil)
	if err := DeprecateOrgDirective(dir, 1); err != nil {
		t.Fatalf("first deprecate: %v", err)
	}
	all, _ := ListOrgDirectives(dir)
	if !all[0].Deprecated {
		t.Error("Deprecated flag not set")
	}
	if err := DeprecateOrgDirective(dir, 1); err != ErrOrgDirectiveAlreadyDeprecated {
		t.Errorf("double-deprecate: got %v, want ErrAlreadyDeprecated", err)
	}
	if err := DeprecateOrgDirective(dir, 99); err != ErrOrgDirectiveNotFound {
		t.Errorf("unknown id: got %v, want ErrNotFound", err)
	}
}

// TestUpdateOrgDirective_ReplacesText verifies in-place text update. [356.B]
func TestUpdateOrgDirective_ReplacesText(t *testing.T) {
	dir := newOrgDirectivesDir(t)
	_, _ = AppendOrgDirective(dir, "Use TLS 1.2+", nil)
	if err := UpdateOrgDirective(dir, 1, "Use TLS 1.3+ only"); err != nil {
		t.Fatalf("update: %v", err)
	}
	all, _ := ListOrgDirectives(dir)
	if all[0].Text != "Use TLS 1.3+ only" {
		t.Errorf("update failed: %q", all[0].Text)
	}
}

// TestListOrgDirectives_MissingFile verifies graceful (nil, nil). [356.B]
func TestListOrgDirectives_MissingFile(t *testing.T) {
	got, err := ListOrgDirectives(t.TempDir() + "/nonexistent")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

// TestAppendOrgDirective_RendersList verifies the markdown output includes
// the numbered list + deprecation strikethrough. [356.B]
func TestAppendOrgDirective_RendersList(t *testing.T) {
	dir := newOrgDirectivesDir(t)
	_, _ = AppendOrgDirective(dir, "Active rule", nil)
	_, _ = AppendOrgDirective(dir, "Old rule", nil)
	_, _ = AppendOrgDirective(dir, "New rule", []int{2}) // supersedes old

	raw, _ := os.ReadFile(filepath.Join(dir, orgDirectivesFile))
	s := string(raw)
	for _, needle := range []string{
		"# Org-Level Architectural Directives",
		"neo-org-directives-v1",
		"## Active (2 / 3 total)",
		"1. Active rule",
		"2. ~~OBSOLETO~~ Old rule",
		"3. New rule",
		"supersedes: [2]",
	} {
		if !strings.Contains(s, needle) {
			t.Errorf("missing %q in output:\n%s", needle, s)
		}
	}
}

// TestAppendOrgDirective_EmptyTextError verifies validation. [356.B]
func TestAppendOrgDirective_EmptyTextError(t *testing.T) {
	dir := newOrgDirectivesDir(t)
	if _, err := AppendOrgDirective(dir, "", nil); err == nil {
		t.Error("expected error on empty text")
	}
}
