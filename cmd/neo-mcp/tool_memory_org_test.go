package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/knowledge"
)

// TestResolveStoreTier_OrgNilReturnsError verifies tier="org" without orgKS
// wired produces a clear actionable error message. [355.A]
func TestResolveStoreTier_OrgNilReturnsError(t *testing.T) {
	tool := &MemoryTool{ks: nil, orgKS: nil}
	_, _, err := tool.resolveStoreTier("org")
	if err == nil {
		t.Fatal("expected error for tier=org with orgKS=nil")
	}
	if !strings.Contains(err.Error(), "org coordinator") {
		t.Errorf("error should mention coordinator requirement, got: %v", err)
	}
}

// TestResolveStoreTier_OrgRoutesToOrgKS verifies tier="org" with orgKS wired
// returns the org store (and NOT the project ks). [355.A]
func TestResolveStoreTier_OrgRoutesToOrgKS(t *testing.T) {
	dir := t.TempDir()
	orgKS, err := knowledge.Open(filepath.Join(dir, "org.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer orgKS.Close()
	// Fake project ks with a DIFFERENT backing file — we assert the pointer
	// returned matches orgKS, not ks.
	projectKS, err := knowledge.Open(filepath.Join(dir, "project.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer projectKS.Close()

	tool := &MemoryTool{ks: projectKS, orgKS: orgKS}
	got, _, err := tool.resolveStoreTier("org")
	if err != nil {
		t.Fatalf("tier=org with orgKS wired: %v", err)
	}
	if got != orgKS {
		t.Error("tier=org routed to project ks instead of orgKS")
	}
}

// TestResolveStoreTier_ProjectStillRoutesToKS verifies the existing behaviour
// is preserved: tier="project" and "" and "workspace" keep pointing to
// MemoryTool.ks regardless of whether orgKS is set. [355.A regression]
func TestResolveStoreTier_ProjectStillRoutesToKS(t *testing.T) {
	dir := t.TempDir()
	projectKS, err := knowledge.Open(filepath.Join(dir, "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer projectKS.Close()
	orgKS, _ := knowledge.Open(filepath.Join(dir, "o.db"))
	defer orgKS.Close()

	tool := &MemoryTool{ks: projectKS, orgKS: orgKS}
	for _, tier := range []string{"", "project", "workspace"} {
		got, _, err := tool.resolveStoreTier(tier)
		if err != nil {
			t.Errorf("tier=%q: %v", tier, err)
			continue
		}
		if got != projectKS {
			t.Errorf("tier=%q routed to wrong store", tier)
		}
	}
}

// TestResolveStoreTier_UnknownTierErrorsOutExplicitly verifies an unknown tier
// mentions all supported values including `org`. [355.A]
func TestResolveStoreTier_UnknownTierErrorsOutExplicitly(t *testing.T) {
	tool := &MemoryTool{}
	_, _, err := tool.resolveStoreTier("galaxy")
	if err == nil {
		t.Fatal("expected error for unknown tier")
	}
	for _, needle := range []string{"workspace", "project", "org"} {
		if !strings.Contains(err.Error(), needle) {
			t.Errorf("error should mention supported tier %q: %v", needle, err)
		}
	}
}
