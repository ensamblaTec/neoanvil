package chunker

import "strings"

const (
	defaultChunkSizeTokens = 2000
	charsPerToken          = 4
	overlapRatio           = 0.10
)

// LineChunker divides arbitrary text into fixed-size windows with 10% overlap.
// Window size is estimated as chunkSizeTokens × 4 bytes.
type LineChunker struct {
	chunkBytes   int // window size in bytes
	overlapBytes int
}

// NewLineChunker creates a LineChunker.
// chunkSizeTokens: 0 → defaults to 2000.
func NewLineChunker(chunkSizeTokens int) *LineChunker {
	if chunkSizeTokens <= 0 {
		chunkSizeTokens = defaultChunkSizeTokens
	}
	cb := chunkSizeTokens * charsPerToken
	return &LineChunker{
		chunkBytes:   cb,
		overlapBytes: int(float64(cb) * overlapRatio),
	}
}

// Chunk splits src into overlapping windows.
// Each chunk's StartLine / EndLine approximates the 1-based line range.
func (c *LineChunker) Chunk(src string) []Chunk {
	if len(src) == 0 {
		return nil
	}
	lines := strings.Split(src, "\n")

	var chunks []Chunk
	pos := 0
	stride := c.chunkBytes - c.overlapBytes

	for pos < len(src) {
		end := min(pos+c.chunkBytes, len(src))
		body := src[pos:end]

		// Compute approximate line numbers.
		startLine := countNewlines(src[:pos]) + 1
		endLine := startLine + countNewlines(body)

		chunks = append(chunks, Chunk{
			Body:      body,
			StartLine: startLine,
			EndLine:   endLine,
		})

		if end == len(src) {
			break
		}
		pos += stride
	}

	_ = lines // used for doc only
	return chunks
}

func countNewlines(s string) int {
	n := 0
	for _, c := range s {
		if c == '\n' {
			n++
		}
	}
	return n
}
