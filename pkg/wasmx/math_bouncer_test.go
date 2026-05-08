package wasmx

import (
	"testing"
)

// Fuzz test para someter a estrés máximo al Evaluador SIMD y asegurar que nunca produzca pánicos
func FuzzVectorizedEntropy(f *testing.F) {
	// Corpus Semilla (Patrones críticos)
	f.Add("for i := 0; i < N; i++ { sum += 1.0 }")
	f.Add("make([]float32, needed)")
	f.Add("if a && b || c { new(int) }")
	f.Add("go func() { defer mu.Unlock() }()")
	f.Add("switch a { case b: break }")

	f.Fuzz(func(t *testing.T, code string) {
		entropy, length, conns, cyc, esc := computeVectorizedHeuristics(code)

		if entropy < 0 || entropy > 8.0 {
			t.Errorf("Entropía fuera de límites teóricos (0-8 bits): %f", entropy)
		}
		if length != len(code) {
			t.Errorf("Cálculo de longitud corrupto: %d vs esperado %d", length, len(code))
		}
		if conns < 0 || cyc < 0 || esc < 0 {
			t.Errorf("Corrupción de memoria: Herísticas negativas detectadas")
		}

		// Verificamos lógica de Shannon
		if length == 0 && entropy != 0.0 {
			t.Errorf("Entropía de cadena vacía debe ser 0.0, recibida: %f", entropy)
		}
	})
}
