package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
	"github.com/ensamblatec/neoanvil/pkg/telemetry"
)

func (t *RadarTool) handleDBSchema(ctx context.Context, args map[string]any) (any, error) {
	if t.dbaEngine == nil {
		return nil, fmt.Errorf("DBA engine not initialized")
	}
	alias, _ := args["db_alias"].(string)
	query, _ := args["target"].(string)
	if alias == "" || query == "" {
		return nil, fmt.Errorf("db_alias and target (query) are required for DB_SCHEMA")
	}

	var driver, dsn string
	var maxOpenConns int
	for _, db := range t.cfg.Databases {
		if db.Name == alias {
			driver = db.Driver
			dsn = db.DSN
			maxOpenConns = db.MaxOpenConns
			break
		}
	}
	if driver == "" || dsn == "" {
		return nil, fmt.Errorf("database alias '%s' not found in neo.yaml databases", alias)
	}

	// [SRE-75.4] Read-only guard: reject any mutating or structural SQL.
	upperQuery := strings.ToUpper(strings.TrimSpace(query))
	for _, banned := range []string{"DROP ", "DELETE ", "UPDATE ", "INSERT ", "TRUNCATE ", "ALTER ", "CREATE ", "REPLACE "} {
		if strings.Contains(upperQuery, banned) {
			return nil, fmt.Errorf("destructive/structural query '%s' is prohibited in DB_SCHEMA — use SELECT or PRAGMA only", banned)
		}
	}

	results, err := t.dbaEngine.QuerySchema(ctx, driver, dsn, query, maxOpenConns)
	if err != nil {
		return nil, err
	}
	return mcpText(fmt.Sprintf("## DB_SCHEMA: %s\n\n%s", alias, results)), nil
}

func (t *RadarTool) handleTechDebtMap(ctx context.Context, args map[string]any) (any, error) {
	limit := 10
	if lFloat, ok := args["limit"].(float64); ok && lFloat > 0 {
		limit = int(lFloat)
	}
	hotspots, err := telemetry.GetTopHotspots(limit)
	if err != nil {
		return nil, err
	}
	var sb strings.Builder

	// [164.A/164.C] Session mutations section.
	sessionMuts, _ := t.wal.GetSessionMutations(briefingSessionID(t.workspace))
	formatDebtSessionMuts(&sb, sessionMuts)

	sb.WriteString("🔥 TOP AST HOTSPOTS 🔥\n")
	if len(hotspots) == 0 {
		sb.WriteString("_(no mutation data yet — certify files to populate hotspots)_\n")
	}
	for i, hs := range hotspots {
		// [275.B] 🔴 flag for files with >= 80 certified mutations — critical debt signal.
		critMarker := ""
		if hs.Mutations >= 80 {
			critMarker = " 🔴"
		}
		// [159.C] Distinguish certified vs bypassed mutations.
		if hs.Bypassed > 0 {
			fmt.Fprintf(&sb, "%d. %s%s - %d mutations (%d certified, %d bypassed ⚠️)\n",
				i+1, hs.File, critMarker, hs.Mutations+hs.Bypassed, hs.Mutations, hs.Bypassed)
		} else {
			fmt.Fprintf(&sb, "%d. %s%s - %d mutations\n", i+1, hs.File, critMarker, hs.Mutations)
		}
	}

	formatDebtCodeRank(t, &sb, limit)
	sb.WriteString(t.formatScatterSection("TECH_DEBT_MAP", map[string]any{"limit": limit}))

	// [333.A] target_workspace:"project" — scatter to member workspaces via Nexus and
	// append each result as a labeled section.
	twRaw, _ := args["target_workspace"].(string)
	if twRaw == "project" {
		t.appendProjectDebtSections(ctx, &sb, limit)
	}

	return mcpText(sb.String()), nil
}

// appendProjectDebtSections scatters TECH_DEBT_MAP to all member workspaces and
// appends each result as a labeled Markdown section. [333.A]
func (t *RadarTool) appendProjectDebtSections(_ context.Context, sb *strings.Builder, limit int) {
	if t.cfg.Project == nil || len(t.cfg.Project.MemberWorkspaces) == 0 {
		sb.WriteString("\n\n> ℹ️ target_workspace:project — no member_workspaces configured in .neo-project/neo.yaml\n")
		return
	}
	sb.WriteString("\n\n---\n## Cross-Workspace Tech Debt [project scatter]\n\n")
	results := t.scatterToMembers(context.Background(), "TECH_DEBT_MAP", map[string]any{"limit": limit})
	if len(results) == 0 {
		sb.WriteString("_No remote member workspaces reachable via Nexus. Ensure workspaces are running._\n")
		return
	}
	for _, r := range results {
		fmt.Fprintf(sb, "### %s\n\n", r.name)
		if r.err != nil {
			fmt.Fprintf(sb, "_error: %v_\n\n", r.err)
			continue
		}
		if r.text == "" {
			sb.WriteString("_empty response_\n\n")
			continue
		}
		sb.WriteString(r.text)
		sb.WriteString("\n\n")
	}
}

func formatDebtSessionMuts(sb *strings.Builder, sessionMuts []string) {
	if len(sessionMuts) == 0 {
		return
	}
	sb.WriteString("### Session Mutations (esta sesión)\n")
	sb.WriteString("| # | Archivo | Estado |\n")
	sb.WriteString("|---|---------|--------|\n")
	for i, m := range sessionMuts {
		base := m
		if idx := strings.LastIndex(m, "/"); idx >= 0 {
			base = m[idx+1:]
		}
		fmt.Fprintf(sb, "| %d | `%s` | ✅ certified |\n", i+1, base)
	}
	sb.WriteString("\n")
}

func formatDebtCodeRank(t *RadarTool, sb *strings.Builder, limit int) {
	if t.cpgManager == nil {
		return
	}
	g, gerr := t.cpgManager.Graph(100 * time.Millisecond)
	if gerr != nil || g == nil {
		return
	}
	ranks := cpg.CachedComputePageRank(g, 0.85, 50)
	topLocal := g.TopN(limit, ranks, "github.com/ensamblatec/neoanvil")
	if len(topLocal) == 0 {
		return
	}
	sb.WriteString("\n📊 STRUCTURAL RANK (CodeRank / ensamblatec)\n")
	for ri, rn := range topLocal {
		fmt.Fprintf(sb, "  %2d. %-40s pkg=%-30s score=%.6f line=%d\n",
			ri+1, rn.Name, rn.Package, rn.Score, rn.Line)
	}
}
