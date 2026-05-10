// Package incidents provides indexing and search over the .neo/incidents/ corpus.
package incidents

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/rag"
	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

// Session-scoped indexing counters. [Épica 158.A / 169.A]
var (
	incIndexedCount int64 // HNSW (via embedder)
	incSkippedCount int64
	incBM25Count    int64 // BM25-only (no embedder)
)

// IndexedCount returns the total incidents successfully indexed in this session.
func IndexedCount() int64 { return atomic.LoadInt64(&incIndexedCount) }

// SkippedCount returns the total incidents skipped (already in WAL) in this session.
func SkippedCount() int64 { return atomic.LoadInt64(&incSkippedCount) }

// BM25IndexedCount returns the total incidents added to the BM25-only lexical
// index in this session. Unlike IndexedCount() this never depends on the
// embedder — guaranteed to equal the total INC file count. [Épica 169.A]
func BM25IndexedCount() int64 { return atomic.LoadInt64(&incBM25Count) }

// ArchiveOldIncidents moves INC-*.md files older than `days` days from
// `.neo/incidents/` to `.neo/incidents/archive/`. Keeps the BM25 / HNSW
// index focused on recent incidents and prevents unbounded disk growth.
// days <= 0 disables the sweep. Errors from individual files are logged but
// don't abort the walk. [Épica 330.C]
func ArchiveOldIncidents(workspace string, days int) (archived int) {
	if days <= 0 {
		return 0
	}
	incDir := filepath.Join(workspace, ".neo", "incidents")
	entries, err := os.ReadDir(incDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[INC-ARCHIVE] ReadDir %s: %v", incDir, err)
		}
		return 0
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	archiveDir := filepath.Join(incDir, "archive")
	var mkdirDone bool
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "INC-") || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		info, statErr := e.Info()
		if statErr != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if !mkdirDone {
			if mkErr := os.MkdirAll(archiveDir, 0o750); mkErr != nil {
				log.Printf("[INC-ARCHIVE] mkdir %s: %v", archiveDir, mkErr)
				return archived
			}
			mkdirDone = true
		}
		src := filepath.Join(incDir, e.Name())
		dst := filepath.Join(archiveDir, e.Name())
		if rnErr := os.Rename(src, dst); rnErr != nil {
			log.Printf("[INC-ARCHIVE] rename %s → %s: %v", src, dst, rnErr)
			continue
		}
		archived++
	}
	if archived > 0 {
		log.Printf("[INC-ARCHIVE] moved %d incidents older than %dd to %s", archived, days, archiveDir)
	}
	return archived
}

// CountIncidentFiles returns the number of INC-*.md files in workspace/.neo/incidents/. [Épica 158.B]
func CountIncidentFiles(workspace string) int {
	entries, err := os.ReadDir(filepath.Join(workspace, ".neo", "incidents"))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "INC-") && strings.HasSuffix(e.Name(), ".md") {
			n++
		}
	}
	return n
}

// IncidentMeta holds the parsed metadata from an INC-*.md file.
type IncidentMeta struct {
	Path             string
	ID               string    // e.g. "INC-20260417-083232"
	Timestamp        time.Time // parsed from the ID
	Severity         string    // "CRITICAL", "WARNING", "INFO"
	Anomaly          string    // first non-empty line after "## ANOMALÍA CRÍTICA DETECTADA"
	AffectedServices []string  // parsed from **Affected Services:** line [153.C]
}

var (
	reAnomalySection   = regexp.MustCompile(`(?m)^## ANOMALÍA CRÍTICA DETECTADA\s*\n>(.*?)$`)
	reIncID            = regexp.MustCompile(`INC-(\d{8})-(\d{6})`)
	reSeverityKWD      = regexp.MustCompile(`(?i)panic|CRITICAL|OOM|QUARANTINE`)
	reWarningKWD       = regexp.MustCompile(`(?i)\bwarn(ing)?\b|timeout|retry`)
	reAffectedServices = regexp.MustCompile(`(?m)^\*\*Affected Services:\*\*\s*(.+)$`)
)

// ParseIncidentMeta extracts structured metadata from the raw bytes of an INC-*.md file.
func ParseIncidentMeta(path string, content []byte) IncidentMeta {
	text := string(content)
	m := IncidentMeta{Path: path}

	// Extract ID from filename.
	if base := filepath.Base(path); reIncID.MatchString(base) {
		m.ID = strings.TrimSuffix(base, ".md")
		if match := reIncID.FindStringSubmatch(base); len(match) == 3 {
			ts, err := time.Parse("20060102-150405", match[1]+"-"+match[2])
			if err == nil {
				m.Timestamp = ts
			}
		}
	}

	// Extract anomaly description.
	if sub := reAnomalySection.FindStringSubmatch(text); len(sub) == 2 {
		m.Anomaly = strings.TrimSpace(sub[1])
	}

	// Classify severity: prefer explicit **Severity:** header if present. [153.B]
	if strings.Contains(text, "**Severity:** CRITICAL") {
		m.Severity = "CRITICAL"
	} else if strings.Contains(text, "**Severity:** WARNING") {
		m.Severity = "WARNING"
	} else if strings.Contains(text, "**Severity:** INFO") {
		m.Severity = "INFO"
	} else if reSeverityKWD.MatchString(text) {
		m.Severity = "CRITICAL"
	} else if reWarningKWD.MatchString(text) {
		m.Severity = "WARNING"
	} else {
		m.Severity = "INFO"
	}

	// Parse affected services from **Affected Services:** header. [153.C]
	if sub := reAffectedServices.FindStringSubmatch(text); len(sub) == 2 {
		for svc := range strings.SplitSeq(sub[1], ",") {
			svc = strings.TrimSpace(svc)
			if svc != "" {
				m.AffectedServices = append(m.AffectedServices, svc)
			}
		}
	}

	return m
}

