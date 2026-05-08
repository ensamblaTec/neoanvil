package chunker

import (
	"slices"
	"strings"
	"testing"
)

const goSrc = `package demo

// Add adds two numbers.
func Add(a, b int) int {
	return a + b
}

// Sub subtracts b from a.
func Sub(a, b int) int {
	return a - b
}

// Point is a 2D coordinate.
type Point struct {
	X, Y float64
}
`

func TestGoFileASTChunked(t *testing.T) {
	c := NewASTChunker(2000)
	chunks := c.Chunk(goSrc)

	// Expect 3 chunks: Add, Sub, Point.
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d: %v", len(chunks), chunkNames(chunks))
	}
	names := chunkNames(chunks)
	for _, want := range []string{"Add", "Sub", "Point"} {
		found := slices.Contains(names, want)
		if !found {
			t.Errorf("chunk %q not found in %v", want, names)
		}
	}
	// Verify doc comment extraction.
	for _, ch := range chunks {
		if ch.Name == "Add" && !strings.Contains(ch.DocComment, "Add adds") {
			t.Errorf("Add doccomment = %q, want 'Add adds'", ch.DocComment)
		}
	}
}

func TestLogFileLineChunked(t *testing.T) {
	// 50 lines of log text — each line ~10 chars → 500 chars total.
	var sb strings.Builder
	for range 50 {
		sb.WriteString("INFO log line\n")
	}
	log := sb.String()

	// Small chunk size (50 chars = ~12 tokens) to get multiple chunks.
	c := NewLineChunker(13) // 13*4=52 bytes per chunk
	chunks := c.Chunk(log)
	if len(chunks) < 2 {
		t.Errorf("expected multiple chunks for large log, got %d", len(chunks))
	}
	// Last chunk must cover the last line.
	last := chunks[len(chunks)-1]
	if !strings.Contains(last.Body, "INFO log line") {
		t.Error("last chunk should contain log content")
	}
}

func TestFallbackToLineOnParseError(t *testing.T) {
	invalid := `this is not valid go source @@@`
	c := NewASTChunker(2000)
	chunks := c.Chunk(invalid)
	// Should fall back to LineChunker — must return ≥1 chunk.
	if len(chunks) == 0 {
		t.Error("expected fallback to LineChunker on parse error")
	}
	// AST chunk names should be empty (line chunks have no name).
	for _, ch := range chunks {
		if ch.Name != "" {
			t.Errorf("fallback chunks must have no Name, got %q", ch.Name)
		}
	}
}

func TestLineChunkerOverlap(t *testing.T) {
	// Build a string of exactly 3 × chunkSize to verify overlap produces more chunks.
	chunkSize := 10 // tokens → 40 chars per chunk
	c := NewLineChunker(chunkSize)
	// 120 chars with no newlines.
	src := strings.Repeat("a", 120)
	chunks := c.Chunk(src)
	// With 40-char window and 10% (4-char) overlap, stride = 36.
	// 120 bytes / 36 stride ≈ 4 chunks.
	if len(chunks) < 3 {
		t.Errorf("expected overlap to produce ≥3 chunks, got %d", len(chunks))
	}
}

func TestLineChunkerEmptyInput(t *testing.T) {
	c := NewLineChunker(2000)
	if chunks := c.Chunk(""); len(chunks) != 0 {
		t.Errorf("empty input should produce no chunks, got %d", len(chunks))
	}
}

func chunkNames(chunks []Chunk) []string {
	names := make([]string, len(chunks))
	for i, c := range chunks {
		names[i] = c.Name
	}
	return names
}
