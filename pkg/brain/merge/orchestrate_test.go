package merge

import (
	"errors"
	"strings"
	"testing"
)

// trivialAuto returns one append-only conflict + one generic conflict.
// AutoResolve handles the first; the second falls through to strategy.
func trivialAuto() []Conflict {
	return []Conflict{
		{Bucket: "memex", Key: "k1", LocalValue: []byte("L1"), RemoteValue: []byte("R1")},
	}
}

func remainingNeeded() []Conflict {
	return []Conflict{
		{Bucket: "config", Key: "endpoint", AncestorValue: []byte("v0"),
			LocalValue: []byte("local-edit"), RemoteValue: []byte("remote-edit"),
			Reason: "modified on both sides with different values"},
	}
}

// TestOrchestrate_AutoOnlyHappy — every conflict is append-only, so
// AutoResolve handles all of them and Strategy=auto-only succeeds.
func TestOrchestrate_AutoOnlyHappy(t *testing.T) {
	in := OrchestrateInput{Bucket: "memex", Kind: BucketAppendOnly, Conflicts: trivialAuto()}
	out, err := Orchestrate(in, StrategyAutoOnly, nil)
	if err != nil {
		t.Fatalf("auto-only with all-trivial should succeed: %v", err)
	}
	if out.Stats.AutoResolved != 1 || out.Stats.InteractivePicked != 0 {
		t.Errorf("stats drift: %+v", out.Stats)
	}
	if len(out.Merged) != 1 {
		t.Errorf("merged = %d, want 1", len(out.Merged))
	}
}

// TestOrchestrate_AutoOnlyFailsOnConflict — non-trivial conflict in
// auto-only mode → error referencing the bucket name.
func TestOrchestrate_AutoOnlyFailsOnConflict(t *testing.T) {
	in := OrchestrateInput{Bucket: "config", Kind: BucketGeneric, Conflicts: remainingNeeded()}
	_, err := Orchestrate(in, StrategyAutoOnly, nil)
	if err == nil {
		t.Fatal("auto-only with conflict should error")
	}
	if !strings.Contains(err.Error(), "config") {
		t.Errorf("error missing bucket name: %v", err)
	}
}

// TestOrchestrate_TakeLocal — every remaining conflict resolves to
// LocalValue.
func TestOrchestrate_TakeLocal(t *testing.T) {
	in := OrchestrateInput{Bucket: "config", Kind: BucketGeneric, Conflicts: remainingNeeded()}
	out, err := Orchestrate(in, StrategyTakeLocal, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Merged) != 1 || string(out.Merged[0].Value) != "local-edit" {
		t.Errorf("take-local drift: %+v", out.Merged)
	}
	if out.Stats.StrategyForced != 1 {
		t.Errorf("StrategyForced = %d, want 1", out.Stats.StrategyForced)
	}
}

// TestOrchestrate_TakeRemote — every remaining conflict resolves to
// RemoteValue.
func TestOrchestrate_TakeRemote(t *testing.T) {
	in := OrchestrateInput{Bucket: "config", Kind: BucketGeneric, Conflicts: remainingNeeded()}
	out, _ := Orchestrate(in, StrategyTakeRemote, nil)
	if string(out.Merged[0].Value) != "remote-edit" {
		t.Errorf("take-remote drift: %q", out.Merged[0].Value)
	}
}

// TestOrchestrate_InteractiveResolver — caller-supplied resolver
// returns a custom value; orchestrator threads it through.
func TestOrchestrate_InteractiveResolver(t *testing.T) {
	called := 0
	resolver := func(c Conflict) ([]byte, error) {
		called++
		return []byte("operator-merged"), nil
	}
	in := OrchestrateInput{Bucket: "config", Kind: BucketGeneric, Conflicts: remainingNeeded()}
	out, err := Orchestrate(in, StrategyInteractive, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Errorf("resolver called %d times, want 1", called)
	}
	if string(out.Merged[0].Value) != "operator-merged" {
		t.Errorf("resolver value not applied: %q", out.Merged[0].Value)
	}
	if out.Stats.InteractivePicked != 1 {
		t.Errorf("InteractivePicked = %d, want 1", out.Stats.InteractivePicked)
	}
}

// TestOrchestrate_InteractiveResolverNil — interactive without
// resolver is a programming error.
func TestOrchestrate_InteractiveResolverNil(t *testing.T) {
	in := OrchestrateInput{Bucket: "x", Kind: BucketGeneric, Conflicts: remainingNeeded()}
	_, err := Orchestrate(in, StrategyInteractive, nil)
	if err == nil || !strings.Contains(err.Error(), "non-nil resolver") {
		t.Errorf("got %v, want non-nil-resolver error", err)
	}
}

// TestOrchestrate_InteractiveResolverAbort — resolver error aborts
// the merge and surfaces in the result.
func TestOrchestrate_InteractiveResolverAbort(t *testing.T) {
	resolver := func(c Conflict) ([]byte, error) {
		return nil, errors.New("operator chose abort")
	}
	in := OrchestrateInput{Bucket: "x", Kind: BucketGeneric, Conflicts: remainingNeeded()}
	out, err := Orchestrate(in, StrategyInteractive, resolver)
	if err == nil {
		t.Fatal("resolver error should propagate")
	}
	if !out.Aborted {
		t.Error("Aborted flag not set on resolver error")
	}
}

// TestOrchestrate_UnknownStrategy — typed error.
func TestOrchestrate_UnknownStrategy(t *testing.T) {
	in := OrchestrateInput{Bucket: "x", Kind: BucketGeneric, Conflicts: remainingNeeded()}
	_, err := Orchestrate(in, Strategy("nonsense"), nil)
	if err == nil || !strings.Contains(err.Error(), "unknown strategy") {
		t.Errorf("unknown strategy: got %v", err)
	}
}

// TestFormatStats — output shape matches the runbook example.
func TestFormatStats(t *testing.T) {
	s := MergeStats{AutoResolved: 5, InteractivePicked: 2, StrategyForced: 1, Aborted: 0}
	got := FormatStats(s)
	for _, expected := range []string{"5 auto", "2 interactive", "1 strategy-forced", "0 aborted"} {
		if !strings.Contains(got, expected) {
			t.Errorf("FormatStats missing %q in %q", expected, got)
		}
	}
}

// TestAddStats — accumulator semantics.
func TestAddStats(t *testing.T) {
	a := MergeStats{AutoResolved: 3}
	AddStats(&a, MergeStats{AutoResolved: 2, InteractivePicked: 4})
	if a.AutoResolved != 5 || a.InteractivePicked != 4 {
		t.Errorf("Add drift: %+v", a)
	}
}

// TestOrchestrate_DefensiveCopy — modifying caller's conflict bytes
// after Orchestrate returns must not poison the merged output.
func TestOrchestrate_DefensiveCopy(t *testing.T) {
	c := Conflict{Bucket: "x", Key: "k", LocalValue: []byte("original"), RemoteValue: []byte("R")}
	in := OrchestrateInput{Bucket: "x", Kind: BucketGeneric, Conflicts: []Conflict{c}}
	out, _ := Orchestrate(in, StrategyTakeLocal, nil)
	c.LocalValue[0] = 'X'
	if string(out.Merged[0].Value) != "original" {
		t.Errorf("merged value not defensively copied: %q", out.Merged[0].Value)
	}
}