// IndexIncidents scans workspace/.neo/incidents/INC-*.md, embeds each file into the HNSW
// graph, and stores metadata in the WAL. Already-indexed files (docID already in WAL)
// are skipped unless their mtime is newer than the stored entry.
// Safe to call in a background goroutine; errors are logged, not returned.
func IndexIncidents(ctx context.Context, workspace string, embedder rag.Embedder, graph *rag.Graph, wal *rag.WAL, cpu tensorx.ComputeDevice) {
	incDir := filepath.Join(workspace, ".neo", "incidents")
	entries, err := os.ReadDir(incDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[INC-INDEX] ReadDir %s: %v", incDir, err)
		}
		return
	}

	indexed := 0
	skipped := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "INC-") || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		absPath := filepath.Join(incDir, e.Name())
		docID := incDocID(absPath)

		// Skip if already indexed (WAL hit).
		if existingPath, _, _, walErr := wal.GetDocMeta(docID); walErr == nil && existingPath != "" {
			info, statErr := e.Info()
			if statErr == nil {
				_ = info // mtime check: always re-index for now; WAL dedup prevents duplication
			}
			atomic.AddInt64(&incSkippedCount, 1)
			skipped++
			continue
		}

		data, readErr := os.ReadFile(absPath) //nolint:gosec // G304-DIR-WALK
		if readErr != nil {
			log.Printf("[INC-INDEX] read %s: %v", e.Name(), readErr)
			continue
		}

		// [PILAR-XXIII] nomic-embed-text has a 2048-token hard limit (~5KB mixed text).
		// Longer INC files must be chunked and their vectors averaged — otherwise
		// Ollama returns HTTP 500 "input length exceeds the context length" and the
		// incident is silently skipped.
		vec, embedErr := chunkedEmbed(ctx, embedder, string(data))
		if embedErr != nil {
			log.Printf("[INC-INDEX] embed %s: %v", e.Name(), embedErr)
			continue
		}

		if insertErr := graph.InsertBatch(ctx, []uint64{docID}, [][]float32{vec}, 5, cpu, wal); insertErr != nil {
			log.Printf("[INC-INDEX] insert %s: %v", e.Name(), insertErr)
			continue
		}
		if metaErr := wal.SaveDocMeta(docID, absPath, string(data), 0); metaErr != nil {
			log.Printf("[INC-INDEX] SaveDocMeta %s: %v", e.Name(), metaErr)
		}
		atomic.AddInt64(&incIndexedCount, 1)
		indexed++
	}
	log.Printf("[INC-INDEX] done: %d indexed, %d skipped", indexed, skipped)
}

// embedChunkMaxBytes is the safe input budget for a single embedder call.
// Tied to nomic-embed-text's 2048-token hard limit. Conservative 3000-byte
// budget maps to ~700-800 tokens for mixed Spanish/English text with code
// fences and punctuation (symbol-dense content tokenizes denser than prose).
// Previous 5000-byte budget triggered HTTP 500 on chunk 3/5 of denser INC
// files where a single chunk exceeded 2048 tokens. If the embedder changes,
// update this to its context window × avg bytes/token for the target corpus.
const embedChunkMaxBytes = 3000

// chunkedEmbed embeds arbitrarily long text by splitting on newlines, embedding
// each chunk ≤ embedChunkMaxBytes, and averaging the resulting vectors. Short
// inputs take the fast path (single embedder call).
func chunkedEmbed(ctx context.Context, embedder rag.Embedder, text string) ([]float32, error) {
	if len(text) <= embedChunkMaxBytes {
		return embedder.Embed(ctx, text)
	}
	chunks := splitForEmbedding(text, embedChunkMaxBytes)
	if len(chunks) == 0 {
		return nil, fmt.Errorf("chunkedEmbed: empty input after split")
	}
	vecs, err := rag.EmbedMany(ctx, embedder, chunks)
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("chunkedEmbed: embedder returned 0 vectors for %d chunks", len(chunks))
	}
	sum := make([]float32, len(vecs[0]))
	for _, v := range vecs {
		for j, f := range v {
			sum[j] += f
		}
	}
	inv := float32(1) / float32(len(vecs))
	for i := range sum {
		sum[i] *= inv
	}
	return sum, nil
}

