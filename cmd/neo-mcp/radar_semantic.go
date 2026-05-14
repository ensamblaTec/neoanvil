package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/astx"
	"github.com/ensamblatec/neoanvil/pkg/cpg"
	"github.com/ensamblatec/neoanvil/pkg/mctx"
	"github.com/ensamblatec/neoanvil/pkg/rag"
)

func (t *RadarTool) backgroundIndexFile(target string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[SRE-99.A] backgroundIndexFile panic for %s: %v", target, r)
		}
	}()

	absTarget := target
	if !filepath.IsAbs(target) {
		absTarget = filepath.Join(t.workspace, target)
	}
	// Skip paths that resolve outside this workspace — they originate from a
	// federation member and cannot be indexed relative to our workspace root.
	if !strings.HasPrefix(absTarget, t.workspace+"/") && absTarget != t.workspace {
		log.Printf("[SRE-99.A] backgroundIndexFile skip cross-workspace path: %s", target)
		return
	}
	// [ÉPICA 146.C-mirror, DS audit 2026-05-02] Symlink traversal hardening.
	// The string-prefix check above only validates the JOINED path — but
	// os.ReadFile follows symlinks at read time. An attacker with shell
	// access to the workspace can plant a symlink (e.g.
	// .neo/secret -> /etc/passwd), then trigger BLAST_RADIUS with
	// target=".neo/secret" → joined path passes prefix check → os.ReadFile
	// follows the link → secret content gets indexed into HNSW + reachable
	// to federation peers via SEMANTIC_CODE.
	//
	// Mitigation: resolve symlinks via filepath.EvalSymlinks BEFORE reading,
	// then re-validate the resolved path is still under the workspace root.
	// EvalSymlinks(non-existent) returns an error → caught by the existing
	// os.ReadFile error path (we don't fail-fast here on missing files
	// because background indexing happens on advisory paths that may not
	// exist yet, e.g. a target file just deleted from disk).
	if resolved, evalErr := filepath.EvalSymlinks(absTarget); evalErr == nil {
		if !strings.HasPrefix(resolved, t.workspace+"/") && resolved != t.workspace {
			log.Printf("[SRE-99.A] backgroundIndexFile reject symlink escape: %s → %s",
				absTarget, resolved)
			return
		}
		absTarget = resolved
	}
	data, err := os.ReadFile(absTarget) //nolint:gosec // G304-WORKSPACE-CANON: post-EvalSymlinks re-verified prefix
	if err != nil {
		log.Printf("[SRE-99.A] backgroundIndexFile read %s: %v", target, err)
		return
	}

	ext := filepath.Ext(absTarget)
	chunks := computeIndexChunks(data, ext, t.cfg.RAG.ChunkSize, t.cfg.RAG.Overlap)
	if len(chunks) == 0 {
		return
	}

	// Embed sequentially — the embedder's own semaphore caps concurrency,
	// and BLAST_RADIUS is an advisory call so latency here is not user-facing.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := embedAndInsert(ctx, t, absTarget, target, chunks); err != nil {
		return
	}
	saveIndexDependencies(t, data, ext, absTarget)
	log.Printf("[SRE-99.A] backgroundIndexFile indexed %s (%d chunks)", target, len(chunks))
}

// computeIndexChunks returns semantic chunks for data, falling back to fixed-size chunks when semantic yields nothing.
func computeIndexChunks(data []byte, ext string, chunkSize, overlap int) [][]byte {
	chunks := astx.SemanticChunk(context.Background(), data, ext)
	if len(chunks) == 0 {
		if chunkSize <= 0 {
			chunkSize = 3000
		}
		for start := 0; start < len(data); start += chunkSize - overlap {
			end := min(start+chunkSize, len(data))
			chunks = append(chunks, data[start:end])
		}
	}
	return chunks
}

