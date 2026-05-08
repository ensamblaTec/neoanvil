package brain

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/workspace"
)

// TestWalkWorkspaces_DropsMissingPaths — registry with one extant + one
// stale workspace returns only the extant one. Verifies the silent-drop
// contract.
func TestWalkWorkspaces_DropsMissingPaths(t *testing.T) {
	live := t.TempDir()
	stale := filepath.Join(t.TempDir(), "deleted-workspace")

	reg := &workspace.Registry{
		Workspaces: []workspace.WorkspaceEntry{
			{ID: "live-1", Path: live, Name: filepath.Base(live), Type: "workspace"},
			{ID: "stale-1", Path: stale, Name: "deleted-workspace", Type: "workspace"},
		},
		ActiveID: "live-1",
	}

	out := WalkWorkspaces(reg)
	if len(out) != 1 {
		t.Fatalf("got %d entries, want 1 (stale should be dropped)", len(out))
	}
	if out[0].ID != "live-1" {
		t.Errorf("kept ID = %q, want live-1", out[0].ID)
	}
	if out[0].CanonicalID == "" {
		t.Error("CanonicalID empty — resolver should always produce something")
	}
}

// TestWalkWorkspaces_NilRegistry — nil registry yields nil slice (no panic).
func TestWalkWorkspaces_NilRegistry(t *testing.T) {
	if got := WalkWorkspaces(nil); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

// TestWalkWorkspaces_EmptyRegistry — empty registry yields nil slice.
func TestWalkWorkspaces_EmptyRegistry(t *testing.T) {
	reg := &workspace.Registry{}
	if got := WalkWorkspaces(reg); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

// TestWalkDependencies_DedupSharedProject — two workspaces under the same
// project root produce ONE WalkedProject entry with both workspace IDs.
func TestWalkDependencies_DedupSharedProject(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".neo-project"), 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte("project_name: planifier\nmember_workspaces: []\ndominant_lang: go\n")
	if err := os.WriteFile(filepath.Join(root, ".neo-project", "neo.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}

	backend := filepath.Join(root, "backend")
	frontend := filepath.Join(root, "frontend")
	for _, d := range []string{backend, frontend} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	wss := []WalkedWorkspace{
		{ID: "be-1", Path: backend, Name: "backend"},
		{ID: "fe-1", Path: frontend, Name: "frontend"},
	}
	projects, orgs := WalkDependencies(wss)

	if len(projects) != 1 {
		t.Fatalf("got %d projects, want 1 (dedup failed)", len(projects))
	}
	if len(projects[0].Members) != 2 {
		t.Errorf("got %d members, want 2", len(projects[0].Members))
	}
	if projects[0].CanonicalID != "project:planifier:_root" {
		t.Errorf("CanonicalID = %q, want project:planifier:_root", projects[0].CanonicalID)
	}
	if len(orgs) != 0 {
		t.Errorf("orgs not empty, got %v", orgs)
	}
}

// TestWalkDependencies_NoNeoProject — workspace without .neo-project
// neighbour returns no projects. Verifies the walk doesn't false-positive.
func TestWalkDependencies_NoNeoProject(t *testing.T) {
	dir := t.TempDir()
	wss := []WalkedWorkspace{{ID: "lone", Path: dir, Name: "lone"}}
	projects, orgs := WalkDependencies(wss)
	if len(projects) != 0 {
		t.Errorf("got %d projects, want 0", len(projects))
	}
	if len(orgs) != 0 {
		t.Errorf("got %d orgs, want 0", len(orgs))
	}
}

// TestWalkDependencies_OrgRoot — workspace under .neo-org/ produces a
// WalkedOrg entry.
func TestWalkDependencies_OrgRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".neo-org"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}

	wss := []WalkedWorkspace{{ID: "ws-1", Path: ws, Name: "ws"}}
	_, orgs := WalkDependencies(wss)

	if len(orgs) != 1 {
		t.Fatalf("got %d orgs, want 1", len(orgs))
	}
	if !filepathHasSuffix(orgs[0].Path, filepath.Base(root)) {
		t.Errorf("org Path = %q, want suffix %q", orgs[0].Path, filepath.Base(root))
	}
	if orgs[0].CanonicalID == "" {
		t.Error("org CanonicalID empty")
	}
}

// TestReplaceLastSegment — boundary cases. Used internally by
// projectCanonical.
func TestReplaceLastSegment(t *testing.T) {
	cases := []struct {
		in, seg, want string
	}{
		{"project:planifier:backend", "_root", "project:planifier:_root"},
		{"foo", "_root", "foo:_root"},  // no colon → append
		{":x", "_root", ":_root"},      // leading colon → replace x
		{"a:b:c:d", "_root", "a:b:c:_root"},
	}
	for _, c := range cases {
		if got := replaceLastSegment(c.in, c.seg); got != c.want {
			t.Errorf("replaceLastSegment(%q,%q) = %q, want %q", c.in, c.seg, got, c.want)
		}
	}
}

// TestPathExists — file present, file absent, empty path.
func TestPathExists(t *testing.T) {
	if pathExists("") {
		t.Error("empty path should be false")
	}
	if !pathExists(t.TempDir()) {
		t.Error("temp dir should be true")
	}
	if pathExists(filepath.Join(t.TempDir(), "definitely-not-here")) {
		t.Error("missing file should be false")
	}
}

// TestSortByPath — sort by key produces deterministic order. Confirms
// the manifest will hash consistently across runs.
func TestSortByPath(t *testing.T) {
	type item struct{ Path string }
	in := []item{{"/c"}, {"/a"}, {"/b"}}
	sortByPath(in, func(i item) string { return i.Path })
	want := []string{"/a", "/b", "/c"}
	for i, e := range in {
		if e.Path != want[i] {
			t.Errorf("idx %d: got %q, want %q", i, e.Path, want[i])
		}
	}
}

// filepathHasSuffix is a small helper to assert path-tail matches without
// requiring the full path which differs between TempDir invocations.
func filepathHasSuffix(p, suffix string) bool {
	return filepath.Base(p) == suffix
}
