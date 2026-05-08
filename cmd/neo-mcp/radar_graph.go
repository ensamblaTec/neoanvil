package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
	"github.com/ensamblatec/neoanvil/pkg/rag"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

func graphWalkVariant(maxDepth int, edgeKindStr string) int {
	return (maxDepth << 16) | int(fnvStr8(edgeKindStr))
}

func (t *RadarTool) graphWalkCacheLookup(startSym string, maxDepth int, edgeKindStr string, bypassCache bool) (rag.TextCacheKey, string, bool) {
	if t.textCache == nil {
		return rag.TextCacheKey{}, "", false
	}
	key := rag.NewTextCacheKey("GRAPH_WALK", startSym, graphWalkVariant(maxDepth, edgeKindStr))
	if bypassCache {
		return key, "", false
	}
	if cached, ok := t.textCache.Get(key, t.graph.Gen.Load()); ok {
		return key, cached, true
	}
	return key, "", false
}

func graphWalkEdgeKinds(edgeKindStr string) []cpg.EdgeKind {
	switch edgeKindStr {
	case "call":
		return []cpg.EdgeKind{cpg.EdgeCall}
	case "cfg":
		return []cpg.EdgeKind{cpg.EdgeCFG}
	case "contain":
		return []cpg.EdgeKind{cpg.EdgeContain}
	}
	return nil
}

func symbolInCPG(g *cpg.Graph, name string) (found bool, file string) {
	for _, n := range g.Nodes {
		if n.Name == name {
			return true, n.File
		}
	}
	return false, ""
}

func formatGraphWalkBody(g *cpg.Graph, startSym string, maxDepth int, edgeKindStr string, nodes []cpg.Node) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "## GRAPH_WALK: `%s` (depth=%d, kind=%s)\n\n", startSym, maxDepth, edgeKindStr)
	if len(nodes) == 0 {
		found, file := symbolInCPG(g, startSym)
		if found {
			sb.WriteString("_No outgoing edges from this symbol in the CPG._\n")
			sb.WriteString("_This is a known SSA limitation for receiver methods and functions that only call stdlib — the symbol exists and is indexed, but the SSA pass didn't emit call edges for its body._\n\n")
			if file != "" {
				fmt.Fprintf(&sb, "**Hint:** use `BLAST_RADIUS target=%s` to see callers (reverse direction), or `READ_SLICE` the body to inspect callees manually.\n", file)
			}
		} else {
			sb.WriteString("_No reachable nodes — symbol not present in the CPG._\n")
			sb.WriteString("_Check the exact name (CPG uses simple names, not `Type.Method`). If the symbol exists on disk but isn't indexed, certify the hosting file to trigger a CPG rebuild._\n")
		}
		return sb.String()
	}
	fmt.Fprintf(&sb, "**reachable_count:** %d\n\n", len(nodes))
	for i, n := range nodes {
		fmt.Fprintf(&sb, "%3d. `%-35s` pkg=%-25s %s:%d\n", i+1, n.Name, n.Package, filepath.Base(n.File), n.Line)
	}
	return sb.String()
}

func (t *RadarTool) handleGraphWalk(ctx context.Context, args map[string]any) (any, error) {
	startSym, _ := args["target"].(string)
	if startSym == "" {
		return nil, fmt.Errorf("target (symbol name) is required for GRAPH_WALK")
	}
	maxDepth := 2
	if d, ok := args["max_depth"].(float64); ok && d > 0 {
		maxDepth = int(d)
	}
	edgeKindStr, _ := args["edge_kind"].(string)
	bypassCache, _ := args["bypass_cache"].(bool)

	walkKey, cached, hit := t.graphWalkCacheLookup(startSym, maxDepth, edgeKindStr, bypassCache)
	if hit {
		return mcpText(cached), nil
	}

	if t.cpgManager == nil {
		return mcpText("CPG Manager not available — GRAPH_WALK requires CPG to be enabled."), nil
	}
	g, err := t.cpgManager.Graph(200 * time.Millisecond)
	if err != nil {
		return mcpText(fmt.Sprintf("CPG not ready yet: %v", err)), nil
	}

	q := cpg.TraversalQuery{StartSymbol: startSym, EdgeKinds: graphWalkEdgeKinds(edgeKindStr), MaxDepth: maxDepth}
	nodes := g.Walk(q)
	body := formatGraphWalkBody(g, startSym, maxDepth, edgeKindStr, nodes)

	// [347.A] Cross-workspace scatter: follow EdgeContract http_boundary nodes.
	if xSection := t.crossWalkHTTPBoundary(ctx, nodes, maxDepth, edgeKindStr); xSection != "" {
		body += xSection
	}

	if t.textCache != nil {
		t.textCache.PutAnnotated(walkKey, body, t.graph.Gen.Load(), "GRAPH_WALK", startSym, graphWalkVariant(maxDepth, edgeKindStr))
	}
	return mcpText(body), nil
}

