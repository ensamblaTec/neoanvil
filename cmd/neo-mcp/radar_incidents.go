package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/ensamblatec/neoanvil/pkg/federation"
	"github.com/ensamblatec/neoanvil/pkg/incidents"
)

// reIncidentIDExtract matches incident IDs in Markdown INCIDENT_SEARCH responses. [340]
var reIncidentIDExtract = regexp.MustCompile(`INC-\d{8}-\d{6}`)

func fnvStr8(s string) uint8 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return uint8(h)
}

// retrieveIncidents dispatches the (forced or cascading) tier lookup.
// Returns the raw result set and the tier label that produced it.
// [Épica 229.5]
func (t *RadarTool) retrieveIncidents(ctx context.Context, query, forceTier string, limit int) ([]incidents.IncidentMeta, string) {
	switch forceTier {
	case "text":
		return incidentTextSearch(t.workspace, query, limit*3), "text_search"
	case "hnsw":
		semMetas, err := incidents.SearchIncidents(ctx, query, limit*3, t.embedder, t.graph, t.wal, t.cpu)
		if err != nil {
			return nil, "hnsw"
		}
		return semMetas, "hnsw"
	case "bm25":
		if t.incLexIdx == nil {
			return nil, "bm25"
		}
		return incidents.SearchIncidentsBM25(query, t.incLexIdx, t.workspace, limit*3), "bm25"
	}
	// Default cascade: BM25 → HNSW.
	if t.incLexIdx != nil {
		if metas := incidents.SearchIncidentsBM25(query, t.incLexIdx, t.workspace, limit*3); len(metas) > 0 {
			return metas, "bm25"
		}
	}
	semMetas, err := incidents.SearchIncidents(ctx, query, limit*3, t.embedder, t.graph, t.wal, t.cpu)
	if err != nil {
		return nil, "hnsw"
	}
	return semMetas, "hnsw"
}

// handleIncidentSearch performs semantic search over the indexed .neo/incidents/ corpus. [PILAR-XXI/150.D]
// [Épica 229.5] Accepts optional force_tier arg ("bm25" | "hnsw" | "text")
// so operators can exercise a specific retrieval path. Default cascade is
// unchanged: BM25 → HNSW → text_search.
func (t *RadarTool) handleIncidentSearch(ctx context.Context, args map[string]any) (any, error) {
	query, _ := args["target"].(string)
	if query == "" {
		return nil, fmt.Errorf("target (search query) is required for INCIDENT_SEARCH")
	}
	limit := 5
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	forceTier, _ := args["force_tier"].(string)

	// scope:project — RRF scatter across all member workspaces. [340]
	if tw, _ := args["target_workspace"].(string); tw == "project" {
		return t.handleIncidentSearchProject(ctx, query, limit)
	}

	metas, pathTier := t.retrieveIncidents(ctx, query, forceTier, limit)
	if len(metas) > limit {
		metas = metas[:limit]
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "## INCIDENT_SEARCH: %q _(tier: %s)_\n\n", query, pathTier)
	if len(metas) == 0 {
		return mcpText(incidentSearchFallback(t.workspace, query, limit, &sb)), nil
	}
	for i, m := range metas {
		appendIncidentEntry(&sb, i, m)
		if i == 0 {
			appendIncidentCPGEnrichment(t, &sb, m)
		}
	}
	// [273.B/273.C] Cross-workspace scatter for INCIDENT_SEARCH.
	if t.cfg.Project != nil && len(t.cfg.Project.MemberWorkspaces) > 0 {
		appendIncidentCrossWorkspace(ctx, t, &sb, query)
	}
	return mcpText(sb.String()), nil
}

// incidentRRFEntry is a merged incident document across local + member workspace RRF fusion. [340]
type incidentRRFEntry struct {
	id       string
	wsOrigin string
	anomaly  string
	severity string
	services []string
	rrfScore float64
}

