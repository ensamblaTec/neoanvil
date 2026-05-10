package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/astx"
	"github.com/ensamblatec/neoanvil/pkg/cpg"
	"github.com/ensamblatec/neoanvil/pkg/graph"
	"github.com/ensamblatec/neoanvil/pkg/rag"
	"github.com/ensamblatec/neoanvil/pkg/sre"
	"github.com/ensamblatec/neoanvil/pkg/workspace"
)

// blastBatchEntry holds per-target results for BLAST_RADIUS batch mode. [SRE-136.A]
type blastBatchEntry struct {
	target      string
	impacted    []string
	fallback    string
	confidence  string
	graphStat   string
	coverage    float64
	errMsg      string
	cpgCallers  []cpgCaller
	cpgReachable int
	cpgConf     string
}

// cpgCaller is a single caller node enriched with CodeRank score. [PILAR-XX/146.D]
type cpgCaller struct {
	Name string
	File string
	Line int
	Rank float64
}

// parseBlastTargets returns the deduped/cleaned "targets" slice when the
// caller invoked batch mode, or nil when single-target mode should run.
// [Épica 228]
func parseBlastTargets(args map[string]any) []string {
	rawTargets, ok := args["targets"]
	if !ok {
		return nil
	}
	slice, ok := rawTargets.([]any)
	if !ok || len(slice) == 0 {
		return nil
	}
	var targets []string
	for _, v := range slice {
		if s, ok := v.(string); ok && s != "" {
			targets = append(targets, s)
		}
	}
	return targets
}

// blastRadiusCacheLookup returns the cache key, cached text and a hit
// flag. A nil text cache or bypass=true both short-circuit to a miss.
// [Épica 228]
func (t *RadarTool) blastRadiusCacheLookup(target string, bypassCache bool) (rag.TextCacheKey, string, bool) {
	if t.textCache == nil {
		return rag.TextCacheKey{}, "", false
	}
	key := rag.NewTextCacheKey("BLAST_RADIUS", target, 0)
	if bypassCache {
		return key, "", false
	}
	if cached, ok := t.textCache.Get(key, t.graph.Gen.Load()); ok {
		return key, cached, true
	}
	return key, "", false
}

// cpgEnrichBlast returns the CPG structural caller section for target,
// or "" when CPG is disabled or unavailable. [Épica 228]
func (t *RadarTool) cpgEnrichBlast(target string) string {
	if t.cpgManager == nil {
		return ""
	}
	g, err := t.cpgManager.Graph(100 * time.Millisecond)
	if err != nil {
		return ""
	}
	ranks := cpg.CachedComputePageRank(g, 0.85, 50)
	callers, reachable := resolveCPGCallers(g, ranks, target)
	return formatCPGBlastSection(callers, reachable)
}

func (t *RadarTool) handleBlastRadius(ctx context.Context, args map[string]any) (any, error) {
	// [Épica 260.B] target_workspace:"project" — scatter BLAST_RADIUS to all member workspaces.
	if tw, _ := args["target_workspace"].(string); tw == "project" && t.cfg.Project != nil {
		return t.handleBlastRadiusProjectScatter(ctx, args)
	}
	// [SRE-136.A] Batch mode: "targets" array overrides single "target".
	if targets := parseBlastTargets(args); len(targets) > 0 {
		return t.handleBlastRadiusBatch(ctx, targets)
	}
	target, _ := args["target"].(string)
	if target == "" {
		return nil, fmt.Errorf("target or targets is required for BLAST_RADIUS")
	}
	// [SRE-102.A] force_grep: bypass graph/AST, return deterministic grep results.
	if forceGrep, _ := args["force_grep"].(bool); forceGrep {
		return t.blastRadiusGrepOnly(target)
	}
	// [Épica 256.B] force_contract: skip CPG walk, return only cross-boundary callers.
	if forceContract, _ := args["force_contract"].(bool); forceContract {
		return t.blastRadiusContractOnly(target)
	}
	// [Épica 179/183] Cache the full BLAST_RADIUS response (including CPG
	// enrichment) — CPG PageRank over the whole graph is the heaviest part
	// and skipping it on a repeat query saves ~millisecond-class work.
	bypassCache, _ := args["bypass_cache"].(bool)
	cacheKey, cached, hit := t.blastRadiusCacheLookup(target, bypassCache)
	if hit {
		return mcpText(cached), nil
	}
	// [Épica 249.A] Auto-fallback: skip HNSW walk when workspace is under-indexed.
	indexCoverage := rag.IndexCoverage(t.graph, t.workspace)
	if indexCoverage < blastRadiusAutoFallbackThreshold {
		go t.backgroundIndexFile(target)
		lang := detectWorkspaceLang(target)
		hits := grepDependentsWithLinesLang(t.workspace, target, lang)
		text := formatBlastRadiusAutoFallback(target, hits, indexCoverage, lang)
		// [287.E] When workspace index is sparse, augment with shared HNSW tier hits.
		if t.sharedGraph != nil {
			text += t.sharedGraphBlastAugment(ctx, target)
		}
		if t.textCache != nil {
			t.textCache.RecordMiss(target)
			t.textCache.PutAnnotated(cacheKey, text, t.graph.Gen.Load(), "BLAST_RADIUS", target, 0)
		}
		return mcpText(text), nil
	}
	impacted, fallbackUsed, graphStatus, grepDepLines, astBreakdown, err := t.resolveImpactedNodes(ctx, target)
	if err != nil {
		return nil, err
	}
	// [SRE-99.A] On not_indexed, index in background so next call has coverage.
	if graphStatus == "not_indexed" {
		go t.backgroundIndexFile(target)
	}
	confidence := computeBlastConfidence(fallbackUsed, graphStatus)
	text := formatBlastRadius(target, impacted, grepDepLines, graphStatus, fallbackUsed, confidence, indexCoverage, astBreakdown)
	text += t.cpgEnrichBlast(target)
	text += t.contractEnrichBlast(target)
	// [Épica 179/190] Generation stamp captured AFTER the work completes
	// so an ingest that raced concurrently doesn't cache a pre-ingest view
	// under the post-ingest generation.
	if t.textCache != nil {
		t.textCache.RecordMiss(target)
		t.textCache.PutAnnotated(cacheKey, text, t.graph.Gen.Load(), "BLAST_RADIUS", target, 0)
	}
	return mcpText(text), nil
}