// embedAndInsert embeds each chunk via Ollama and writes vectors + doc metadata to the HNSW graph.
// Uses rag.EmbedMany so the embedder.EmbedBatch path is taken on Ollama (single
// /api/embed round-trip for N chunks) instead of N sequential calls.
func embedAndInsert(ctx context.Context, t *RadarTool, absTarget, target string, chunks [][]byte) error {
	if len(chunks) == 0 {
		return nil
	}
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = string(c)
	}
	vecs, embedErr := rag.EmbedMany(ctx, t.embedder, texts)
	if embedErr != nil {
		// [SRE-104.C] Mirror the original behaviour: silent abort on initial failure,
		// log when the batch is mid-stream (we can't tell which chunk failed in the
		// batch path, so log once for the whole batch when len > 1).
		if len(chunks) > 1 {
			log.Printf("[SRE-99.A] backgroundIndexFile batch embed (%d chunks) of %s: %v", len(chunks), target, embedErr)
		}
		return embedErr
	}
	docIDs := make([]uint64, len(chunks))
	for i := range chunks {
		docIDs[i] = fnvHash64_Chunk(absTarget, i)
		if err := t.wal.SaveDocMeta(docIDs[i], absTarget, texts[i], 0); err != nil {
			log.Printf("[SRE-99.A] SaveDocMeta %s: %v", absTarget, err)
		}
	}
	if err := t.graph.InsertBatch(ctx, docIDs, vecs, 5, t.cpu, t.wal); err != nil {
		log.Printf("[SRE-99.A] backgroundIndexFile InsertBatch %s: %v", target, err)
		return err
	}
	return nil
}

// saveIndexDependencies refreshes the indexed file's file→file edges in the
// GRAPH_EDGES dep-graph so a future BLAST_RADIUS finds its dependents — same
// resolver path bootstrapWorkspace and certify use. Was writing import-strings
// into the orphan hnsw_deps bucket that nothing read. [BLAST_RADIUS dep-graph fix]
func saveIndexDependencies(t *RadarTool, data []byte, ext, absTarget string) {
	rel, err := filepath.Rel(t.workspace, absTarget)
	if err != nil {
		return
	}
	relSlash := filepath.ToSlash(rel)
	edges := fileDepEdges(t.workspace, workspaceModulePath(t.workspace), relSlash,
		extractImports(string(data), ext))
	if err := rag.ReplaceFileEdges(t.wal, relSlash, edges); err != nil {
		log.Printf("[SRE-99.A] dep-graph edges for %s: %v", relSlash, err)
	}
}

// formatSemanticDown formats the embedder-down response with grep fallback. [SRE-120.A]
func formatSemanticDown(grepHits []grepHit) string {
	var sb strings.Builder
	if len(grepHits) > 0 {
		sb.WriteString("### Grep Fallback _(embedder down)_\n")
		for hitIdx, hit := range grepHits {
			fmt.Fprintf(&sb, "#### %d. `%s:%d`\n```\n%s\n```\n\n", hitIdx+1, hit.File, hit.Line, hit.Snippet)
		}
	} else {
		sb.WriteString("_No local matches and embedder is unreachable._\n")
	}
	return sb.String()
}

// formatSemanticResults formats successful vector search results into markdown. [SRE-120.A]
func (t *RadarTool) formatSemanticResults(results []uint32) string {
	var sb strings.Builder
	for resultIdx, nodeIdx := range results {
		if int(nodeIdx) < len(t.graph.Nodes) {
			docID := t.graph.Nodes[nodeIdx].DocID
			path, content, _, _ := t.wal.GetDocMeta(docID)
			lang := strings.TrimPrefix(filepath.Ext(path), ".")
			if lang == "" {
				lang = "text"
			}
			fmt.Fprintf(&sb, "### %d. `%s`\n```%s\n%s\n```\n\n",
				resultIdx+1, path, lang, sanitizeFenced(content))
		}
	}
	return sb.String()
}

// sanitizeFenced escapes Markdown fence terminators inside the content
// payload that gets embedded between ``` blocks. Without this, a file
// containing a literal triple-backtick line breaks the surrounding
// fence — subsequent content leaks into the prose layer or, worse, is
// interpreted as new instructions by an LLM consumer downstream.
//
// [ÉPICA 153.H — DS audit 2026-05-02 catched as F2 SEV 6 pre-existing]
//
// We escape via zero-width-joiner insertion: turning "```" into a
// sequence the renderer displays as triple-backtick-ish but which no
// longer terminates the parent fence. The ZWJ (U+200D) is invisible
// in monospace rendering so the displayed content is faithful to the
// original — the only difference is that Markdown parsers no longer
// treat it as a fence delimiter.
func sanitizeFenced(content string) string {
	if !strings.Contains(content, "```") {
		return content
	}
	// Insert a zero-width joiner between the second and third backtick
	// of every triple-backtick run. The ZWJ doesn't render but breaks
	// the parser's match for the fence terminator.
	return strings.ReplaceAll(content, "```", "``‍`")
}

