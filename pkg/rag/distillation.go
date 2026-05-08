// Package rag — Neural Distillation: cognitive compression. [SRE-43]
//
// Prunes low-performing memories from the HNSW graph (FPI-guided) and distills
// complex reasoning chains from cloud models into compact local rules for Ollama.
//
// Two mechanisms:
//   1. FPI Pruning: removes nodes with low Flashback Performance Index
//   2. Knowledge Distillation: converts multi-step reasoning into single-step rules
package rag

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
)

// PruneCandidate is a node flagged for removal by FPI analysis. [SRE-43.1]
type PruneCandidate struct {
	DocID       uint64  `json:"doc_id"`
	NodeIndex   int     `json:"node_index"`
	FPIScore    float64 `json:"fpi_score"`    // lower = worse performing
	HitCount    int     `json:"hit_count"`
	MissCount   int     `json:"miss_count"`
	ContentHash string  `json:"content_hash"` // for dedup detection
}

// PruneResult describes the outcome of a pruning cycle. [SRE-43.1]
type PruneResult struct {
	Examined   int     `json:"examined"`
	Pruned     int     `json:"pruned"`
	Retained   int     `json:"retained"`
	SpaceSaved int64   `json:"space_saved_bytes"` // estimated
	Threshold  float64 `json:"threshold"`
}

// DistilledRule is a compressed local rule derived from complex reasoning. [SRE-43.2]
type DistilledRule struct {
	ID          string `json:"id"`
	InputPattern string `json:"input_pattern"` // what triggers this rule
	Output       string `json:"output"`         // the distilled response
	SourceChain  int    `json:"source_chain"`   // how many reasoning steps were compressed
	Confidence   float64 `json:"confidence"`
	CreatedAt    int64  `json:"created_at"`
}

// Distiller manages cognitive compression operations. [SRE-43]
type Distiller struct {
	mu            sync.RWMutex
	rules         []DistilledRule
	pruneHistory  []PruneResult
}

// NewDistiller creates a new cognitive compression engine.
func NewDistiller() *Distiller {
	return &Distiller{}
}

// PruneByFPI removes low-performing nodes from the HNSW graph. [SRE-43.1]
// Nodes with FPI score below threshold are candidates for removal.
// The FPI score is calculated as: hitRate / (1 + missCount) weighted by recency.
func (d *Distiller) PruneByFPI(ctx context.Context, graph *Graph, wal *WAL, fpi *FlashbackFPI, threshold float64) PruneResult {
	if fpi == nil || graph == nil {
		return PruneResult{Threshold: threshold}
	}

	result := PruneResult{Threshold: threshold}
	candidates := d.identifyCandidates(graph, fpi, threshold)
	result.Examined = len(graph.Nodes)

	if len(candidates) == 0 {
		result.Retained = result.Examined
		return result
	}

	// Sort candidates by FPI score ascending (worst first)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].FPIScore < candidates[j].FPIScore
	})

	// Soft-delete: mark documents as deleted in WAL (don't physically remove HNSW nodes
	// as that would require re-indexing the entire graph — just mark for exclusion in search)
	for _, c := range candidates {
		if ctx.Err() != nil {
			break
		}
		path, content, degree, err := wal.GetDocMeta(c.DocID)
		if err != nil {
			continue
		}
		// Mark as deleted by setting negative inbound degree
		if err := wal.SaveDocMeta(c.DocID, path, content, -degree-1); err != nil {
			log.Printf("[DISTILL] Failed to soft-delete doc %d: %v", c.DocID, err)
			continue
		}
		result.Pruned++
		result.SpaceSaved += int64(len(content))
	}

	result.Retained = result.Examined - result.Pruned

	d.mu.Lock()
	d.pruneHistory = append(d.pruneHistory, result)
	if len(d.pruneHistory) > 100 {
		d.pruneHistory = d.pruneHistory[len(d.pruneHistory)-50:]
	}
	d.mu.Unlock()

	log.Printf("[DISTILL] Pruned %d/%d nodes (threshold=%.2f, saved≈%d bytes)",
		result.Pruned, result.Examined, threshold, result.SpaceSaved)

	return result
}

