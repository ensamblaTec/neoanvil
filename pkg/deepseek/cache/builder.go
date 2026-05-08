package cache

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// StructuralCacheBuilder assembles two-block prompts:
//
//	Block 1 (static)  = systemPrompt + globalDirectives + code files (capped at maxBlock1Chars).
//	Block 2 (dynamic) = task-specific content, always freshly supplied by the caller.
//
// Block 1 is computed once per unique CacheKey and stored with a TTL. Callers check
// CacheHit() to know whether the block was served from cache for telemetry purposes.
type StructuralCacheBuilder struct {
	mu               sync.Mutex
	tracker          *CacheKeyTracker
	systemPrompt     string
	globalDirectives string

	maxBlock1Chars int
	ttl            time.Duration

	// codeBlocks stores (block1 string, expiry) keyed by CacheKey.
	codeBlocks map[CacheKey]cachedBlock
}

type cachedBlock struct {
	content string
	expiry  time.Time
}

// NewBuilder creates a StructuralCacheBuilder.
//   - systemPrompt:     prepended to every Block 1 (e.g. plugin instructions).
//   - directivesPath:   path to a file loaded once as globalDirectives. Empty = no file.
//   - maxBlock1Chars:   hard cap on Block 1 length — content is truncated at the right.
//   - ttl:              how long a cached Block 1 remains valid.
func NewBuilder(systemPrompt, directivesPath string, maxBlock1Chars int, ttl time.Duration) *StructuralCacheBuilder {
	directives := ""
	if directivesPath != "" {
		data, err := os.ReadFile(directivesPath) //nolint:gosec // G304-CLI-CONSENT: path from operator config
		if err == nil {
			directives = string(data)
		}
	}
	return &StructuralCacheBuilder{
		tracker:          NewTracker(),
		systemPrompt:     systemPrompt,
		globalDirectives: directives,
		maxBlock1Chars:   maxBlock1Chars,
		ttl:              ttl,
		codeBlocks:       make(map[CacheKey]cachedBlock),
	}
}

// BuildBlock1 returns Block 1 for the given files together with the CacheKey.
// If the key is already cached and the TTL has not expired, the cached string is
// returned unchanged (CacheHit = true). Otherwise Block 1 is rebuilt and stored.
func (b *StructuralCacheBuilder) BuildBlock1(files []string) (block1 string, key CacheKey, cacheHit bool) {
	key = b.tracker.Snapshot(files)

	b.mu.Lock()
	defer b.mu.Unlock()

	if cb, ok := b.codeBlocks[key]; ok && time.Now().Before(cb.expiry) {
		return cb.content, key, true
	}

	// Build fresh Block 1.
	raw := b.assembleBlock1Raw(files)
	if len(raw) > b.maxBlock1Chars {
		raw = raw[:b.maxBlock1Chars]
	}
	b.codeBlocks[key] = cachedBlock{content: raw, expiry: time.Now().Add(b.ttl)}
	return raw, key, false
}

// AssemblePrompt concatenates Block 1 and Block 2 with a fixed separator.
func (b *StructuralCacheBuilder) AssemblePrompt(block1, task string) string {
	return fmt.Sprintf("%s\n\n---TASK---\n\n%s", block1, task)
}

// assembleBlock1Raw builds the raw (uncapped) Block 1. Caller holds b.mu.
func (b *StructuralCacheBuilder) assembleBlock1Raw(files []string) string {
	var parts strings.Builder
	parts.WriteString(b.systemPrompt)
	if b.globalDirectives != "" {
		parts.WriteString("\n\n" + b.globalDirectives)
	}
	for _, path := range files {
		data, err := os.ReadFile(path) //nolint:gosec // G304-CLI-CONSENT: paths from operator config
		if err != nil {
			continue
		}
		parts.WriteString(fmt.Sprintf("\n\n--- FILE: %s ---\n%s", path, string(data)))
	}
	return parts.String()
}

// Evict removes all expired cache entries. Call periodically if long-lived.
func (b *StructuralCacheBuilder) Evict() {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	for k, cb := range b.codeBlocks {
		if now.After(cb.expiry) {
			delete(b.codeBlocks, k)
		}
	}
}
