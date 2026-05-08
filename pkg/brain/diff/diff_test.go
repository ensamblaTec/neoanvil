package diff

import (
	"slices"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/brain"
)

// =============================================================================
// FindCommonAncestor (136.A.1)
// =============================================================================

func TestFindCommonAncestor_SharedHistory(t *testing.T) {
	a := StaticLineage{HLCs: []brain.HLC{{WallMS: 1, LogicalCounter: 0}, {WallMS: 5, LogicalCounter: 0}, {WallMS: 7, LogicalCounter: 0}}}
	b := StaticLineage{HLCs: []brain.HLC{{WallMS: 5, LogicalCounter: 0}, {WallMS: 8, LogicalCounter: 0}}}
	got, ok, err := FindCommonAncestor(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected common ancestor")
	}
	if got != (brain.HLC{WallMS: 5}) {
		t.Errorf("ancestor = %v, want HLC{5,0}", got)
	}
}

func TestFindCommonAncestor_PicksMostRecent(t *testing.T) {
	// Multiple shared HLCs — the highest under CompareHLC wins.
	a := StaticLineage{HLCs: []brain.HLC{{WallMS: 1}, {WallMS: 5}, {WallMS: 7}}}
	b := StaticLineage{HLCs: []brain.HLC{{WallMS: 1}, {WallMS: 5}, {WallMS: 6}}}
	got, ok, _ := FindCommonAncestor(a, b)
	if !ok || got != (brain.HLC{WallMS: 5}) {
		t.Errorf("got %v ok=%v, want HLC{5,0} ok=true", got, ok)
	}
}

func TestFindCommonAncestor_NoOverlap(t *testing.T) {
	a := StaticLineage{HLCs: []brain.HLC{{WallMS: 1}, {WallMS: 2}}}
	b := StaticLineage{HLCs: []brain.HLC{{WallMS: 100}, {WallMS: 200}}}
	_, ok, _ := FindCommonAncestor(a, b)
	if ok {
		t.Error("disjoint lineages should report ok=false")
	}
}

func TestFindCommonAncestor_NilProviders(t *testing.T) {
	if _, ok, _ := FindCommonAncestor(nil, nil); ok {
		t.Error("nil providers should report ok=false")
	}
	good := StaticLineage{HLCs: []brain.HLC{{WallMS: 1}}}
	if _, ok, _ := FindCommonAncestor(nil, good); ok {
		t.Error("nil local should report ok=false")
	}
	if _, ok, _ := FindCommonAncestor(good, nil); ok {
		t.Error("nil remote should report ok=false")
	}
}

func TestLineageFromManifest(t *testing.T) {
	m := &brain.Manifest{
		HLC:        brain.HLC{WallMS: 100},
		MergedFrom: []brain.HLC{{WallMS: 50}, {WallMS: 75}},
	}
	got := LineageFromManifest(m)
	if len(got.HLCs) != 3 {
		t.Errorf("len = %d, want 3", len(got.HLCs))
	}
	if LineageFromManifest(nil).HLCs != nil {
		t.Error("nil manifest should yield empty lineage")
	}
}

// =============================================================================
// DiffBuckets (136.A.2)
// =============================================================================

func TestDiffBuckets_AllKinds(t *testing.T) {
	ancestor := map[string][]byte{
		"unchanged":    []byte("v1"),
		"will-modify":  []byte("v1"),
		"will-delete":  []byte("gone-soon"),
	}
	side := map[string][]byte{
		"unchanged":   []byte("v1"),
		"will-modify": []byte("v2"),
		"newly-added": []byte("hello"),
	}
	d := DiffBuckets(ancestor, side)

	if d.Unchanged != 1 {
		t.Errorf("unchanged = %d, want 1", d.Unchanged)
	}
	if len(d.Modified) != 1 || d.Modified[0].Key != "will-modify" {
		t.Errorf("modified = %v", d.Modified)
	}
	if len(d.Deleted) != 1 || d.Deleted[0].Key != "will-delete" {
		t.Errorf("deleted = %v", d.Deleted)
	}
	if len(d.Added) != 1 || d.Added[0].Key != "newly-added" {
		t.Errorf("added = %v", d.Added)
	}
	if d.IsEmpty() {
		t.Error("IsEmpty should be false with changes")
	}
	if d.Total() != 3 {
		t.Errorf("Total = %d, want 3", d.Total())
	}
}

func TestDiffBuckets_NoChanges(t *testing.T) {
	m := map[string][]byte{"a": []byte("1"), "b": []byte("2")}
	d := DiffBuckets(m, m)
	if !d.IsEmpty() {
		t.Errorf("identical maps should yield empty diff")
	}
	if d.Unchanged != 2 {
		t.Errorf("unchanged = %d, want 2", d.Unchanged)
	}
}