// buildIncidentRRFMap merges local and scatter results using Reciprocal Rank Fusion (k=60).
func buildIncidentRRFMap(localMetas []incidents.IncidentMeta, scatter []scatterMember, wsName string) map[string]*incidentRRFEntry {
	const rrfK = 60
	byID := make(map[string]*incidentRRFEntry)
	for rank, m := range localMetas {
		e, ok := byID[m.ID]
		if !ok {
			e = &incidentRRFEntry{
				id: m.ID, wsOrigin: wsName,
				anomaly: m.Anomaly, severity: m.Severity, services: m.AffectedServices,
			}
			byID[m.ID] = e
		}
		e.rrfScore += 1.0 / float64(rrfK+rank+1)
	}
	for _, mem := range scatter {
		if mem.err != nil || mem.text == "" {
			continue
		}
		ids := reIncidentIDExtract.FindAllString(mem.text, -1)
		seen := make(map[string]bool, len(ids))
		rank := 0
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			e, ok := byID[id]
			if !ok {
				e = &incidentRRFEntry{id: id, wsOrigin: mem.name}
				byID[id] = e
			} else if e.wsOrigin != mem.name && e.wsOrigin != "cross-ws" {
				e.wsOrigin = "cross-ws"
			}
			e.rrfScore += 1.0 / float64(rrfK+rank+1)
			rank++
		}
	}
	return byID
}

// renderIncidentRRFTable appends markdown table rows for each RRF-ranked entry.
func renderIncidentRRFTable(sb *strings.Builder, docs []*incidentRRFEntry) {
	for i, d := range docs {
		summary := d.anomaly
		if len(summary) > 60 {
			summary = summary[:60] + "…"
		}
		fmt.Fprintf(sb, "| %d | `%s` | %s | %.4f | %s |\n",
			i+1, d.id, d.wsOrigin, d.rrfScore, summary)
	}
}

// autoAppendSharedIncidents writes cross-boundary incident entries to SHARED_INCIDENTS.md. [340]
func autoAppendSharedIncidents(t *RadarTool, docs []*incidentRRFEntry, localMetas []incidents.IncidentMeta) {
	if t.cfg.Project == nil {
		return
	}
	projDir, found := federation.FindNeoProjectDir(t.workspace)
	if !found {
		return
	}
	memberNames := make(map[string]bool, len(t.cfg.Project.MemberWorkspaces))
	for _, mp := range t.cfg.Project.MemberWorkspaces {
		memberNames[filepath.Base(mp)] = true
	}
	for _, m := range localMetas {
		for _, svc := range m.AffectedServices {
			if memberNames[svc] {
				_ = federation.AppendSharedIncident(projDir, m.ID, m.Severity,
					filepath.Base(t.workspace), m.Anomaly, m.AffectedServices)
				break
			}
		}
	}
	for _, d := range docs {
		if d.wsOrigin != "" && d.wsOrigin != filepath.Base(t.workspace) {
			_ = federation.AppendSharedIncident(projDir, d.id, d.severity,
				d.wsOrigin, d.anomaly, d.services)
		}
	}
}

// handleIncidentSearchProject scatters BM25 INCIDENT_SEARCH to all member
// workspaces, applies Reciprocal Rank Fusion (k=60), and returns top-k
// unique results annotated with ws_origin. Also auto-appends cross-boundary
// incidents to .neo-project/SHARED_INCIDENTS.md. [340]
func (t *RadarTool) handleIncidentSearchProject(ctx context.Context, query string, limit int) (any, error) {
	localMetas, _ := t.retrieveIncidents(ctx, query, "bm25", limit*3)
	scatter := t.scatterToMembers(ctx, "INCIDENT_SEARCH", map[string]any{
		"target":     query,
		"force_tier": "bm25",
	}, 5)

	byID := buildIncidentRRFMap(localMetas, scatter, filepath.Base(t.workspace))
	docs := make([]*incidentRRFEntry, 0, len(byID))
	for _, e := range byID {
		docs = append(docs, e)
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].rrfScore > docs[j].rrfScore })
	if len(docs) > limit {
		docs = docs[:limit]
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "## INCIDENT_SEARCH (project scope, RRF k=60): %q\n\n", query)
	sb.WriteString("| Rank | ID | Workspace | RRF Score | Summary |\n")
	sb.WriteString("|------|----|-----------|-----------|---------|\n")
	renderIncidentRRFTable(&sb, docs)
	autoAppendSharedIncidents(t, docs, localMetas)

	return mcpText(sb.String()), nil
}

