package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/incidents"
	"github.com/ensamblatec/neoanvil/pkg/rag"
	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

// LogAnalyzerTool provides semantic log analysis with incident corpus correlation. [PILAR-XXI/151]
type LogAnalyzerTool struct {
	embedder rag.Embedder
	graph    *rag.Graph
	wal      *rag.WAL
	cpu      tensorx.ComputeDevice
}

func NewLogAnalyzerTool(embedder rag.Embedder, graph *rag.Graph, wal *rag.WAL, cpu tensorx.ComputeDevice) *LogAnalyzerTool {
	return &LogAnalyzerTool{embedder: embedder, graph: graph, wal: wal, cpu: cpu}
}

func (t *LogAnalyzerTool) Name() string { return "neo_log_analyzer" }

func (t *LogAnalyzerTool) Description() string {
	return "Análisis semántico de logs runtime: cuenta patrones de error, detecta gaps temporales, identifica componentes críticos, y correlaciona con el corpus histórico de incidentes via HNSW."
}

func (t *LogAnalyzerTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"content": map[string]any{
				"type":        "string",
				"description": "Texto del log a analizar (máx 50 KB). Usar esto o log_path.",
			},
			"log_path": map[string]any{
				"type":        "string",
				"description": "Ruta absoluta al fichero de log. Alternativa a content.",
			},
			"max_lines": map[string]any{
				"type":        "integer",
				"description": "Número máximo de líneas a procesar (default 1000).",
			},
			"transcript_path": map[string]any{
				"type":        "string",
				"description": "[130.2] Path to a Claude Code .jsonl transcript file. When provided, activates transcript analysis mode (tool usage, edit patterns, retry loops).",
			},
		},
		Required: []string{},
	}
}

