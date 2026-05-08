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
	// Matches function declarations, arrow functions, and methods.
	tsFuncRe = regexp.MustCompile(
		`(?:function\s+(\w+)\s*\(|(?:^|[\s,{(])(\w+)\s*[=:]\s*(?:async\s*)?\(.*\)\s*(?::\s*\S+\s*)?=>|(?:async\s+)?(\w+)\s*\([^)]*\)\s*(?::\s*\S+\s*)?\{)`)
	// Decision-point keywords for CC.
	tsCCRe = regexp.MustCompile(`\b(if|else\s+if|for|while|switch|case|catch)\b|[?][^?:.]|\|\||&&`)
	// Variable declarations that could shadow outer scope.
	tsDeclRe = regexp.MustCompile(`\b(?:let|const|var)\s+(\w+)`)
	// Single-line comment stripper.
	tsLineCommentRe = regexp.MustCompile(`//.*$`)
)

// tsFuncFrame tracks state for one function scope during TS/JS analysis.
type tsFuncFrame struct {
	name       string
	startLine  int
	braceDepth int
	cc         int
	vars       map[int][]string // brace level → declared names
}

// TSAnalyzer detects high CC and shadow variables in TypeScript/JavaScript files.
// Uses regex-based analysis — CC may vary for complex arrow chains and generics.
type TSAnalyzer struct{}

// extractTSFuncName returns the first non-empty capture group from a tsFuncRe match.
func extractTSFuncName(m []string) string {
	for _, g := range m[1:4] {
		if g != "" {
			return g
		}
	}
	return "<anonymous>"
}

// checkTSShadows appends SHADOW findings when names declared at outer brace levels
// appear again at the current depth.
func checkTSShadows(top *tsFuncFrame, depth, lineNum int, path string) []AuditFinding {
	var out []AuditFinding
	for outerLevel, outerNames := range top.vars {
		if outerLevel >= depth {
			continue
		}
		for _, ov := range outerNames {
			for _, iv := range top.vars[depth] {
				if iv == ov {
					out = append(out, AuditFinding{
						File:    path,
						Line:    lineNum,
						Kind:    "SHADOW",
						Message: fmt.Sprintf("func %s: '%s' shadows outer declaration", top.name, ov),
					})
				}
			}
		}
	}
	return out
}

// closeTSFrames pops frames whose brace scope has ended and reports CC violations.
func closeTSFrames(stack []tsFuncFrame, depth int, path string) ([]tsFuncFrame, []AuditFinding) {
	var findings []AuditFinding
	for len(stack) > 0 && depth <= stack[len(stack)-1].braceDepth {
		frame := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if frame.cc > 15 {
			findings = append(findings, AuditFinding{
				File:    path,
				Line:    frame.startLine,
				Kind:    "COMPLEXITY",
				Message: fmt.Sprintf("func %s: CC≈%d (limit 15, regex estimate)", frame.name, frame.cc),
			})
		}
	}
	return stack, findings
}

func (TSAnalyzer) Analyze(_ context.Context, path string, src []byte) ([]AuditFinding, error) {
	var findings []AuditFinding
	scanner := bufio.NewScanner(bytes.NewReader(src))
	var allLines []string
	for scanner.Scan() {
		allLines = append(allLines, scanner.Text())
	}

	var stack []tsFuncFrame
	depth := 0

	for i, line := range allLines {
		stripped := tsLineCommentRe.ReplaceAllString(line, "")

		if m := tsFuncRe.FindStringSubmatch(stripped); m != nil {
			stack = append(stack, tsFuncFrame{
				name: extractTSFuncName(m), startLine: i + 1,
				braceDepth: depth, cc: 1, vars: make(map[int][]string),
			})
		}

		opens := strings.Count(stripped, "{")
		closes := strings.Count(stripped, "}")
		depth += opens

		if len(stack) > 0 {
			top := &stack[len(stack)-1]
			top.cc += len(tsCCRe.FindAllString(stripped, -1))
			for _, dm := range tsDeclRe.FindAllStringSubmatch(stripped, -1) {
				top.vars[depth] = append(top.vars[depth], dm[1])
			}
			if opens > 0 {
				findings = append(findings, checkTSShadows(top, depth, i+1, path)...)
			}
		}

		depth -= closes
		if depth < 0 {
			depth = 0
		}

		var closed []AuditFinding
		stack, closed = closeTSFrames(stack, depth, path)
		findings = append(findings, closed...)
	}

	// Flush any unclosed frames (e.g. malformed or incomplete files).
	for _, frame := range stack {
		if frame.cc > 15 {
			findings = append(findings, AuditFinding{
				File:    path,
				Line:    frame.startLine,
				Kind:    "COMPLEXITY",
				Message: fmt.Sprintf("func %s: CC≈%d (limit 15, regex estimate)", frame.name, frame.cc),
			})
		}
	}
	return findings, nil
}