// handleBlastRadiusBatch runs BLAST_RADIUS in parallel for multiple targets. [SRE-136.B]
// Semaphore of 4 goroutines prevents HNSW saturation.
func (t *RadarTool) handleBlastRadiusBatch(ctx context.Context, targets []string) (any, error) {
	const maxConcurrent = 4
	sem := make(chan struct{}, maxConcurrent)
	entries := make([]blastBatchEntry, len(targets))

	var wg sync.WaitGroup
	for i, tgt := range targets {
		wg.Add(1)
		go func(idx int, target string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			entry := blastBatchEntry{target: target}
			impacted, fallbackUsed, graphStatus, _, _, runErr := t.resolveImpactedNodes(ctx, target)
			if graphStatus == "not_indexed" {
				go t.backgroundIndexFile(target)
			}
			if runErr != nil {
				entry.errMsg = runErr.Error()
			} else {
				entry.impacted = impacted
				entry.fallback = fallbackUsed
				entry.graphStat = graphStatus
				entry.confidence = computeBlastConfidence(fallbackUsed, graphStatus)
				entry.coverage = rag.IndexCoverage(t.graph, t.workspace)
			}
			// [PILAR-XX/146.G] CPG enrichment per-batch-entry.
			if t.cpgManager != nil {
				if g, gerr := t.cpgManager.Graph(100 * time.Millisecond); gerr == nil {
					ranks := cpg.CachedComputePageRank(g, 0.85, 50)
					callers, reachable := resolveCPGCallers(g, ranks, target)
					entry.cpgCallers = callers
					entry.cpgReachable = reachable
					entry.cpgConf = "structural"
				}
			}
			entries[idx] = entry
		}(i, tgt)
	}
	wg.Wait()

	confOrder := map[string]int{"high": 3, "medium": 2, "low": 1, "none": 0}
	maxConf := "none"
	for _, e := range entries {
		if confOrder[e.confidence] > confOrder[maxConf] {
			maxConf = e.confidence
		}
	}
	return mcpText(formatBlastRadiusBatch(entries, maxConf)), nil
}

// nexusDispatcherBase derives the Nexus dispatcher root URL from NEO_EXTERNAL_URL injected by
// Nexus into every child process. "http://127.0.0.1:9000/workspaces/id" → "http://127.0.0.1:9000".
// Returns "" when not running under Nexus (standalone / stdio mode).
func nexusDispatcherBase() string {
	ext := os.Getenv("NEO_EXTERNAL_URL")
	if ext == "" {
		return ""
	}
	if base, _, ok := strings.Cut(ext, "/workspaces/"); ok {
		return base
	}
	return ""
}

// resolveWorkspaceID returns the registry ID for absPath by reading ~/.neo/workspaces.json.
// Returns "" when the path is not registered (caller should fall back to slug).
func resolveWorkspaceID(absPath string) string {
	reg, err := workspace.LoadRegistry()
	if err != nil {
		return ""
	}
	clean := filepath.Clean(absPath)
	for _, e := range reg.Workspaces {
		if filepath.Clean(e.Path) == clean {
			return e.ID
		}
	}
	return ""
}

// nexusMemberWorkspaceIDs queries Nexus /status and returns a path→ID map
// restricted to paths present in memberPaths. Prevents scatter to workspaces
// not declared as project members (fix for 330.F cross-workspace leak).
func nexusMemberWorkspaceIDs(nexusBase string, memberPaths []string) map[string]string {
	out := make(map[string]string, len(memberPaths))
	if nexusBase == "" {
		return out
	}
	client := sre.SafeInternalHTTPClient(3)
	resp, err := client.Get(nexusBase + "/status") //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: nexusBase loopback-only via SafeInternalHTTPClient
	if err != nil {
		return out
	}
	defer resp.Body.Close() //nolint:errcheck
	var statuses []struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	if decErr := json.NewDecoder(resp.Body).Decode(&statuses); decErr != nil {
		return out
	}
	nexusMap := make(map[string]string, len(statuses))
	for _, s := range statuses {
		nexusMap[filepath.Clean(s.Path)] = s.ID
	}
	for _, mp := range memberPaths {
		clean := filepath.Clean(mp)
		if id, ok := nexusMap[clean]; ok {
			out[clean] = id
		}
	}
	return out
}

// scatterMember is one result from a federated scatter call. [Épica 267/PILAR XXXIV]
type scatterMember struct {
	workspace string
	name      string
	text      string
	err       error
}

