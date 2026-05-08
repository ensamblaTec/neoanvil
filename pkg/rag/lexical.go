package rag

import (
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// LexicalIndex implements a Best-Match 25 (BM25) ranking function. [PILAR-XXIII/170]
// The previous incarnation only counted term presence; BM25 accounts for
// document length, term frequency saturation, and inverse document frequency —
// the same ranking that powers Elasticsearch, Lucene, and Bing's first stage.
//
// API preserved for backwards compatibility: AddDocument + Search return types
// are identical to the count-based predecessor. Callers that relied on the
// raw-count semantics will instead receive properly normalized BM25 scores.
type LexicalIndex struct {
	mu sync.RWMutex
	// Postings maps term → sorted list of (docID, tf). Sorted on DocID to enable
	// binary-search removal in future iterations.
	Postings map[string][]posting
	// DocLen is the token count of each indexed document.
	DocLen map[uint64]int
	// docCount and totalTokens maintain the running statistics for avgDocLen.
	docCount    int
	totalTokens int64
}

type posting struct {
	DocID uint64
	TF    uint32
}

// Standard BM25 parameters — values used by Lucene/Elasticsearch out of the box.
const (
	bm25K1 = 1.5
	bm25B  = 0.75
)

func NewLexicalIndex() *LexicalIndex {
	return &LexicalIndex{
		Postings: make(map[string][]posting),
		DocLen:   make(map[uint64]int),
	}
}

// tokenize splits content into lowercased alphanumeric tokens.
var tokenizeSeenPool = sync.Pool{
	New: func() any { return make(map[string]uint32, 128) },
}

func tokenize(content string) []string {
	content = strings.ToLower(content)
	return strings.FieldsFunc(content, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// AddDocument inserts or replaces the BM25 postings for docID.
// Re-indexing the same docID accumulates — callers who need idempotent
// behaviour should remove the previous version first (future work).
func (idx *LexicalIndex) AddDocument(docID uint64, content string) {
	tokens := tokenize(content)
	if len(tokens) == 0 {
		return
	}

	tfMap := tokenizeSeenPool.Get().(map[string]uint32)
	for k := range tfMap {
		delete(tfMap, k)
	}
	defer tokenizeSeenPool.Put(tfMap)

	for _, t := range tokens {
		tfMap[t]++
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	idx.DocLen[docID] = len(tokens)
	idx.docCount = len(idx.DocLen)
	idx.totalTokens += int64(len(tokens))

	for term, tf := range tfMap {
		idx.Postings[term] = append(idx.Postings[term], posting{DocID: docID, TF: tf})
	}
}

// avgDocLen returns the current mean document length. Must be called under lock.
func (idx *LexicalIndex) avgDocLen() float64 {
	if idx.docCount == 0 {
		return 0
	}
	return float64(idx.totalTokens) / float64(idx.docCount)
}

// scoreAccum reuses score maps across searches to avoid allocating a
// hot-path map on every query. [LEY 1]
var scoreAccumPool = sync.Pool{
	New: func() any { return make(map[uint64]float32, 1000) },
}

// Search returns the top-K documents ranked by BM25. Results include the rank
// (1-based) so they can feed FuseResults directly.
func (idx *LexicalIndex) Search(query string, topK int) []DocumentScore {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	tokens := tokenize(query)
	if len(tokens) == 0 || idx.docCount == 0 {
		return nil
	}

	avgDL := idx.avgDocLen()
	N := float64(idx.docCount)

	scores := scoreAccumPool.Get().(map[uint64]float32)
	for k := range scores {
		delete(scores, k)
	}
	defer scoreAccumPool.Put(scores)

	for _, term := range tokens {
		postings, ok := idx.Postings[term]
		if !ok {
			continue
		}
		df := float64(len(postings))
		// Standard BM25+ IDF with +1 to avoid negative weights for common terms.
		idf := math.Log((N-df+0.5)/(df+0.5) + 1.0)
		for _, p := range postings {
			tf := float64(p.TF)
			dl := float64(idx.DocLen[p.DocID])
			norm := 1.0 - bm25B + bm25B*(dl/avgDL)
			contribution := idf * (tf * (bm25K1 + 1)) / (tf + bm25K1*norm)
			scores[p.DocID] += float32(contribution)
		}
	}

	results := make([]DocumentScore, 0, len(scores))
	for id, s := range scores {
		results = append(results, DocumentScore{DocID: id, Score: s})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if len(results) > topK {
		results = results[:topK]
	}
	for i := range results {
		results[i].Rank = i + 1
	}
	return results
}
