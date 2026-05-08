package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func fileExtractEndMap(absFile string) map[string]int {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, absFile, nil, 0)
	if err != nil {
		return nil
	}
	ends := make(map[string]int, len(f.Decls))
	for _, decl := range f.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok {
			ends[parseFuncDeclName(fd)] = fset.Position(fd.End()).Line
		}
	}
	return ends
}

// fileExtractMinQueryLen is the smallest accepted FILE_EXTRACT query. Single-character
// queries match nearly every line (letters inside identifiers) and produce useless
// import-block output. [AUDIT-2026-04-23]
const fileExtractMinQueryLen = 2

// fileExtractWordBoundaryThreshold: queries strictly shorter than this must match at a
// word boundary during the substring-scan fallback. Longer queries are specific enough
// that plain substring is fine. [AUDIT-2026-04-23]
const fileExtractWordBoundaryThreshold = 5

func fileExtractFindMatches(symMap map[string]int, allLines []string, queryLower string) []int {
	var hits []int
	for sym, lineNo := range symMap {
		sl := strings.ToLower(sym)
		if sl == queryLower || strings.HasSuffix(sl, "."+queryLower) {
			hits = append(hits, lineNo)
		}
	}
	if len(hits) > 0 {
		return hits
	}
	useWordBoundary := len(queryLower) < fileExtractWordBoundaryThreshold
	for i, line := range allLines {
		low := strings.ToLower(line)
		var ok bool
		if useWordBoundary {
			ok = containsAtWordBoundary(low, queryLower)
		} else {
			ok = strings.Contains(low, queryLower)
		}
		if !ok {
			continue
		}
		hits = append(hits, i+1)
		if len(hits) >= 3 {
			break
		}
	}
	return hits
}

// containsAtWordBoundary returns true when needle appears in hay with both edges
// abutting non-word characters (or string boundaries). Avoids letter-in-identifier
// noise when queries are short. [AUDIT-2026-04-23]
func containsAtWordBoundary(hay, needle string) bool {
	if needle == "" {
		return false
	}
	for offset := 0; offset < len(hay); {
		rel := strings.Index(hay[offset:], needle)
		if rel < 0 {
			return false
		}
		abs := offset + rel
		leftOK := abs == 0 || !isWordByte(hay[abs-1])
		rightOff := abs + len(needle)
		rightOK := rightOff == len(hay) || !isWordByte(hay[rightOff])
		if leftOK && rightOK {
			return true
		}
		offset = abs + 1
	}
	return false
}

func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
}

func fileExtractMergeWindows(hits []int, ctx, total int) [][2]int {
	var windows [][2]int
	for _, lineNo := range hits {
		s, e := max(lineNo-ctx, 1), min(lineNo+ctx, total)
		if len(windows) > 0 && s <= windows[len(windows)-1][1]+1 {
			windows[len(windows)-1][1] = max(windows[len(windows)-1][1], e)
		} else {
			windows = append(windows, [2]int{s, e})
		}
	}
	return windows
}

func (t *RadarTool) handleFileExtract(_ context.Context, args map[string]any) (any, error) {
	target, _ := args["target"].(string)
	if target == "" {
		return nil, fmt.Errorf("target (filepath) is required for FILE_EXTRACT")
	}
	contextLines := 5
	exactBody := false
	if cl, ok := args["context_lines"].(float64); ok {
		if cl == 0 {
			exactBody = true // [300.B] context_lines:0 → full symbol body via ast.Node.End()
		} else {
			contextLines = int(cl)
		}
	}
	absPath := target
	if !filepath.IsAbs(target) {
		absPath = filepath.Join(t.workspace, target)
	}
	data, err := os.ReadFile(absPath) //nolint:gosec // G304-WORKSPACE-CANON: absPath via filepath.Join(t.workspace,...), workspace is boot-pinned
	if err != nil {
		return nil, fmt.Errorf("FILE_EXTRACT: cannot read %s: %w", target, err)
	}
	allLines := strings.Split(string(data), "\n")
	total := len(allLines)
	relDir, _ := filepath.Rel(t.workspace, filepath.Dir(absPath))
	symMap := buildSymbolMap(t.workspace, "./"+relDir, true)

	// [317.A] Multi-symbol mode: symbols[] overrides query when present.
	if rawSyms, ok := args["symbols"].([]any); ok && len(rawSyms) > 0 {
		return t.handleFileExtractMulti(target, absPath, rawSyms, symMap, allLines, total, contextLines, exactBody)
	}
	query, _ := args["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("query (symbol or search term) is required for FILE_EXTRACT")
	}
	if len(strings.TrimSpace(query)) < fileExtractMinQueryLen {
		return nil, fmt.Errorf("FILE_EXTRACT: query %q too short (min %d chars) — single-letter queries match nearly every line", query, fileExtractMinQueryLen)
	}
	return mcpText(fileExtractSingle(absPath, target, query, symMap, allLines, total, contextLines, exactBody)), nil
}

