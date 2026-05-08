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
	// Matches fn declarations (including async fn, pub fn, pub(crate) fn).
	rustFuncRe = regexp.MustCompile(`\bfn\s+(\w+)\s*[<(]`)
	// Decision-point keywords for CC.
	rustCCRe = regexp.MustCompile(`\b(if|else\s+if|for|while|loop|match)\b|=>\s*\{?|\|\||&&`)
	// Let bindings for shadow detection (let x = or let mut x =).
	rustLetRe       = regexp.MustCompile(`\blet\s+(?:mut\s+)?(\w+)\s*[=:]`)
	rustLineCommentRe = regexp.MustCompile(`//.*$`)
)

// RustAnalyzer detects high CC and intentional shadowing in Rust functions.
// Rust intentional shadowing (let x = ...; let x = ...) is reported as SHADOW_INFO, not an error.
// CC uses regex keyword counting; match arms are counted via "=>" occurrences.
type RustAnalyzer struct{}

func (RustAnalyzer) Analyze(_ context.Context, path string, src []byte) ([]AuditFinding, error) {
	var findings []AuditFinding

	scanner := bufio.NewScanner(bytes.NewReader(src))
	var allLines []string
	for scanner.Scan() {
		allLines = append(allLines, scanner.Text())
	}

	type funcFrame struct {
		name      string
		startLine int
		depth     int // brace depth at function entry
		cc        int
		bindings  map[string]int // varName → first binding line
	}

	var stack []funcFrame
	depth := 0

	for i, line := range allLines {
		stripped := rustLineCommentRe.ReplaceAllString(line, "")

		// Detect function start.
		if m := rustFuncRe.FindStringSubmatch(stripped); m != nil {
			stack = append(stack, funcFrame{
				name:      m[1],
				startLine: i + 1,
				depth:     depth,
				cc:        1,
				bindings:  make(map[string]int),
			})
		}

		opens := strings.Count(stripped, "{")
		closes := strings.Count(stripped, "}")
		depth += opens

		if len(stack) > 0 {
			top := &stack[len(stack)-1]
			top.cc += len(rustCCRe.FindAllString(stripped, -1))

			// Shadow detection: re-binding same name with let.
			for _, lm := range rustLetRe.FindAllStringSubmatch(stripped, -1) {
				varName := lm[1]
				if varName == "_" {
					continue
				}
				if firstLine, seen := top.bindings[varName]; seen {
					findings = append(findings, AuditFinding{
						File:    path,
						Line:    i + 1,
						Kind:    "SHADOW_INFO",
						Message: fmt.Sprintf("fn %s: '%s' shadowed (first bound at line %d) — intentional Rust shadowing", top.name, varName, firstLine),
					})
				} else {
					top.bindings[varName] = i + 1
				}
			}
		}

		depth -= closes
		if depth < 0 {
			depth = 0
		}

		// Pop closed frames.
		for len(stack) > 0 && depth <= stack[len(stack)-1].depth {
			frame := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if frame.cc > 15 {
				findings = append(findings, AuditFinding{
					File:    path,
					Line:    frame.startLine,
					Kind:    "COMPLEXITY",
					Message: fmt.Sprintf("fn %s: CC≈%d (limit 15, regex estimate)", frame.name, frame.cc),
				})
			}
		}
	}

	// Flush unclosed frames.
	for _, frame := range stack {
		if frame.cc > 15 {
			findings = append(findings, AuditFinding{
				File:    path,
				Line:    frame.startLine,
				Kind:    "COMPLEXITY",
				Message: fmt.Sprintf("fn %s: CC≈%d (limit 15, regex estimate)", frame.name, frame.cc),
			})
		}
	}

	return findings, nil
}
