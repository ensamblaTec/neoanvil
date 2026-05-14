package rag

import (
	"sort"
	"testing"
)

// TestReplaceFileEdges_Idempotent is the core regression for the BLAST_RADIUS
// dep-graph fix: re-indexing a file after it drops an import must NOT leave a
// stale edge behind (a plain Put-only SaveGraphEdges would).
func TestReplaceFileEdges_Idempotent(t *testing.T) {
	ws := t.TempDir()
	wal := openTestWAL(t, ws)
	if err := InitGraphRAG(wal); err != nil {
		t.Fatal(err)
	}

	// First index: foo.go imports bar.go + baz.go.
	if err := ReplaceFileEdges(wal, "pkg/a/foo.go", []GraphEdge{
		{SourceNode: "pkg/a/foo.go", TargetNode: "pkg/b/bar.go", Relation: "imports"},
		{SourceNode: "pkg/a/foo.go", TargetNode: "pkg/b/baz.go", Relation: "imports"},
	}); err != nil {
		t.Fatal(err)
	}
	if imp, _ := GetImpactedNodes(wal, "pkg/b/bar.go"); len(imp) != 1 || imp[0] != "pkg/a/foo.go" {
		t.Fatalf("after first index, GetImpactedNodes(bar.go) = %v, want [pkg/a/foo.go]", imp)
	}

	// Re-index foo.go after it DROPPED the bar.go import — only baz.go remains.
	if err := ReplaceFileEdges(wal, "pkg/a/foo.go", []GraphEdge{
		{SourceNode: "pkg/a/foo.go", TargetNode: "pkg/b/baz.go", Relation: "imports"},
	}); err != nil {
		t.Fatal(err)
	}
	if imp, _ := GetImpactedNodes(wal, "pkg/b/bar.go"); len(imp) != 0 {
		t.Errorf("stale edge survived re-index: GetImpactedNodes(bar.go) = %v, want []", imp)
	}
	if imp, _ := GetImpactedNodes(wal, "pkg/b/baz.go"); len(imp) != 1 || imp[0] != "pkg/a/foo.go" {
		t.Errorf("live edge lost on re-index: GetImpactedNodes(baz.go) = %v, want [pkg/a/foo.go]", imp)
	}

	// Empty edge set clears the file entirely.
	if err := ReplaceFileEdges(wal, "pkg/a/foo.go", nil); err != nil {
		t.Fatal(err)
	}
	if imp, _ := GetImpactedNodes(wal, "pkg/b/baz.go"); len(imp) != 0 {
		t.Errorf("clear failed: GetImpactedNodes(baz.go) = %v, want []", imp)
	}
}

// TestReplaceFileEdges_PrefixIsolation verifies the "<source>->" delete prefix
// does not clobber a sibling whose path is a string-prefix of the source —
// e.g. replacing foo.go must not touch foobar.go's edges.
func TestReplaceFileEdges_PrefixIsolation(t *testing.T) {
	ws := t.TempDir()
	wal := openTestWAL(t, ws)
	if err := InitGraphRAG(wal); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFileEdges(wal, "pkg/a/foo.go", []GraphEdge{
		{SourceNode: "pkg/a/foo.go", TargetNode: "x.go", Relation: "imports"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFileEdges(wal, "pkg/a/foobar.go", []GraphEdge{
		{SourceNode: "pkg/a/foobar.go", TargetNode: "y.go", Relation: "imports"},
	}); err != nil {
		t.Fatal(err)
	}
	// Clearing foo.go must leave foobar.go's edge intact.
	if err := ReplaceFileEdges(wal, "pkg/a/foo.go", nil); err != nil {
		t.Fatal(err)
	}
	if imp, _ := GetImpactedNodes(wal, "y.go"); len(imp) != 1 || imp[0] != "pkg/a/foobar.go" {
		t.Errorf("prefix collision: foobar.go edge wrongly removed, GetImpactedNodes(y.go) = %v", imp)
	}
}

// TestReplaceFileEdges_MultiSourceTopology verifies GetAllGraphEdges returns the
// full topology after several files are indexed independently.
func TestReplaceFileEdges_MultiSourceTopology(t *testing.T) {
	ws := t.TempDir()
	wal := openTestWAL(t, ws)
	if err := InitGraphRAG(wal); err != nil {
		t.Fatal(err)
	}
	for _, e := range [][2]string{{"a.go", "shared.go"}, {"b.go", "shared.go"}, {"c.go", "b.go"}} {
		if err := ReplaceFileEdges(wal, e[0], []GraphEdge{
			{SourceNode: e[0], TargetNode: e[1], Relation: "imports"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	edges, err := GetAllGraphEdges(wal)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 3 {
		t.Errorf("GetAllGraphEdges returned %d sources, want 3", len(edges))
	}
	imp, _ := GetImpactedNodes(wal, "shared.go")
	sort.Strings(imp)
	if len(imp) != 2 || imp[0] != "a.go" || imp[1] != "b.go" {
		t.Errorf("GetImpactedNodes(shared.go) = %v, want [a.go b.go]", imp)
	}
}
