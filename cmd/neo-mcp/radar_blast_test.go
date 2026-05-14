package main

import (
	"reflect"
	"testing"
)

// TestImpactedFromEdges covers the reverse-lookup helper that replaced the
// redundant second BoltDB scan in resolveImpactedNodes [cb27b69]: it must
// return every DIRECT source of an edge into target, and nothing transitive.
func TestImpactedFromEdges(t *testing.T) {
	edges := map[string][]string{
		"a.go":    {"shared.go", "x.go"},
		"b.go":    {"shared.go"},
		"c.go":    {"b.go"},
		"leaf.go": {},
	}
	cases := []struct {
		name, target string
		want         []string
	}{
		{"two direct dependents, sorted", "shared.go", []string{"a.go", "b.go"}},
		{"single dependent", "x.go", []string{"a.go"}},
		{"direct only — not transitive", "b.go", []string{"c.go"}},
		{"target with no dependents", "leaf.go", nil},
		{"unknown target", "nonexistent.go", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := impactedFromEdges(edges, tc.target)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("impactedFromEdges(%q) = %v, want %v", tc.target, got, tc.want)
			}
		})
	}
}

// TestImpactedFromEdges_Deterministic verifies the result is sorted and stable
// across calls regardless of Go's randomized map iteration order — the old
// rag.GetImpactedNodes returned BoltDB cursor order, so BLAST_RADIUS output
// must stay deterministic after the switch to an in-memory derive.
func TestImpactedFromEdges_Deterministic(t *testing.T) {
	edges := map[string][]string{
		"zeta.go":  {"t.go"},
		"alpha.go": {"t.go"},
		"mid.go":   {"t.go"},
		"beta.go":  {"t.go"},
	}
	want := []string{"alpha.go", "beta.go", "mid.go", "zeta.go"}
	for i := 0; i < 25; i++ {
		got := impactedFromEdges(edges, "t.go")
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d: impactedFromEdges = %v, want sorted %v", i, got, want)
		}
	}
}
