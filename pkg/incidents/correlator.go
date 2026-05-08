package incidents

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
)

// CPGCorrelation holds CPG-derived enrichment for a function referenced in an incident.
type CPGCorrelation struct {
	File        string
	FuncName    string
	CodeRank    float64
	CallerCount int
}

var reGoFile = regexp.MustCompile(`[\w/.-]+\.go(?::\d+)?`)

// CorrelateWithCPG extracts .go file references and function names from incident content
// and looks them up in the CPG. Returns up to maxResults correlations ranked by CodeRank.
func CorrelateWithCPG(content []byte, cpgMgr *cpg.Manager, maxResults int) []CPGCorrelation {
	if cpgMgr == nil {
		return nil
	}
	g, err := cpgMgr.Graph(200 * time.Millisecond)
	if err != nil {
		return nil // CPG not ready; skip silently
	}

	text := string(content)

	// Extract candidate .go file basenames.
	goFiles := make(map[string]bool)
	for _, m := range reGoFile.FindAllString(text, -1) {
		base := filepath.Base(strings.TrimRight(m, ":0123456789"))
		if strings.HasSuffix(base, ".go") {
			goFiles[base] = true
		}
	}

	// Compute CodeRank once.
	ranks := cpg.ComputePageRank(g, 0.85, 50)

	// Index CPG nodes by file basename for O(N) scan.
	var candidates []cpgCandidate
	for i, node := range g.Nodes {
		if node.Kind != cpg.NodeFunc {
			continue
		}
		base := filepath.Base(node.File)
		if !goFiles[base] {
			continue
		}
		r := 0.0
		if cpg.NodeID(i) < cpg.NodeID(len(ranks)) {
			r = ranks[cpg.NodeID(i)]
		}
		candidates = append(candidates, cpgCandidate{node, r})
	}

	sortCandidatesByRank(candidates)

	if maxResults > 0 && len(candidates) > maxResults {
		candidates = candidates[:maxResults]
	}

	result := make([]CPGCorrelation, 0, len(candidates))
	for _, c := range candidates {
		nodeID, ok := g.NodeByName(c.node.Package, c.node.Name)
		callers := 0
		if ok {
			callers = len(g.CallersOf(nodeID))
		}
		result = append(result, CPGCorrelation{
			File:        c.node.File,
			FuncName:    c.node.Name,
			CodeRank:    c.rank,
			CallerCount: callers,
		})
	}
	return result
}

// cpgCandidate is a CPG node with its computed CodeRank score.
type cpgCandidate struct {
	node cpg.Node
	rank float64
}

// sortCandidatesByRank sorts candidates by CodeRank descending (insertion sort, small N).
func sortCandidatesByRank(c []cpgCandidate) {
	for i := 1; i < len(c); i++ {
		for j := i; j > 0 && c[j].rank > c[j-1].rank; j-- {
			c[j], c[j-1] = c[j-1], c[j]
		}
	}
}

// FormatCPGSection renders the CPG correlations as a Markdown section for appending to an INC file.
func FormatCPGSection(correlations []CPGCorrelation) string {
	if len(correlations) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n## CPG Blast Radius (auto-correlación)\n\n")
	sb.WriteString("| Función | Archivo | CodeRank | Callers |\n")
	sb.WriteString("|---------|---------|----------|---------|\n")
	for _, c := range correlations {
		fmt.Fprintf(&sb, "| `%s` | `%s` | %.6f | %d |\n",
			c.FuncName, filepath.Base(c.File), c.CodeRank, c.CallerCount)
	}
	return sb.String()
}
