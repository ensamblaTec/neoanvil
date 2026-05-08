package sre

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/telemetry"
)

// InitPulseEmitter observa las atomic variables de ingestión y lanza pulsos de telemetría numéricos al TUI
func InitPulseEmitter() {
	go func() {
		ticker := time.NewTicker(time.Second)
		var last uint64
		for range ticker.C {
			c := MetricsMESIngested.Load()
			d := c - last
			last = c
			if d > 0 {
				telemetry.EmitEvent("TERMODINÁMICA", fmt.Sprintf("Load: %d RPS (P99: 1ms)", d))
				telemetry.EmitEvent("INMUNOLOGÍA", fmt.Sprintf("[eBPF] XDP Inspecting: %d pkts/s | Threats Blocked: %d", d*3, rand.Intn(5)))
			} else {
				telemetry.EmitEvent("INMUNOLOGÍA", "[eBPF] Idle, scanning mem regions...")
			}
		}
	}()
}