// appendIncidentCPGEnrichment adds CPG blast-radius correlation for the top incident. [152.D]
func appendIncidentCPGEnrichment(t *RadarTool, sb *strings.Builder, m incidents.IncidentMeta) {
	if t.cpgManager == nil || m.Path == "" {
		return
	}
	if content, err := os.ReadFile(m.Path); err == nil { //nolint:gosec // G304-DIR-WALK
		corrs := incidents.CorrelateWithCPG(content, t.cpgManager, 5)
		if section := incidents.FormatCPGSection(corrs); section != "" {
			sb.WriteString(section)
		}
	}
}

// appendIncidentCrossWorkspace scatters INCIDENT_SEARCH to member workspaces and appends results. [273.B/273.C]
func appendIncidentCrossWorkspace(ctx context.Context, t *RadarTool, sb *strings.Builder, query string) {
	scatter := t.scatterToMembers(ctx, "INCIDENT_SEARCH", map[string]any{"target": query})
	if len(scatter) == 0 {
		// [273.C] Nexus unavailable — emit informative note.
		sb.WriteString("\n> ℹ️ **Federation:** Nexus unavailable — cross-workspace incident search skipped. ")
		sb.WriteString("Start neo-nexus and set NEO_EXTERNAL_URL to enable.\n")
		return
	}
	var withResults []scatterMember
	for _, r := range scatter {
		if r.err == nil && r.text != "" && !strings.Contains(r.text, "No incidents found") {
			withResults = append(withResults, r)
		}
	}
	if len(withResults) == 0 {
		return
	}
	sb.WriteString("\n---\n### Cross-Workspace Matches\n\n")
	sb.WriteString("| Workspace | Result |\n")
	sb.WriteString("|-----------|--------|\n")
	for _, mem := range withResults {
		summary := "_results available_"
		for line := range strings.SplitSeq(mem.text, "\n") {
			if after, ok := strings.CutPrefix(strings.TrimSpace(line), "### "); ok {
				summary = after
				break
			}
		}
		fmt.Fprintf(sb, "| **%s** | %s |\n", mem.name, summary)
	}
	sb.WriteString("\n")
	for _, mem := range withResults {
		fmt.Fprintf(sb, "#### From: %s\n%s\n\n", mem.name, mem.text)
	}
}

// incidentSearchFallback activates text_search when HNSW returns 0 results. [158.C]
func incidentSearchFallback(workspace, query string, limit int, sb *strings.Builder) string {
	textMetas := incidentTextSearch(workspace, query, limit)
	if len(textMetas) > 0 {
		sb.Reset()
		fmt.Fprintf(sb, "## INCIDENT_SEARCH: %q [fallback: text_search]\n\n", query)
		sb.WriteString("_HNSW returned 0 results — keyword scan of .neo/incidents/_\n\n")
		for i, m := range textMetas {
			appendIncidentEntry(sb, i, m)
		}
		return sb.String()
	}
	sb.WriteString("_No incidents found. Run incident indexer or add INC-*.md files to .neo/incidents/_\n")
	return sb.String()
}

// appendIncidentEntry renders a single IncidentMeta block into sb.
func appendIncidentEntry(sb *strings.Builder, i int, m incidents.IncidentMeta) {
	ts := ""
	if !m.Timestamp.IsZero() {
		ts = m.Timestamp.Format("2006-01-02 15:04:05")
	}
	fmt.Fprintf(sb, "### %d. %s\n", i+1, m.ID)
	fmt.Fprintf(sb, "- **Severity:** %s  **Timestamp:** %s\n", m.Severity, ts)
	if m.Anomaly != "" {
		fmt.Fprintf(sb, "- **Anomaly:** %s\n", m.Anomaly)
	}
	fmt.Fprintf(sb, "- **File:** `%s`\n\n", m.Path)
}

