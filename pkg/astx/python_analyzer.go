package astx

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
)

var (
	pyFuncRe    = regexp.MustCompile(`^(\s*)def\s+(\w+)\s*\(([^)]*)\)`)
	pyCCRe      = regexp.MustCompile(`\b(if|elif|for|while|except|with|and|or|lambda)\b|\[.+\s+for\s+`)
	pyCommentRe = regexp.MustCompile(`#.*$`)
	pyShadowRe  = regexp.MustCompile(`\bfor\s+(\w+)\s+in\b|\bexcept\s+\w+\s+as\s+(\w+)|\bwith\s+\S+\s+as\s+(\w+)`)
)

// PythonAnalyzer detects high CC and shadow variables in Python functions.
// Uses regex-based keyword counting — CC may be slightly overestimated due to string literals.
type PythonAnalyzer struct{}

// parsePyFuncBody collects body lines (strictly more indented than the def).
func parsePyFuncBody(allLines []string, defIdx, funcIndent int) []string {
	var body []string
	for j := defIdx + 1; j < len(allLines); j++ {
		line := allLines[j]
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" {
			body = append(body, line)
			continue
		}
		if len(line)-len(trimmed) <= funcIndent {
			break
		}
		body = append(body, line)
	}
	return body
}

// countPyCC returns the cyclomatic complexity estimate for the given body lines.
func countPyCC(body []string) int {
	cc := 1
	for _, bodyLine := range body {
		stripped := pyCommentRe.ReplaceAllString(bodyLine, "")
		cc += len(pyCCRe.FindAllString(stripped, -1))
	}
	return cc
}

// detectPyShadows returns SHADOW findings for loop/except/with vars that reuse parameter names.
func detectPyShadows(body []string, params map[string]struct{}, path string, funcStartLine int, funcName string) []AuditFinding {
	var findings []AuditFinding
	for lineIdx, bodyLine := range body {
		stripped := pyCommentRe.ReplaceAllString(bodyLine, "")
		for _, sm := range pyShadowRe.FindAllStringSubmatch(stripped, -1) {
			for _, cap := range sm[1:] {
				if cap == "" {
					continue
				}
				if _, ok := params[cap]; ok {
					findings = append(findings, AuditFinding{
						File:    path,
						Line:    funcStartLine + lineIdx + 1,
						Kind:    "SHADOW",
						Message: fmt.Sprintf("def %s: variable '%s' shadows parameter", funcName, cap),
					})
				}
			}
		}
	}
	return findings
}

func (PythonAnalyzer) Analyze(_ context.Context, path string, src []byte) ([]AuditFinding, error) {
	var findings []AuditFinding
	scanner := bufio.NewScanner(bytes.NewReader(src))
	var allLines []string
	for scanner.Scan() {
		allLines = append(allLines, scanner.Text())
	}

	// ccMap collects all function CCs for the CC_SUMMARY finding.
	type funcCC struct {
		name string
		cc   int
		line int
	}
	var ccMap []funcCC

	for i, line := range allLines {
		m := pyFuncRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		funcIndent := len(m[1])
		funcName := m[2]
		funcStartLine := i + 1
		params := extractPyParams(m[3])
		body := parsePyFuncBody(allLines, i, funcIndent)

		cc := countPyCC(body)
		ccMap = append(ccMap, funcCC{name: funcName, cc: cc, line: funcStartLine})

		if cc > 15 {
			findings = append(findings, AuditFinding{
				File:    path,
				Line:    funcStartLine,
				Kind:    "COMPLEXITY",
				Message: fmt.Sprintf("def %s: CC≈%d (limit 15, regex estimate)", funcName, cc),
				CCValue: cc,
			})
		}
		findings = append(findings, detectPyShadows(body, params, path, funcStartLine, funcName)...)
	}

	// Emit a CC_SUMMARY finding so agents can verify post-refactor CC without re-running.
	if len(ccMap) > 0 {
		var sb strings.Builder
		sb.WriteString("cc_detail: {")
		for idx, fc := range ccMap {
			if idx > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "%s:%d", fc.name, fc.cc)
		}
		sb.WriteString("}")
		// Use line 0 so it sorts before any file findings.
		findings = append(findings, AuditFinding{
			File:    path,
			Line:    0,
			Kind:    "CC_SUMMARY",
			Message: sb.String(),
			CCValue: len(ccMap),
		})
	}

	return findings, nil
}

func extractPyParams(paramStr string) map[string]struct{} {
	params := make(map[string]struct{})
	for _, p := range strings.Split(paramStr, ",") {
		p = strings.TrimSpace(p)
		// Strip defaults (x=0) and type hints (x: int)
		if idx := strings.IndexAny(p, ":="); idx >= 0 {
			p = strings.TrimSpace(p[:idx])
		}
		// Strip * and ** prefixes
		p = strings.TrimLeft(p, "*")
		if p != "" && p != "self" && p != "cls" {
			params[p] = struct{}{}
		}
	}
	return params
}