// applySemanticCPGActivation augments search results with CPG spreading activation energy. [PILAR-XX/147.C]
// [Épica 233.F] ctx removed — no cancellation surface here (cpgManager.Graph
// has its own internal timeout; Activate+NormalizeEnergy are pure in-memory
// passes over the graph).
func applySemanticCPGActivation(t *RadarTool, results []uint32, sb *strings.Builder) {
	if t.cpgManager == nil || len(results) == 0 {
		return
	}
	cpgGraph, gerr := t.cpgManager.Graph(100 * time.Millisecond)
	if gerr != nil {
		return
	}
	alphaVal := 0.5
	if t.cfg != nil && t.cfg.CPG.ActivationAlpha > 0 {
		alphaVal = t.cfg.CPG.ActivationAlpha
	}
	ranks := cpg.CachedComputePageRank(cpgGraph, 0.85, 50)
	seeds := resolveRAGSeedsInCPG(results, t.graph, t.wal, cpgGraph)
	if len(seeds) > 0 {
		rawEnergy := cpg.Activate(cpgGraph, seeds, alphaVal, 3)
		normEnergy := cpg.NormalizeEnergy(rawEnergy)
		sb.WriteString(formatActivationContext(cpgGraph, normEnergy, ranks, seeds))
	}
}

// semanticResultQuality classifies the dense retrieval outcome independently
// of embedder health. Used by handleSemanticCode to decide layout + footer
// tags. [ÉPICA 153]
//
//	"ok"          → denseCount >= minResults (dense answered the query)
//	"undershoot"  → 0 < denseCount < minResults (some dense hits, low confidence)
//	"empty"       → denseCount == 0 (dense had nothing — fall back to grep/BM25)
func semanticResultQuality(denseCount, minResults int) string {
	switch {
	case denseCount == 0:
		return "empty"
	case denseCount < minResults:
		return "undershoot"
	default:
		return "ok"
	}
}

// classifySemanticFallback computes the fallback_used tag from retrieval
// quality and per-source hit counts. The tag values are documented as part
// of the SEMANTIC_CODE response schema. [ÉPICA 153]
//
//	"none"          → quality=ok (no fallback fired)
//	"grep"          → grep produced ≥1 hit (literal match found)
//	"grep_no_match" → undershoot/empty AND grep returned 0 (operator should
//	                  rephrase or switch to GRAPH_WALK / Grep)
//	"bm25_only"     → undershoot AND BM25 had hits but grep didn't
func classifySemanticFallback(quality string, bm25Count, grepCount int) string {
	if quality == "ok" {
		return "none"
	}
	if grepCount > 0 {
		return "grep"
	}
	if bm25Count > 0 {
		return "bm25_only"
	}
	return "grep_no_match"
}

// buildSemanticGrepFallback runs literal grep when vector results undershoot
// minResults and writes a section to sb. Returns the number of grep hits
// emitted (0 when grep tried but no match — caller distinguishes
// `fallback_used: grep` vs `grep_no_match`). Caller decides embedder health
// independently — this function only reflects retrieval quality. [ÉPICA 153]
func buildSemanticGrepFallback(t *RadarTool, sb *strings.Builder, target string, denseCount, minResults int) int {
	if denseCount >= minResults {
		return 0
	}
	coverage := rag.IndexCoverage(t.graph, t.workspace) * 100
	log.Printf("[SRE-100.C] SEMANTIC_CODE undershoot: query=%q results=%d min=%d index_coverage=%.0f%% — running grep fallback",
		target, denseCount, minResults, coverage)
	grepHits := grepLiteralMatches(t.workspace, target, 10)
	if len(grepHits) == 0 {
		return 0
	}
	fmt.Fprintf(sb, "\n### Grep Fallback _(vector search returned %d < min=%d)_\n", denseCount, minResults)
	for idx, hit := range grepHits {
		fmt.Fprintf(sb, "#### %d. `%s:%d`\n```\n%s\n```\n\n", idx+1, hit.File, hit.Line, hit.Snippet)
	}
	return len(grepHits)
}

