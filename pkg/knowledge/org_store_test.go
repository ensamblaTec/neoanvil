package knowledge

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestOpenOrgStore_StandaloneWorkspaceShortCircuits verifies that a workspace
// without `.neo-project/` (IsStandaloneWorkspace=true) returns (nil, nil)
// without attempting to open any DB. [354.B]
func TestOpenOrgStore_StandaloneWorkspaceShortCircuits(t *testing.T) {
	ks, err := OpenOrgStore(OrgStoreConfig{
		IsStandaloneWorkspace: true,
		OrgDir:                "/any/path",
		ProjectRoot:           "/whatever",
	})
	if err != nil {
		t.Errorf("standalone should not error: %v", err)
	}
	if ks != nil {
		t.Error("standalone should return nil store")
	}
}

// TestOpenOrgStore_NoOrgDirShortCircuits verifies that OrgDir=="" (no
// `.neo-org/` discovered) returns (nil, nil). [354.B]
func TestOpenOrgStore_NoOrgDirShortCircuits(t *testing.T) {
	ks, err := OpenOrgStore(OrgStoreConfig{
		OrgDir:      "",
		ProjectRoot: "/p",
	})
	if err != nil {
		t.Errorf("no org should not error: %v", err)
	}
	if ks != nil {
		t.Error("no org should return nil store")
	}
}

// TestOpenOrgStore_NonCoordinatorReturnsReadOnlyError verifies that a project
// that isn't the coordinator receives ErrOrgStoreReadOnly — signalling the
// caller to proxy writes via Nexus. [354.B]
func TestOpenOrgStore_NonCoordinatorReturnsReadOnlyError(t *testing.T) {
	_, err := OpenOrgStore(OrgStoreConfig{
		OrgDir:             t.TempDir(),
		ProjectRoot:        "/some/non-coord-project",
		CoordinatorProject: "the-chosen-one",
	})
	if !errors.Is(err, ErrOrgStoreReadOnly) {
		t.Errorf("expected ErrOrgStoreReadOnly, got: %v", err)
	}
}

// TestOpenOrgStore_CoordinatorOpensRW verifies that the designated coordinator
// opens a live RW KnowledgeStore at the default path under OrgDir. [354.B]
func TestOpenOrgStore_CoordinatorOpensRW(t *testing.T) {
	orgDir := t.TempDir()
	ks, err := OpenOrgStore(OrgStoreConfig{
		OrgDir:             orgDir,
		ProjectRoot:        "/abs/path/the-coord",
		CoordinatorProject: "the-coord", // basename match
	})
	if err != nil {
		t.Fatalf("coordinator open: %v", err)
	}
	if ks == nil {
		t.Fatal("coordinator should receive a live store")
	}
	defer ks.Close()

	// The store should accept Put — smoke-test RW mode.
	if err := ks.Put("contracts", "test-key", KnowledgeEntry{
		Namespace: "contracts",
		Key:       "test-key",
		Content:   "hello from org",
	}); err != nil {
		t.Errorf("coordinator RW put failed: %v", err)
	}

	// Verify the DB file landed at the expected path.
	want := filepath.Join(orgDir, "db", "org.db")
	if got, err := ks.db.Path(), error(nil); err != nil || got != want {
		t.Errorf("DB path = %q, want %q (err=%v)", got, want, err)
	}
}

// TestOpenOrgStore_LegacyModeEveryProjectIsCoord verifies that when
// CoordinatorProject is empty, every project claims coordinator — the
// backwards-compatible escape hatch. [354.B]
func TestOpenOrgStore_LegacyModeEveryProjectIsCoord(t *testing.T) {
	ks, err := OpenOrgStore(OrgStoreConfig{
		OrgDir:             t.TempDir(),
		ProjectRoot:        "/any/project",
		CoordinatorProject: "", // legacy
	})
	if err != nil {
		t.Fatalf("legacy open: %v", err)
	}
	if ks == nil {
		t.Fatal("legacy should give any project a live store")
	}
	ks.Close()
}

// TestIsCoordinatorProject_ParityWithConfigVariant verifies the local mirror
// stays aligned with config.IsCoordinatorProject semantics. [354.B]
func TestIsCoordinatorProject_ParityWithConfigVariant(t *testing.T) {
	cases := []struct {
		projectRoot, coord string
		want               bool
	}{
		{"/abs/path/x", "/abs/path/x", true},         // exact
		{"/abs/path/x", "x", true},                   // basename
		{"/abs/path/x", "y", false},                  // mismatch
		{"/abs/path/x", "", true},                    // legacy
		{"/abs/alpha", "/different/path/alpha", true}, // basename of coord-abs matches basename of projectRoot
	}
	for _, c := range cases {
		got := isCoordinatorProject(c.projectRoot, c.coord)
		if got != c.want {
			t.Errorf("isCoordinatorProject(%q, %q) = %v, want %v",
				c.projectRoot, c.coord, got, c.want)
		}
	}
}
