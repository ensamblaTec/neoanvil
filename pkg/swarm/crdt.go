package swarm

import (
	"sync/atomic"

	"github.com/ensamblatec/neoanvil/pkg/telemetry"
)

// PNCounter representa un Positive-Negative Counter descentralizado (O(1) Allocations).
type PNCounter struct {
	Counts [256]NodeCount
}

type NodeCount struct {
	NodeID atomic.Uint64
	P      atomic.Uint64
	N      atomic.Uint64
}

// Add incrementa/decrementa para un ID de nodo dado de manera lineal y atómica.
func (c *PNCounter) Add(nodeID uint64, p, n uint64) {
	for i := 0; i < len(c.Counts); i++ {
		currentID := c.Counts[i].NodeID.Load()
		if currentID == nodeID {
			c.Counts[i].P.Add(p)
			c.Counts[i].N.Add(n)
			return
		}
		if currentID == 0 { // Empty slot
			if c.Counts[i].NodeID.CompareAndSwap(0, nodeID) {
				c.Counts[i].P.Add(p)
				c.Counts[i].N.Add(n)
				return
			}
			// Alguien ocupó la celda, seguir iterando
			i--
			continue
		}
	}
	telemetry.EmitEvent("FIREHOSE", "[SRE-WARN] PNCounter OOM Drop")
}

func (c *PNCounter) Value() int64 {
	var sum int64
	for i := 0; i < len(c.Counts); i++ {
		if c.Counts[i].NodeID.Load() == 0 {
			break
		}
		sum += int64(c.Counts[i].P.Load()) - int64(c.Counts[i].N.Load())
	}
	return sum
}

// Merge une el estado local con la replica foránea
func (c *PNCounter) Merge(remote *PNCounter) {
	for i := 0; i < len(remote.Counts); i++ {
		rID := remote.Counts[i].NodeID.Load()
		if rID == 0 {
			break
		}
		rP := remote.Counts[i].P.Load()
		rN := remote.Counts[i].N.Load()

		found := false
		for j := 0; j < len(c.Counts); j++ {
			cID := c.Counts[j].NodeID.Load()
			if cID == rID {
				// Max(P)
				for {
					currP := c.Counts[j].P.Load()
					if rP <= currP || c.Counts[j].P.CompareAndSwap(currP, rP) {
						break
					}
				}
				// Max(N)
				for {
					currN := c.Counts[j].N.Load()
					if rN <= currN || c.Counts[j].N.CompareAndSwap(currN, rN) {
						break
					}
				}
				found = true
				break
			}
			if cID == 0 {
				if c.Counts[j].NodeID.CompareAndSwap(0, rID) {
					c.Counts[j].P.Store(rP)
					c.Counts[j].N.Store(rN)
					found = true
					break
				}
				// Retry curr slot
				j--
				continue
			}
		}
		_ = found
	}
}

// LWWSet representa Last-Writer-Wins Element Set atómico (O(1) Eviction).
type LWWSet struct {
	Elems  [1024]LWWElement
	Cursor atomic.Uint32
}

type LWWElement struct {
	ItemHash atomic.Uint64
	Ts       atomic.Int64
	IsAdded  atomic.Bool
}

func (s *LWWSet) Add(hash uint64, ts int64, isAdded bool) {
	for i := 0; i < len(s.Elems); i++ {
		h := s.Elems[i].ItemHash.Load()
		if h == hash {
			for {
				currTs := s.Elems[i].Ts.Load()
				if ts <= currTs {
					return
				}
				if s.Elems[i].Ts.CompareAndSwap(currTs, ts) {
					s.Elems[i].IsAdded.Store(isAdded)
					return
				}
			}
		}
		if h == 0 {
			if s.Elems[i].ItemHash.CompareAndSwap(0, hash) {
				s.Elems[i].Ts.Store(ts)
				s.Elems[i].IsAdded.Store(isAdded)
				return
			}
			i-- // Retry slot
			continue
		}
	}

	// Torneo Lexicográfico de Evicción LWW-CRDT
	c := s.Cursor.Add(1)
	var oldestIdx uint32 = c % 1024
	oldestTs := s.Elems[oldestIdx].Ts.Load()
	for k := uint32(1); k < 4; k++ {
		testIdx := (c + k*257) % 1024
		currTs := s.Elems[testIdx].Ts.Load()
		if currTs < oldestTs {
			oldestTs = currTs
			oldestIdx = testIdx
		}
	}

	s.Elems[oldestIdx].ItemHash.Store(hash)
	s.Elems[oldestIdx].Ts.Store(ts)
	s.Elems[oldestIdx].IsAdded.Store(isAdded)
}

func (s *LWWSet) Contains(hash uint64) bool {
	for i := 0; i < len(s.Elems); i++ {
		h := s.Elems[i].ItemHash.Load()
		if h == hash {
			return s.Elems[i].IsAdded.Load()
		}
		if h == 0 {
			break
		}
	}
	return false
}

func (s *LWWSet) Merge(remote *LWWSet) {
	for i := 0; i < len(remote.Elems); i++ {
		h := remote.Elems[i].ItemHash.Load()
		if h == 0 {
			break
		}
		ts := remote.Elems[i].Ts.Load()
		isAdded := remote.Elems[i].IsAdded.Load()
		s.Add(h, ts, isAdded)
	}
}