// applySemanticGossipFallback queries Gossip peers when local search returns nothing. [SRE-27.1.2]
func applySemanticGossipFallback(ctx context.Context, t *RadarTool, sb *strings.Builder, results []uint32, target string) {
	if len(results) > 0 || t.cfg == nil || !t.cfg.Server.Tailscale || len(t.cfg.Server.GossipPeers) == 0 {
		return
	}
	peerSnippets := mctx.QueryPeers(ctx, t.cfg.Server.GossipPeers, t.cfg.Server.GossipPort, target)
	if len(peerSnippets) > 0 {
		sb.WriteString("### 🌐 Cross-Pollination (Gossip P2P)\n\n")
		for idx, snippet := range peerSnippets {
			fmt.Fprintf(sb, "#### Peer Result %d\n```\n%s\n```\n\n", idx+1, snippet)
		}
	}
}

func (t *RadarTool) handleSemanticCode(ctx context.Context, args map[string]any) (any, error) {
	target, _ := args["target"].(string)
	if target == "" {
		return nil, fmt.Errorf("target is required for SEMANTIC_CODE")
	}
	minResults := 1
	if v, ok := args["min_results"].(float64); ok && v > 0 {
		minResults = int(v)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "## Semantic Search: '%s'\n\n", target)

	const topKDefault = 5
	bypassCache, _ := args["bypass_cache"].(bool)

	// [Épica 175/183/199] Cache lookup → embed → HNSW search pipeline.
	results, handled, err := semanticFetchResults(ctx, t, &sb, target, topKDefault, bypassCache)
	if err != nil {
		return nil, err
	}
	if handled {
		// embed-down path already wrote sb (formatSemanticDown + footer).
		return mcpText(sb.String()), nil
	}

	// [ÉPICA 153] Compute retrieval quality independently of embedder health.
	// quality reflects how well the dense vector pass answered the query;
	// embed_status separately reflects the Ollama embedder process health.
	quality := semanticResultQuality(len(results), minResults)

	// Build sub-sections in separate builders so we can reorder by quality.
	// PILAR-XXIII/170: BM25 + dense fused via RRF; ÉPICA 153 promotes BM25 to
	// primary header when dense undershoots so the agent sees it first.
	var denseSection, bm25Section, grepSection, cpgSection strings.Builder
	denseSection.WriteString(t.formatSemanticResults(results))
	applySemanticCPGActivation(t, results, &cpgSection)
	bm25Count := buildHybridFusionSection(t, &bm25Section, target, results, quality == "ok")
	grepCount := buildSemanticGrepFallback(t, &grepSection, target, len(results), minResults)

	// Assemble: when dense did NOT satisfy minResults AND BM25 has hits,
	// promote BM25 to primary position. Covers both "undershoot" (some dense)
	// and "empty" (zero dense) — F4 from DS audit 2026-05-02 spotted that
	// quality=="empty" was falling to the else branch and emitting the empty
	// dense section first. Otherwise dense first, BM25 (augment) after.
	if quality != "ok" && bm25Count > 0 {
		sb.WriteString(bm25Section.String())
		sb.WriteString(denseSection.String())
	} else {
		sb.WriteString(denseSection.String())
		sb.WriteString(bm25Section.String())
	}
	sb.WriteString(cpgSection.String())
	sb.WriteString(grepSection.String())

	// Tip when undershoot AND grep tried but didn't match — point operator at
	// alternative tools rather than re-running SEMANTIC_CODE with rephrasing
	// that won't help (the index doesn't have closer hits).
	grepNoMatch := quality != "ok" && grepCount == 0
	if grepNoMatch {
		sb.WriteString("\n💡 **Tip:** SEMANTIC_CODE undershoots with abstract phrases. Try (a) Grep with a specific symbol/string; (b) PROJECT_DIGEST + GRAPH_WALK; (c) rephrase as a function name.\n")
	}

	applySemanticGossipFallback(ctx, t, &sb, results, target)

	// [274.C] cross_workspace: true → scatter + dedup by file:line across
	// members. Emit BEFORE the footer so match_summary reflects the full
	// response — F3 from DS audit 2026-05-02 spotted that the original order
	// (footer → crossWS) made the agent see "dense=N bm25=M grep=P" with a
	// dash before extra hits appeared, undercounting.
	crossWS, _ := args["cross_workspace"].(bool)
	crossWSAdded := 0
	if crossWS {
		// [330.I] Augment with SharedGraph hits when project has shared_rag_enabled.
		// Unlike per-workspace scatter (which depends on each workspace's HNSW
		// coverage), SharedGraph query sees every doc merged by the coordinator's
		// REM cycle across all member workspaces.
		sharedSection := t.sharedGraphSemanticAugment(ctx, target)
		sb.WriteString(sharedSection)
		crossWSAdded += strings.Count(sharedSection, "\n- ")
		var scatterBuf strings.Builder
		appendSemanticCrossWorkspace(ctx, t, &scatterBuf, target)
		sb.WriteString(scatterBuf.String())
		crossWSAdded += strings.Count(scatterBuf.String(), "\n#### ")
	} else {
		sb.WriteString(t.formatScatterSection("SEMANTIC_CODE", map[string]any{"target": target}))
	}

	// Structured footer — separates embed health from result quality and
	// names the fallback path explicitly so the agent can decide remediation.
	// Emitted LAST so it summarizes the entire response including crossWS.
	fallbackUsed := classifySemanticFallback(quality, bm25Count, grepCount)
	if crossWS {
		fmt.Fprintf(&sb,
			"\n---\n_match_summary: dense=%d bm25=%d grep=%d crossWS=%d_\n_embed_status: healthy_\n_result_quality: %s_\n_fallback_used: %s_\n",
			len(results), bm25Count, grepCount, crossWSAdded, quality, fallbackUsed)
	} else {
		fmt.Fprintf(&sb,
			"\n---\n_match_summary: dense=%d bm25=%d grep=%d_\n_embed_status: healthy_\n_result_quality: %s_\n_fallback_used: %s_\n",
			len(results), bm25Count, grepCount, quality, fallbackUsed)
	}
	return mcpText(sb.String()), nil
}