// parseBlastRadiusScatterText extracts impact/confidence/coverage from a BLAST_RADIUS text body.
func parseBlastRadiusScatterText(text string) (impact, conf, cov string) {
	impact, conf, cov = "—", "—", "—"
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "- confidence:"); ok {
			parts := strings.SplitN(after, "|", 4)
			conf = strings.TrimSpace(parts[0])
			for _, p := range parts[1:] {
				p = strings.TrimSpace(p)
				if cpart, ok2 := strings.CutPrefix(p, "coverage:"); ok2 {
					cov = strings.TrimSpace(cpart)
				}
			}
		}
		if strings.HasPrefix(line, "- impacted (") {
			end := strings.Index(line, ")")
			if end > len("- impacted (") {
				impact = line[len("- impacted ("):end] + " nodes"
			}
		}
		if strings.Contains(line, "impacted: none") {
			impact = "0 nodes"
		}
	}
	return
}

// computeBlastMaxConfidence returns the highest confidence level across a slice of labels.
func computeBlastMaxConfidence(confs []string) string {
	if slices.Contains(confs, "high") {
		return "high"
	}
	if slices.Contains(confs, "medium") {
		return "medium"
	}
	if slices.Contains(confs, "low") {
		return "low"
	}
	return "unknown"
}

// scatterToMembers sends an MCP intent call to all remote member workspaces concurrently (sem=4).
// Returns nil when not in project mode or Nexus base is unavailable. [Épica 267/PILAR XXXV]
// timeoutSec controls the per-request HTTP timeout (0 = default 5s). [274.B]
func (t *RadarTool) scatterToMembers(_ context.Context, intent string, extraArgs map[string]any, timeoutSec ...int) []scatterMember {
	if t.cfg.Project == nil {
		return nil
	}
	nexusBase := nexusDispatcherBase()
	if nexusBase == "" {
		return nil
	}
	memberIDs := nexusMemberWorkspaceIDs(nexusBase, t.cfg.Project.MemberWorkspaces)
	type remoteWS struct{ ws, name, id string }
	var remotes []remoteWS
	for _, ws := range t.cfg.Project.MemberWorkspaces {
		if ws == "." || ws == "" || filepath.Clean(ws) == filepath.Clean(t.workspace) {
			continue
		}
		id := memberIDs[filepath.Clean(ws)]
		if id == "" {
			continue // not running in Nexus — skip to avoid non-member scatter [330.F]
		}
		remotes = append(remotes, remoteWS{ws, filepath.Base(ws), id})
	}
	if len(remotes) == 0 {
		return nil
	}
	timeout := 5
	if len(timeoutSec) > 0 && timeoutSec[0] > 0 {
		timeout = timeoutSec[0]
	}
	results := make([]scatterMember, len(remotes))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	client := sre.SafeInternalHTTPClient(timeout)
	for i, rem := range remotes {
		results[i] = scatterMember{workspace: rem.ws, name: rem.name}
		wg.Add(1)
		go func(idx int, r remoteWS) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			wsID := r.id
			rpcArgs := map[string]any{"intent": intent}
			maps.Copy(rpcArgs, extraArgs)
			argsJSON, marshalErr := json.Marshal(rpcArgs)
			if marshalErr != nil {
				results[idx].err = marshalErr
				return
			}
			payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"neo_radar","arguments":%s}}`, argsJSON)
			resp, postErr := client.Post( //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: nexusBase from NEO_EXTERNAL_URL, loopback-only via SafeInternalHTTPClient
				fmt.Sprintf("%s/workspaces/%s/mcp/message", nexusBase, wsID),
				"application/json",
				strings.NewReader(payload),
			)
			if postErr != nil {
				results[idx].err = postErr
				return
			}
			defer resp.Body.Close() //nolint:errcheck
			body, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				results[idx].err = readErr
				return
			}
			var rpcResp struct {
				Result struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
				RPCErr *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if jsonErr := json.Unmarshal(body, &rpcResp); jsonErr != nil {
				results[idx].err = jsonErr
				return
			}
			if rpcResp.RPCErr != nil {
				results[idx].err = fmt.Errorf("rpc: %s", rpcResp.RPCErr.Message)
				return
			}
			if len(rpcResp.Result.Content) > 0 {
				results[idx].text = rpcResp.Result.Content[0].Text
			}
		}(i, rem)
	}
	wg.Wait()
	return results
}

// formatScatterSection sends intent to remote member workspaces and returns a Markdown section.
// Returns "" when not in project mode or Nexus unavailable. [PILAR XXXV]
func (t *RadarTool) formatScatterSection(intent string, extra map[string]any) string {
	results := t.scatterToMembers(context.Background(), intent, extra)
	if len(results) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n---\n### Cross-Workspace: %s\n\n", intent)
	for _, r := range results {
		if r.err != nil {
			fmt.Fprintf(&sb, "#### %s\n> ⚠️ error: %v\n\n", r.name, r.err)
			continue
		}
		if r.text == "" {
			fmt.Fprintf(&sb, "#### %s\n> _(no data)_\n\n", r.name)
			continue
		}
		fmt.Fprintf(&sb, "#### %s\n%s\n\n", r.name, r.text)
	}
	return sb.String()
}

// handleBlastRadiusProjectScatter runs BLAST_RADIUS for each member workspace concurrently
// (sem=4) and returns a summary table with parsed impact/confidence/coverage. [Épica 260.B/267]
func (t *RadarTool) handleBlastRadiusProjectScatter(ctx context.Context, args map[string]any) (any, error) {
	proj := t.cfg.Project
	target, _ := args["target"].(string)
	var sb strings.Builder
	fmt.Fprintf(&sb, "## BLAST_RADIUS — Project: %s | target: %s\n\n", proj.ProjectName, target)
	sb.WriteString("| Workspace | Impact | Confidence | Coverage |\n")
	sb.WriteString("|-----------|--------|------------|----------|\n")

	type rowData struct {
		name   string
		impact string
		conf   string
		cov    string
	}
	rows := make([]rowData, len(proj.MemberWorkspaces))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	nexusBase := nexusDispatcherBase()
	memberIDs := nexusMemberWorkspaceIDs(nexusBase, proj.MemberWorkspaces)
	// [T005 nexus] 10s timeout — was 2s, but slow workspaces (cold
	// CPG rebuild, large RAG WAL) needed up to 8s to respond and
	// were appearing as "unreachable" → confidence:low fallback.
	// 10s gives them enough headroom while still bounded for the
	// scatter-gather UX.
	client := sre.SafeInternalHTTPClient(10)

	for i, ws := range proj.MemberWorkspaces {
		rows[i] = rowData{name: filepath.Base(ws), impact: "—", conf: "—", cov: "—"}
		wg.Add(1)
		go func(idx int, wsPath string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			isSelf := wsPath == "." || wsPath == "" || filepath.Clean(wsPath) == filepath.Clean(t.workspace)
			if isSelf {
				idxCov := rag.IndexCoverage(t.graph, t.workspace)
				rows[idx].cov = fmt.Sprintf("%.0f%%", idxCov*100)
				imp, fb, gs, _, _, _ := t.resolveImpactedNodes(ctx, target)
				if gs != "not_indexed" {
					rows[idx].impact = fmt.Sprintf("%d nodes", len(imp))
					rows[idx].conf = computeBlastConfidence(fb, gs)
				} else {
					rows[idx].impact = "not_indexed"
					rows[idx].conf = "low"
				}
				return
			}
			if nexusBase == "" {
				rows[idx].impact = "nexus_unavailable"
				return
			}
			wsID := memberIDs[filepath.Clean(wsPath)]
			if wsID == "" {
				rows[idx].impact = "not_in_nexus"
				rows[idx].conf = "none"
				return
			}
			payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"neo_radar","arguments":{"intent":"BLAST_RADIUS","target":%q}}}`, target)
			resp, err := client.Post( //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: nexusBase from NEO_EXTERNAL_URL, loopback-only via SafeInternalHTTPClient
				fmt.Sprintf("%s/workspaces/%s/mcp/message", nexusBase, wsID),
				"application/json",
				strings.NewReader(payload),
			)
			if err != nil {
				rows[idx].impact = "unreachable"
				rows[idx].conf = "none"
				return
			}
			defer resp.Body.Close() //nolint:errcheck
			body, _ := io.ReadAll(resp.Body)
			var rpcResp struct {
				Result struct {
					Content []struct{ Text string `json:"text"` } `json:"content"`
				} `json:"result"`
			}
			if json.Unmarshal(body, &rpcResp) == nil && len(rpcResp.Result.Content) > 0 {
				rows[idx].impact, rows[idx].conf, rows[idx].cov = parseBlastRadiusScatterText(rpcResp.Result.Content[0].Text)
			} else {
				rows[idx].impact = "reached"
				rows[idx].conf = "medium"
			}
		}(i, ws)
	}
	wg.Wait()

	confs := make([]string, 0, len(rows))
	for _, r := range rows {
		fmt.Fprintf(&sb, "| %s | %s | %s | %s |\n", r.name, r.impact, r.conf, r.cov)
		confs = append(confs, r.conf)
	}
	fmt.Fprintf(&sb, "\n**max_confidence:** %s\n", computeBlastMaxConfidence(confs))
	return mcpText(sb.String()), nil
}

