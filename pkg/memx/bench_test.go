package memx

import (
	"testing"
)

type Slab struct {
	Data []byte
}

func BenchmarkObservablePool_AcquireRelease(b *testing.B) {
	b.ReportAllocs()
	pool := NewObservablePool(
		func() *Slab { return &Slab{Data: make([]byte, 1024)} },
		func(s *Slab) { s.Data = s.Data[:0] },
		1024,
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		slab := pool.Acquire()
		pool.Release(slab, 0)
	}
}
