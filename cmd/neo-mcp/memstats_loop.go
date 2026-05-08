package main

// memstats_loop.go — periodic runtime memory snapshot writer.
// [PILAR-XXVII/243.C]
//
// Runs one ticker in a goroutine; every 30 s builds a MemStatsSnapshot
// (runtime + CPG heap + cache hit ratios) and persists it to the
// observability store's memstats_ring bucket. Stops when ctx is done —
// flushLoop inside Store persists any remaining ring entries.

import (
	"context"
	"log"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
	"github.com/ensamblatec/neoanvil/pkg/observability"
	"github.com/ensamblatec/neoanvil/pkg/rag"
)

const memStatsInterval = 30 * time.Second

// startMemStatsLoop launches the periodic memory snapshot goroutine.
// No-op when the Store wasn't initialised.
func startMemStatsLoop(
	ctx context.Context,
	cpgMgr *cpg.Manager,
	queryCache *rag.QueryCache,
	textCache *rag.TextCache,
	embCache *rag.Cache[[]float32],
) {
	if observability.GlobalStore == nil {
		log.Printf("[SRE-WARN] memstats loop disabled — observability store not initialised")
		return
	}
	go func() {
		ticker := time.NewTicker(memStatsInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cpgHeap := cpgMgr.CurrentHeapMB()
				cpgLimit := cpgMgr.HeapLimitMB()
				qHit := ratioOrZero(queryCache)
				tHit := textRatioOrZero(textCache)
				eHit := embRatioOrZero(embCache)
				snap := captureFullMemStats(cpgHeap, cpgLimit, qHit, tHit, eHit)
				observability.GlobalStore.RecordMemStats(snap)
			}
		}
	}()
}

const memStatsWindow = 5 * time.Minute

func ratioOrZero(c *rag.QueryCache) float64 {
	if c == nil {
		return 0
	}
	_, _, r := c.WindowedHitRatio(memStatsWindow)
	return r
}

func textRatioOrZero(c *rag.TextCache) float64 {
	if c == nil {
		return 0
	}
	_, _, r := c.WindowedHitRatio(memStatsWindow)
	return r
}

func embRatioOrZero(c *rag.Cache[[]float32]) float64 {
	if c == nil {
		return 0
	}
	_, _, r := c.WindowedHitRatio(memStatsWindow)
	return r
}
