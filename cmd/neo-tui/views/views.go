// Package views renders each tab of the TUI. Every view is a pure
// function (Snapshot → string) so the Model can swap them trivially.
// [PILAR-XXVII/246.D-K]
package views

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Renderable is the sub-tree of fields each view consumes from Snapshot.
// Declared in api.go at the package root of cmd/neo-tui — we mirror it
// here via an interface so views don't depend on the api package
// symbol directly (avoids an import cycle).
type Renderable interface {
	RenderableSnapshot() snapshot
}

// snapshot is the decoupled shape the views use. Matches api.Snapshot
// field-by-field but lives in this package so views import nothing
// outside stdlib. Callers (the model) adapt api.Snapshot → snapshot via
// a tiny conversion function.
type snapshot struct {
	WorkspaceID   string
	WorkspaceName string
	UptimeSeconds int64
	GeneratedAt   time.Time

	HeapMB         float64
	StackMB        float64
	Goroutines     int
	CPGHeapMB      int
	CPGHeapLimitMB int
	CPGHeapPct     int
	QueryHit       float64
	TextHit        float64
	EmbHit         float64

	Total24h    int
	TopByCalls  []ToolRow
	TopByErrors []ToolRow
	TopByP99    []ToolRow

	TokensToday       int
	TokensMCPIn       int
	TokensMCPOut      int
	TokensInternalIn  int
	TokensInternalOut int
	TokensCostUSD     float64
	ByAgent           map[string]int
	ByTool            map[string]int
	Last7Days         []DayRow

	Certified24h int
	Bypassed24h  int
	Hotspots     []HotspotRow

	Events []EventRow
}

// ToolRow / HotspotRow / EventRow / DayRow are the view-local
// flattenings of the JSON payload.
type ToolRow struct {
	Name      string
	Calls     int
	Errors    int
	ErrorRate float64
	P99Ms     float64
}

type HotspotRow struct {
	Path  string
	Count int
}

type EventRow struct {
	Timestamp time.Time
	Type      string
	Severity  string
}

type DayRow struct {
	Day            string
	MCPInput       int
	MCPOutput      int
	InternalInput  int
	InternalOutput int
	CostUSD        float64
}

// Snapshot is the exported accessor — callers build it via a helper in
// main.go (adapt()). Kept in this package so every tab's Render(...) has
// a uniform signature.
type Snapshot = snapshot

// RenderOverview is tab 0 — executive summary. Keeps it short so the
// first impression fits a 40-row terminal.
func RenderOverview(s Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Workspace: %s (%s)\n", s.WorkspaceName, shortID(s.WorkspaceID))
	fmt.Fprintf(&b, "Uptime:    %s\n", humanDuration(s.UptimeSeconds))
	fmt.Fprintf(&b, "Generated: %s\n\n", s.GeneratedAt.Format("15:04:05 MST"))
	fmt.Fprintf(&b, "Heap:        %.1f MB    Stack: %.1f MB    Goroutines: %d\n",
		s.HeapMB, s.StackMB, s.Goroutines)
	if s.CPGHeapLimitMB > 0 {
		fmt.Fprintf(&b, "CPG:         %d/%d MB (%d%%)\n", s.CPGHeapMB, s.CPGHeapLimitMB, s.CPGHeapPct)
	}
	fmt.Fprintf(&b, "Cache hit:   Q=%.0f%%  T=%.0f%%  E=%.0f%%\n\n",
		s.QueryHit*100, s.TextHit*100, s.EmbHit*100)
	fmt.Fprintf(&b, "Tools 24h:   %d calls  |  top=%s\n", s.Total24h, topName(s.TopByCalls))
	fmt.Fprintf(&b, "Tokens 24h:  MCP=%d/%d   Inference=%d/%d   $%.4f\n",
		s.TokensMCPIn, s.TokensMCPOut, s.TokensInternalIn, s.TokensInternalOut, s.TokensCostUSD)
	fmt.Fprintf(&b, "Mutations:   %d certified, %d bypassed\n", s.Certified24h, s.Bypassed24h)
	fmt.Fprintf(&b, "Events:      %d recent\n", len(s.Events))
	return b.String()
}

// RenderTools is tab 1 — top-10 tables.
func RenderTools(s Snapshot) string {
	var b strings.Builder
	b.WriteString("Top by calls:\n")
	b.WriteString(formatToolTable(s.TopByCalls))
	b.WriteString("\nTop by errors:\n")
	b.WriteString(formatToolTable(s.TopByErrors))
	b.WriteString("\nTop by p99 latency:\n")
	b.WriteString(formatToolTable(s.TopByP99))
	return b.String()
}

