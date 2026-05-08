package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
	"github.com/ensamblatec/neoanvil/pkg/federation"
	"github.com/ensamblatec/neoanvil/pkg/rag"
	"github.com/ensamblatec/neoanvil/pkg/telemetry"
)

func buildPkgCoupling(g *cpg.Graph) map[string]int {
	out := make(map[string]int)
	for _, e := range g.Edges {
		if e.Kind != cpg.EdgeCall || int(e.From) >= len(g.Nodes) || int(e.To) >= len(g.Nodes) {
			continue
		}
		if from, to := g.Nodes[e.From].Package, g.Nodes[e.To].Package; from != to {
			out[from+"→"+to]++
		}
	}
	return out
}

// filterPkgPairs applies the minCalls + filterPackage predicate and
// sorts by call volume descending. [Épica 229.6 refactor]
func filterPkgPairs(coupling map[string]int, minCalls int, filterPackage string) []pkgPair {
	var pairs []pkgPair
	for p, c := range coupling {
		if c < minCalls {
			continue
		}
		if filterPackage != "" && !strings.Contains(p, filterPackage) {
			continue
		}
		pairs = append(pairs, pkgPair{p, c})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].count > pairs[j].count })
	// Cap only when operator didn't explicitly filter — once they ask for
	// a specific package or a min threshold, returning just 5 hides signal.
	if minCalls == 0 && filterPackage == "" && len(pairs) > 5 {
		pairs = pairs[:5]
	}
	return pairs
}

// appendCPGPackageCoupling writes the per-package call-edge summary.
// [Épica 229.6] minCalls drops any edge with fewer than N calls;
// filterPackage restricts to edges whose "from→to" string contains the
// substring (empty = no filter). Default behaviour (minCalls=0, filter="")
// reproduces the historical "top-5 by volume".
func appendCPGPackageCoupling(g *cpg.Graph, sb *strings.Builder, minCalls int, filterPackage string) {
	pairs := filterPkgPairs(buildPkgCoupling(g), minCalls, filterPackage)
	if len(pairs) == 0 {
		return
	}
	header := "### Package Coupling (top cross-package calls)"
	if minCalls > 0 || filterPackage != "" {
		header = fmt.Sprintf("### Package Coupling (min_calls=%d filter=%q)", minCalls, filterPackage)
	}
	sb.WriteString(header + "\n")
	for _, pp := range pairs {
		fmt.Fprintf(sb, "- `%s` — %d calls\n", pp.pair, pp.count)
	}
	sb.WriteString("\n")
}

func appendCPGDigest(t *RadarTool, sb *strings.Builder, limit, minCalls int, filterPackage string) {
	if t.cpgManager == nil {
		sb.WriteString("_CPG Manager not configured — enable cpg in neo.yaml._\n")
		return
	}
	g, gerr := t.cpgManager.Graph(200 * time.Millisecond)
	if gerr != nil {
		fmt.Fprintf(sb, "_CPG not ready: %v_\n", gerr)
		return
	}
	ranks := cpg.CachedComputePageRank(g, 0.85, 50)
	topLocal := g.TopN(limit, ranks, "github.com/ensamblatec/neoanvil")
	sb.WriteString("### Top CodeRank (ensamblatec)\n")
	for i, rn := range topLocal {
		fmt.Fprintf(sb, "%3d. %-40s score=%.6f  %s:%d\n", i+1, rn.Name, rn.Score, filepath.Base(rn.File), rn.Line)
	}
	sb.WriteString("\n")
	appendCPGPackageCoupling(g, sb, minCalls, filterPackage)
	coverage := rag.IndexCoverage(t.graph, t.workspace) * 100
	fmt.Fprintf(sb, "### HNSW Coverage: %.0f%%\n", coverage)
	fmt.Fprintf(sb, "**cpg_nodes:** %d  **cpg_edges:** %d\n", len(g.Nodes), len(g.Edges))
}

func (t *RadarTool) handleProjectDigest(_ context.Context, args map[string]any) (any, error) {
	limit := 10
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	// [Épica 229.6] Coupling filters.
	minCalls := 0
	if v, ok := args["min_calls"].(float64); ok && v > 0 {
		minCalls = int(v)
	}
	filterPackage, _ := args["filter_package"].(string)

	// [Épica 181] PROJECT_DIGEST is dominated by appendCPGDigest → runs
	// full PageRank over the CPG (millisecond class on ~2k-node graphs).
	// Cache keyed by (limit, minCalls, filterPackage) so distinct filter
	// views don't collide; the legacy default path (0,"") still hashes to
	// the historical slot so repeated plain calls still hit the cache.
	var digestKey rag.TextCacheKey
	bypassCache, _ := args["bypass_cache"].(bool)
	cacheTarget := filterPackage
	cacheVariant := limit<<16 | minCalls
	if t.textCache != nil {
		digestKey = rag.NewTextCacheKey("PROJECT_DIGEST", cacheTarget, cacheVariant)
		if !bypassCache {
			if cached, ok := t.textCache.Get(digestKey, t.graph.Gen.Load()); ok {
				t.lastDigestTime = time.Now()
				return mcpText(cached), nil
			}
		}
	}

	var sb strings.Builder
	sb.WriteString("## PROJECT_DIGEST\n\n")
	digestLocalHotspots(&sb)
	appendCPGDigest(t, &sb, limit, minCalls, filterPackage)

	// [272.B/C] When in project federation mode, produce federated digest.
	if text, ok := digestFederatedSections(t, &sb, limit, cacheTarget, cacheVariant); ok {
		return mcpText(text), nil
	}

	t.lastDigestTime = time.Now()
	body := sb.String()
	if t.textCache != nil {
		t.textCache.PutAnnotated(digestKey, body, t.graph.Gen.Load(), "PROJECT_DIGEST", cacheTarget, cacheVariant)
	}
	return mcpText(body), nil
}

