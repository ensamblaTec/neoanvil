// pkg/darwin/proposal.go — Evolution proposal pipeline. [SRE-93.C]
//
// Formats evolution results as Markdown reports, integrates with the REM
// sleep cycle, and provides the apply-champion workflow.
package darwin

import (
	"fmt"
	"strings"
	"time"
)

// EvolutionReport is a formatted Markdown report of an evolution run. [SRE-93.C.1]
type EvolutionReport struct {
	Hotspot    Hotspot   `json:"hotspot"`
	Champion   Champion  `json:"champion"`
	Timestamp  time.Time `json:"timestamp"`
	Markdown   string    `json:"markdown"`
}

// FormatEvolutionReport creates a Markdown report comparing the original function
// with the evolved champion. [SRE-93.C.1]
func FormatEvolutionReport(champion Champion, hotspot Hotspot, baselineNs, baselineAllocs int64) EvolutionReport {
	var sb strings.Builder

	fmt.Fprintf(&sb, "## 🧬 Darwin Evolution Report\n\n")
	fmt.Fprintf(&sb, "**Target:** `%s.%s` (%s:%d)\n\n", hotspot.Package, hotspot.Function, hotspot.File, hotspot.Line)
	fmt.Fprintf(&sb, "### Performance Comparison\n\n")
	fmt.Fprintf(&sb, "| Metric | Original | Champion (Gen %d) | Delta |\n", champion.Generation)
	fmt.Fprintf(&sb, "|--------|----------|-------------------|-------|\n")
	fmt.Fprintf(&sb, "| ns/op | %d | %d | **%.1f%%** |\n", baselineNs, champion.NsPerOp,
		champion.ImprovementPct)

	allocImprovement := float64(0)
	if baselineAllocs > 0 {
		allocImprovement = float64(baselineAllocs-champion.AllocsPerOp) / float64(baselineAllocs) * 100
	}
	fmt.Fprintf(&sb, "| allocs/op | %d | %d | **%.1f%%** |\n", baselineAllocs, champion.AllocsPerOp, allocImprovement)

	fmt.Fprintf(&sb, "\n### Champion Source (Mutation %d, Generation %d)\n\n", champion.MutationID, champion.Generation)
	fmt.Fprintf(&sb, "```go\n%s\n```\n\n", champion.Source)

	fmt.Fprintf(&sb, "### ⚠️ Risk Assessment\n\n")
	fmt.Fprintf(&sb, "- This is an LLM-generated optimization — verify correctness manually\n")
	fmt.Fprintf(&sb, "- Run `neo_sre_certify_mutation` after applying\n")
	fmt.Fprintf(&sb, "- Consider shadow validation (Epic 92) for production safety\n")

	return EvolutionReport{
		Hotspot:   hotspot,
		Champion:  champion,
		Timestamp: time.Now(),
		Markdown:  sb.String(),
	}
}

// StoredChampion is the BoltDB-serializable form of a champion proposal. [SRE-93.C.2]
type StoredChampion struct {
	ID             string    `json:"id"`
	Hotspot        Hotspot   `json:"hotspot"`
	Champion       Champion  `json:"champion"`
	BaselineNs     int64     `json:"baseline_ns"`
	BaselineAllocs int64     `json:"baseline_allocs"`
	CreatedAt      time.Time `json:"created_at"`
	Applied        bool      `json:"applied"`
}

// PendingChampionsSummary returns a BRIEFING-compatible summary of unapplied champions.
func PendingChampionsSummary(champions []StoredChampion) string {
	pending := 0
	var best float64
	for _, c := range champions {
		if !c.Applied {
			pending++
			if c.Champion.ImprovementPct > best {
				best = c.Champion.ImprovementPct
			}
		}
	}
	if pending == 0 {
		return ""
	}
	return fmt.Sprintf("darwin_proposals: %d (best: %.1f%% faster)", pending, best)
}
