package brain

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestNewPathMap_Empty — fresh map starts at current version + non-nil
// entries.
func TestNewPathMap_Empty(t *testing.T) {
	pm := NewPathMap()
	if pm.Version != PathMapVersion {
		t.Errorf("version = %d, want %d", pm.Version, PathMapVersion)
	}
	if pm.Entries == nil {
		t.Error("Entries nil; want non-nil empty map")
	}
}

// TestLoadPathMap_Missing — file absent yields empty map, not error.
func TestLoadPathMap_Missing(t *testing.T) {
	dir := t.TempDir()
	pm, err := LoadPathMap(filepath.Join(dir, "no-such.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if pm == nil || len(pm.Entries) != 0 {
		t.Errorf("got %v, want empty PathMap", pm)
	}
}

// TestSaveLoad_Roundtrip — Save+LoadPathMap preserves entries.
func TestSaveLoad_Roundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".neo", "path_map.json")
	pm := NewPathMap()
	if err := pm.Set("github.com/x/y", PathMapEntry{Path: "/local/y"}); err != nil {
		t.Fatal(err)
	}
	if err := pm.Set("project:foo:bar", PathMapEntry{Action: ActionSkip}); err != nil {
		t.Fatal(err)
	}
	if err := pm.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := LoadPathMap(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Entries) != 2 {
		t.Errorf("entries = %d, want 2", len(got.Entries))
	}
	e1, _ := got.Lookup("github.com/x/y")
	if e1.Action != ActionRestore || e1.Path != "/local/y" {
		t.Errorf("entry 1 drift: %+v", e1)
	}
	e2, _ := got.Lookup("project:foo:bar")
	if e2.Action != ActionSkip {
		t.Errorf("entry 2 action = %q, want skip", e2.Action)
	}
}

// TestSet_RejectsEmptyCanonicalID — defensive.
func TestSet_RejectsEmptyCanonicalID(t *testing.T) {
	pm := NewPathMap()
	if err := pm.Set("", PathMapEntry{Path: "/x"}); err == nil {
		t.Error("empty canonical_id should error")
	}
}

// TestSet_RestoreRequiresPath — action=restore + empty path is invalid.
func TestSet_RestoreRequiresPath(t *testing.T) {
	pm := NewPathMap()
	if err := pm.Set("id", PathMapEntry{Action: ActionRestore, Path: ""}); err == nil {
		t.Error("restore with empty path should error")
	}
}

// TestSet_DefaultActionSkipWhenPathEmpty — path empty + action unset → skip.
func TestSet_DefaultActionSkipWhenPathEmpty(t *testing.T) {
	pm := NewPathMap()
	if err := pm.Set("id", PathMapEntry{}); err != nil {
		t.Fatal(err)
	}
	e, _ := pm.Lookup("id")
	if e.Action != ActionSkip {
		t.Errorf("action = %q, want skip", e.Action)
	}
}

// TestLoadPathMap_FutureVersion — version > current → error with hint.
func TestLoadPathMap_FutureVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "path_map.json")
	data, _ := json.Marshal(map[string]any{"version": PathMapVersion + 9, "entries": map[string]any{}})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadPathMap(path)
	if err == nil {
		t.Fatal("future version should error")
	}
}