// sharedGraphSemanticAugment queries the project-level shared HNSW tier for the
// semantic target and returns a Markdown section with dedup'd hits. Returns the
// empty string when the shared tier is not configured, shared_rag_enabled is
// false, the embedder is down, or the search returns zero results. [330.I]
// Mirrors sharedGraphBlastAugment but activates only on explicit opt-in via
// `project.shared_rag_enabled: true` to avoid polluting single-workspace runs.
func (t *RadarTool) sharedGraphSemanticAugment(ctx context.Context, target string) string {
	if t.sharedGraph == nil || t.embedder == nil {
		return ""
	}
	if t.cfg == nil || t.cfg.Project == nil || !t.cfg.Project.SharedRAGEnabled {
		return ""
	}
	vec, err := t.embedder.Embed(ctx, target)
	if err != nil {
		return ""
	}
	hits, err := t.sharedGraph.Search(vec, 5)
	if err != nil || len(hits) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n---\n### Shared tier hits (project memory, shared_hits: %d)\n", len(hits))
	for _, m := range hits {
		fmt.Fprintf(&sb, "- **%s** `%s`\n", m.Path, m.WorkspaceID)
	}
	return sb.String()
}

// semanticFetchResults resolves HNSW result IDs for target via QueryCache → EmbeddingCache → Ollama embed → HNSW.
// When the embedder is down it writes a grep fallback to sb and returns (nil, true, nil) — caller must return immediately.
func semanticFetchResults(ctx context.Context, t *RadarTool, sb *strings.Builder, target string, topKDefault int, bypassCache bool) ([]uint32, bool, error) {
	// [Épica 175/183] QueryCache: skip embed + HNSW entirely on cache hit.
	if t.queryCache != nil && !bypassCache {
		cacheKey := rag.NewQueryCacheKey("SEMANTIC_CODE|"+target, topKDefault)
		if cached, ok := t.queryCache.Get(cacheKey, t.graph.Gen.Load()); ok {
			return cached, false, nil
		}
	}
	// [Épica 199] Two-tier embed: EmbeddingCache before Ollama.
	var queryVec []float32
	embCacheKey := rag.NewCacheKey("EMBED|"+target, 0)
	if t.embCache != nil {
		if cachedVec, ok := t.embCache.Get(embCacheKey, t.graph.Gen.Load()); ok {
			queryVec = cachedVec
		}
	}
	if queryVec == nil {
		var embedErr error
		queryVec, embedErr = t.embedder.Embed(ctx, target)
		// [SRE-104.A] Any embed error degrades to literal grep instead of returning a hard error.
		if embedErr != nil {
			log.Printf("[SRE-104.A] SEMANTIC_CODE embedder down for query=%q: %v — degrading to grep", target, embedErr)
			hits := grepLiteralMatches(t.workspace, target, 10)
			if len(hits) == 0 {
				hits = grepTokenizedMatches(t.workspace, target, 10)
			}
			sb.WriteString(formatSemanticDown(hits))
			// [AUDIT-2026-04-23] Surface concrete embed failure (model-not-found vs network)
			// so the agent can distinguish remediation paths.
			// [ÉPICA 153] Structured footer mirrors the healthy path: match_summary
			// + result_quality (always "empty" — no dense pass ran) + fallback_used
			// names down_grep so triage can distinguish "embedder dead" from
			// "embedder alive but query too abstract".
			fallbackTag := "down_grep"
			if len(hits) == 0 {
				fallbackTag = "down_grep_no_match"
			}
			fmt.Fprintf(sb,
				"\n---\n_match_summary: dense=0 bm25=0 grep=%d_\n_embed_status: down_\n_result_quality: empty_\n_fallback_used: %s_\n_embed_error: %s_\n",
				len(hits), fallbackTag, shortEmbedError(embedErr))
			return nil, true, nil // handled — caller should return mcpText(sb.String()), nil
		}
		if t.embCache != nil {
			t.embCache.RecordMiss(target)
			t.embCache.PutAnnotated(embCacheKey, queryVec, t.graph.Gen.Load(), target)
		}
	}
	results, searchErr := t.graph.SearchAuto(ctx, queryVec, topKDefault, t.cpu, t.cfg.RAG.VectorQuant)
	if searchErr != nil {
		return nil, false, fmt.Errorf("vector search failed: %w", searchErr)
	}
	if t.queryCache != nil {
		// [196] Record the miss target so neo_cache_stats can surface warmup candidates.
		cacheKey := rag.NewQueryCacheKey("SEMANTIC_CODE|"+target, topKDefault)
		t.queryCache.RecordMiss(target)
		t.queryCache.PutAnnotated(cacheKey, results, t.graph.Gen.Load(), target)
	}
	return results, false, nil
}

