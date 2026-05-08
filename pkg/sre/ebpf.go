package sre

import (
	"fmt"
	"github.com/ensamblatec/neoanvil/pkg/telemetry"
	"sync"
)

// EBPFLRUMap emula la inyección de mapas eBPF de Kernel para bloqueo LRU (Parche 10SRE)
type EBPFLRUMap struct {
	mu      sync.RWMutex
	blocked map[string]int
	max     int
}

func NewEBPFLRUMap(maxSize int) *EBPFLRUMap {
	return &EBPFLRUMap{
		blocked: make(map[string]int),
		max:     maxSize,
	}
}

func (e *EBPFLRUMap) RegisterDrop(ip string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.blocked[ip]++
	if len(e.blocked) >= e.max {
		// LRU Purge simple (en eBPF real es Kernel Space)
		for k := range e.blocked {
			delete(e.blocked, k)
			break
		}
	}
	telemetry.EmitEvent("INMUNOLOGÍA", fmt.Sprintf("DROP BPF: IP %s sumida en el abismo", ip))
	telemetry.EmitEvent("INMUNOLOGÍA", fmt.Sprintf("eBPF LRU MAP: %d / %d IPs", len(e.blocked), e.max))
}