// TestLoadPathMap_ZeroVersion_Upgraded — files written by very early
// builds without version field still load and are stamped.
func TestLoadPathMap_ZeroVersion_Upgraded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "path_map.json")
	if err := os.WriteFile(path, []byte(`{"entries":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pm, err := LoadPathMap(path)
	if err != nil {
		t.Fatal(err)
	}
	if pm.Version != PathMapVersion {
		t.Errorf("version not auto-upgraded: %d", pm.Version)
	}
}

// TestResolveWorkspacePath_LocalRegistryHit — registry wins over path_map.
func TestResolveWorkspacePath_LocalRegistryHit(t *testing.T) {
	hits := map[string]string{"github.com/x/y": "/home/me/y"}
	pm := NewPathMap()
	_ = pm.Set("github.com/x/y", PathMapEntry{Path: "/should-not-win"})
	res := ResolveWorkspacePath("github.com/x/y", "/Users/them/y", hits, pm, false, "")
	if res.Source != ResolutionSourceLocalRegistry {
		t.Errorf("source = %q, want local_registry", res.Source)
	}
	if res.Path != "/home/me/y" {
		t.Errorf("path = %q, want /home/me/y", res.Path)
	}
}

// TestResolveWorkspacePath_PathMapFallback — registry miss → path_map.
func TestResolveWorkspacePath_PathMapFallback(t *testing.T) {
	pm := NewPathMap()
	_ = pm.Set("github.com/x/y", PathMapEntry{Path: "/mapped/y"})
	res := ResolveWorkspacePath("github.com/x/y", "/Users/them/y", nil, pm, false, "")
	if res.Source != ResolutionSourcePathMap || res.Path != "/mapped/y" || res.Action != ActionRestore {
		t.Errorf("unexpected res: %+v", res)
	}
}

// TestResolveWorkspacePath_PathMapSkip — entry with action=skip surfaces.
func TestResolveWorkspacePath_PathMapSkip(t *testing.T) {
	pm := NewPathMap()
	_ = pm.Set("github.com/x/y", PathMapEntry{Action: ActionSkip})
	res := ResolveWorkspacePath("github.com/x/y", "/Users/them/y", nil, pm, false, "")
	if res.Action != ActionSkip || !contains(res.Reason, "skip") {
		t.Errorf("unexpected res: %+v", res)
	}
}

// TestResolveWorkspacePath_AutoClone — git-shaped canonical_id + autoClone.
func TestResolveWorkspacePath_AutoClone(t *testing.T) {
	pm := NewPathMap()
	res := ResolveWorkspacePath("github.com/foo/bar", "/Users/them/bar", nil, pm, true, "/home/me/projects")
	if res.Source != ResolutionSourceAutoClone {
		t.Errorf("source = %q, want auto_clone", res.Source)
	}
	if res.Action != ActionClone {
		t.Errorf("action = %q, want clone", res.Action)
	}
	want := filepath.Join("/home/me/projects", "github.com/foo/bar")
	if res.Path != want {
		t.Errorf("path = %q, want %q", res.Path, want)
	}
}

// TestResolveWorkspacePath_AutoCloneRejectsLocalShape — "local:..." or
// "project:..." canonical_ids never auto-clone (not a real URL).
func TestResolveWorkspacePath_AutoCloneRejectsLocalShape(t *testing.T) {
	pm := NewPathMap()
	for _, id := range []string{"local:abc123", "project:foo:bar", "bare-name"} {
		res := ResolveWorkspacePath(id, "/x", nil, pm, true, "/home/me/projects")
		if res.Source == ResolutionSourceAutoClone {
			t.Errorf("canonical_id %q should NOT auto-clone, got %+v", id, res)
		}
	}
}

// TestResolveWorkspacePath_PromptNeeded — no registry hit, no path_map,
// auto-clone disabled (or canonical not git-shaped) → operator prompt.
func TestResolveWorkspacePath_PromptNeeded(t *testing.T) {
	pm := NewPathMap()
	res := ResolveWorkspacePath("local:abc", "/Users/them/x", nil, pm, false, "")
	if res.Source != ResolutionSourcePromptNeeded {
		t.Errorf("source = %q, want prompt_needed", res.Source)
	}
	if res.Action != ActionSkip {
		t.Errorf("action = %q, want skip (prompt-needed defaults to skip)", res.Action)
	}
	if !contains(res.Reason, "no local registry") {
		t.Errorf("reason missing context: %q", res.Reason)
	}
}

// TestBuildRegistryHits — basic + filters out empty IDs.
func TestBuildRegistryHits(t *testing.T) {
	walked := []WalkedWorkspace{
		{ID: "a", Path: "/a", CanonicalID: "github.com/x/y"},
		{ID: "b", Path: "/b", CanonicalID: ""}, // dropped
		{ID: "c", Path: "", CanonicalID: "z"},  // dropped (empty path)
	}
	hits := BuildRegistryHits(walked)
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1: %v", len(hits), hits)
	}
	if hits["github.com/x/y"] != "/a" {
		t.Errorf("hit drift: %v", hits)
	}
}

// TestCanonicalLooksGitClonable — covers the heuristic edge cases.
func TestCanonicalLooksGitClonable(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"github.com/foo/bar", true},
		{"gitlab.local:8443/x/y", true},
		{"local:abc", false},
		{"project:foo:bar", false},
		{"foo/bar", false},     // only one slash
		{"foo/bar/baz", false}, // first segment has no dot
	}
	for _, c := range cases {
		if got := canonicalLooksGitClonable(c.in); got != c.want {
			t.Errorf("canonicalLooksGitClonable(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// contains is a tiny helper for substring checks in error messages.
func contains(s, sub string) bool { return len(s) > 0 && len(sub) > 0 && stringIndex(s, sub) >= 0 }

// stringIndex avoids importing strings just for one Index call in tests.
func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