func fileExtractSingle(absPath, target, query string, symMap map[string]int, allLines []string, total, contextLines int, exactBody bool) string {
	hits := fileExtractFindMatches(symMap, allLines, strings.ToLower(query))
	if len(hits) == 0 {
		return fmt.Sprintf("FILE_EXTRACT: `%s` — no match for `%s` in %d lines", target, query, total)
	}
	slices.Sort(hits)
	hits = slices.Compact(hits)
	if exactBody && len(hits) == 1 {
		if result := fileExtractExactBody(absPath, target, query, symMap, allLines, hits[0], total); result != "" {
			return result
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## FILE_EXTRACT: `%s` — query: `%s`\n\n", target, query)
	fileExtractRenderWindows(target, fileExtractMergeWindows(hits, contextLines, total), allLines, total, &sb)
	return sb.String()
}

func fileExtractRenderWindows(target string, windows [][2]int, allLines []string, total int, sb *strings.Builder) {
	for _, w := range windows {
		fmt.Fprintf(sb, "```go\n// %s:%d-%d\n", target, w[0], w[1])
		for i := w[0] - 1; i < w[1] && i < total; i++ {
			fmt.Fprintf(sb, "%4d\t%s\n", i+1, allLines[i])
		}
		sb.WriteString("```\n\n")
	}
}

func (t *RadarTool) handleFileExtractMulti(
	target, absPath string,
	rawSyms []any,
	symMap map[string]int,
	allLines []string,
	total, contextLines int,
	exactBody bool,
) (any, error) {
	const maxSymbols = 10
	if len(rawSyms) > maxSymbols {
		rawSyms = rawSyms[:maxSymbols]
	}
	allHits, notFound := fileExtractCollectHits(absPath, target, rawSyms, symMap, allLines, total, exactBody)

	var sb strings.Builder
	fmt.Fprintf(&sb, "## FILE_EXTRACT: `%s` — symbols: %d\n\n", target, len(rawSyms))
	if len(notFound) > 0 {
		fmt.Fprintf(&sb, "> **not found:** %s\n\n", strings.Join(notFound, ", "))
	}
	if len(allHits) == 0 {
		sb.WriteString("No matches found for any of the requested symbols.\n")
		return mcpText(sb.String()), nil
	}
	if exactBody {
		fileExtractMultiExact(absPath, target, rawSyms, symMap, allLines, total, &sb)
		return mcpText(sb.String()), nil
	}
	slices.Sort(allHits)
	allHits = slices.Compact(allHits)
	fileExtractRenderWindows(target, fileExtractMergeWindows(allHits, contextLines, total), allLines, total, &sb)
	return mcpText(sb.String()), nil
}

func fileExtractCollectHits(absPath, target string, rawSyms []any, symMap map[string]int, allLines []string, total int, exactBody bool) (hits []int, notFound []string) {
	for _, rs := range rawSyms {
		sym, _ := rs.(string)
		if sym == "" {
			continue
		}
		h := fileExtractFindMatches(symMap, allLines, strings.ToLower(sym))
		if len(h) == 0 {
			notFound = append(notFound, sym)
			continue
		}
		if exactBody && len(h) == 1 {
			if result := fileExtractExactBody(absPath, target, sym, symMap, allLines, h[0], total); result != "" {
				hits = append(hits, h[0])
				continue
			}
		}
		hits = append(hits, h...)
	}
	return hits, notFound
}

func fileExtractMultiExact(absPath, target string, rawSyms []any, symMap map[string]int, allLines []string, total int, sb *strings.Builder) {
	for _, rs := range rawSyms {
		sym, _ := rs.(string)
		if sym == "" {
			continue
		}
		hits := fileExtractFindMatches(symMap, allLines, strings.ToLower(sym))
		if len(hits) == 0 {
			continue
		}
		if result := fileExtractExactBody(absPath, target, sym, symMap, allLines, hits[0], total); result != "" {
			sb.WriteString(result)
			continue
		}
		fileExtractRenderWindows(target, fileExtractMergeWindows(hits, 5, total), allLines, total, sb)
	}
}

func fileExtractExactBody(absPath, displayPath, query string, symMap map[string]int, allLines []string, hitLine, total int) string {
	endMap := fileExtractEndMap(absPath)
	if endMap == nil {
		return ""
	}
	queryLower := strings.ToLower(query)
	for sym, startLine := range symMap {
		sl := strings.ToLower(sym)
		if startLine != hitLine || (sl != queryLower && !strings.HasSuffix(sl, "."+queryLower)) {
			continue
		}
		endLine, ok := endMap[sym]
		if !ok {
			return ""
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "## FILE_EXTRACT: `%s` — symbol: `%s` (full body)\n\n", displayPath, sym)
		fmt.Fprintf(&sb, "```go\n// %s:%d-%d\n", displayPath, startLine, endLine)
		for i := startLine - 1; i < endLine && i < total; i++ {
			fmt.Fprintf(&sb, "%4d\t%s\n", i+1, allLines[i])
		}
		sb.WriteString("```\n\n")
		return sb.String()
	}
	return ""
}
