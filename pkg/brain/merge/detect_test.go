package merge

import (
	"bytes"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/brain/diff"
)

// =============================================================================
// DetectConflicts (136.B.1)
// =============================================================================

func TestDetectConflicts_NoOverlap(t *testing.T) {
	local := diff.BucketDiff{
		Modified: []diff.BucketChange{{Key: "a", Kind: diff.ChangeModified, AncestorValue: []byte("a0"), SideValue: []byte("a1")}},
	}
	remote := diff.BucketDiff{
		Modified: []diff.BucketChange{{Key: "b", Kind: diff.ChangeModified, AncestorValue: []byte("b0"), SideValue: []byte("b1")}},
	}
	got := DetectConflicts("memex", local, remote)
	if len(got) != 0 {
		t.Errorf("non-overlapping diffs should produce no conflicts, got %d", len(got))
	}
}

func TestDetectConflicts_ModifiedModifiedDifferent(t *testing.T) {
	local := diff.BucketDiff{
		Modified: []diff.BucketChange{{Key: "k", Kind: diff.ChangeModified, AncestorValue: []byte("v0"), SideValue: []byte("local")}},
	}
	remote := diff.BucketDiff{
		Modified: []diff.BucketChange{{Key: "k", Kind: diff.ChangeModified, AncestorValue: []byte("v0"), SideValue: []byte("remote")}},
	}
	got := DetectConflicts("buck", local, remote)
	if len(got) != 1 {
		t.Fatalf("got %d conflicts, want 1", len(got))
	}
	c := got[0]
	if c.Bucket != "buck" || c.Key != "k" {
		t.Errorf("unexpected conflict: %+v", c)
	}
	if !bytes.Equal(c.AncestorValue, []byte("v0")) || !bytes.Equal(c.LocalValue, []byte("local")) || !bytes.Equal(c.RemoteValue, []byte("remote")) {
		t.Errorf("conflict values drift: %+v", c)
	}
}

func TestDetectConflicts_ModifiedModifiedSameValue(t *testing.T) {
	// Both sides converged to the same new value — auto-merges.
	local := diff.BucketDiff{
		Modified: []diff.BucketChange{{Key: "k", Kind: diff.ChangeModified, AncestorValue: []byte("v0"), SideValue: []byte("same")}},
	}
	remote := diff.BucketDiff{
		Modified: []diff.BucketChange{{Key: "k", Kind: diff.ChangeModified, AncestorValue: []byte("v0"), SideValue: []byte("same")}},
	}
	got := DetectConflicts("buck", local, remote)
	if len(got) != 0 {
		t.Errorf("identical-modify should auto-merge, got %d conflicts", len(got))
	}
}

func TestDetectConflicts_DeleteVsModify(t *testing.T) {
	local := diff.BucketDiff{
		Deleted: []diff.BucketChange{{Key: "k", Kind: diff.ChangeDeleted, AncestorValue: []byte("v0")}},
	}
	remote := diff.BucketDiff{
		Modified: []diff.BucketChange{{Key: "k", Kind: diff.ChangeModified, AncestorValue: []byte("v0"), SideValue: []byte("changed")}},
	}
	got := DetectConflicts("buck", local, remote)
	if len(got) != 1 {
		t.Fatalf("got %d conflicts, want 1", len(got))
	}
	if got[0].LocalValue != nil {
		t.Errorf("local-deleted conflict should have nil LocalValue, got %v", got[0].LocalValue)
	}
	if !bytes.Equal(got[0].RemoteValue, []byte("changed")) {
		t.Errorf("remote-modified value drift: %v", got[0].RemoteValue)
	}
}

func TestDetectConflicts_AddedAddedDifferent(t *testing.T) {
	local := diff.BucketDiff{
		Added: []diff.BucketChange{{Key: "k", Kind: diff.ChangeAdded, SideValue: []byte("from-local")}},
	}
	remote := diff.BucketDiff{
		Added: []diff.BucketChange{{Key: "k", Kind: diff.ChangeAdded, SideValue: []byte("from-remote")}},
	}
	got := DetectConflicts("buck", local, remote)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if got[0].Reason != "concurrent insert with different values" {
		t.Errorf("reason = %q", got[0].Reason)
	}
}

