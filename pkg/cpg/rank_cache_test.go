package cpg_test

import (
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
)

// TestCachedPageRank_MatchesUncached verifies the cached wrapper
// returns bit-identical results to the underlying algorithm — memoisation
// must not silently introduce numerical drift. [Épica 224]
func TestCachedPageRank_MatchesUncached(t *testing.T) {
	g := syntheticCallGraph()
	direct := cpg.ComputePageRank(g, 0.85, 50)
	cached := cpg.CachedComputePageRank(g, 0.85, 50)
	if len(direct) != len(cached) {
		t.Fatalf("length mismatch: direct=%d cached=%d", len(direct), len(cached))
	}
	for id, rDirect := range direct {
		if rCached, ok := cached[id]; !ok {
			t.Errorf("missing key %v in cached result", id)
		} else if rCached != rDirect {
			t.Errorf("drift at %v: direct=%v cached=%v", id, rDirect, rCached)
		}
	}
}

// TestCachedPageRank_NilGraph returns nil without panic.
func TestCachedPageRank_NilGraph(t *testing.T) {
	if got := cpg.CachedComputePageRank(nil, 0.85, 50); got != nil {
		t.Errorf("nil graph should return nil, got %v", got)
	}
}

// BenchmarkComputePageRank measures the uncached (always-compute) path.
// Baseline for the speedup claim in rank_cache.go's docstring.
func BenchmarkComputePageRank(b *testing.B) {
	g := syntheticCallGraph()

	for b.Loop() {
		_ = cpg.ComputePageRank(g, 0.85, 50)
	}
}

// BenchmarkCachedComputePageRank_Hit measures the memoised path for the
// SAME graph — what handleBlastRadiusBatch exercises on the 2nd..Nth
// goroutine. Expected to be an order of magnitude faster than the
// uncached path at any non-trivial graph size.
func BenchmarkCachedComputePageRank_Hit(b *testing.B) {
	g := syntheticCallGraph()
	// Prime the cache with a single uncached call.
	_ = cpg.CachedComputePageRank(g, 0.85, 50)

	for b.Loop() {
		_ = cpg.CachedComputePageRank(g, 0.85, 50)
	}
}
