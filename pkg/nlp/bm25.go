package nlp

import (
	"math"
)

const (
	K1 = 1.2
	B  = 0.75
)

type BM25 struct {
	docs       [][]string
	docLengths []int
	avgDocLen  float64
	df         map[string]int
}

func NewBM25(texts []string) *BM25 {
	var docs [][]string
	for _, t := range texts {
		docs = append(docs, tokenize(t))
	}

	bm := &BM25{
		docs:       docs,
		docLengths: make([]int, len(docs)),
		df:         make(map[string]int),
	}

	var totalLen int
	for i, doc := range docs {
		bm.docLengths[i] = len(doc)
		totalLen += len(doc)

		seen := make(map[string]struct{})
		for _, token := range doc {
			if _, ok := seen[token]; !ok {
				bm.df[token]++
				seen[token] = struct{}{}
			}
		}
	}

	if len(docs) > 0 {
		bm.avgDocLen = float64(totalLen) / float64(len(docs))
	} else {
		bm.avgDocLen = 1.0
	}

	return bm
}

func (bm *BM25) Score(query string, docIdx int) float64 {
	qTokens := tokenize(query)
	var score float64
	doc := bm.docs[docIdx]
	docLen := float64(bm.docLengths[docIdx])
	N := float64(len(bm.docs))

	tf := make(map[string]float64)
	for _, token := range doc {
		tf[token]++
	}

	for _, qToken := range qTokens {
		if f, exists := tf[qToken]; exists {
			idf := math.Log(1.0 + (N-float64(bm.df[qToken])+0.5)/(float64(bm.df[qToken])+0.5))
			if idf < 0 {
				idf = 0
			}
			numerator := f * (K1 + 1.0)
			denominator := f + K1*(1.0-B+B*(docLen/bm.avgDocLen))
			score += idf * (numerator / denominator)
		}
	}

	return score
}
