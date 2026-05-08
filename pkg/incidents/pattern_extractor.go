package incidents

import (
	"fmt"
	"sort"
	"strings"
)

// Pattern describes a recurring failure signature observed across multiple incidents.
type Pattern struct {
	Component   string   // bracket tag (e.g. "RAG", "HNSW")
	Count       int      // number of incidents referencing this component
	Severity    string   // highest severity seen
	IncidentIDs []string // up to 5 example INC IDs
	Directive   string   // candidate directive text for neo_learn_directive [155.B]
}

// ExtractRecurringPatterns groups incidents by affected service and returns patterns
// with recurrence >= 2, sorted by count descending. [155.A]
func ExtractRecurringPatterns(metas []IncidentMeta) []Pattern {
	type bucket struct {
		count    int
		severity string // highest seen
		ids      []string
	}
	byComponent := make(map[string]*bucket)

	for _, m := range metas {
		for _, svc := range m.AffectedServices {
			b, ok := byComponent[svc]
			if !ok {
				b = &bucket{}
				byComponent[svc] = b
			}
			b.count++
			b.severity = higherSeverity(b.severity, m.Severity)
			if len(b.ids) < 5 {
				b.ids = append(b.ids, m.ID)
			}
		}
	}

	var patterns []Pattern
	for comp, b := range byComponent {
		if b.count < 2 {
			continue
		}
		p := Pattern{
			Component:   comp,
			Count:       b.count,
			Severity:    b.severity,
			IncidentIDs: b.ids,
		}
		p.Directive = buildDirective(comp, b.count, b.severity)
		patterns = append(patterns, p)
	}

	sort.Slice(patterns, func(i, j int) bool { return patterns[i].Count > patterns[j].Count })
	return patterns
}

// higherSeverity returns the more severe of two severity strings.
func higherSeverity(a, b string) string {
	rank := map[string]int{"CRITICAL": 3, "WARNING": 2, "INFO": 1, "": 0}
	if rank[a] >= rank[b] {
		return a
	}
	return b
}

// buildDirective generates a candidate SRE directive text for a recurring pattern. [155.B]
func buildDirective(component string, count int, severity string) string {
	mitigation := fmt.Sprintf("investigate %s logs and run neo_log_analyzer with log_path", component)
	return fmt.Sprintf("[SRE-INC-PATTERN] Component %s has failed %d times (max_severity=%s). "+
		"Mitigación: %s.", component, count, severity, mitigation)
}

// FormatPatternAudit renders the pattern list as a Markdown report. [155.C]
func FormatPatternAudit(patterns []Pattern) string {
	if len(patterns) == 0 {
		return "_No recurring patterns detected (need ≥2 incidents per component)._\n"
	}
	var sb strings.Builder
	sb.WriteString("## PATTERN_AUDIT — Recurring Incident Patterns\n\n")
	sb.WriteString("| Component | Count | Severity | Example IDs |\n")
	sb.WriteString("|-----------|-------|----------|-------------|\n")
	for _, p := range patterns {
		ids := strings.Join(p.IncidentIDs, ", ")
		fmt.Fprintf(&sb, "| `%s` | %d | %s | %s |\n", p.Component, p.Count, p.Severity, ids)
	}
	sb.WriteString("\n### Candidate Directives\n\n")
	sb.WriteString("_Run `neo_learn_directive` to persist confirmed patterns:_\n\n")
	for _, p := range patterns {
		fmt.Fprintf(&sb, "- `%s`\n", p.Directive)
	}
	return sb.String()
}
