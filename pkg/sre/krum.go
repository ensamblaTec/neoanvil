package sre

import (
	"fmt"
	"github.com/ensamblatec/neoanvil/pkg/telemetry"
	"sync"
)

// MultiKrumDiscarder mitiga gradientes envenenados (Byzantine Faults) en topología Swarm (Parche 09SRE)
type MultiKrumDiscarder struct {
	mu           sync.Mutex
	discardYield int
}

func NewMultiKrumDiscarder() *MultiKrumDiscarder {
	return &MultiKrumDiscarder{}
}

func (k *MultiKrumDiscarder) EvaluateGradient(entropy float64) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	
	// Si el gradiente se desvía brutalmente, es un clon Bizantino
	if entropy > 0.85 {
		k.discardYield++
		telemetry.EmitEvent("INMUNOLOGÍA", fmt.Sprintf("MULTI-KRUM: Desvío detectado (%.2f). %d Gradientes Bizantinos destruidos.", entropy, k.discardYield))
		return false
	}
	return true
}