// formatBlastRadiusBatch renders the merged BLAST_RADIUS batch report. [SRE-136.C]
func formatBlastRadiusBatch(entries []blastBatchEntry, maxConf string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "## BLAST_RADIUS Batch — %d targets | max_confidence: %s\n\n", len(entries), maxConf)
	for _, e := range entries {
		if e.errMsg != "" {
			fmt.Fprintf(&sb, "### ❌ %s\nError: %s\n\n", e.target, e.errMsg)
			continue
		}
		fmt.Fprintf(&sb, "### %s\n", e.target)
		fmt.Fprintf(&sb, "- confidence: %s | fallback: %s | graph: %s | coverage: %.0f%%\n",
			e.confidence, e.fallback, e.graphStat, e.coverage*100)
		if len(e.impacted) == 0 {
			sb.WriteString("- impacted: none detected\n")
		} else {
			fmt.Fprintf(&sb, "- impacted (%d): %s\n", len(e.impacted), strings.Join(e.impacted, ", "))
		}
		if e.cpgConf == "structural" {
			sb.WriteString(formatCPGBlastSection(e.cpgCallers, e.cpgReachable))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// resolveCPGCallers finds callers of any node in the target file, ranked by CodeRank. [PILAR-XX/146.D]
func resolveCPGCallers(g *cpg.Graph, ranks map[cpg.NodeID]float64, target string) (callers []cpgCaller, reachableCount int) {
	// Collect all nodes whose File path contains target (normalise separators).
	targetNorm := filepath.ToSlash(target)
	var fileNodeIDs []cpg.NodeID
	for _, n := range g.Nodes {
		if n.Kind != cpg.NodeFunc {
			continue
		}
		if strings.Contains(filepath.ToSlash(n.File), targetNorm) {
			fileNodeIDs = append(fileNodeIDs, n.ID)
		}
	}

	// Collect unique callers across all nodes in the file.
	seen := make(map[cpg.NodeID]struct{})
	for _, id := range fileNodeIDs {
		for _, callerID := range g.CallersOf(id) {
			if _, ok := seen[callerID]; ok {
				continue
			}
			seen[callerID] = struct{}{}
			if int(callerID) >= len(g.Nodes) {
				continue
			}
			callerNode := g.Nodes[callerID]
			callers = append(callers, cpgCaller{
				Name: callerNode.Name,
				File: callerNode.File,
				Line: callerNode.Line,
				Rank: ranks[callerID],
			})
		}
		reachableCount += len(g.ReachableFrom(id, 2))
	}
	// Sort by CodeRank descending.
	sort.Slice(callers, func(i, j int) bool {
		return callers[i].Rank > callers[j].Rank
	})
	if len(callers) > 10 {
		callers = callers[:10]
	}
	return callers, reachableCount
}

// formatCPGBlastSection formats the CPG structural section appended to BLAST_RADIUS output. [PILAR-XX/146.E]
func formatCPGBlastSection(callers []cpgCaller, reachableCount int) string {
	if len(callers) == 0 && reachableCount == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n### CPG Structural Analysis\n")
	fmt.Fprintf(&sb, "**cpg_confidence:** `structural`  \n")
	fmt.Fprintf(&sb, "**cpg_reachable_count:** %d  \n", reachableCount)
	if len(callers) > 0 {
		sb.WriteString("\n**cpg_callers** _(ranked by CodeRank)_\n")
		for i, c := range callers {
			fmt.Fprintf(&sb, "%2d. `%-35s` score=%.6f  %s:%d\n", i+1, c.Name, c.Rank, filepath.Base(c.File), c.Line)
		}
	}
	return sb.String()
}

// resolveRAGSeedsInCPG maps HNSW result indices to CPG NodeIDs by matching file paths. [PILAR-XX/147.C]
func resolveRAGSeedsInCPG(results []uint32, ragGraph *rag.Graph, wal *rag.WAL, cpgGraph *cpg.Graph) []cpg.NodeID {
	seen := make(map[cpg.NodeID]struct{})
	var seeds []cpg.NodeID
	for _, idx := range results {
		if int(idx) >= len(ragGraph.Nodes) {
			continue
		}
		docID := ragGraph.Nodes[idx].DocID
		path, _, _, _ := wal.GetDocMeta(docID)
		if path == "" {
			continue
		}
		pathNorm := filepath.ToSlash(path)
		for _, n := range cpgGraph.Nodes {
			if n.Kind != cpg.NodeFunc {
				continue
			}
			if !strings.Contains(filepath.ToSlash(n.File), pathNorm) {
				continue
			}
			if _, ok := seen[n.ID]; ok {
				continue
			}
			seen[n.ID] = struct{}{}
			seeds = append(seeds, n.ID)
		}
	}
	return seeds
}

// formatActivationContext renders the spreading activation context block. [PILAR-XX/147.E]
func formatActivationContext(g *cpg.Graph, normEnergy, ranks map[cpg.NodeID]float64, seeds []cpg.NodeID) string {
	seedSet := make(map[cpg.NodeID]struct{}, len(seeds))
	for _, s := range seeds {
		seedSet[s] = struct{}{}
	}

	type entry struct {
		n      cpg.Node
		energy float64
		rank   float64
	}
	var candidates []entry
	for id, en := range normEnergy {
		if _, isSeed := seedSet[id]; isSeed {
			continue
		}
		if en == 0 {
			continue
		}
		if int(id) >= len(g.Nodes) {
			continue
		}
		n := g.Nodes[id]
		if n.Kind != cpg.NodeFunc {
			continue
		}
		// Final score: 0.6 × semantic_sim (approximated by energy) + 0.4 × CodeRank
		finalScore := 0.6*en + 0.4*ranks[id]
		candidates = append(candidates, entry{n: n, energy: en, rank: finalScore})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].rank > candidates[j].rank
	})
	if len(candidates) > 5 {
		candidates = candidates[:5]
	}
	if len(candidates) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n### Activation Context _(CPG structural neighbors)_\n")
	for i, c := range candidates {
		fmt.Fprintf(&sb, "%2d. `%-35s` energy=%.4f  %s:%d\n",
			i+1, c.n.Name, c.energy, filepath.Base(c.n.File), c.n.Line)
	}
	return sb.String()
}