// RenderTokens is tab 2 — token spend & cost breakdown.
func RenderTokens(s Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TODAY  total: %d in / %d out   $%.4f\n\n",
		s.TokensMCPIn+s.TokensInternalIn,
		s.TokensMCPOut+s.TokensInternalOut, s.TokensCostUSD)
	fmt.Fprintf(&b, "  mcp_traffic:      %d in / %d out\n", s.TokensMCPIn, s.TokensMCPOut)
	fmt.Fprintf(&b, "  internal_infer:   %d in / %d out\n\n", s.TokensInternalIn, s.TokensInternalOut)
	if len(s.ByAgent) > 0 {
		b.WriteString("By agent:\n")
		for _, kv := range sortedByValueDesc(s.ByAgent) {
			fmt.Fprintf(&b, "  %-32s %d\n", kv.K, kv.V)
		}
		b.WriteString("\n")
	}
	if len(s.ByTool) > 0 {
		b.WriteString("By tool:\n")
		for _, kv := range sortedByValueDesc(s.ByTool) {
			fmt.Fprintf(&b, "  %-32s %d\n", kv.K, kv.V)
		}
		b.WriteString("\n")
	}
	if len(s.Last7Days) > 0 {
		b.WriteString("Last 7 days:\n")
		for _, d := range s.Last7Days {
			fmt.Fprintf(&b, "  %s  mcp=%d/%d  internal=%d/%d  $%.4f\n",
				d.Day, d.MCPInput, d.MCPOutput, d.InternalInput, d.InternalOutput, d.CostUSD)
		}
	}
	return b.String()
}

// RenderMutations is tab 3 — certify vs bypass, top hotspots.
func RenderMutations(s Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Last 24h:  %d certified   %d bypassed\n\n", s.Certified24h, s.Bypassed24h)
	if len(s.Hotspots) == 0 {
		b.WriteString("No hotspots recorded yet. Edit + certify some files to populate this view.\n")
		return b.String()
	}
	b.WriteString("Top hotspots:\n")
	for i, h := range s.Hotspots {
		fmt.Fprintf(&b, "  %2d. %-4d  %s\n", i+1, h.Count, h.Path)
	}
	return b.String()
}

// RenderMemory is tab 4 — memory deep-dive.
func RenderMemory(s Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Heap:       %.1f MB\n", s.HeapMB)
	fmt.Fprintf(&b, "Stack:      %.1f MB\n", s.StackMB)
	fmt.Fprintf(&b, "Goroutines: %d\n", s.Goroutines)
	if s.CPGHeapLimitMB > 0 {
		fmt.Fprintf(&b, "CPG:        %d / %d MB (%d%%)\n", s.CPGHeapMB, s.CPGHeapLimitMB, s.CPGHeapPct)
	}
	b.WriteString("\nCache hit rates (5 min window):\n")
	fmt.Fprintf(&b, "  QueryCache:      %.1f%%\n", s.QueryHit*100)
	fmt.Fprintf(&b, "  TextCache:       %.1f%%\n", s.TextHit*100)
	fmt.Fprintf(&b, "  EmbeddingCache:  %.1f%%\n", s.EmbHit*100)
	return b.String()
}

// RenderSystem is tab 5 — system + config info.
func RenderSystem(s Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Workspace ID:   %s\n", s.WorkspaceID)
	fmt.Fprintf(&b, "Workspace Name: %s\n", s.WorkspaceName)
	fmt.Fprintf(&b, "Uptime:         %s\n", humanDuration(s.UptimeSeconds))
	fmt.Fprintf(&b, "Last refresh:   %s\n", s.GeneratedAt.Format(time.RFC3339))
	return b.String()
}

// RenderRules is tab 6 — placeholder (rules endpoint not yet in API).
func RenderRules(Snapshot) string {
	return "Rules tab — pending API. See .claude/rules/neo-synced-directives.md on disk for now."
}

// RenderIncidents is tab 7 — placeholder (incident endpoint not yet in API).
func RenderIncidents(s Snapshot) string {
	if len(s.Events) == 0 {
		return "No recent events.\n"
	}
	var b strings.Builder
	b.WriteString("Recent events:\n")
	max := 20
	if len(s.Events) < max {
		max = len(s.Events)
	}
	for _, e := range s.Events[:max] {
		sev := e.Severity
		if sev == "" {
			sev = "info"
		}
		fmt.Fprintf(&b, "  [%s] %s  %s\n",
			e.Timestamp.Format("15:04:05"), sev, e.Type)
	}
	return b.String()
}

// ───── helpers ─────

func formatToolTable(rows []ToolRow) string {
	if len(rows) == 0 {
		return "  (no data)\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  %-32s %8s %8s %8s %8s\n", "tool", "calls", "errors", "err %", "p99 ms")
	for _, r := range rows {
		fmt.Fprintf(&b, "  %-32s %8d %8d %7.1f%% %8.2f\n",
			r.Name, r.Calls, r.Errors, r.ErrorRate*100, r.P99Ms)
	}
	return b.String()
}

func topName(rows []ToolRow) string {
	if len(rows) == 0 {
		return "(none)"
	}
	return rows[0].Name
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

func humanDuration(seconds int64) string {
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	if seconds < 3600 {
		return fmt.Sprintf("%dm %ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%dh %dm", seconds/3600, (seconds%3600)/60)
}

// KV is the flattened (key, value) pair used for sorting maps in views.
type KV struct {
	K string
	V int
}

// sortedByValueDesc returns the map entries sorted by value desc.
func sortedByValueDesc(m map[string]int) []KV {
	if len(m) == 0 {
		return nil
	}
	out := make([]KV, 0, len(m))
	for k, v := range m {
		out = append(out, KV{K: k, V: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].V > out[j].V })
	return out
}
