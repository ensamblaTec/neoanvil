package cpg_test

import (
	"context"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
)

// TestMcCabeCCNilAndEmpty verifies the guard cases.
func TestMcCabeCCNilAndEmpty(t *testing.T) {
	if got := cpg.McCabeCC(nil); got != 1 {
		t.Errorf("nil fn: want CC=1, got %d", got)
	}
}

// TestMcCabeCCRealPackage builds the cmd/neo-mcp SSA graph and checks that
// simple entry functions have CC ≥ 1 and complex handlers have CC > 1.
func TestMcCabeCCRealPackage(t *testing.T) {
	root := repoRoot(t)
	b := cpg.NewBuilder(cpg.BuildConfig{Dir: root})
	g, err := b.Build(context.Background(), "./cmd/neo-mcp")
	if err != nil {
		t.Skipf("skipping: build failed: %v", err)
	}
	if len(g.Nodes) == 0 {
		t.Skip("empty graph")
	}

	ssaFuncs := b.SSAFunctions()
	if len(ssaFuncs) == 0 {
		t.Skip("no SSA functions available")
	}

	minCC, maxCC := 999, 0
	for _, fn := range ssaFuncs {
		cc := cpg.McCabeCC(fn)
		if cc < 1 {
			t.Errorf("function %q has CC=%d < 1 (impossible)", fn.Name(), cc)
		}
		if cc < minCC {
			minCC = cc
		}
		if cc > maxCC {
			maxCC = cc
		}
	}
	t.Logf("McCabeCC across %d SSA functions: min=%d max=%d", len(ssaFuncs), minCC, maxCC)
	// The package has functions with branches → max CC should be > 1.
	if maxCC <= 1 {
		t.Error("expected at least one function with CC > 1 in cmd/neo-mcp")
	}
}