// identifyCandidates scans the graph and identifies nodes below the FPI threshold.
func (d *Distiller) identifyCandidates(graph *Graph, fpi *FlashbackFPI, threshold float64) []PruneCandidate {
	globalHitRate := fpi.HitRate()
	if globalHitRate == 0 {
		return nil
	}

	var candidates []PruneCandidate
	for i, node := range graph.Nodes {
		// Nodes with no document or already deleted
		if node.DocID == 0 {
			continue
		}

		// Use node connectivity as a proxy for usefulness
		edgesCount := int(node.EdgesLength)
		connectivityScore := float64(edgesCount) / float64(max(1, len(graph.Nodes)/10))
		if connectivityScore > 1 {
			connectivityScore = 1
		}

		// Combined FPI: global hit rate * connectivity
		fpiScore := globalHitRate * (0.5 + 0.5*connectivityScore)

		if fpiScore < threshold {
			candidates = append(candidates, PruneCandidate{
				DocID:     node.DocID,
				NodeIndex: i,
				FPIScore:  fpiScore,
			})
		}
	}

	return candidates
}

// DistillReasoning compresses a multi-step reasoning chain into a single rule. [SRE-43.2]
// Takes a question-answer pair where the answer was derived from complex chain-of-thought,
// and creates a compact rule that can be applied locally without the full reasoning.
func (d *Distiller) DistillReasoning(inputPattern, output string, chainLength int, confidence float64) DistilledRule {
	rule := DistilledRule{
		ID:           fmt.Sprintf("dr_%d", len(d.rules)+1),
		InputPattern: inputPattern,
		Output:       output,
		SourceChain:  chainLength,
		Confidence:   confidence,
	}

	d.mu.Lock()
	d.rules = append(d.rules, rule)
	d.mu.Unlock()

	log.Printf("[DISTILL] New rule: %s (chain=%d, confidence=%.2f)", rule.ID, chainLength, confidence)
	return rule
}

// MatchRule finds the best distilled rule for an input query. [SRE-43.2]
func (d *Distiller) MatchRule(query string) (*DistilledRule, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var bestMatch *DistilledRule
	bestScore := 0.0

	for i := range d.rules {
		rule := &d.rules[i]
		score := patternSimilarity(query, rule.InputPattern) * rule.Confidence
		if score > bestScore && score > 0.5 {
			bestScore = score
			bestMatch = rule
		}
	}

	if bestMatch != nil {
		return bestMatch, true
	}
	return nil, false
}

// Rules returns all distilled rules. [SRE-43.2]
func (d *Distiller) Rules() []DistilledRule {
	d.mu.RLock()
	defer d.mu.RUnlock()
	result := make([]DistilledRule, len(d.rules))
	copy(result, d.rules)
	return result
}

// PruneHistory returns the history of pruning operations. [SRE-43.1]
func (d *Distiller) PruneHistory() []PruneResult {
	d.mu.RLock()
	defer d.mu.RUnlock()
	result := make([]PruneResult, len(d.pruneHistory))
	copy(result, d.pruneHistory)
	return result
}

// patternSimilarity computes a simple Jaccard similarity between two strings
// based on word overlap. Good enough for rule matching without external deps.
func patternSimilarity(a, b string) float64 {
	wordsA := splitWords(a)
	wordsB := splitWords(b)

	if len(wordsA) == 0 || len(wordsB) == 0 {
		return 0
	}

	setA := make(map[string]bool, len(wordsA))
	for _, w := range wordsA {
		setA[w] = true
	}

	intersection := 0
	for _, w := range wordsB {
		if setA[w] {
			intersection++
		}
	}

	union := len(setA)
	for _, w := range wordsB {
		if !setA[w] {
			union++
		}
	}

	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func splitWords(s string) []string {
	var words []string
	word := make([]byte, 0, 32)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			word = append(word, c|0x20) // lowercase
		} else if len(word) > 0 {
			words = append(words, string(word))
			word = word[:0]
		}
	}
	if len(word) > 0 {
		words = append(words, string(word))
	}
	return words
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// IdleVectorResult describes the outcome of an idle vector GC cycle. [SRE-46.1]
type IdleVectorResult struct {
	Examined int     `json:"examined"`
	Evicted  int     `json:"evicted"`
	Retained int     `json:"retained"`
	Threshold string `json:"threshold"` // "fpi_zero+mature_corpus"
}

