package federation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newOrgDebtDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	orgDir := filepath.Join(d, ".neo-org")
	if err := os.MkdirAll(orgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	return orgDir
}

// TestAppendOrgDebt_AssignsDefaultsAndPersists verifies ID auto-gen + defaults
// (Priority=P2, DetectedAt=now) + round-trip via ListOrgDebt. [356.A]
func TestAppendOrgDebt_AssignsDefaultsAndPersists(t *testing.T) {
	dir := newOrgDebtDir(t)
	e, err := AppendOrgDebt(dir, OrgDebtEntry{
		Title:            "Go 1.26 upgrade pending",
		AffectedProjects: []string{"strategos-project", "neoanvil"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.ID == "" {
		t.Error("expected generated ID")
	}
	if e.Priority != "P2" {
		t.Errorf("default Priority = %q, want P2", e.Priority)
	}
	if e.DetectedAt.IsZero() {
		t.Error("DetectedAt not set")
	}

	all, err := ListOrgDebt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("ListOrgDebt = %d, want 1", len(all))
	}
	if all[0].Title != "Go 1.26 upgrade pending" {
		t.Errorf("round-trip mismatch: %+v", all[0])
	}
}

// TestResolveOrgDebt_Transitions covers resolve success + errors. [356.A]
func TestResolveOrgDebt_Transitions(t *testing.T) {
	dir := newOrgDebtDir(t)
	e, _ := AppendOrgDebt(dir, OrgDebtEntry{Title: "TLS cert rotation"})

	if err := ResolveOrgDebt(dir, e.ID, "strategos-32492", "rotated manually"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	// Second resolve → ErrOrgDebtAlreadyResolved.
	if err := ResolveOrgDebt(dir, e.ID, "x", "y"); err != ErrOrgDebtAlreadyResolved {
		t.Errorf("double-resolve: got %v, want ErrOrgDebtAlreadyResolved", err)
	}
	// Unknown ID → ErrOrgDebtNotFound.
	if err := ResolveOrgDebt(dir, "bogus", "x", "y"); err != ErrOrgDebtNotFound {
		t.Errorf("unknown id: got %v, want ErrOrgDebtNotFound", err)
	}
}

// TestListOrgDebt_MissingFile_ReturnsNilSlice verifies graceful no-op. [356.A]
func TestListOrgDebt_MissingFile_ReturnsNilSlice(t *testing.T) {
	dir := t.TempDir() + "/no-such-org"
	got, err := ListOrgDebt(dir)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil slice, got: %v", got)
	}
}

// TestAppendOrgDebt_RendersPriorityBuckets verifies output contains headers
// for all 4 priority levels and the entry's row. [356.A]
func TestAppendOrgDebt_RendersPriorityBuckets(t *testing.T) {
	dir := newOrgDebtDir(t)
	_, _ = AppendOrgDebt(dir, OrgDebtEntry{
		Title:            "Blocking migration",
		Priority:         "P0",
		AffectedProjects: []string{"p1", "p2"},
	})

	raw, _ := os.ReadFile(filepath.Join(dir, orgDebtFile))
	s := string(raw)
	for _, needle := range []string{
		"# Org-Level Technical Debt",
		"neo-org-debt-v1",
		"## P0 — Blocker",
		"## P1 — High",
		"## P2 — Medium",
		"## P3 — Observational",
		"## Resolved (0)",
		"Blocking migration",
		"p1,p2",
	} {
		if !strings.Contains(s, needle) {
			t.Errorf("missing %q in rendered output:\n%s", needle, s)
		}
	}
}

// TestAppendOrgDebt_TitleRequired verifies validation. [356.A]
func TestAppendOrgDebt_TitleRequired(t *testing.T) {
	dir := newOrgDebtDir(t)
	_, err := AppendOrgDebt(dir, OrgDebtEntry{Priority: "P1"})
	if err == nil {
		t.Error("expected error when title is empty")
	}
}
