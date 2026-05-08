package brain

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// TestNextHLC_Monotonic — repeated calls produce strictly increasing HLCs.
// Loop count is high enough to guarantee at least some calls land in the
// same wall-clock millisecond and exercise the counter branch.
func TestNextHLC_Monotonic(t *testing.T) {
	prev := NextHLC()
	for i := range 1000 {
		cur := NextHLC()
		if CompareHLC(cur, prev) <= 0 {
			t.Fatalf("HLC went backward at iter %d: %s → %s", i, prev, cur)
		}
		prev = cur
	}
}

// TestNextHLC_Concurrent — N goroutines calling NextHLC concurrently
// MUST all receive distinct HLCs; the atomic counter prevents ties.
func TestNextHLC_Concurrent(t *testing.T) {
	const goroutines = 16
	const callsPerG = 50
	var (
		mu  sync.Mutex
		all = make(map[string]bool, goroutines*callsPerG)
		wg  sync.WaitGroup
	)
	for range goroutines {
		wg.Go(func() {
			for range callsPerG {
				h := NextHLC()
				k := h.String()
				mu.Lock()
				if all[k] {
					t.Errorf("duplicate HLC %s across goroutines", k)
				}
				all[k] = true
				mu.Unlock()
			}
		})
	}
	wg.Wait()
}

// TestCompareHLC_Order — exhaustive on the four comparison cases.
func TestCompareHLC_Order(t *testing.T) {
	cases := []struct {
		a, b HLC
		want int
	}{
		{HLC{1, 0}, HLC{2, 0}, -1}, // wall less
		{HLC{2, 0}, HLC{1, 0}, +1}, // wall greater
		{HLC{1, 0}, HLC{1, 1}, -1}, // counter less
		{HLC{1, 2}, HLC{1, 1}, +1}, // counter greater
		{HLC{5, 5}, HLC{5, 5}, 0},  // equal
	}
	for _, c := range cases {
		if got := CompareHLC(c.a, c.b); got != c.want {
			t.Errorf("CompareHLC(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestNodeFingerprint_NonEmpty — never returns "" (must always produce a
// usable id even on weirdly configured machines).
func TestNodeFingerprint_NonEmpty(t *testing.T) {
	id := NodeFingerprint()
	if id == "" {
		t.Error("NodeFingerprint returned empty")
	}
	if !strings.HasPrefix(id, "node:") {
		t.Errorf("missing node: prefix, got %q", id)
	}
}

// TestNewManifest_HLCAdvances — two manifests built back-to-back have
// strictly increasing HLCs.
func TestNewManifest_HLCAdvances(t *testing.T) {
	m1 := NewManifest(nil, nil, nil)
	m2 := NewManifest(nil, nil, nil)
	if CompareHLC(m2.HLC, m1.HLC) <= 0 {
		t.Errorf("HLC didn't advance: %s → %s", m1.HLC, m2.HLC)
	}
	if m1.SnapshotVersion != CurrentSnapshotVersion {
		t.Errorf("snapshot_version = %d, want %d", m1.SnapshotVersion, CurrentSnapshotVersion)
	}
	if m1.NodeID == "" {
		t.Error("NodeID empty in fresh manifest")
	}
}

// TestNewManifest_PopulatesFromWalks — workspaces/projects/orgs flow
// into the manifest with canonical_id preserved.
func TestNewManifest_PopulatesFromWalks(t *testing.T) {
	wss := []WalkedWorkspace{
		{ID: "neoanvil-1", Path: "/p/neoanvil", Name: "neoanvil", CanonicalID: "github.com/x/neoanvil"},
	}
	ps := []WalkedProject{
		{Path: "/p", CanonicalID: "project:planifier:_root", Members: []string{"neoanvil-1"}},
	}
	os := []WalkedOrg{
		{Path: "/o", CanonicalID: "local:abc123", Members: []string{"neoanvil-1"}},
	}
	m := NewManifest(wss, ps, os)
	if len(m.Workspaces) != 1 || m.Workspaces[0].CanonicalID != "github.com/x/neoanvil" {
		t.Errorf("workspace canonical not preserved: %+v", m.Workspaces)
	}
	if len(m.Projects) != 1 || m.Projects[0].CanonicalID != "project:planifier:_root" {
		t.Errorf("project canonical not preserved: %+v", m.Projects)
	}
	if len(m.Orgs) != 1 {
		t.Errorf("orgs count = %d, want 1", len(m.Orgs))
	}
	// LocalIDAtOrigin should mirror ID for v1.
	if m.Workspaces[0].LocalIDAtOrigin != "neoanvil-1" {
		t.Errorf("LocalIDAtOrigin = %q, want neoanvil-1", m.Workspaces[0].LocalIDAtOrigin)
	}
}

// TestNewManifest_DefensiveCopyMembers — modifying the returned manifest's
// Members slice MUST NOT mutate the caller's input slice.
func TestNewManifest_DefensiveCopyMembers(t *testing.T) {
	original := []string{"a", "b"}
	wss := []WalkedWorkspace{}
	ps := []WalkedProject{{Path: "/p", CanonicalID: "x", Members: original}}
	m := NewManifest(wss, ps, nil)
	m.Projects[0].Members[0] = "MUTATED"
	if original[0] != "a" {
		t.Errorf("input slice was mutated: %v", original)
	}
}

// TestManifest_Validate_OK — well-formed manifest passes.
func TestManifest_Validate_OK(t *testing.T) {
	m := NewManifest(
		[]WalkedWorkspace{{ID: "w1", CanonicalID: "github.com/x/y"}},
		nil, nil,
	)
	if err := m.Validate(); err != nil {
		t.Errorf("Validate failed on OK manifest: %v", err)
	}
}

// TestManifest_Validate_RejectsCases — every invariant violation produces
// a non-nil error.
func TestManifest_Validate_RejectsCases(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Manifest)
		hint string
	}{
		{
			"snapshot_version=0",
			func(m *Manifest) { m.SnapshotVersion = 0 },
			"snapshot_version",
		},
		{
			"snapshot_version above current",
			func(m *Manifest) { m.SnapshotVersion = CurrentSnapshotVersion + 5 },
			"requires a newer neoanvil", // 135.A.7 spec: clear upgrade hint
		},
		{
			"empty node_id",
			func(m *Manifest) { m.NodeID = "" },
			"node_id",
		},
		{
			"zero hlc",
			func(m *Manifest) { m.HLC = HLC{} },
			"hlc",
		},
		{
			"workspace missing canonical_id",
			func(m *Manifest) {
				m.Workspaces = []WorkspaceManifest{{ID: "x", CanonicalID: ""}}
			},
			"canonical_id",
		},
		{
			"workspace missing id",
			func(m *Manifest) {
				m.Workspaces = []WorkspaceManifest{{ID: "", CanonicalID: "x"}}
			},
			".id empty",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := NewManifest([]WalkedWorkspace{{ID: "w1", CanonicalID: "x"}}, nil, nil)
			c.mut(m)
			err := m.Validate()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.hint) {
				t.Errorf("error %q missing hint %q", err, c.hint)
			}
		})
	}
}

