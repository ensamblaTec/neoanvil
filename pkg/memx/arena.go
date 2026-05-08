package memx

import (
	"iter"
	"sync"
)

type Arena[T any] struct {
	pool  *ObservablePool[T]
	slabs []*T
	mu    sync.Mutex
	capFn func(*T) int
}

func NewArena[T any](pool *ObservablePool[T], capFn func(*T) int) *Arena[T] {
	return &Arena[T]{
		pool:  pool,
		slabs: make([]*T, 0, 64),
		capFn: capFn,
	}
}

func (arena *Arena[T]) Alloc() *T {
	arena.mu.Lock()
	defer arena.mu.Unlock()
	slab := arena.pool.Acquire()
	arena.slabs = append(arena.slabs, slab)
	return slab
}

func (arena *Arena[T]) Reset() {
	arena.mu.Lock()
	defer arena.mu.Unlock()
	for _, slab := range arena.slabs {
		arena.pool.Release(slab, arena.capFn(slab))
	}
	clear(arena.slabs)
	arena.slabs = arena.slabs[:0]
}

func (arena *Arena[T]) Slabs() iter.Seq[*T] {
	return func(yield func(*T) bool) {
		for _, slab := range arena.slabs {
			if !yield(slab) {
				return
			}
		}
	}
}