func TestDiffBuckets_DefensiveCopy(t *testing.T) {
	// Mutating the input AFTER DiffBuckets must not affect the result.
	ancestor := map[string][]byte{"k": []byte("original")}
	side := map[string][]byte{"k": []byte("modified")}
	d := DiffBuckets(ancestor, side)
	if len(d.Modified) != 1 {
		t.Fatal("expected 1 modified")
	}
	// Mutate caller's bytes.
	ancestor["k"][0] = 'X'
	if string(d.Modified[0].AncestorValue) != "original" {
		t.Errorf("AncestorValue not defensively copied: got %q", d.Modified[0].AncestorValue)
	}
}

func TestDiffBuckets_EmptyInputs(t *testing.T) {
	if d := DiffBuckets(nil, nil); !d.IsEmpty() {
		t.Error("nil/nil should be empty")
	}
	d := DiffBuckets(nil, map[string][]byte{"k": []byte("v")})
	if len(d.Added) != 1 || len(d.Modified) != 0 || len(d.Deleted) != 0 {
		t.Errorf("nil ancestor should give all-added: %+v", d)
	}
	d = DiffBuckets(map[string][]byte{"k": []byte("v")}, nil)
	if len(d.Deleted) != 1 || len(d.Added) != 0 {
		t.Errorf("nil side should give all-deleted: %+v", d)
	}
}

// =============================================================================
// DiffLines (136.A.3)
// =============================================================================

func TestDiffLines_NoChange(t *testing.T) {
	lines := []string{"a", "b", "c"}
	got := DiffLines(lines, lines)
	if HasChanges(got) {
		t.Errorf("identical inputs should report no changes, got %+v", got)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 Equal edits, got %d", len(got))
	}
}

func TestDiffLines_SimpleInsertion(t *testing.T) {
	a := []string{"line 1", "line 3"}
	b := []string{"line 1", "line 2", "line 3"}
	edits := DiffLines(a, b)
	ins, del := CountChanges(edits)
	if ins != 1 || del != 0 {
		t.Errorf("ins=%d del=%d, want 1/0", ins, del)
	}
	// The inserted line should be "line 2".
	for _, e := range edits {
		if e.Kind == LineInserted && e.Line != "line 2" {
			t.Errorf("inserted line = %q, want line 2", e.Line)
		}
	}
}

func TestDiffLines_SimpleDeletion(t *testing.T) {
	a := []string{"keep", "drop", "keep2"}
	b := []string{"keep", "keep2"}
	edits := DiffLines(a, b)
	ins, del := CountChanges(edits)
	if ins != 0 || del != 1 {
		t.Errorf("ins=%d del=%d, want 0/1", ins, del)
	}
}

func TestDiffLines_BothEmpty(t *testing.T) {
	if got := DiffLines(nil, nil); got != nil {
		t.Errorf("nil/nil should yield nil edits, got %v", got)
	}
}

func TestDiffLines_OneEmpty(t *testing.T) {
	got := DiffLines(nil, []string{"x", "y"})
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
	for _, e := range got {
		if e.Kind != LineInserted {
			t.Errorf("empty-ancestor edits should all be Inserted, got %s", e.Kind)
		}
	}
	got = DiffLines([]string{"x", "y"}, nil)
	for _, e := range got {
		if e.Kind != LineDeleted {
			t.Errorf("empty-side edits should all be Deleted, got %s", e.Kind)
		}
	}
}

func TestDiffLines_Stable(t *testing.T) {
	// Same input → same output (idempotent / deterministic).
	a := []string{"alpha", "beta", "gamma"}
	b := []string{"alpha", "BETA", "gamma", "delta"}
	r1 := DiffLines(a, b)
	r2 := DiffLines(a, b)
	if !slices.Equal(serializeEdits(r1), serializeEdits(r2)) {
		t.Error("DiffLines is non-deterministic")
	}
}

func TestDiffLines_PreservesEqualLines(t *testing.T) {
	a := []string{"x", "y", "z"}
	b := []string{"x", "y", "Z"}
	edits := DiffLines(a, b)
	equalCount := 0
	for _, e := range edits {
		if e.Kind == LineEqual {
			equalCount++
		}
	}
	if equalCount != 2 {
		t.Errorf("expected 2 equal lines (x,y), got %d", equalCount)
	}
}

func TestCountChanges_ZeroOnEmpty(t *testing.T) {
	ins, del := CountChanges(nil)
	if ins != 0 || del != 0 {
		t.Errorf("empty edits should yield 0/0, got %d/%d", ins, del)
	}
}

// serializeEdits is a small helper for the determinism test.
func serializeEdits(edits []LineEdit) []string {
	out := make([]string, len(edits))
	for i, e := range edits {
		out[i] = string(e.Kind) + ":" + e.Line
	}
	return out
}