// splitForEmbedding cuts text into pieces ≤ maxBytes. Prefers newline boundaries
// to preserve semantic context; falls back to hard byte-cuts when a single line
// exceeds the budget.
func splitForEmbedding(text string, maxBytes int) []string {
	var chunks []string
	for len(text) > maxBytes {
		// Prefer the last newline within the budget for a cleaner cut.
		cut := strings.LastIndex(text[:maxBytes], "\n")
		if cut < maxBytes/2 {
			cut = maxBytes // no good boundary — hard slice
		}
		chunks = append(chunks, text[:cut])
		text = strings.TrimLeft(text[cut:], "\n")
	}
	if text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}

// IndexIncidentsBM25Only builds a lexical-only BM25 index over the
// .neo/incidents/ corpus. Unlike IndexIncidents this never contacts the
// embedder — guaranteeing no HTTP 500 or timeout failures, no 2048-token
// limit, and sub-millisecond rebuild time. [Épica 169.A]
//
// The caller supplies a dedicated rag.LexicalIndex that is NOT shared with
// the global workspace lex — this keeps INC tokens isolated from code chunks,
// so SearchIncidentsBM25 returns only incident results.
func IndexIncidentsBM25Only(workspace string, lex *rag.LexicalIndex) {
	if lex == nil {
		return
	}
	incDir := filepath.Join(workspace, ".neo", "incidents")
	entries, err := os.ReadDir(incDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[INC-BM25] ReadDir %s: %v", incDir, err)
		}
		return
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "INC-") || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		absPath := filepath.Join(incDir, e.Name())
		data, readErr := os.ReadFile(absPath) //nolint:gosec // G304-DIR-WALK
		if readErr != nil {
			log.Printf("[INC-BM25] read %s: %v", e.Name(), readErr)
			continue
		}
		docID := incDocID(absPath)
		lex.AddDocument(docID, string(data))
		atomic.AddInt64(&incBM25Count, 1)
		n++
	}
	log.Printf("[INC-BM25] indexed %d incidents (lexical-only, no embedder)", n)
}

// SearchIncidentsBM25 returns incident metadata ranked by BM25 over the
// dedicated incident lexical index. Zero Ollama dependency — runs in
// sub-millisecond on a 50-document corpus. Returns nil on empty index
// (caller should fall back to HNSW or text_search). [Épica 169.C]
func SearchIncidentsBM25(query string, lex *rag.LexicalIndex, workspace string, limit int) []IncidentMeta {
	if lex == nil || query == "" || limit <= 0 {
		return nil
	}
	ranked := lex.Search(query, limit)
	if len(ranked) == 0 {
		return nil
	}
	// Resolve docID → INC file by scanning the directory once and hashing
	// each name with incDocID. The corpus is tiny (<100 files) so O(n·k)
	// is negligible; we avoid keeping a separate reverse map in sync.
	incDir := filepath.Join(workspace, ".neo", "incidents")
	entries, err := os.ReadDir(incDir)
	if err != nil {
		return nil
	}
	byID := make(map[uint64]string, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "INC-") || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		absPath := filepath.Join(incDir, e.Name())
		byID[incDocID(absPath)] = absPath
	}
	metas := make([]IncidentMeta, 0, len(ranked))
	for _, r := range ranked {
		path, ok := byID[r.DocID]
		if !ok {
			continue
		}
		content, readErr := os.ReadFile(path) //nolint:gosec // G304-DIR-WALK
		if readErr != nil {
			continue
		}
		meta := ParseIncidentMeta(path, content)
		metas = append(metas, meta)
	}
	return metas
}

// SearchIncidents performs a semantic search over the indexed incident corpus.
// Returns up to limit IncidentMeta entries ranked by HNSW similarity.
func SearchIncidents(ctx context.Context, query string, limit int, embedder rag.Embedder, graph *rag.Graph, wal *rag.WAL, cpu tensorx.ComputeDevice) ([]IncidentMeta, error) {
	vec, err := embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("incidents: embed query: %w", err)
	}
	results, searchErr := graph.Search(ctx, vec, limit, cpu)
	if searchErr != nil {
		return nil, fmt.Errorf("incidents: HNSW search: %w", searchErr)
	}

	var metas []IncidentMeta
	for _, nodeIdx := range results {
		if int(nodeIdx) >= len(graph.Nodes) {
			continue
		}
		docID := graph.Nodes[nodeIdx].DocID
		path, content, _, walErr := wal.GetDocMeta(docID)
		if walErr != nil || path == "" {
			continue
		}
		// Only surface incident files (filter out source code nodes).
		if !strings.Contains(path, ".neo/incidents/INC-") {
			continue
		}
		metas = append(metas, ParseIncidentMeta(path, []byte(content)))
	}
	return metas, nil
}

// incDocID generates a stable uint64 docID for an incident file path.
func incDocID(absPath string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(absPath))
	h.Write([]byte("_incident_v1"))
	return h.Sum64()
}
