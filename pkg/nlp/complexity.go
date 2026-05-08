package nlp

import (
	"math"
)

// ComputeRootTTR mide la Claridad. TTR = Palabras Únicas / √Total Palabras
func ComputeRootTTR(text string) float64 {
	words := tokenize(text) // Reutiliza tokenize() de tfidf.go
	if len(words) == 0 {
		return 0.0
	}
	unique := make(map[string]struct{})
	for _, w := range words {
		unique[w] = struct{}{}
	}
	return float64(len(unique)) / math.Sqrt(float64(len(words)))
}

// ComputeLZ76 mide la tasa de creación de NUEVOS patrones (Lempel-Ziv 1976).
// Detecta "Reward Gaming" si el LLM repite el mismo bloque de código/texto.
func ComputeLZ76(text string) float64 {
	words := tokenize(text)
	if len(words) == 0 {
		return 0.0
	}

	complexity := 1.0
	prefixEnd := 1
	length := 1
	maxLookback := 256 // Ventana deslizante para O(N) en lugar de O(N^2)

	for prefixEnd+length <= len(words) {
		substring := words[prefixEnd : prefixEnd+length]
		searchStart := 0
		if prefixEnd > maxLookback {
			searchStart = prefixEnd - maxLookback
		}
		history := words[searchStart : prefixEnd+length-1]

		found := false
		for i := 0; i <= len(history)-length; i++ {
			match := true
			for j := 0; j < length; j++ {
				if history[i+j] != substring[j] {
					match = false
					break
				}
			}
			if match {
				found = true
				break
			}
		}

		if found {
			length++
		} else {
			complexity++
			prefixEnd += length
			length = 1
		}
	}

	n := float64(len(words))
	maxComplexity := 1.0
	if n > 1.0 {
		maxComplexity = n / math.Log(n)
	}

	score := complexity / maxComplexity
	if score > 1.0 {
		return 1.0
	}
	return score
}