var (
	reLogTS    = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)?)`)
	reLogLevel = regexp.MustCompile(`\[(ERROR|WARN|WARNING|CRITICAL|BOOT|SRE[^]]*)\]`)
	reCompTag  = regexp.MustCompile(`\[([A-Z][A-Z0-9_-]{1,20})\]`)
)

type compEntry struct {
	name  string
	count int
}

type logLine struct {
	ts    time.Time
	level string
	comp  string
	text  string
}

type logParseResult struct {
	parsed      []logLine
	levelCounts map[string]int
	compErrors  map[string]int
	gaps        []string
}

const maxLogBytes = 50 * 1024

var skipLevelTags = map[string]bool{
	"ERROR": true, "WARN": true, "WARNING": true, "CRITICAL": true, "BOOT": true, "INFO": true,
}

func resolveLogLines(args map[string]any, maxLines int) ([]string, error) {
	var raw string
	if content, ok := args["content"].(string); ok && content != "" {
		raw = content
	} else if logPath, ok := args["log_path"].(string); ok && logPath != "" {
		data, err := os.ReadFile(logPath) //nolint:gosec // G304-CLI-CONSENT
		if err != nil {
			return nil, fmt.Errorf("log_analyzer: read %s: %w", logPath, err)
		}
		if len(data) > maxLogBytes {
			data = data[len(data)-maxLogBytes:]
		}
		raw = string(data)
	} else {
		return nil, fmt.Errorf("log_analyzer: either content or log_path is required")
	}
	lines := strings.Split(raw, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines, nil
}

func parseTimestamp(line string) time.Time {
	m := reLogTS.FindStringSubmatch(line)
	if len(m) != 2 {
		return time.Time{}
	}
	for _, layout := range []string{"2006/01/02 15:04:05.000000", "2006/01/02 15:04:05"} {
		if ts, err := time.Parse(layout, m[1]); err == nil {
			return ts
		}
	}
	return time.Time{}
}

func extractComp(line string) string {
	for _, tm := range reCompTag.FindAllStringSubmatch(line, -1) {
		if len(tm) == 2 && !skipLevelTags[tm[1]] {
			return tm[1]
		}
	}
	return ""
}

func trackGap(ts, prevTS time.Time, gaps []string) []string {
	if ts.IsZero() || prevTS.IsZero() {
		return gaps
	}
	if gap := ts.Sub(prevTS); gap > time.Second {
		gaps = append(gaps, fmt.Sprintf("%.1fs gap at %s", gap.Seconds(), ts.Format("15:04:05")))
		if len(gaps) > 10 {
			gaps = gaps[:10]
		}
	}
	return gaps
}

func isErrorLevel(level string) bool {
	return level == "ERROR" || level == "CRITICAL" || level == "WARN" || level == "WARNING"
}

func parseLogLines(lines []string) logParseResult {
	parsed := make([]logLine, 0, len(lines))
	levelCounts := make(map[string]int)
	compErrors := make(map[string]int)
	var gaps []string
	var prevTS time.Time

	for _, line := range lines {
		if line == "" {
			continue
		}
		ll := logLine{text: line, ts: parseTimestamp(line)}
		gaps = trackGap(ll.ts, prevTS, gaps)
		if !ll.ts.IsZero() {
			prevTS = ll.ts
		}

		if m := reLogLevel.FindStringSubmatch(line); len(m) == 2 {
			ll.level = strings.TrimSpace(m[1])
			levelCounts[ll.level]++
		}

		ll.comp = extractComp(line)
		if ll.comp != "" && isErrorLevel(ll.level) {
			compErrors[ll.comp]++
		}
		parsed = append(parsed, ll)
	}
	return logParseResult{parsed: parsed, levelCounts: levelCounts, compErrors: compErrors, gaps: gaps}
}

func topErrorComponents(compErrors map[string]int) []compEntry {
	topComps := make([]compEntry, 0, len(compErrors))
	for k, v := range compErrors {
		topComps = append(topComps, compEntry{k, v})
	}
	sort.Slice(topComps, func(i, j int) bool { return topComps[i].count > topComps[j].count })
	if len(topComps) > 5 {
		topComps = topComps[:5]
	}
	return topComps
}

func buildMarkdownReport(pr logParseResult, topComps []compEntry, similarIncident string) string {
	var sb strings.Builder
	sb.WriteString("## neo_log_analyzer Report\n\n")
	fmt.Fprintf(&sb, "**Lines analyzed:** %d\n\n", len(pr.parsed))

	sb.WriteString("### Event Level Counts\n")
	for _, lvl := range []string{"BOOT", "WARN", "WARNING", "ERROR", "CRITICAL", "SRE"} {
		if c, ok := pr.levelCounts[lvl]; ok {
			fmt.Fprintf(&sb, "- `[%s]`: %d\n", lvl, c)
		}
	}
	for lvl, c := range pr.levelCounts {
		switch lvl {
		case "BOOT", "WARN", "WARNING", "ERROR", "CRITICAL", "SRE":
		default:
			fmt.Fprintf(&sb, "- `[%s]`: %d\n", lvl, c)
		}
	}
	sb.WriteString("\n")

	if len(topComps) > 0 {
		sb.WriteString("### Top Error Components\n")
		for i, entry := range topComps {
			fmt.Fprintf(&sb, "%d. `[%s]` — %d error/warn events\n", i+1, entry.name, entry.count)
		}
		sb.WriteString("\n")
	}

	if len(pr.gaps) > 0 {
		sb.WriteString("### Timestamp Gaps > 1s (possible blocks)\n")
		for _, g := range pr.gaps {
			fmt.Fprintf(&sb, "- %s\n", g)
		}
		sb.WriteString("\n")
	}

	if similarIncident != "" {
		fmt.Fprintf(&sb, "### Similar Incident (HNSW correlation)\n- **%s**\n\n", similarIncident)
	}
	return sb.String()
}

func (t *LogAnalyzerTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	// [130.2.1] Transcript mode: path ending in .jsonl activates the transcript parser.
	if tp, ok := args["transcript_path"].(string); ok && strings.HasSuffix(tp, ".jsonl") {
		rpt, err := parseTranscript(tp)
		if err != nil {
			return nil, err
		}
		return mcpText(buildTranscriptReport(rpt)), nil
	}

	maxLines := 1000
	if ml, ok := args["max_lines"].(float64); ok && ml > 0 {
		maxLines = int(ml)
	}

	lines, err := resolveLogLines(args, maxLines)
	if err != nil {
		return nil, err
	}

	pr := parseLogLines(lines)
	topComps := topErrorComponents(pr.compErrors)

	var similarIncident string
	errorSummary := buildErrorSummary(pr.levelCounts, topComps)
	if errorSummary != "" && t.embedder != nil {
		metas, searchErr := incidents.SearchIncidents(ctx, errorSummary, 3, t.embedder, t.graph, t.wal, t.cpu)
		if searchErr == nil && len(metas) > 0 {
			m := metas[0]
			similarIncident = fmt.Sprintf("%s (severity=%s)", m.ID, m.Severity)
			if m.Anomaly != "" {
				similarIncident += ": " + m.Anomaly
			}
		}
	}

	return mcpText(buildMarkdownReport(pr, topComps, similarIncident)), nil
}

func buildErrorSummary(levelCounts map[string]int, topComps []compEntry) string {
	var parts []string
	for _, lvl := range []string{"ERROR", "CRITICAL", "WARN"} {
		if c, ok := levelCounts[lvl]; ok && c > 0 {
			parts = append(parts, fmt.Sprintf("%d %s events", c, lvl))
		}
	}
	for _, c := range topComps {
		parts = append(parts, fmt.Sprintf("component %s", c.name))
	}
	return strings.Join(parts, ", ")
}