// resolveImpactedNodes encapsulates the RAG → AST → grep fallback chain. [SRE-55.2/74.4]
// Returns astBreakdown when the fallback is "ast" so the formatter can
// surface symbol-level vs package-level scope counts independently.
// [Sprint package-level fix]
func (t *RadarTool) resolveImpactedNodes(ctx context.Context, target string) (
	impacted []string, fallbackUsed, graphStatus string, grepDepLines []dependentLine,
	astBreakdown astImpactBreakdown, err error,
) {
	edges, edgeErr := rag.GetAllGraphEdges(t.wal)
	if edgeErr != nil {
		return nil, "", "", nil, astImpactBreakdown{}, fmt.Errorf("failed to get graph edges: %w", edgeErr)
	}

	var wg sync.WaitGroup
	var pageRank map[string]float32
	var err1, err2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		impacted, err1 = rag.GetImpactedNodes(t.wal, target)
	}()
	go func() {
		defer wg.Done()
		pageRank, err2 = graph.CalculatePageRank(ctx, t.cpu, t.pool, edges)
	}()
	wg.Wait()
	if err1 != nil {
		return nil, "", "", nil, astImpactBreakdown{}, err1
	}
	if err2 != nil {
		return nil, "", "", nil, astImpactBreakdown{}, err2
	}

	// [SRE-60.2 + sprint package-level fix] graph_status with explicit
	// reason. The previous binary indexed/not_indexed was ambiguous —
	// operator couldn't tell "graph never built" from "target not in
	// graph yet" from "graph stale".
	graphStatus = "up_to_date"
	fallbackUsed = "none"
	switch {
	case len(edges) == 0:
		graphStatus = "empty" // RAG WAL graph never populated (cold workspace)
	case len(pageRank) == 0:
		graphStatus = "stale" // edges exist but PageRank didn't converge
	case len(impacted) == 0:
		graphStatus = "target_not_in_graph" // graph fine; target has no incoming edges yet
	}

	// [SRE-55.2] AST fallback for .go files when RAG returns nothing.
	// Merges TWO scopes:
	//   - symbol-level via astFallbackImpact (callers of THIS file's
	//     exported symbols — change-of-signature blast radius)
	//   - package-level via astx.PackageImporters (every importer of
	//     the package — refactor-level blast radius)
	// We dedupe and use the union. The operator gets BOTH counts in
	// the formatted output so they can distinguish the scopes.
	if len(impacted) == 0 {
		merged, breakdown := mergedASTImpact(t.workspace, target)
		astBreakdown = breakdown
		if len(merged) > 0 {
			impacted = merged
			fallbackUsed = "ast"
		}
	}

	// [SRE-74.4] [Épica 249.C] Language-aware grep fallback.
	if len(impacted) == 0 {
		lang := detectWorkspaceLang(target)
		grepDepLines = grepDependentsWithLinesLang(t.workspace, target, lang)
		if len(grepDepLines) > 0 {
			for _, d := range grepDepLines {
				impacted = append(impacted, d.File)
			}
			fallbackUsed = "grep"
		}
	}
	return impacted, fallbackUsed, graphStatus, grepDepLines, astBreakdown, nil
}