// crossWalkHTTPBoundary detects NodeContract frontend nodes in the BFS result and
// continues the walk in the owning remote workspace via Nexus. [347.A]
func (t *RadarTool) crossWalkHTTPBoundary(ctx context.Context, nodes []cpg.Node, maxDepth int, edgeKindStr string) string {
	if t.registry == nil || t.cfg.Server.NexusDispatcherPort == 0 {
		return ""
	}
	// Collect unique remote workspaces from NodeContract/frontend nodes.
	type remoteTarget struct {
		wsID  string
		sym   string
	}
	seen := map[string]struct{}{}
	var targets []remoteTarget
	absWs, _ := filepath.Abs(t.workspace)

	for _, n := range nodes {
		if n.Kind != cpg.NodeContract || n.Package != "frontend" || n.File == "" {
			continue
		}
		// Resolve the file to absolute. Might be relative to t.workspace.
		absFile := n.File
		if !filepath.IsAbs(n.File) {
			absFile = filepath.Join(absWs, n.File)
		}
		// Skip if already inside this workspace.
		if strings.HasPrefix(absFile, absWs+string(filepath.Separator)) {
			continue
		}
		wsID, _ := workspaceOfFile(absFile, t.registry)
		if wsID == "" {
			continue
		}
		sym := strings.TrimSuffix(filepath.Base(n.File), filepath.Ext(n.File))
		key := wsID + ":" + sym
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		targets = append(targets, remoteTarget{wsID: wsID, sym: sym})
	}

	if len(targets) == 0 {
		return ""
	}

	type scatterResult struct {
		wsID string
		body string
	}
	results := make([]scatterResult, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, tgt := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, rt remoteTarget) {
			defer func() { <-sem; wg.Done() }()
			body, err := t.forwardGraphWalkToNexus(ctx, rt.wsID, rt.sym, maxDepth, edgeKindStr)
			if err != nil {
				body = fmt.Sprintf("❌ %s: %v", rt.wsID, err)
			}
			results[idx] = scatterResult{wsID: rt.wsID, body: body}
		}(i, tgt)
	}
	wg.Wait()

	var sb strings.Builder
	sb.WriteString("\n\n---\n\n### 🔀 HTTP Boundary — Cross-Workspace Continuation\n\n")
	sb.WriteString("_Following EdgeContract nodes into remote workspace(s). Results are approximate — BFS starts from the TS file stem._\n\n")
	for _, r := range results {
		fmt.Fprintf(&sb, "#### Workspace: `%s`\n\n%s\n\n", r.wsID, r.body)
	}
	return sb.String()
}

// forwardGraphWalkToNexus POSTs a GRAPH_WALK scatter request to Nexus for a specific child. [347.A]
func (t *RadarTool) forwardGraphWalkToNexus(ctx context.Context, wsID, startSym string, maxDepth int, edgeKindStr string) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"start_sym":  startSym,
		"max_depth":  maxDepth,
		"edge_kind":  edgeKindStr,
	})
	url := fmt.Sprintf("http://127.0.0.1:%d/internal/graph_walk/%s", t.cfg.Server.NexusDispatcherPort, wsID) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: SafeInternalHTTPClient allows only loopback; port is from NexusDispatcherPort config
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build graph_walk forward request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := sre.SafeInternalHTTPClient(10)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("nexus graph_walk %s: %w", wsID, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("nexus graph_walk %s: HTTP %d: %s", wsID, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, _ := io.ReadAll(resp.Body)
	var nr struct {
		WorkspaceID string `json:"workspace_id"`
		Body        string `json:"body"`
	}
	if jsonErr := json.Unmarshal(body, &nr); jsonErr != nil {
		return string(body), nil
	}
	return nr.Body, nil
}
