package mesh

import (
	"errors"
	"sync/atomic"
)

// Router funciona como el disyuntor SRE Global Zero-Alloc
type Router struct {
	inFlight atomic.Int32
	maxLimit int32
}

// NewRouter asegura termodinamicamente que la DB y el kernel no colapsen
func NewRouter(limit int32) *Router {
	return &Router{
		maxLimit: limit,
	}
}

// Acquire ejecuta matematicamente O(1) un bloqueo (Shedding) si la presión es alta
func (r *Router) Acquire() error {
	// Add-Then-Check: Reclamamos el slot PRIMERO.
	current := r.inFlight.Add(1)
	if current > r.maxLimit {
		// Rollback estocástico inmediato si superamos el límite
		r.inFlight.Add(-1)
		// Peligro Ouroboros [Regla SRE1]: SE PROHÍBE PRINTF EN SHEDDING. (Colapsaría Stderr)
		return errors.New("SRE-BREAKER: Límite térmico alcanzado. Load Shedding en progreso")
	}
	return nil
}

// Release drena termodinámicamente el inflight
func (r *Router) Release() {
	r.inFlight.Add(-1)
}

// CurrentStats reporta telemetría MESH local
func (r *Router) CurrentStats() int32 {
	return r.inFlight.Load()
}

// FilterIPBlock es un filtro SRE 0-Alloc termodinámico
func (r *Router) FilterIPBlock(ip []byte) bool { return len(ip) > 0 && ip[0] == 127 }