// astFallbackImpact returns symbol-level blast radius for a .go file:
// callers that use one of the exported symbols defined in the file.
// Narrow scope — used to detect change-of-signature breakage.
// [SRE-55.2]
func astFallbackImpact(workspace, target string) []string {
	if !strings.HasSuffix(target, ".go") {
		return nil
	}
	absTarget := target
	if !filepath.IsAbs(target) {
		absTarget = filepath.Join(workspace, target)
	}
	src, readErr := os.ReadFile(absTarget) //nolint:gosec // G304-WORKSPACE-CANON
	if readErr != nil {
		return nil
	}
	callers, auditErr := astx.AuditSharedContract(workspace, absTarget, src)
	if auditErr != nil {
		return nil
	}
	seen := make(map[string]struct{})
	var impacted []string
	for _, f := range callers {
		rel, _ := filepath.Rel(workspace, f.File)
		if _, dup := seen[rel]; !dup {
			seen[rel] = struct{}{}
			impacted = append(impacted, rel)
		}
	}
	return impacted
}

// astImpactBreakdown bundles the two scopes BLAST_RADIUS reports
// independently. Symbol-level is the narrow change-of-signature
// blast; package-level is the broader "any file that imports this
// package" set. UnionAll is dedupe(symbol ∪ package). [Sprint fix]
type astImpactBreakdown struct {
	SymbolLevel  []string // callers of target's exported symbols
	PackageLevel []string // importers of target's package (broader)
	UnionAll     []string // dedupe(SymbolLevel ∪ PackageLevel)
}

// computeASTBreakdown runs both AuditSharedContract (symbol-level)
// and PackageImporters (package-level) and dedupes into UnionAll.
// Returns the breakdown for the formatter to surface both counts.
func computeASTBreakdown(workspace, target string) astImpactBreakdown {
	var b astImpactBreakdown
	b.SymbolLevel = astFallbackImpact(workspace, target)
	if pkgImps, err := astx.PackageImporters(workspace, target); err == nil {
		b.PackageLevel = pkgImps
	}
	seen := make(map[string]struct{})
	for _, f := range b.SymbolLevel {
		if _, dup := seen[f]; !dup {
			seen[f] = struct{}{}
			b.UnionAll = append(b.UnionAll, f)
		}
	}
	for _, f := range b.PackageLevel {
		if _, dup := seen[f]; !dup {
			seen[f] = struct{}{}
			b.UnionAll = append(b.UnionAll, f)
		}
	}
	return b
}

// mergedASTImpact returns the union of symbol-level and package-level
// impact as a single deduped list. Used by resolveImpactedNodes when
// the RAG WAL graph is empty/stale/missing for the target.
func mergedASTImpact(workspace, target string) ([]string, astImpactBreakdown) {
	b := computeASTBreakdown(workspace, target)
	return b.UnionAll, b
}

// graphStatusReason returns a one-liner the operator can read without
// guessing what an enum means. New states from the sprint fix are
// explicit; legacy strings are kept for backward compat in case some
// older test or HUD dashboard still emits them.
func graphStatusReason(s string) string {
	switch s {
	case "up_to_date":
		return "RAG dep-graph populated and PageRank converged"
	case "stale":
		return "RAG dep-graph has edges but PageRank empty — recently certified, not consolidated yet"
	case "empty":
		return "RAG dep-graph has zero edges — workspace cold or nothing certified yet"
	case "target_not_in_graph":
		return "RAG dep-graph fine, but this file has no incoming edges yet — never certified by a caller"
	// legacy aliases (pre-sprint):
	case "indexed":
		return "RAG dep-graph populated (legacy state)"
	case "not_indexed":
		return "RAG dep-graph empty (legacy state)"
	}
	return "unknown state"
}

// computeBlastConfidence maps fallback + graph status to a confidence label. [SRE-74.1]
func computeBlastConfidence(fallbackUsed, graphStatus string) string {
	switch fallbackUsed {
	case "ast":
		return "medium"
	case "grep":
		return "low"
	default:
		// "none" when the dep graph cannot back the result.
		// "high" only when PageRank actually ran on a populated graph.
		switch graphStatus {
		case "empty", "not_indexed", "stale", "target_not_in_graph":
			return "none"
		}
		return "high"
	}
}

