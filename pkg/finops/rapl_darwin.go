//go:build darwin

package finops

import (
	"log"
	"math/rand"
	"runtime"
)

type RAPLSensor struct {
}

func MountRAPL() (*RAPLSensor, error) {
	log.Println("💻 Iniciando Mock Simulator Intel RAPL (Estimación Metabólica para macOS)")
	return &RAPLSensor{}, nil
}

// ReadWatts_O1 en Darwin estima térmicas basado en la saturación actual del Runtime Go
func (r *RAPLSensor) ReadWatts_O1(deltaTimeSeconds float64) float64 {
	routines := float64(runtime.NumGoroutine())

	// Base Wattage Apple Silicon M3 (Idling ~10-15W)
	baseW := 15.0

	// Dynamic Load: Asumimos ~0.05W por thread activo
	loadW := routines * 0.05

	// Noise: fluctuación +- 5% para realismo eBPF telemetry
	noise := (rand.Float64() - 0.5) * 2.0

	estimatedWatts := baseW + loadW + noise

	// Hard Capping for Simulation Bounds (Mac Max 45-60W)
	if estimatedWatts > 60.0 {
		return 60.0
	}
	return estimatedWatts
}

func (r *RAPLSensor) Close() {
	// No-op en Simulador
}
