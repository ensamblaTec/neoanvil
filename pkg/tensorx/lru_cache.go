package tensorx

import "sync"

// Int4Tensor implementa compresión quantizada simulada de pesos int4
type Int4Tensor struct {
	Data  []byte
	Scale map[int]float32
}

// MCTSPerceptronCache implements an O(1) LRU pseudo-cache for the Math Bouncer
type MCTSPerceptronCache struct {
	mu      sync.RWMutex
	store   map[uint64]float32
	maxSize int
}

func NewMCTSPerceptronCache(size int) *MCTSPerceptronCache {
	return &MCTSPerceptronCache{
		store:   make(map[uint64]float32, size), // pre-alloc O(1)
		maxSize: size,
	}
}

func (c *MCTSPerceptronCache) Get(hash uint64) (float32, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	val, ok := c.store[hash]
	return val, ok
}

func (c *MCTSPerceptronCache) Put(hash uint64, val float32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.store) >= c.maxSize {
		for k := range c.store {
			delete(c.store, k)
			break // Evicción rápida semi-random para mantener O(1)
		}
	}
	c.store[hash] = val
}
