package federation

import (
	"math"
	"strings"
	"testing"
	"time"
)

// TestCosineSimilarity_Identical [Épica 231.G]
func TestCosineSimilarity_Identical(t *testing.T) {
	a := []float32{1, 0, 0}
	if sim := cosineSimilarity(a, a); math.Abs(float64(sim-1.0)) > 1e-6 {
		t.Errorf("identical vectors should give 1.0, got %v", sim)
	}
}

// TestCosineSimilarity_Orthogonal [Épica 231.G]
func TestCosineSimilarity_Orthogonal(t *testing.T) {
	a := []float32{1, 0}
	b := []float32{0, 1}
	if sim := cosineSimilarity(a, b); math.Abs(float64(sim)) > 1e-6 {
		t.Errorf("orthogonal vectors should give 0.0, got %v", sim)
	}
}

// TestCosineSimilarity_DifferentDims [Épica 231.G]
func TestCosineSimilarity_DifferentDims(t *testing.T) {
	a := []float32{1, 0}
	b := []float32{1, 0, 0}
	// Different dims — implementation-specific fallback, should not panic.
	_ = cosineSimilarity(a, b)
}

// TestCosineSimilarity_ZeroVector [Épica 231.G]
func TestCosineSimilarity_ZeroVector(t *testing.T) {
	a := []float32{0, 0, 0}
	b := []float32{1, 1, 1}
	sim := cosineSimilarity(a, b)
	if !math.IsNaN(float64(sim)) && sim != 0 {
		// Either 0 or NaN is acceptable for zero-vector edge case.
		if math.Abs(float64(sim)) > 1e-6 {
			t.Errorf("zero-vector sim should be 0 or NaN, got %v", sim)
		}
	}
}

// TestDeduplicateVectors_EmptyAndSingle [Épica 231.G]
func TestDeduplicateVectors_EmptyAndSingle(t *testing.T) {
	if got := DeduplicateVectors(nil, 0.9); got != nil {
		t.Errorf("expected nil for nil input, got %v", got)
	}
	single := []MemexVector{{Content: "solo"}}
	if got := DeduplicateVectors(single, 0.9); len(got) != 1 {
		t.Errorf("expected 1 for singleton input, got %d", len(got))
	}
}

// TestDeduplicateVectors_MergesDuplicates [Épica 231.G]
func TestDeduplicateVectors_MergesDuplicates(t *testing.T) {
	older := time.Now().Add(-time.Hour)
	newer := time.Now()
	vecs := []MemexVector{
		{Content: "primary", Embedding: []float32{1, 0, 0}, Timestamp: older},
		{Content: "secondary", Embedding: []float32{1, 0.001, 0}, Timestamp: newer}, // ~identical
		{Content: "distinct", Embedding: []float32{0, 1, 0}, Timestamp: newer},
	}
	out := DeduplicateVectors(vecs, 0.9)
	if len(out) != 2 {
		t.Fatalf("expected 2 after dedup, got %d: %+v", len(out), out)
	}
	// The merged vector should contain BOTH contents (newer wins, content concat).
	found := false
	for _, v := range out {
		if strings.Contains(v.Content, "primary") && strings.Contains(v.Content, "secondary") {
			found = true
		}
	}
	if !found {
		t.Errorf("merged content should contain both primary+secondary, got %+v", out)
	}
}

// TestDeduplicateVectors_ThresholdDefault [Épica 231.G]
func TestDeduplicateVectors_ThresholdDefault(t *testing.T) {
	// threshold=0 triggers the default (0.92).
	vecs := []MemexVector{
		{Content: "A", Embedding: []float32{1, 0}, Timestamp: time.Now()},
		{Content: "B", Embedding: []float32{1, 0}, Timestamp: time.Now()},
	}
	out := DeduplicateVectors(vecs, 0)
	if len(out) != 1 {
		t.Errorf("expected dedup to 1 with default threshold (0.92), got %d", len(out))
	}
}

// TestDeduplicateVectors_EmptyEmbedding [Épica 231.G]
func TestDeduplicateVectors_EmptyEmbedding(t *testing.T) {
	vecs := []MemexVector{
		{Content: "A", Embedding: []float32{}, Timestamp: time.Now()},
		{Content: "B", Embedding: []float32{1, 2}, Timestamp: time.Now()},
	}
	out := DeduplicateVectors(vecs, 0.9)
	if len(out) != 2 {
		t.Errorf("empty embeddings should not be merged, got %d", len(out))
	}
}

// TestSanitizePromptInput_Truncates [Épica 231.G]
func TestSanitizePromptInput_Truncates(t *testing.T) {
	s := strings.Repeat("x", 500)
	out := sanitizePromptInput(s, 100)
	if len(out) <= 100 || !strings.HasSuffix(out, "...") {
		t.Errorf("expected truncation with '...' suffix, got len=%d suffix=%q", len(out), out[len(out)-3:])
	}
}

// TestSanitizePromptInput_StripsBackticks [Épica 231.G]
func TestSanitizePromptInput_StripsBackticks(t *testing.T) {
	in := "hello ```injected``` world"
	out := sanitizePromptInput(in, 1000)
	if strings.Contains(out, "```") {
		t.Errorf("triple backticks should be replaced, got %q", out)
	}
	if !strings.Contains(out, "'''") {
		t.Errorf("expected triple-quote replacement, got %q", out)
	}
}

// TestSanitizePromptInput_StripsEscapedNewlines [Épica 231.G]
func TestSanitizePromptInput_StripsEscapedNewlines(t *testing.T) {
	in := `line1\nline2\nline3`
	out := sanitizePromptInput(in, 1000)
	if strings.Contains(out, `\n`) {
		t.Errorf(`literal \n should be replaced, got %q`, out)
	}
}
