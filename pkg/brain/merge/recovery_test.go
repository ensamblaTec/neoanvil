// recovery_test.go — 136.E recovery + edge-case tests for the merge
// engine. Live BoltDB extraction is integration-pending; these tests
// exercise the engine's invariants on synthetic inputs that mirror the
// shapes a real bbolt walk would produce.

package merge

import (
	"bytes"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/brain/diff"
)

// TestMerge_NConflictsConsistentOutput — 136.E.1 covered at the
// engine level: every conflict in the input produces exactly one
// MergedKey in the output (no duplicates, no drops, no schema drift).
// Production rollout will add a real-bbolt smoke test once extraction
// is wired; this guards the orchestrator against silent regressions.
func TestMerge_NConflictsConsistentOutput(t *testing.T) {
	conflicts := []Conflict{
		{Bucket: "b", Key: "k1", LocalValue: []byte("L"), RemoteValue: []byte("R")},
		{Bucket: "b", Key: "k2", LocalValue: []byte("L"), RemoteValue: []byte("R")},
		{Bucket: "b", Key: "k3", LocalValue: []byte("L"), RemoteValue: []byte("R")},
	}
	in := OrchestrateInput{Bucket: "b", Kind: BucketGeneric, Conflicts: conflicts}
	out, err := Orchestrate(in, StrategyTakeLocal, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Merged) != len(conflicts) {
		t.Errorf("merged %d, want %d (one MergedKey per Conflict)", len(out.Merged), len(conflicts))
	}
	// All keys present, no duplicates.
	seen := map[string]bool{}
	for _, m := range out.Merged {
		if seen[m.Key] {
			t.Errorf("duplicate merged key: %s", m.Key)
		}
		seen[m.Key] = true
	}
}

// TestMerge_RollbackTagSemantics — 136.E.2: documents that the engine
// emits a stable, deterministic snapshot per (input, strategy) pair so
// the higher layer can always reproduce a "pre-merge-<hlc>" tag from
// scratch without storing a separate copy.
//
// Concretely: given the same input twice, Orchestrate must produce
// byte-identical Merged slices (modulo Stats, which counts call sites).
func TestMerge_RollbackTagSemantics(t *testing.T) {
	conflicts := []Conflict{
		{Bucket: "b", Key: "a", LocalValue: []byte("la"), RemoteValue: []byte("ra")},
		{Bucket: "b", Key: "b", LocalValue: []byte("lb"), RemoteValue: []byte("rb")},
	}
	in := OrchestrateInput{Bucket: "b", Kind: BucketGeneric, Conflicts: conflicts}
	a, _ := Orchestrate(in, StrategyTakeLocal, nil)
	b, _ := Orchestrate(in, StrategyTakeLocal, nil)
	if len(a.Merged) != len(b.Merged) {
		t.Fatalf("non-deterministic length")
	}
	for i := range a.Merged {
		if a.Merged[i].Key != b.Merged[i].Key || !bytes.Equal(a.Merged[i].Value, b.Merged[i].Value) {
			t.Errorf("non-deterministic at idx %d", i)
		}
	}
}

// TestEdgeCase_EmptySide — 136.E.3: empty side input → no diff, no
// conflicts, no merge work.
func TestEdgeCase_EmptySide(t *testing.T) {
	d := diff.DiffBuckets(nil, nil)
	if !d.IsEmpty() {
		t.Error("nil/nil DiffBuckets should be empty")
	}
	conflicts := DetectConflicts("b", d, d)
	if len(conflicts) != 0 {
		t.Errorf("empty diffs should produce no conflicts, got %d", len(conflicts))
	}
}

// TestEdgeCase_IdenticalSides — both sides made the same change. Per
// the modified-modified-same-value rule in detect.go, this is NOT a
// conflict.
func TestEdgeCase_IdenticalSides(t *testing.T) {
	local := diff.BucketDiff{
		Modified: []diff.BucketChange{{Key: "k", Kind: diff.ChangeModified, AncestorValue: []byte("v0"), SideValue: []byte("same")}},
	}
	if len(DetectConflicts("b", local, local)) != 0 {
		t.Error("identical sides should not conflict")
	}
}

// TestEdgeCase_ConcurrentDeletes — both sides deleted the same key.
// Trivial merge: tombstone wins on both, no conflict needed.
func TestEdgeCase_ConcurrentDeletes(t *testing.T) {
	local := diff.BucketDiff{
		Deleted: []diff.BucketChange{{Key: "k", Kind: diff.ChangeDeleted, AncestorValue: []byte("v0")}},
	}
	remote := diff.BucketDiff{
		Deleted: []diff.BucketChange{{Key: "k", Kind: diff.ChangeDeleted, AncestorValue: []byte("v0")}},
	}
	if len(DetectConflicts("b", local, remote)) != 0 {
		t.Error("concurrent deletes should auto-merge (no conflict)")
	}
}

// TestEdgeCase_LargeBinaryValues — 1 MiB binary blobs roundtrip
// through DetectConflicts + AutoResolve without truncation or copy
// errors.
func TestEdgeCase_LargeBinaryValues(t *testing.T) {
	bigA := bytes.Repeat([]byte{0xAA}, 1<<20)
	bigB := bytes.Repeat([]byte{0xBB}, 1<<20)
	local := diff.BucketDiff{
		Modified: []diff.BucketChange{{Key: "k", Kind: diff.ChangeModified, AncestorValue: []byte("v0"), SideValue: bigA}},
	}
	remote := diff.BucketDiff{
		Modified: []diff.BucketChange{{Key: "k", Kind: diff.ChangeModified, AncestorValue: []byte("v0"), SideValue: bigB}},
	}
	conflicts := DetectConflicts("b", local, remote)
	if len(conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1", len(conflicts))
	}
	if !bytes.Equal(conflicts[0].LocalValue, bigA) || !bytes.Equal(conflicts[0].RemoteValue, bigB) {
		t.Error("large-value conflict drift")
	}
	// AutoResolve generic path doesn't auto, but take-local flow should
	// preserve the full payload.
	in := OrchestrateInput{Bucket: "b", Kind: BucketGeneric, Conflicts: conflicts}
	out, _ := Orchestrate(in, StrategyTakeLocal, nil)
	if len(out.Merged[0].Value) != len(bigA) {
		t.Errorf("merged value size = %d, want %d", len(out.Merged[0].Value), len(bigA))
	}
}