// formatBlastRadius renders the BLAST_RADIUS markdown report. [SRE-117.B]
func formatBlastRadius(target string, impacted []string, grepDepLines []dependentLine,
	graphStatus, fallbackUsed, confidence string, indexCoverage float64, astBreakdown astImpactBreakdown) string {
	pagerankUsed := fallbackUsed == "none" && graphStatus == "up_to_date"
	var sb strings.Builder
	fmt.Fprintf(&sb, "## BLAST_RADIUS: %s\n\n", target)
	fmt.Fprintf(&sb, "**graph_status:** `%s` _(%s)_  \n", graphStatus, graphStatusReason(graphStatus))
	fmt.Fprintf(&sb, "**hnsw_coverage:** `%.0f%%` _(semantic embedding index — independent of dep graph)_  \n", indexCoverage*100)
	fmt.Fprintf(&sb, "**pagerank_used:** `%v`  \n", pagerankUsed)
	fmt.Fprintf(&sb, "**fallback:** `%s`  \n", fallbackUsed)
	fmt.Fprintf(&sb, "**confidence:** `%s`  \n", confidence)
	fmt.Fprintf(&sb, "**impacted_count:** %d  \n", len(impacted))
	if fallbackUsed == "ast" && (len(astBreakdown.SymbolLevel) > 0 || len(astBreakdown.PackageLevel) > 0) {
		fmt.Fprintf(&sb, "**symbol_impact_count:** %d _(callers of this file's exported symbols — change-of-signature scope)_  \n",
			len(astBreakdown.SymbolLevel))
		fmt.Fprintf(&sb, "**package_importers_count:** %d _(every importer of the package — refactor scope)_  \n",
			len(astBreakdown.PackageLevel))
	}
	sb.WriteString("\n")
	if len(grepDepLines) > 0 {
		sb.WriteString("### Grep Dependents _(confidence: low — import scan, not PageRank)_\n")
		for _, d := range grepDepLines {
			if d.CallerType != "" {
				fmt.Fprintf(&sb, "- %s:%d (%s)\n", d.File, d.Line, d.CallerType)
			} else if d.Line > 0 {
				fmt.Fprintf(&sb, "- %s:%d\n", d.File, d.Line)
			} else {
				fmt.Fprintf(&sb, "- %s\n", d.File)
			}
		}
	} else if len(impacted) > 0 {
		sb.WriteString("### Impacted Nodes\n")
		for _, n := range impacted {
			fmt.Fprintf(&sb, "- %s\n", n)
		}
	} else {
		sb.WriteString("_No impacted nodes found — file may be a leaf or graph not yet indexed._\n")
	}
	return sb.String()
}

// [SRE-102.A] blastRadiusGrepOnly returns grep-only dependents — skips graph
// entirely. Lets the agent request deterministic output when the index is
// known stale without paying for a graph query that will fall back anyway.
func (t *RadarTool) blastRadiusGrepOnly(target string) (any, error) {
	hits := grepDependentsWithLines(t.workspace, target)
	var sb strings.Builder
	fmt.Fprintf(&sb, "## BLAST_RADIUS: %s (force_grep)\n\n", target)
	sb.WriteString("**graph_status:** `bypassed`  \n")
	sb.WriteString("**fallback:** `grep`  \n")
	sb.WriteString("**confidence:** `low`  \n")
	fmt.Fprintf(&sb, "**impacted_count:** %d  \n\n", len(hits))
	if len(hits) == 0 {
		sb.WriteString("_No imports reference this target's package._\n")
		return mcpText(sb.String()), nil
	}
	sb.WriteString("### Grep Dependents _(force_grep — no PageRank)_\n")
	for _, d := range hits {
		if d.CallerType != "" {
			fmt.Fprintf(&sb, "- %s:%d (%s)\n", d.File, d.Line, d.CallerType)
		} else if d.Line > 0 {
			fmt.Fprintf(&sb, "- %s:%d\n", d.File, d.Line)
		} else {
			fmt.Fprintf(&sb, "- %s\n", d.File)
		}
	}
	return mcpText(sb.String()), nil
}

// blastRadiusContractOnly returns cross-boundary frontend callers for target via
// HTTP contract analysis (OpenAPI + Go route AST scan + TS fetch patterns).
// Used when force_contract:true. [Épica 256.B]
func (t *RadarTool) blastRadiusContractOnly(target string) (any, error) {
	contracts, coverage := t.resolveContracts()
	var sb strings.Builder
	fmt.Fprintf(&sb, "## BLAST_RADIUS: %s (force_contract)\n\n", target)
	sb.WriteString("**graph_status:** `bypassed`  \n")
	sb.WriteString("**fallback:** `contract`  \n")
	fmt.Fprintf(&sb, "**contract_coverage:** `%s`  \n\n", coverage)
	targetBase := filepath.Base(target)
	found := false
	for _, c := range contracts {
		if c.BackendFile == target || filepath.Base(c.BackendFile) == targetBase {
			fmt.Fprintf(&sb, "### Frontend callers (contract: %s %s)\n", c.Method, c.Path)
			if len(c.FrontendCallers) == 0 {
				sb.WriteString("_No TypeScript/JavaScript callers detected for this route._\n")
				continue
			}
			for _, caller := range c.FrontendCallers {
				fmt.Fprintf(&sb, "- %s:%d [via fetch/axios]\n", caller.File, caller.Line)
			}
			found = true
		}
	}
	if !found {
		sb.WriteString("_No HTTP routes detected in this file, or contract_coverage: none._\n")
	}
	return mcpText(sb.String()), nil
}

