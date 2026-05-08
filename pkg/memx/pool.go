package memx

import (
	"sync"
	"sync/atomic"
)

//go:align 64 // [366.A] atomic counters (inFlight/totalGet/totalDrop/totalMiss) start on
// a cache-line boundary — each ObservablePool instance avoids false sharing with
// adjacent pool objects when stored in a struct array.
type ObservablePool[T any] struct {
	pool      sync.Pool
	inFlight  atomic.Int64
	totalGet  atomic.Uint64
	totalDrop atomic.Uint64
	totalMiss atomic.Uint64 // [SRE-36.3.1] new allocations from factory (pool was empty)
	resetFn   func(*T)
	maxCap    int
}

func NewObservablePool[T any](factory func() *T, reset func(*T), maxCap int) *ObservablePool[T] {
	p := &ObservablePool[T]{
		resetFn: reset,
		maxCap:  maxCap,
	}
	// Capture p so the closure can increment the miss counter each time the
	// pool is empty and must allocate a fresh object via factory.
	p.pool.New = func() any {
		p.totalMiss.Add(1)
		return factory()
	}
	return p
}

func (observablePool *ObservablePool[T]) Acquire() *T {
	observablePool.totalGet.Add(1)
	observablePool.inFlight.Add(1)

	slab := observablePool.pool.Get().(*T)
	observablePool.resetFn(slab)
	return slab
}

func (observablePool *ObservablePool[T]) Release(slab *T, currentCap int) {
	if slab == nil {
		return
	}
	observablePool.inFlight.Add(-1)

	if currentCap > observablePool.maxCap {
		observablePool.totalDrop.Add(1)
		return
	}

	if observablePool.resetFn != nil {
		observablePool.resetFn(slab)
	}
	observablePool.pool.Put(slab)
}

// Metrics returns (inFlight, totalGet, totalDrop, totalMiss).
// totalMiss counts calls where the pool was empty and factory() was invoked.
func (observablePool *ObservablePool[T]) Metrics() (inFlight int64, totalGet uint64, totalDrop uint64, totalMiss uint64) {
	return observablePool.inFlight.Load(),
		observablePool.totalGet.Load(),
		observablePool.totalDrop.Load(),
		observablePool.totalMiss.Load()
}

// Flush resets observable counters so post-cleanup metrics start fresh.
// [SRE-57.3] Called by OOMGuard before runtime.GC() which clears the sync.Pool slabs.
func (observablePool *ObservablePool[T]) Flush() {
	observablePool.inFlight.Store(0)
	observablePool.totalGet.Store(0)
	observablePool.totalDrop.Store(0)
	observablePool.totalMiss.Store(0)
}

// MissRate returns misses/(acquires+misses) as a fraction [0,1].
// Returns 0 if no activity has occurred yet.
func (observablePool *ObservablePool[T]) MissRate() float64 {
	miss := observablePool.totalMiss.Load()
	get := observablePool.totalGet.Load()
	total := get + miss
	if total == 0 {
		return 0
	}
	return float64(miss) / float64(total)
}
