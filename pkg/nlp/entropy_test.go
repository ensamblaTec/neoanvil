package nlp

import "testing"

func TestShannonEntropy(t *testing.T) {
	exactQuery := "ValidateJWT ValidateJWT"
	entropy1 := CalculateEntropyRatio(exactQuery)
	if entropy1 != 0.0 {
		t.Errorf("Expected entropy 0.0 for repetition, got %f", entropy1)
	}

	conceptualQuery := "optimizar concurrencia pasarela pagos"
	entropy2 := CalculateEntropyRatio(conceptualQuery)
	if entropy2 < 0.8 {
		t.Errorf("Expected entropy >= 0.8 for conceptual string, got %f", entropy2)
	}
}
