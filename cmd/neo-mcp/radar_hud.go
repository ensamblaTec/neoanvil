package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
	"github.com/ensamblatec/neoanvil/pkg/incidents"
	"github.com/ensamblatec/neoanvil/pkg/kanban"
	"github.com/ensamblatec/neoanvil/pkg/rag"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// connection pool per HUD poll. Audit after the neo-nexus fix found
// these four additional call sites in cmd/neo-mcp/.
var (
	hudStateClient       = sre.SafeInternalHTTPClient(5)
	frontendErrorsClient = sre.SafeHTTPClient()
)

func (t *RadarTool) handleHUDState(_ context.Context, _ map[string]any) (any, error) {
	// Self-call to own worker HTTP — plain client, SSRF shield not needed for loopback self.
	client := hudStateClient
	workerURL := fmt.Sprintf("http://%s:%d", t.cfg.Server.Host, t.cfg.Server.SSEPort)
	resp, err := client.Get(workerURL + "/api/v1/sre/state")
	if err != nil {
		return nil, fmt.Errorf("HUD unreachable: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var hudState map[string]any
	if jsonErr := json.Unmarshal(body, &hudState); jsonErr != nil {
		return nil, fmt.Errorf("HUD state decode error: %v", jsonErr)
	}
	nodes, _ := hudState["mcts_nodes"].(float64)
	color, _ := hudState["color"].(string)
	ram, _ := hudState["ram_mb"].(float64)

	// [Épica 177/180/204/225] RAG cache + search path metrics.
	cacheAndPathStr := collectHUDCacheMetrics(t)
	// [Épica 257.C] Contract coverage summary.
	contractStr := collectHUDContractMetrics(t.workspace)
	// [Épica 178/188] Per-tool latency percentiles.
	latencyStr := collectHUDLatencyMetrics()

	return mcpText(fmt.Sprintf(
		"HUD: %.0f MCTS nodes | Color: %s | RAM: %.0f MB | incidents_indexed_count: %d%s%s%s",
		nodes, color, ram, incidents.IndexedCount(), cacheAndPathStr, contractStr, latencyStr)), nil
}

func collectHUDCacheMetrics(t *RadarTool) string {
	var s string
	if t.queryCache != nil {
		h, m, e, sz := t.queryCache.Stats()
		total := h + m
		ratio := float64(0)
		if total > 0 {
			ratio = float64(h) * 100.0 / float64(total)
		}
		s = fmt.Sprintf(" | Qcache: %.0f%% (%dH/%dM evict=%d sz=%d)", ratio, h, m, e, sz)
	}
	if t.textCache != nil {
		h, m, e, sz := t.textCache.Stats()
		total := h + m
		ratio := float64(0)
		if total > 0 {
			ratio = float64(h) * 100.0 / float64(total)
		}
		s += fmt.Sprintf(" | Tcache: %.0f%% (%dH/%dM evict=%d sz=%d)", ratio, h, m, e, sz)
	}
	if t.embCache != nil {
		h, m, e, sz := t.embCache.Stats()
		total := h + m
		ratio := float64(0)
		if total > 0 {
			ratio = float64(h) * 100.0 / float64(total)
		}
		s += fmt.Sprintf(" | Ecache: %.0f%% (%dH/%dM evict=%d sz=%d)", ratio, h, m, e, sz)
	}
	s += fmt.Sprintf(" | Search paths: bin=%d hybrid=%d int8=%d",
		rag.SearchBinaryCount(), rag.HybridSearchCount(), rag.SearchInt8Count())
	prHits, prMisses := cpg.PageRankCacheStats()
	if prHits+prMisses > 0 {
		prRatio := float64(prHits) * 100.0 / float64(prHits+prMisses)
		s += fmt.Sprintf(" | PRcache: %.0f%% (%dH/%dM)", prRatio, prHits, prMisses)
	}
	return s
}

func collectHUDLatencyMetrics() string {
	rows := collectToolRows() // persisted Store first, in-memory ring fallback
	if len(rows) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\nTool Latency (p50/p95/p99, persisted across restarts):")
	for _, r := range rows {
		fmt.Fprintf(&sb, "\n  %-34s p50=%-8s p95=%-8s p99=%-8s (n=%d total=%d errs=%d)",
			r.Name,
			r.P50.Truncate(time.Microsecond),
			r.P95.Truncate(time.Microsecond),
			r.P99.Truncate(time.Microsecond),
			r.Window, r.Lifetime, r.Errors)
	}
	return sb.String()
}

func collectHUDContractMetrics(workspace string) string {
	openapi, _ := cpg.ParseOpenAPIContracts(workspace)
	parsed, _ := cpg.ExtractGoRoutes(workspace)
	if len(openapi)+len(parsed) == 0 {
		return ""
	}
	merged := cpg.MergeContracts(openapi, parsed)
	linked := cpg.LinkTSCallers(workspace, merged)
	mapped := 0
	for _, c := range linked {
		if len(c.FrontendCallers) > 0 {
			mapped++
		}
	}
	return fmt.Sprintf(" | contract_routes: %d mapped, %d unmapped", mapped, len(linked)-mapped)
}

func (t *RadarTool) handleFrontendErrors(_ context.Context, _ map[string]any) (any, error) {
	client := frontendErrorsClient
	resp, err := client.Get(t.cfg.Integrations.SandboxBaseURL + "/api/v1/sre/frontend_errors")
	if err != nil {
		return mcpText(fmt.Sprintf("[SRE-WARN] Sandbox unreachable: %v", err)), nil
	}
	defer resp.Body.Close()
	errBytes, _ := io.ReadAll(resp.Body)
	var logs []map[string]any
	if jsonErr := json.Unmarshal(errBytes, &logs); jsonErr == nil && len(logs) > 0 {
		var sb strings.Builder
		for i, lg := range logs {
			fmt.Fprintf(&sb, "Error %d: %v — %v\n", i, lg["type"], lg["message"])
		}
		return mcpText(sb.String()), nil
	}
	return mcpText("[OK] No frontend errors captured."), nil
}

func (t *RadarTool) handleWiringAudit(_ context.Context, args map[string]any) (any, error) {
	target, _ := args["target"].(string)
	if target == "" {
		// Default: scan main.go relative to workspace.
		target = filepath.Join(t.workspace, "cmd", "neo-mcp", "main.go")
	} else if !filepath.IsAbs(target) {
		target = filepath.Join(t.workspace, target)
	}

	src, err := os.ReadFile(target) //nolint:gosec // G304-WORKSPACE-CANON
	if err != nil {
		return nil, fmt.Errorf("WIRING_AUDIT: cannot read %s: %w", target, err)
	}

	imported, used := extractWiringInfo(src)

	var sb strings.Builder
	fmt.Fprintf(&sb, "## WIRING_AUDIT: `%s`\n\n", filepath.Base(target))

	var orphans []string
	for pkg, alias := range imported {
		if !used[alias] && !used[pkg] {
			orphans = append(orphans, pkg)
		}
	}

	if len(orphans) == 0 {
		sb.WriteString("✅ All imported packages have at least one instantiation call.\n")
	} else {
		fmt.Fprintf(&sb, "⚠️  **%d package(s) imported but never instantiated:**\n\n", len(orphans))
		for _, orphanPkg := range orphans {
			fmt.Fprintf(&sb, "- `%s`\n", orphanPkg)
		}
		sb.WriteString("\n> Run `neo_radar BLAST_RADIUS` on each to assess removal safety.\n")
		// [SRE-TECH-DEBT] Orphan packages auto-detected as tech debt
		_ = kanban.AppendTechDebt(t.workspace,
			fmt.Sprintf("Wiring: %d orphan package(s) in %s", len(orphans), filepath.Base(target)),
			fmt.Sprintf("Packages imported but never instantiated in %s:\n- %s", target, strings.Join(orphans, "\n- ")), "media")
	}

	fmt.Fprintf(&sb, "\nScanned %d imports, %d unique call prefixes detected.\n", len(imported), len(used))

	// [277.B] API route coverage: compare TS fetch patterns against backend contracts.
	openapi, _ := cpg.ParseOpenAPIContracts(t.workspace)
	goRoutes, _ := cpg.ExtractGoRoutes(t.workspace)
	if len(openapi)+len(goRoutes) > 0 {
		merged := cpg.MergeContracts(openapi, goRoutes)
		linked := cpg.LinkTSCallers(t.workspace, merged)
		uncalled := 0
		for _, c := range linked {
			if len(c.FrontendCallers) == 0 {
				uncalled++
			}
		}
		sb.WriteString("\n### API Route Coverage\n\n")
		fmt.Fprintf(&sb, "| Route | Method | TS Callers |\n|-------|--------|------------|\n")
		for _, c := range linked {
			callerStr := fmt.Sprintf("%d", len(c.FrontendCallers))
			if len(c.FrontendCallers) == 0 {
				callerStr = "⚠️ 0"
			}
			fmt.Fprintf(&sb, "| `%s` | %s | %s |\n", c.Path, c.Method, callerStr)
		}
		fmt.Fprintf(&sb, "\n%d routes total, %d with TS callers, %d with no frontend callers.\n",
			len(linked), len(linked)-uncalled, uncalled)
	}

	sb.WriteString(t.formatScatterSection("WIRING_AUDIT", nil))
	return mcpText(sb.String()), nil
}

func extractWiringInfo(src []byte) (imported map[string]string, used map[string]bool) {
	imported = make(map[string]string)
	used = make(map[string]bool)

	// Use line-by-line parsing for import blocks (fast, no full AST needed for imports).
	inImport := false
	for line := range strings.SplitSeq(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "import (" {
			inImport = true
			continue
		}
		if inImport {
			if trimmed == ")" {
				inImport = false
				continue
			}
			// Parse: [alias] "path/to/pkg"
			trimmed = strings.Trim(trimmed, `"`)
			parts := strings.Fields(trimmed)
			var alias, path string
			switch len(parts) {
			case 1: // "path/to/pkg" — strip quotes
				path = strings.Trim(parts[0], `"`)
				// alias is last path component
				alias = filepath.Base(path)
			case 2: // alias "path/to/pkg"
				alias = parts[0]
				path = strings.Trim(parts[1], `"`)
			default:
				continue
			}
			if alias == "_" || alias == "." || path == "" {
				continue
			}
			imported[path] = alias
		}
	}

	// Scan for pkg.XxxYyy call patterns.
	for line := range strings.SplitSeq(string(src), "\n") {
		for _, alias := range imported {
			if strings.Contains(line, alias+".") {
				used[alias] = true
			}
		}
	}
	return imported, used
}
