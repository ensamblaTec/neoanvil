package federation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestProjDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d, ".neo-project"), 0o750); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(d, ".neo-project")
}

// TestAppendContractProposal_AssignsIDAndPersists verifies the initial write
// generates an ID, sets status=pending, and round-trips through ListProposals. [343.A]
func TestAppendContractProposal_AssignsIDAndPersists(t *testing.T) {
	dir := newTestProjDir(t)
	p, err := AppendContractProposal(dir, ContractProposal{
		FromWorkspace:   "backend-go",
		Endpoint:        "POST /api/users",
		ChangeType:      "request_schema_changed",
		AffectedCallers: []string{"features/users/CreateUser.tsx"},
	})
	if err != nil {
		t.Fatalf("AppendContractProposal: %v", err)
	}
	if p.ID == "" {
		t.Error("expected generated ID")
	}
	if p.Status != "pending" {
		t.Errorf("Status = %q, want pending", p.Status)
	}
	if p.ProposedAt.IsZero() {
		t.Error("expected ProposedAt set")
	}

	all, err := ListProposals(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("ListProposals = %d, want 1", len(all))
	}
	if all[0].ID != p.ID {
		t.Errorf("round-trip ID mismatch")
	}
}

// TestListPendingProposals_FiltersByStatus verifies the filter helper. [343.A]
func TestListPendingProposals_FiltersByStatus(t *testing.T) {
	dir := newTestProjDir(t)
	a, _ := AppendContractProposal(dir, ContractProposal{Endpoint: "GET /a", FromWorkspace: "x"})
	_, _ = AppendContractProposal(dir, ContractProposal{Endpoint: "GET /b", FromWorkspace: "x"})

	// Resolve one; the other stays pending.
	if err := ResolveProposal(dir, a.ID, "approved", "frontend-ts", "LGTM"); err != nil {
		t.Fatal(err)
	}

	pending, err := ListPendingProposals(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	if pending[0].Endpoint != "GET /b" {
		t.Errorf("unexpected pending: %+v", pending[0])
	}
}

// TestResolveProposal_Transitions verifies status transitions + errors. [343.A]
func TestResolveProposal_Transitions(t *testing.T) {
	dir := newTestProjDir(t)
	p, _ := AppendContractProposal(dir, ContractProposal{Endpoint: "POST /x"})

	if err := ResolveProposal(dir, p.ID, "approved", "frontend-ts", "ok"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	// Second resolve → ErrProposalAlreadyResolved
	if err := ResolveProposal(dir, p.ID, "rejected", "frontend-ts", "nope"); err != ErrProposalAlreadyResolved {
		t.Errorf("expected ErrProposalAlreadyResolved, got: %v", err)
	}
	// Unknown ID → ErrProposalNotFound
	if err := ResolveProposal(dir, "bogus-id", "approved", "x", ""); err != ErrProposalNotFound {
		t.Errorf("expected ErrProposalNotFound, got: %v", err)
	}
	// Invalid status → error
	q, _ := AppendContractProposal(dir, ContractProposal{Endpoint: "GET /y"})
	if err := ResolveProposal(dir, q.ID, "invalid-status", "x", ""); err == nil {
		t.Error("expected error on invalid status")
	}
}

// TestAppendContractProposal_RendersReadableTable verifies the markdown output
// contains both the JSON block (source of truth) and a human table. [343.A]
func TestAppendContractProposal_RendersReadableTable(t *testing.T) {
	dir := newTestProjDir(t)
	_, _ = AppendContractProposal(dir, ContractProposal{
		FromWorkspace: "backend",
		Endpoint:      "DELETE /api/users/{id}",
		ChangeType:    "route_removed",
		AffectedCallers: []string{
			"features/users/DeleteUserDialog.tsx",
			"pages/admin.tsx",
		},
	})
	raw, _ := os.ReadFile(filepath.Join(dir, contractProposalsFile))
	s := string(raw)
	for _, needle := range []string{
		"# Contract Proposals Pending Approval",
		"neo-contract-proposals-v1",
		"## Pending (1)",
		"DELETE /api/users/{id}",
		"route_removed",
	} {
		if !strings.Contains(s, needle) {
			t.Errorf("missing %q in output:\n%s", needle, s)
		}
	}
}

// TestListProposals_MissingFile returns (nil, nil) when file absent. [343.A]
func TestListProposals_MissingFile(t *testing.T) {
	got, err := ListProposals(t.TempDir() + "/no-project-dir")
	if err != nil {
		t.Errorf("missing file should not error, got: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil slice, got: %v", got)
	}
}