// digestLocalHotspots appends the Tech-Debt Hotspots section to sb.
func digestLocalHotspots(sb *strings.Builder) {
	if hotspots, err := telemetry.GetTopHotspots(5); err == nil {
		sb.WriteString("### Tech-Debt Hotspots\n")
		for i, hs := range hotspots {
			fmt.Fprintf(sb, "%d. %s — %d mutations\n", i+1, hs.File, hs.Mutations)
		}
		sb.WriteString("\n")
	}
}

// digestFederatedSections handles the project-federation path of PROJECT_DIGEST.
// Returns (text, true) when federation mode is active and scatter results are available.
func digestFederatedSections(t *RadarTool, sb *strings.Builder, limit int, cacheTarget string, cacheVariant int) (string, bool) {
	if t.cfg.Project == nil || len(t.cfg.Project.MemberWorkspaces) == 0 {
		return "", false
	}
	projectName := t.cfg.Project.ProjectName
	scatter := t.scatterToMembers(context.Background(), "PROJECT_DIGEST", map[string]any{"limit": limit})
	if len(scatter) == 0 {
		return "", false
	}
	// Re-write header with project name.
	body := sb.String()
	sb.Reset()
	fmt.Fprintf(sb, "## Project: %s — Federated Digest\n\n", projectName)
	sb.WriteString(body)
	sb.WriteString("\n---\n")
	digestFederatedWorkspaces(sb, scatter)
	digestContractHealth(t, sb)
	// [272.C] Project-scoped cache key includes project name.
	t.lastDigestTime = time.Now()
	result := sb.String()
	if t.textCache != nil {
		projectKey := rag.NewTextCacheKey("project_digest:"+projectName, cacheTarget, cacheVariant)
		t.textCache.PutAnnotated(projectKey, result, t.graph.Gen.Load(), "PROJECT_DIGEST", projectName, cacheVariant)
	}
	return result, true
}

// digestFederatedWorkspaces writes per-workspace sections and aggregates cross-workspace hotspots.
func digestFederatedWorkspaces(sb *strings.Builder, scatter []scatterMember) {
	hotspotAgg := map[string]uint64{}
	for _, r := range scatter {
		if r.err != nil || r.text == "" {
			fmt.Fprintf(sb, "#### %s\n> ⚠️ %v\n\n", r.name, r.err)
			continue
		}
		fmt.Fprintf(sb, "#### %s\n%s\n\n", r.name, r.text)
		// Extract hotspot lines: "N. <file> — <M> mutations"
		for line := range strings.SplitSeq(r.text, "\n") {
			parts := strings.SplitN(strings.TrimSpace(line), ". ", 2)
			if len(parts) != 2 {
				continue
			}
			fileAndMut := strings.SplitN(parts[1], " — ", 2)
			if len(fileAndMut) != 2 {
				continue
			}
			var count uint64
			if _, scanErr := fmt.Sscanf(strings.Fields(fileAndMut[1])[0], "%d", &count); scanErr == nil {
				hotspotAgg[fileAndMut[0]] += count
			}
		}
	}
	digestCrossHotspots(sb, hotspotAgg)
}

// digestCrossHotspots writes the cross-workspace hotspot ranking section.
func digestCrossHotspots(sb *strings.Builder, hotspotAgg map[string]uint64) {
	if len(hotspotAgg) == 0 {
		return
	}
	type hs struct {
		file  string
		total uint64
	}
	var ranked []hs
	for f, c := range hotspotAgg {
		ranked = append(ranked, hs{f, c})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].total > ranked[j].total })
	if len(ranked) > 10 {
		ranked = ranked[:10]
	}
	sb.WriteString("### Cross-Workspace Hotspots (top-10 by total mutations)\n")
	for i, h := range ranked {
		fmt.Fprintf(sb, "%d. %s — %d mutations total\n", i+1, h.file, h.total)
	}
	sb.WriteString("\n")
}

// digestContractHealth appends the Contract Health section when SHARED_DEBT.md exists. [316.C]
func digestContractHealth(t *RadarTool, sb *strings.Builder) {
	projDir, ok := federation.FindNeoProjectDir(t.workspace)
	if !ok {
		return
	}
	entries, err := federation.ParseSharedDebt(projDir)
	if err != nil || len(entries) == 0 {
		return
	}
	pending := 0
	for _, e := range entries {
		if strings.Contains(e.Status, "pending") {
			pending++
		}
	}
	sb.WriteString("## Contract Health\n\n")
	fmt.Fprintf(sb, "- Shared debt file: `.neo-project/SHARED_DEBT.md` (%d pending contract(s))\n", pending)
	sb.WriteString("- Run `neo_radar(intent:\"CONTRACT_GAP\")` for detailed gap analysis.\n\n")
}