// appendSemanticCrossWorkspace scatters SEMANTIC_CODE to member workspaces and appends deduplicated results. [274.C]
func appendSemanticCrossWorkspace(ctx context.Context, t *RadarTool, sb *strings.Builder, target string) {
	scatter := t.scatterToMembers(ctx, "SEMANTIC_CODE", map[string]any{"target": target}, 2)
	if len(scatter) == 0 {
		return
	}
	type dedupEntry struct{ key, text string }
	seen := map[string]struct{}{}
	var deduped []dedupEntry
	for _, r := range scatter {
		if r.err != nil || r.text == "" {
			continue
		}
		for line := range strings.SplitSeq(r.text, "\n") {
			// Lines with file:line pattern: "  file.go:42 ..."
			trimmed := strings.TrimSpace(line)
			if colonIdx := strings.Index(trimmed, ":"); colonIdx > 0 {
				key := trimmed[:colonIdx+strings.IndexByte(trimmed[colonIdx:], ' ')]
				if _, dup := seen[key]; !dup {
					seen[key] = struct{}{}
					deduped = append(deduped, dedupEntry{key: key, text: line})
				}
			}
		}
	}
	if len(deduped) > 0 {
		sb.WriteString("\n---\n### Cross-Workspace: SEMANTIC_CODE (deduplicated)\n\n")
		for _, e := range deduped {
			sb.WriteString(e.text + "\n")
		}
	}
}

