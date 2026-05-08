package rag

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

// FlashbackResult holds the result of a flashback search.
type FlashbackResult struct {
	Distance float32
	FilePath string
	Content  string
}

// SearchFlashback extracts an error signature from errOutput, embeds it combined
// with the base filename, searches the HNSW graph, and returns a match if the
// cosine distance is below the relevance threshold (< 0.25 → similarity > 75%).
// [SRE-28.4.1-3] Extracted to pkg/rag for testability.
//
// fpi and drift are optional (nil-safe) — pass nil to skip FPI/Drift recording.
// [SRE-35.2.1] RecordHit/Miss is O(1) and safe on the query hot-path.
func SearchFlashback(
	ctx context.Context,
	graph *Graph,
	wal *WAL,
	cpu tensorx.ComputeDevice,
	embedder Embedder,
	errOutput string,
	filename string,
	fpi *FlashbackFPI,
	drift *DriftMonitor,
) (*FlashbackResult, error) {
	if len(graph.Nodes) == 0 {
		return nil, nil
	}

	// [28.4.1] Extract error signature: first non-empty line, capped at 120 chars.
	sig := extractErrorSig(errOutput)
	if sig == "" {
		return nil, nil
	}

	// [28.4.2] Embed: error signature + base filename for contextual retrieval.
	query := sig + " " + filepath.Base(filename)
	queryVec, err := embedder.Embed(ctx, query)
	if err != nil || len(queryVec) == 0 {
		return nil, fmt.Errorf("embed failed: %w", err)
	}

	results, err := graph.Search(ctx, queryVec, 1, cpu)
	if err != nil || len(results) == 0 {
		return nil, err
	}

	nodeIdx := results[0]
	if int(nodeIdx) >= len(graph.Nodes) {
		return nil, nil
	}

	// [28.4.3] Compute cosine distance; threshold = 0.25.
	nodeVec := graph.GetVector(nodeIdx)
	dim := len(queryVec)
	qTensor := &tensorx.Tensor[float32]{Data: queryVec, Shape: tensorx.Shape{dim}, Strides: []int{1}}
	nTensor := &tensorx.Tensor[float32]{Data: nodeVec, Shape: tensorx.Shape{dim}, Strides: []int{1}}
	dist, distErr := cpu.CosineDistance(qTensor, nTensor)
	if distErr != nil {
		return nil, distErr
	}
	// Record distance for Cognitive Drift Monitor (O(1)). [SRE-35.1.2]
	if drift != nil {
		drift.RecordDistance(dist)
	}

	dir := filepath.Dir(filename)

	if dist >= 0.25 {
		// Miss: no relevant result. [SRE-35.2.1]
		if fpi != nil {
			fpi.RecordMiss(dir)
		}
		return nil, nil // Below relevance threshold
	}

	docID := graph.Nodes[nodeIdx].DocID
	path, content, _, _ := wal.GetDocMeta(docID)
	if content == "" {
		if fpi != nil {
			fpi.RecordMiss(dir)
		}
		return nil, nil
	}

	// Hit: relevant flashback found. [SRE-35.2.1]
	if fpi != nil {
		fpi.RecordHit(dir)
	}

	return &FlashbackResult{
		Distance: dist,
		FilePath: path,
		Content:  content,
	}, nil
}

// FormatFlashbackMessage renders a FlashbackResult as the injected error suffix.
func FormatFlashbackMessage(r *FlashbackResult) string {
	return fmt.Sprintf(
		"\n🧠 **FLASHBACK** (dist=%.3f): Patrón similar visto en `%s`.\n```\n%s\n```",
		r.Distance, r.FilePath, r.Content,
	)
}

// extractErrorSig returns the first non-empty line of errOutput, capped at 120 chars.
func extractErrorSig(errOutput string) string {
	for _, line := range strings.Split(errOutput, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			if len(line) > 120 {
				return line[:120]
			}
			return line
		}
	}
	return ""
}
