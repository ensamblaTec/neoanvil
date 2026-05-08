package nlp

import (
	"strings"
)

var antonymPairs = [][2]string{
	{"increase", "decrease"}, {"add", "remove"}, {"enable", "disable"},
	{"start", "stop"}, {"open", "close"}, {"create", "delete"},
	{"allow", "deny"}, {"true", "false"}, {"always", "never"},
	{"all", "none"}, {"must", "must not"}, {"should", "should not"},
}

// DetectContradiction evalúa si el parche actual contradice el historial de razonamiento.
func DetectContradiction(current string, previousClaims []string) []string {
	var contradictions []string
	currentLower := strings.ToLower(current)

	for _, prev := range previousClaims {
		prevLower := strings.ToLower(prev)

		// 1. Conflictos de Cuantificadores (Todo vs Nada)
		if (strings.Contains(currentLower, "all ") && strings.Contains(prevLower, "none ")) ||
			(strings.Contains(currentLower, "none ") && strings.Contains(prevLower, "all ")) {
			contradictions = append(contradictions, "⚠️ CONTRADICCIÓN LÓGICA: Conflicto de cuantificadores (All vs None).")
		}

		// 2. Pares de Antónimos en el mismo contexto
		for _, pair := range antonymPairs {
			if strings.Contains(currentLower, pair[0]) && strings.Contains(prevLower, pair[1]) {
				// Evaluar si comparten al menos 2 palabras clave de contexto
				wordsA := tokenize(current)
				sharedContext := 0
				for _, w := range wordsA {
					if len(w) > 3 && strings.Contains(prevLower, w) {
						sharedContext++
					}
				}
				if sharedContext >= 2 {
					contradictions = append(contradictions, "⚠️ CONTRADICCIÓN LÓGICA: Inversión semántica detectada ("+pair[0]+" vs "+pair[1]+").")
				}
			}
		}
	}
	return contradictions
}