func TestDetectConflicts_AddedAddedSameValue(t *testing.T) {
	local := diff.BucketDiff{
		Added: []diff.BucketChange{{Key: "k", Kind: diff.ChangeAdded, SideValue: []byte("same")}},
	}
	remote := diff.BucketDiff{
		Added: []diff.BucketChange{{Key: "k", Kind: diff.ChangeAdded, SideValue: []byte("same")}},
	}
	got := DetectConflicts("buck", local, remote)
	if len(got) != 0 {
		t.Errorf("identical insert should auto-merge, got %d", len(got))
	}
}

// =============================================================================
// AutoResolve (136.D.1)
// =============================================================================

func TestAutoResolve_AppendOnly(t *testing.T) {
	c := Conflict{Key: "k", LocalValue: []byte("L"), RemoteValue: []byte("R")}
	r, ok := AutoResolve(c, BucketAppendOnly)
	if !ok {
		t.Fatal("append-only should auto-resolve")
	}
	if r.Strategy != "append_union" {
		t.Errorf("strategy = %q", r.Strategy)
	}
}

func TestAutoResolve_MonotonicCounter(t *testing.T) {
	c := Conflict{Key: "tokens", LocalValue: []byte("100"), RemoteValue: []byte("250")}
	r, ok := AutoResolve(c, BucketMonotonicCounter)
	if !ok {
		t.Fatal("monotonic counter should auto-resolve")
	}
	if string(r.ResolvedValue) != "250" {
		t.Errorf("max = %q, want 250", r.ResolvedValue)
	}
	if r.Strategy != "monotonic_max" {
		t.Errorf("strategy = %q", r.Strategy)
	}
}

func TestAutoResolve_MonotonicCounterRefusesNonInt(t *testing.T) {
	c := Conflict{Key: "x", LocalValue: []byte("not-a-number"), RemoteValue: []byte("10")}
	if _, ok := AutoResolve(c, BucketMonotonicCounter); ok {
		t.Error("non-numeric value should not auto-resolve as counter")
	}
}

func TestAutoResolve_TombstoneWins(t *testing.T) {
	cases := []struct {
		name       string
		local      []byte
		remote     []byte
		shouldAuto bool
	}{
		{"local-deleted", nil, []byte("live"), true},
		{"remote-deleted", []byte("live"), nil, true},
		{"both-live-different", []byte("live-1"), []byte("live-2"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conflict := Conflict{Key: "k", LocalValue: c.local, RemoteValue: c.remote}
			r, ok := AutoResolve(conflict, BucketTombstones)
			if ok != c.shouldAuto {
				t.Errorf("auto = %v, want %v", ok, c.shouldAuto)
			}
			if c.shouldAuto && len(r.ResolvedValue) != 0 {
				t.Errorf("tombstone resolution should yield empty value, got %v", r.ResolvedValue)
			}
		})
	}
}

func TestAutoResolve_GenericNoAuto(t *testing.T) {
	c := Conflict{Key: "k", LocalValue: []byte("L"), RemoteValue: []byte("R")}
	if _, ok := AutoResolve(c, BucketGeneric); ok {
		t.Error("generic bucket should never auto-resolve")
	}
}

func TestAutoResolveBatch_SplitsCorrectly(t *testing.T) {
	conflicts := []Conflict{
		{Key: "n1", LocalValue: []byte("1"), RemoteValue: []byte("5")},
		{Key: "n2", LocalValue: []byte("not-int"), RemoteValue: []byte("10")},
		{Key: "n3", LocalValue: []byte("3"), RemoteValue: []byte("9")},
	}
	resolved, remaining := AutoResolveBatch(conflicts, BucketMonotonicCounter)
	if len(resolved) != 2 {
		t.Errorf("resolved = %d, want 2 (n1, n3)", len(resolved))
	}
	if len(remaining) != 1 || remaining[0].Key != "n2" {
		t.Errorf("remaining unexpected: %+v", remaining)
	}
}