// buildHybridFusionSection renders the top documents ranked by Reciprocal
// Rank Fusion of BM25 and dense HNSW results into sb. Returns the number of
// fused entries emitted. Skipped silently (returns 0) when either side
// returns nothing — we don't want to pollute the output when the lexical
// index has not been warmed up yet. [PILAR-XXIII/170]
//
// [ÉPICA 153] When `densePromoted` is false (dense undershot minResults), the
// header is upgraded to "BM25 Lexical Match (primary — dense undershoot)"
// signalling that the BM25 hits are the primary actionable signal for this
// query, not an optional augmentation.
func buildHybridFusionSection(t *RadarTool, sb *strings.Builder, target string, denseNodes []uint32, densePromoted bool) int {
	if t.lexicalIdx == nil {
		return 0
	}
	lexical := t.lexicalIdx.Search(target, 10)
	if len(lexical) == 0 {
		return 0
	}
	vectorial := make([]rag.DocumentScore, 0, len(denseNodes))
	for i, nodeIdx := range denseNodes {
		if int(nodeIdx) >= len(t.graph.Nodes) {
			continue
		}
		vectorial = append(vectorial, rag.DocumentScore{
			DocID: t.graph.Nodes[nodeIdx].DocID,
			Rank:  i + 1,
		})
	}
	if len(vectorial) == 0 {
		return 0
	}
	fused := rag.FuseResults(lexical, vectorial, 0.01, t.wal)
	if len(fused) == 0 {
		return 0
	}
	if len(fused) > 5 {
		fused = fused[:5]
	}
	if densePromoted {
		sb.WriteString("\n### Hybrid Fusion _(BM25 ⊕ Dense via RRF)_\n\n")
	} else {
		sb.WriteString("\n### BM25 Lexical Match _(primary — dense undershoot)_\n\n")
	}
	for i, r := range fused {
		path, _, _, _ := t.wal.GetDocMeta(r.DocID)
		if path == "" {
			path = fmt.Sprintf("docID=%d", r.DocID)
		}
		fmt.Fprintf(sb, "%d. `%s` _(score=%.4f)_\n", i+1, path, r.Score)
	}
	sb.WriteString("\n")
	return len(fused)
}

// grepHit is a literal-match result with enough context for the agent to
// jump to the line and read the surrounding 2 lines.
type grepHit struct {
	File    string
	Line    int
	Snippet string
}

// shortEmbedError trims wrapper prefixes from an embed error so the response stays
// useful for the agent. Keeps at most ~120 chars. [AUDIT-2026-04-23]
func shortEmbedError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// Strip the outer "fallo en generacion de embedding:" + "failed to contact ollama endpoint:" wrappers.
	for _, prefix := range []string{
		"fallo en generacion de embedding: ",
		"failed to contact ollama endpoint: ",
		"circuit breaker open: ",
	} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	if len(msg) > 140 {
		msg = msg[:137] + "..."
	}
	// Collapse newlines to keep the trailing metadata block on single lines.
	msg = strings.ReplaceAll(msg, "\n", " ")
	return msg
}

// grepTokenizedMatches splits the query into words (min 3 chars, skipping common
// stopwords) and ranks lines by how many tokens they contain. Used when a literal
// phrase search yields 0 results — natural-language queries rarely match verbatim.
// [AUDIT-2026-04-23]
func grepTokenizedMatches(workspace, query string, maxHits int) []grepHit {
	tokens := tokenizeQuery(query)
	if len(tokens) == 0 {
		return nil
	}
	type scored struct {
		hit   grepHit
		score int
	}
	var ranked []scored
	_ = filepath.Walk(workspace, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		switch ext {
		case ".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".md", ".yaml", ".yml", ".json":
		default:
			return nil
		}
		rel, _ := filepath.Rel(workspace, path)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, ".neo/") ||
			strings.Contains(rel, "/vendor/") ||
			strings.Contains(rel, "node_modules/") {
			return nil
		}
		data, rErr := os.ReadFile(path) //nolint:gosec // G304-DIR-WALK
		if rErr != nil {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		for i, ln := range lines {
			low := strings.ToLower(ln)
			score := 0
			for _, tok := range tokens {
				if strings.Contains(low, tok) {
					score++
				}
			}
			if score == 0 {
				continue
			}
			start := max(0, i-1)
			end := min(len(lines), i+2)
			ranked = append(ranked, scored{
				hit:   grepHit{File: rel, Line: i + 1, Snippet: strings.Join(lines[start:end], "\n")},
				score: score,
			})
		}
		return nil
	})
	// Sort descending by score, break ties by earlier file/line for determinism.
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		if ranked[i].hit.File != ranked[j].hit.File {
			return ranked[i].hit.File < ranked[j].hit.File
		}
		return ranked[i].hit.Line < ranked[j].hit.Line
	})
	if len(ranked) > maxHits {
		ranked = ranked[:maxHits]
	}
	out := make([]grepHit, len(ranked))
	for i, s := range ranked {
		out[i] = s.hit
	}
	return out
}

