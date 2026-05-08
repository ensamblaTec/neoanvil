package nlp

import (
	"math"
)

// CalculateEntropyRatio computes the normalized Shannon entropy of a text query.
// It returns a value between 0.0 and 1.0 representing lexical diversity.
func CalculateEntropyRatio(text string) float64 {
	tokens := tokenize(text)
	if len(tokens) <= 1 {
		return 0.0
	}

	freq := make(map[string]float64)
	for _, t := range tokens {
		freq[t]++
	}

	uniqueTokens := len(freq)
	if uniqueTokens <= 1 {
		return 0.0
	}

	totalTokens := float64(len(tokens))
	var entropy float64

	for _, count := range freq {
		p := count / totalTokens
		entropy -= p * math.Log2(p)
	}

	maxEntropy := math.Log2(float64(uniqueTokens))
	if maxEntropy == 0 {
		return 0.0
	}

	ratio := entropy / maxEntropy
	if ratio < 0.0 {
		return 0.0
	}
	if ratio > 1.0 {
		return 1.0
	}
	return ratio
}
