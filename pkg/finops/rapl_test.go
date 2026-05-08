package finops

import (
	"testing"
)

func TestRAPLSensor_SimulatorLimit(t *testing.T) {
	sensor, err := MountRAPL()
	if err != nil {
		t.Skip("Linux local RAPL no está disponible en este nodo, ignorando Test estricto.")
	}
	defer sensor.Close()

	watts := sensor.ReadWatts_O1(1.0) // 1 second delta
	if watts < 0 || watts > 300.0 {
		t.Fatalf("Violación Térmica: El cálculo de Watts asimiló un Overflow o Underflow: %f W", watts)
	}
}

func BenchmarkRAPL_Telemetry(b *testing.B) {
	sensor, err := MountRAPL()
	if err != nil {
		b.Skip("Sin driver Intel local")
	}
	defer sensor.Close()

	b.ReportAllocs()

	for b.Loop() {
		// Evaluamos el Zero-Alloc de la rutina sysfs pure-pread
		_ = sensor.ReadWatts_O1(0.5)
	}
}