// contractEnrichBlast appends a cross-boundary section to an existing BLAST_RADIUS
// report when the target is a Go HTTP handler file. [Épica 256.A]
func (t *RadarTool) contractEnrichBlast(target string) string {
	contracts, coverage := t.resolveContracts()
	if len(contracts) == 0 {
		return ""
	}
	targetBase := filepath.Base(target)
	var sb strings.Builder
	for _, c := range contracts {
		if c.BackendFile != target && filepath.Base(c.BackendFile) != targetBase {
			continue
		}
		if len(c.FrontendCallers) == 0 {
			continue
		}
		if sb.Len() == 0 {
			fmt.Fprintf(&sb, "\n### Frontend callers via contract (%s)\n", coverage)
		}
		fmt.Fprintf(&sb, "\n**%s %s** (`%s`)\n", c.Method, c.Path, c.BackendFn)
		for _, caller := range c.FrontendCallers {
			fmt.Fprintf(&sb, "- %s:%d\n", caller.File, caller.Line)
		}
	}
	return sb.String()
}

// sharedGraphBlastAugment queries the project-level shared HNSW tier and appends
// relevant cross-workspace docs to the BLAST_RADIUS output. [287.E]
// Called only when workspace index coverage is below the auto-fallback threshold.
func (t *RadarTool) sharedGraphBlastAugment(ctx context.Context, target string) string {
	if t.sharedGraph == nil || t.embedder == nil {
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
	fmt.Fprintf(&sb, "\n### Shared tier hits (project memory, shared_hits: %d)\n", len(hits))
	for _, m := range hits {
		fmt.Fprintf(&sb, "- **%s** `%s`\n", m.Path, m.WorkspaceID)
	}
	return sb.String()
}

// resolveContracts runs the multi-source contract analysis and returns contracts
// plus a human-readable coverage string. Results are not cached (fast enough
// for advisory use). [Épica 256.A/C]
func (t *RadarTool) resolveContracts() ([]cpg.ContractNode, string) {
	openapi, _ := cpg.ParseOpenAPIContracts(t.workspace)
	parsed, _ := cpg.ExtractGoRoutes(t.workspace)
	if len(openapi) == 0 && len(parsed) == 0 {
		return nil, "none"
	}
	merged := cpg.MergeContracts(openapi, parsed)
	linked := cpg.LinkTSCallers(t.workspace, merged)
	mapped := 0
	for _, c := range linked {
		if len(c.FrontendCallers) > 0 {
			mapped++
		}
	}
	coverage := fmt.Sprintf("%d/%d routes mapped", mapped, len(linked))
	return linked, coverage
}

// dependentLine is a grep fallback hit with the exact line that references
// the target's package — useful when the RAG graph is stale and the agent
// needs to jump directly to the import site without rescanning the file.
type dependentLine struct {
	File       string
	Line       int
	CallerType string // "mod_decl" for Rust mod declarations (Épica 251.C); "" otherwise
}

// [SRE-99.B] grepDependentsWithLines scans imports across the workspace and
// returns the file + line number for each reference. Line number comes from
// go/ast import parsing when possible, falling back to substring match.
// Used as grep fallback when RAG graph and AST contract both return nothing.
// Not a hot path — O(files) I/O, acceptable for advisory BLAST_RADIUS use.
func grepDependentsWithLines(workspace, target string) []dependentLine {
	pkgDir := filepath.ToSlash(filepath.Dir(target))
	if pkgDir == "." {
		pkgDir = filepath.ToSlash(target)
	}
	targetRel := filepath.ToSlash(target)
	var results []dependentLine

	_ = filepath.Walk(workspace, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(workspace, path)
		rel = filepath.ToSlash(rel)
		if rel == targetRel ||
			strings.HasPrefix(rel, ".neo/") ||
			strings.Contains(rel, "/vendor/") ||
			strings.Contains(rel, "node_modules/") {
			return nil
		}

		data, rErr := os.ReadFile(path) //nolint:gosec // G304-DIR-WALK
		if rErr != nil {
			return nil
		}
		content := string(data)
		if !strings.Contains(content, pkgDir) {
			return nil
		}

		// Try to pinpoint the import line via go/ast. On any parse error, fall
		// back to substring scan across lines.
		line := findImportLine(path, data, pkgDir)
		if line == 0 {
			line = findSubstringLine(content, pkgDir)
		}
		results = append(results, dependentLine{File: rel, Line: line})
		return nil
	})
	return results
}

// findImportLine parses the file with go/ast and returns the line of the
// import spec whose path contains pkgDir, or 0 on parse failure.
func findImportLine(path string, data []byte, pkgDir string) int {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, data, parser.ImportsOnly)
	if err != nil {
		return 0
	}
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		if strings.Contains(p, pkgDir) {
			return fset.Position(imp.Pos()).Line
		}
	}
	return 0
}

// findSubstringLine returns the 1-based line number of the first occurrence
// of needle, or 0 if not found.
func findSubstringLine(content, needle string) int {
	before, _, ok := strings.Cut(content, needle)
	if !ok {
		return 0
	}
	return strings.Count(before, "\n") + 1
}

// [SRE-99.A] backgroundIndexFile reads, chunks, embeds and inserts the target
// file into the RAG graph in a goroutine-safe manner. Called when BLAST_RADIUS
// encounters not_indexed so the next call has graph coverage without forcing
// the user to trigger a full re-ingestion. Best-effort — failures are logged.
