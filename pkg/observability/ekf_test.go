package observability

import (
	"testing"
)

func TestEKF5D_ThermalSheddingTrigger(t *testing.T) {
	// TDP Límite a 45 Watts, Darwin Mock
	ekf := NewEKF5D(45.0, true)

	// No debe accionar panic ni shedding agresivo
	ekf.Predict(20.0)
	if ekf.x[4] != 20.0 {
		t.Fatal("Predicción térmica falló")
	}

	// Inducimos estrés para provocar el Shedding Thermal
	// La salida STD mostrará el Warning de la operación XDP.
	ekf.Predict(48.5)
}

func BenchmarkEKF_MatInverse5x5(b *testing.B) {
	ekf := NewEKF5D(45.0, true)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = ekf.ValidateO1Inverse()
	}
}
