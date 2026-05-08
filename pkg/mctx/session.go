package mctx

import (
	"math"
	"sync"
)

var (
	CusumMu = 0.70 // Target baseline (expectativa de calidad)
	CusumK  = 0.05 // Noise slack (tolerancia a fluctuación)
	CusumH  = 0.15 // Degradation threshold (colapso irreversible)
)

const (
	RingSize = 100 // Memory boundary for continuous zero-alloc ingestion
)

type Session struct {
	mu            sync.Mutex
	RewardHistory [RingSize]float64
	head          int
	count         int
	CusumState    float64
}

var (
	globalSession = &Session{
		CusumState: 0.0,
		head:       0,
		count:      0,
	}
)

// UpdateAndCheckCollapse actualiza la historia en O(1) Zero-Alloc y evalúa CUSUM
func (s *Session) UpdateAndCheckCollapse(reward float64) (bool, float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Inserción en Anillo (Zero-Allocation Buffer)
	s.RewardHistory[s.head] = reward
	s.head = (s.head + 1) % RingSize
	if s.count < RingSize {
		s.count++
	}

	// Heurística CUSUM termal
	delta := CusumMu - reward - CusumK
	s.CusumState = math.Max(0, s.CusumState+delta)

	collapse := s.CusumState > CusumH
	return collapse, s.CusumState
}

// RecordEvaluation es la interfaz pública para el Orquestador
func RecordEvaluation(reward float64) (bool, float64) {
	return globalSession.UpdateAndCheckCollapse(reward)
}