// TestManifest_Validate_NilManifest — calling Validate on a nil receiver
// returns a clear error rather than panicking.
func TestManifest_Validate_NilManifest(t *testing.T) {
	var m *Manifest
	if err := m.Validate(); err == nil {
		t.Error("Validate(nil) should error")
	}
}

// TestManifest_RoundTripJSON — marshal then unmarshal yields a manifest
// that still validates. The schema is stable as JSON.
func TestManifest_RoundTripJSON(t *testing.T) {
	src := NewManifest(
		[]WalkedWorkspace{{ID: "w1", Path: "/x", Name: "x", CanonicalID: "github.com/a/b"}},
		[]WalkedProject{{Path: "/p", CanonicalID: "project:planifier:_root", Members: []string{"w1"}}},
		nil,
	)
	src.MergedFrom = []HLC{{WallMS: 1, LogicalCounter: 0}, {WallMS: 2, LogicalCounter: 7}}

	data, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Manifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("round-trip manifest fails Validate: %v", err)
	}
	if got.NodeID != src.NodeID || got.HLC != src.HLC {
		t.Errorf("identity fields drifted: src %s/%s vs got %s/%s", src.NodeID, src.HLC, got.NodeID, got.HLC)
	}
	if len(got.MergedFrom) != 2 || got.MergedFrom[1].LogicalCounter != 7 {
		t.Errorf("MergedFrom not preserved: %+v", got.MergedFrom)
	}
}

// TestHLC_StringStable — String() output is deterministic for stable hash
// reproducibility.
func TestHLC_StringStable(t *testing.T) {
	h := HLC{WallMS: 12345, LogicalCounter: 7}
	want := "12345.7"
	if got := h.String(); got != want {
		t.Errorf("HLC.String = %q, want %q", got, want)
	}
}

// TestHLC_IsZero — covers detection of zero-value HLCs.
func TestHLC_IsZero(t *testing.T) {
	if !(HLC{}).IsZero() {
		t.Error("zero HLC reported as non-zero")
	}
	if (HLC{WallMS: 1}).IsZero() {
		t.Error("non-zero HLC reported as zero")
	}
	if (HLC{LogicalCounter: 1}).IsZero() {
		t.Error("HLC with counter only reported as zero")
	}
}