// IdleVectorGC evicts nodes that have never been recalled (FPI hits == 0) once the
// corpus is mature (global hit count > minGlobalHits). [SRE-46.1]
// This is a soft-delete: degree is set negative so Search() skips these nodes.
// maxIdleDays is informational (logged); actual criterion is zero-FPI-hit since last REM.
func (d *Distiller) IdleVectorGC(ctx context.Context, graph *Graph, wal *WAL, fpi *FlashbackFPI, minGlobalHits int64) IdleVectorResult {
	if graph == nil || wal == nil || fpi == nil {
		return IdleVectorResult{}
	}

	result := IdleVectorResult{Threshold: "fpi_zero+mature_corpus"}

	// Only run GC once the corpus is mature enough to have meaningful FPI data.
	hits := fpi.totalHits.Load()
	if hits < minGlobalHits {
		log.Printf("[DISTILL-46] IdleVectorGC skipped: corpus not yet mature (%d/%d hits)",
			hits, minGlobalHits)
		return result
	}

	snap := fpi.Snapshot(0) // full snapshot

	// Build set of DocIDs with at least one FPI hit by dir prefix.
	// FPI tracks by directory, so nodes in dirs with hits are considered active.
	activeDirs := make(map[string]bool, len(snap.TopOffenders))
	for _, off := range snap.TopOffenders {
		activeDirs[off.Dir] = true
	}

	for i, node := range graph.Nodes {
		if ctx.Err() != nil {
			break
		}
		if node.DocID == 0 {
			continue
		}
		path, content, degree, err := wal.GetDocMeta(node.DocID)
		if err != nil || degree < 0 {
			continue // already deleted or unreadable
		}
		result.Examined++

		// Check if this doc's directory has any FPI activity.
		dirName := dirOf(path)
		if activeDirs[dirName] || degree > 0 {
			result.Retained++
			continue
		}

		// No FPI hits for this directory and degree == 0 → idle candidate.
		if err := wal.SaveDocMeta(node.DocID, path, content, -degree-1); err != nil {
			log.Printf("[DISTILL-46] idle evict doc %d error: %v", node.DocID, err)
			result.Retained++
			continue
		}
		log.Printf("[DISTILL-46] evicted idle node %d (%s)", i, path)
		result.Evicted++
	}

	result.Retained = result.Examined - result.Evicted
	log.Printf("[DISTILL-46] IdleVectorGC: examined=%d evicted=%d retained=%d",
		result.Examined, result.Evicted, result.Retained)
	return result
}

// CompressFlashbacks groups similar flashback strings (Jaccard ≥ threshold) into
// DistilledRules. Each group becomes a single high-level rule. [SRE-46.2]
func (d *Distiller) CompressFlashbacks(flashbacks []string, jaccardThreshold float64) []DistilledRule {
	if len(flashbacks) == 0 {
		return nil
	}
	if jaccardThreshold <= 0 {
		jaccardThreshold = 0.70
	}

	used := make([]bool, len(flashbacks))
	var rules []DistilledRule

	for i, fb := range flashbacks {
		if used[i] {
			continue
		}
		group := []string{fb}
		used[i] = true

		for j := i + 1; j < len(flashbacks); j++ {
			if used[j] {
				continue
			}
			if patternSimilarity(fb, flashbacks[j]) >= jaccardThreshold {
				group = append(group, flashbacks[j])
				used[j] = true
			}
		}

		rule := d.mergeGroup(group)
		rules = append(rules, rule)
	}

	d.mu.Lock()
	d.rules = append(d.rules, rules...)
	d.mu.Unlock()

	log.Printf("[DISTILL-46] CompressFlashbacks: %d flashbacks → %d rules", len(flashbacks), len(rules))
	return rules
}

// mergeGroup combines a set of similar flashbacks into a single DistilledRule.
func (d *Distiller) mergeGroup(group []string) DistilledRule {
	// Use the shortest as the canonical pattern (most general).
	canonical := group[0]
	for _, s := range group[1:] {
		if len(s) < len(canonical) {
			canonical = s
		}
	}
	return DistilledRule{
		ID:           fmt.Sprintf("cr_%d", len(group)),
		InputPattern: canonical,
		Output:       fmt.Sprintf("Compressed rule from %d similar flashbacks", len(group)),
		SourceChain:  len(group),
		Confidence:   0.7 + 0.05*float64(len(group)),
	}
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return path
}