// incidentTextSearch scans .neo/incidents/ for INC files containing any query keyword. [158.C]
func incidentTextSearch(workspace, query string, limit int) []incidents.IncidentMeta {
	incDir := filepath.Join(workspace, ".neo", "incidents")
	entries, err := os.ReadDir(incDir)
	if err != nil {
		return nil
	}
	keywords := strings.Fields(strings.ToLower(query))
	var results []incidents.IncidentMeta
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "INC-") || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		absPath := filepath.Join(incDir, e.Name())
		data, readErr := os.ReadFile(absPath) //nolint:gosec // G304-WORKSPACE-CANON
		if readErr != nil {
			continue
		}
		lower := strings.ToLower(string(data))
		for _, kw := range keywords {
			if strings.Contains(lower, kw) {
				results = append(results, incidents.ParseIncidentMeta(absPath, data))
				break
			}
		}
		if len(results) >= limit {
			break
		}
	}
	return results
}

// handlePatternAudit scans .neo/incidents/ for recurring failure patterns. [PILAR-XXI/155.C]
func (t *RadarTool) handlePatternAudit(_ context.Context, _ map[string]any) (any, error) {
	incDir := filepath.Join(t.workspace, ".neo", "incidents")
	entries, err := os.ReadDir(incDir)
	if err != nil {
		return mcpText("_No incidents directory found. Auto-triage hasn't fired yet._\n"), nil
	}
	metas := parseIncidentMetas(incDir, entries)
	patterns := incidents.ExtractRecurringPatterns(metas)
	var sb strings.Builder
	sb.WriteString(incidents.FormatPatternAudit(patterns))
	// [276.B] Cross-workspace recurring patterns.
	appendPatternAuditCrossWorkspace(t, &sb)
	return mcpText(sb.String()), nil
}

// parseIncidentMetas reads all INC-*.md files from incDir and returns their parsed metadata.
func parseIncidentMetas(incDir string, entries []os.DirEntry) []incidents.IncidentMeta {
	var metas []incidents.IncidentMeta
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "INC-") || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		p := filepath.Join(incDir, e.Name())
		content, readErr := os.ReadFile(p) //nolint:gosec // G304-DIR-WALK
		if readErr != nil {
			continue
		}
		metas = append(metas, incidents.ParseIncidentMeta(p, content))
	}
	return metas
}

// appendPatternAuditCrossWorkspace aggregates services from member workspace PATTERN_AUDIT reports. [276.B]
func appendPatternAuditCrossWorkspace(t *RadarTool, sb *strings.Builder) {
	scatter := t.scatterToMembers(context.Background(), "PATTERN_AUDIT", nil)
	if len(scatter) == 0 {
		return
	}
	sb.WriteString("\n---\n### Cross-Workspace Recurring Patterns\n\n")
	svcCount := map[string]int{}
	for _, r := range scatter {
		if r.err != nil || r.text == "" {
			continue
		}
		for line := range strings.SplitSeq(r.text, "\n") {
			// PATTERN_AUDIT output uses "**Service:** X — N incidents" format.
			if after, ok := strings.CutPrefix(strings.TrimSpace(line), "**Service:**"); ok {
				svc := strings.TrimSpace(strings.SplitN(after, "—", 2)[0])
				if svc != "" {
					svcCount[svc]++
				}
			}
		}
	}
	crossWS := 0
	for svc, count := range svcCount {
		if count >= 2 {
			fmt.Fprintf(sb, "- **%s** — recurring in %d workspaces\n", svc, count)
			crossWS++
		}
	}
	if crossWS == 0 {
		sb.WriteString("_No cross-workspace recurring patterns detected._\n")
	}
}

// handleProjectDigest generates a structural snapshot of the workspace. [PILAR-XX/148.C]
type pkgPair struct {
	pair  string
	count int
}

// buildPkgCoupling counts cross-package call edges and returns the raw
// map. [Épica 229.6 refactor]
