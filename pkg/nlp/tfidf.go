package nlp

import (
	"math"
	"strings"
)

var stopWords = map[string]struct{}{
	"el": {}, "la": {}, "los": {}, "las": {}, "un": {}, "una": {}, "unos": {}, "unas": {},
	"y": {}, "o": {}, "pero": {}, "si": {}, "en": {}, "a": {}, "de": {}, "por": {}, "con": {}, "sin": {}, "para": {}, "como": {},
	"es": {}, "son": {}, "ser": {}, "estar": {}, "que": {}, "se": {}, "su": {}, "sus": {}, "del": {}, "al": {}, "lo": {}, "le": {},
	"implementar": {}, "crear": {}, "hacer": {}, "añadir": {}, "agregar": {}, "modificar": {}, "usar": {}, "utilizar": {},
	"the": {}, "to": {}, "an": {}, "and": {}, "or": {}, "for": {}, "in": {}, "of": {}, "with": {}, "on": {}, "by": {},
	"is": {}, "are": {}, "be": {}, "this": {}, "that": {}, "it": {}, "implement": {}, "create": {}, "make": {}, "add": {}, "use": {},
}

func tokenize(text string) []string {
	words := strings.Fields(strings.ToLower(text))
	var tokens []string
	for _, word := range words {
		word = strings.Trim(word, ".,:;()[]{}!?'\"")
		if len(word) > 2 {
			if _, ok := stopWords[word]; !ok {
				tokens = append(tokens, word)
			}
		}
	}
	return tokens
}

// CosineSimilarity implementing a basic TF-IDF representation and cosine measure.
func CosineSimilarity(textA, textB string) float64 {
	tokensA := tokenize(textA)
	tokensB := tokenize(textB)

	vocab := make(map[string]struct{})
	tfA := make(map[string]float64)
	tfB := make(map[string]float64)
	docFreq := make(map[string]int)

	for _, t := range tokensA {
		vocab[t] = struct{}{}
		tfA[t]++
	}
	for t := range tfA {
		docFreq[t]++
	}

	for _, t := range tokensB {
		vocab[t] = struct{}{}
		tfB[t]++
	}
	for t := range tfB {
		docFreq[t]++
	}

	if len(vocab) == 0 {
		return 0.0
	}

	var dotProduct, magA, magB float64

	// Use a mock N=10 to simulate a background corpus for TF-IDF.
	// This prevents the IDF penalty from zeroing out the intersection.
	for word := range vocab {
		idf := math.Log10(10.0 / float64(docFreq[word]))

		valA := tfA[word] * idf
		valB := tfB[word] * idf

		dotProduct += valA * valB
		magA += valA * valA
		magB += valB * valB
	}

	if magA == 0 || magB == 0 {
		return 0.0
	}

	return dotProduct / (math.Sqrt(magA) * math.Sqrt(magB))
}