// tokenizeQuery splits a natural-language query into lowercase tokens ≥ 3 chars,
// dropping a small stopword list. Kept simple on purpose — this is a ranking hint,
// not a real search engine. [AUDIT-2026-04-23]
func tokenizeQuery(query string) []string {
	var stopwords = map[string]struct{}{
		"the": {}, "and": {}, "for": {}, "with": {}, "from": {}, "that": {},
		"this": {}, "have": {}, "has": {}, "are": {}, "was": {}, "were": {},
		"into": {}, "how": {}, "what": {}, "when": {}, "where": {}, "which": {},
	}
	lower := strings.ToLower(query)
	splitFn := func(r rune) bool {
		return !(r == '_' || r == '-' || r == '.' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	}
	raw := strings.FieldsFunc(lower, splitFn)
	seen := map[string]struct{}{}
	tokens := make([]string, 0, len(raw))
	for _, t := range raw {
		if len(t) < 3 {
			continue
		}
		if _, skip := stopwords[t]; skip {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		tokens = append(tokens, t)
	}
	return tokens
}

// grepLiteralMatches scans the workspace for literal substring matches of query.
// Returns up to maxHits results with a 3-line snippet (line + 1 above/below).
// Used as SEMANTIC_CODE fallback when the vector search has low coverage.
func grepLiteralMatches(workspace, query string, maxHits int) []grepHit {
	var hits []grepHit
	if query == "" {
		return hits
	}
	lower := strings.ToLower(query)

	_ = filepath.Walk(workspace, func(path string, info os.FileInfo, err error) error {
		if len(hits) >= maxHits {
			return filepath.SkipDir
		}
		if err != nil || info.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		// Skip binaries and irrelevant extensions.
		switch ext {
		case ".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".md", ".yaml", ".yml", ".json":
		default:
			return nil
		}
		rel, _ := filepath.Rel(workspace, path)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, ".neo/") ||
			strings.Contains(rel, "/vendor/") ||
			strings.Contains(rel, "node_modules/") {
			return nil
		}

		data, rErr := os.ReadFile(path) //nolint:gosec // G304-DIR-WALK
		if rErr != nil {
			return nil
		}
		if !strings.Contains(strings.ToLower(string(data)), lower) {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		for i, ln := range lines {
			if len(hits) >= maxHits {
				break
			}
			if !strings.Contains(strings.ToLower(ln), lower) {
				continue
			}
			start := max(0, i-1)
			end := min(len(lines), i+2)
			snippet := strings.Join(lines[start:end], "\n")
			hits = append(hits, grepHit{File: rel, Line: i + 1, Snippet: snippet})
		}
		return nil
	})
	return hits
}


// handleGraphWalk performs a CPG BFS walk from a named symbol. [PILAR-XX/148.B]
// graphWalkVariant packs maxDepth (≤255) and the edge-kind FNV digest
// into a single int for the TextCacheKey discriminator. Depth sits in
// the high bits so adjacent depths stay easy to read during debugging.
// [Épica 228]

// fnvStr8 returns a compact 8-bit FNV-1a digest of s. Used to squeeze
// short enum-like strings ("call"/"cfg"/...) into the int variant slot
// of TextCacheKey without pulling in a full hash allocation.
