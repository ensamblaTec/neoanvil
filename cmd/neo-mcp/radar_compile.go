package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/kanban"
)

// [SRE-102.B] symbolMapCache memoizes buildSymbolMap results so repeat
// COMPILE_AUDIT calls on the same package skip the go/ast parse when none
// of the files changed. Keyed by absolute directory + aggregated mtime.
var (
	symbolMapCache   = map[string]map[string]int{}
	symbolMapCacheMu sync.RWMutex
)

// handleCompileAudit reports build status, stale cert files, and a symbol_map of exported identifiers.
// It surfaces undefined symbols with nearest-match suggestions, import completeness, and cert seal status.
// [SRE-60.4] Eliminates manual grep loops for undefined symbols.
func (t *RadarTool) handleCompileAudit(_ context.Context, args map[string]any) (any, error) {
	target, _ := args["target"].(string)
	if target == "" {
		target = "./..."
	}
	// [SRE-105.A] target normalization: file (.go) → derive parent dir for symbol_map.
	targetResolved := "package:" + strings.TrimPrefix(target, "./")
	pkgArg := target
	if strings.HasSuffix(target, ".go") {
		pkgDir := filepath.Dir(target)
		if pkgDir == "." || pkgDir == "" {
			pkgDir = "./"
		}
		pkgArg = pkgDir
		targetResolved = "file:" + target
	}
	if !strings.HasPrefix(pkgArg, "./") && !strings.HasPrefix(pkgArg, "/") {
		pkgArg = "./" + pkgArg
	}

	buildStatus, errLines := runGoBuild(t.workspace, pkgArg)

	var sb strings.Builder
	fmt.Fprintf(&sb, "## COMPILE_AUDIT: `%s`\n\n", target)
	// [SRE-105.B] target_resolved tells the agent how the input was interpreted.
	fmt.Fprintf(&sb, "**target_resolved:** `%s`\n", targetResolved)
	fmt.Fprintf(&sb, "**build:** `%s`\n\n", buildStatus)
	if len(errLines) == 0 && buildStatus == "ok" {
		sb.WriteString("✅ Package compiles cleanly.\n")
	}
	sb.WriteString(formatBuildErrors(errLines, t.workspace, pkgArg))

	sb.WriteString("\n### Cert Status\n")
	stale := collectStaleCertFiles(t.workspace, target)
	if len(stale) == 0 {
		sb.WriteString("✅ All files certified.\n")
	} else {
		sb.WriteString("⚠️  Stale (needs neo_sre_certify_mutation):\n")
		for _, f := range stale {
			fmt.Fprintf(&sb, "- `%s`\n", f)
		}
	}

	// [SRE-65] AST Topography: symbol_map — exported (and optionally unexported) symbols with line numbers.
	includeUnexported, _ := args["include_unexported"].(bool)
	filterSym, _ := args["filter_symbol"].(string)
	sb.WriteString(renderSymbolMapSection(t.workspace, pkgArg, buildStatus, includeUnexported, filterSym))

	if len(errLines) > 0 {
		_ = kanban.AppendTechDebt(t.workspace,
			fmt.Sprintf("Compile errors in %s (%d errors)", target, len(errLines)),
			fmt.Sprintf("Package %s fails to build:\n%s", target, strings.Join(errLines, "\n")), "alta")
	}
	return mcpText(sb.String()), nil
}

// renderSymbolMapSection builds the "### Symbol Map" markdown section for COMPILE_AUDIT. [299.A]
// Extracted from handleCompileAudit to keep its CC below 15.
func renderSymbolMapSection(workspace, pkgArg, buildStatus string, includeUnexported bool, filterSym string) string {
	if pkgArg == "./..." {
		return "\n### Symbol Map\n_empty — \"./...\" (too broad — pass a single package or file)_\n"
	}
	symMap := buildSymbolMap(workspace, pkgArg, includeUnexported)
	if filterSym != "" {
		lf := strings.ToLower(filterSym)
		filtered := make(map[string]int, 8)
		for k, v := range symMap {
			if strings.Contains(strings.ToLower(k), lf) {
				filtered[k] = v
			}
		}
		symMap = filtered
	}
	if len(symMap) == 0 {
		if filterSym != "" {
			return "\n### Symbol Map\n_no matches for filter `" + filterSym + "`_\n"
		}
		reason := classifySymbolMapEmpty(workspace, pkgArg, buildStatus)
		log.Printf("[SRE-COMPILE_AUDIT] empty symbol_map for target=%s reason=%s", pkgArg, reason)
		return "\n### Symbol Map\n_empty — " + reason + "_\n"
	}
	var sb strings.Builder
	sb.WriteString("\n### Symbol Map\n")
	if includeUnexported {
		sb.WriteString("_includes unexported symbols_\n")
	}
	if filterSym != "" {
		fmt.Fprintf(&sb, "_filtered by `%s`_\n", filterSym)
	}
	sb.WriteString(formatSymbolMap(symMap))
	return sb.String()
}

// runGoBuild executes `go build pkgArg` in workspace and returns status + error lines. [SRE-119.A]
func runGoBuild(workspace, pkgArg string) (status string, errLines []string) {
	buildCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(buildCtx, "go", "build", pkgArg) //nolint:gosec // G204-LITERAL-BIN
	cmd.Dir = workspace
	out, buildErr := cmd.CombinedOutput()
	status = "ok"
	if buildErr != nil {
		status = "fail"
		for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				errLines = append(errLines, line)
			}
		}
	}
	return status, errLines
}

// formatBuildErrors formats compiler error lines with nearest-symbol suggestions. [SRE-119.A]
func formatBuildErrors(errLines []string, workspace, pkgArg string) string {
	var sb strings.Builder
	for _, line := range errLines {
		fmt.Fprintf(&sb, "```\n%s\n```\n", line)
		if strings.Contains(line, "undefined: ") {
			parts := strings.SplitN(line, "undefined: ", 2)
			if len(parts) == 2 {
				sym := strings.TrimSpace(parts[1])
				if match := findNearestSymbolInWorkspace(workspace, pkgArg, sym); match != "" {
					fmt.Fprintf(&sb, "  → **nearest match:** `%s`\n", match)
				}
			}
		}
	}
	return sb.String()
}

// formatSymbolMap renders a symbol→line map as a sorted JSON code block. [SRE-119.A]
func formatSymbolMap(symMap map[string]int) string {
	var sb strings.Builder
	sb.WriteString("```json\n{\n")
	keys := make([]string, 0, len(symMap))
	for k := range symMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		comma := ","
		if i == len(keys)-1 {
			comma = ""
		}
		fmt.Fprintf(&sb, "  %q: %d%s\n", k, symMap[k], comma)
	}
	sb.WriteString("}\n```\n")
	return sb.String()
}

// findNearestSymbolInWorkspace scans exported identifiers in the workspace for the best match
// to an undefined symbol, using prefix and case-insensitive heuristics. [SRE-60.4]
func findNearestSymbolInWorkspace(workspace, pkgArg, sym string) string {
	pkgDir := strings.TrimPrefix(pkgArg, "./")
	absDir := filepath.Join(workspace, pkgDir)
	if pkgArg == "./..." {
		absDir = workspace
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		return ""
	}

	symLower := strings.ToLower(sym)
	candidates := scanSymbolCandidates(absDir, entries)

	for _, c := range candidates {
		name := strings.SplitN(c, " ", 2)[0]
		if strings.EqualFold(name, sym) {
			return c
		}
	}
	for _, c := range candidates {
		name := strings.SplitN(c, " ", 2)[0]
		if strings.HasPrefix(strings.ToLower(name), symLower[:minInt(len(symLower), 4)]) {
			return c
		}
	}
	return ""
}

// scanSymbolCandidates reads each .go file in absDir via line scan and returns
// "Name (in file.go:N)" strings for all top-level declarations. [SRE-120.B]
func scanSymbolCandidates(absDir string, entries []os.DirEntry) []string {
	var candidates []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		src, readErr := os.ReadFile(filepath.Join(absDir, e.Name())) //nolint:gosec // G304-DIR-WALK
		if readErr != nil {
			continue
		}
		for lineNo, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			for _, kw := range []string{"func ", "type ", "var ", "const "} {
				if !strings.HasPrefix(trimmed, kw) {
					continue
				}
				rest := strings.TrimPrefix(trimmed, kw)
				ident := strings.FieldsFunc(rest, func(r rune) bool {
					return r == ' ' || r == '(' || r == '[' || r == '\t'
				})
				if len(ident) == 0 {
					continue
				}
				name := ident[0]
				if strings.HasPrefix(name, "(") {
					continue
				}
				candidates = append(candidates, fmt.Sprintf("%s (in %s:%d)", name, e.Name(), lineNo+1))
			}
		}
	}
	return candidates
}

// buildSymbolMap parses all non-test .go files in a package with go/ast and returns a symbol→line map.
// Exported-only by default; pass includeUnexported=true to include package-private symbols.
// Returns nil for ./... (too broad). [SRE-65/102.B cached]
func buildSymbolMap(workspace, pkgArg string, includeUnexported bool) map[string]int {
	if pkgArg == "./..." {
		return nil
	}
	pkgDir := strings.TrimPrefix(pkgArg, "./")
	absDir := filepath.Join(workspace, pkgDir)

	entries, err := os.ReadDir(absDir)
	if err != nil {
		return map[string]int{}
	}

	// [SRE-102.B] Cache key: absDir + sum of go-file mtimes + unexported flag.
	var latestMtime int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if info, infoErr := e.Info(); infoErr == nil {
			latestMtime += info.ModTime().UnixNano()
		}
	}
	cacheKey := fmt.Sprintf("%s@%d@%v", absDir, latestMtime, includeUnexported)
	symbolMapCacheMu.RLock()
	if cached, ok := symbolMapCache[cacheKey]; ok {
		symbolMapCacheMu.RUnlock()
		return cached
	}
	symbolMapCacheMu.RUnlock()

	symbols := parsePackageSymbols(absDir, includeUnexported)
	symbolMapCacheMu.Lock()
	symbolMapCache[cacheKey] = symbols
	symbolMapCacheMu.Unlock()
	return symbols
}

// parsePackageSymbols walks absDir with go/ast and returns a symbol→line map. [SRE-119.B]
func parsePackageSymbols(absDir string, includeUnexported bool) map[string]int {
	fset := token.NewFileSet()
	symbols := make(map[string]int)
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return symbols
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		parseFileSymbols(fset, filepath.Join(absDir, e.Name()), symbols, includeUnexported)
	}
	return symbols
}

// parseFileSymbols parses one .go file and writes symbols into the provided map.
// When includeUnexported is true, package-private funcs, methods, and types are included. [SRE-119.B]
func parseFileSymbols(fset *token.FileSet, absFile string, symbols map[string]int, includeUnexported bool) {
	f, parseErr := parser.ParseFile(fset, absFile, nil, 0)
	if parseErr != nil {
		return
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if !includeUnexported && !d.Name.IsExported() {
				continue
			}
			symbols[parseFuncDeclName(d)] = fset.Position(d.Pos()).Line
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && (includeUnexported || ts.Name.IsExported()) {
					symbols[ts.Name.Name] = fset.Position(ts.Pos()).Line
				}
			}
		}
	}
}

// parseFuncDeclName returns "ReceiverType.Method" for methods, or just the function name. [SRE-119.B]
func parseFuncDeclName(d *ast.FuncDecl) string {
	name := d.Name.Name
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return name
	}
	switch rt := d.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := rt.X.(*ast.Ident); ok {
			return id.Name + "." + name
		}
	case *ast.Ident:
		return rt.Name + "." + name
	}
	return name
}

// [SRE-105.B/C] classifySymbolMapEmpty inspects why buildSymbolMap returned an
// empty map so handleCompileAudit can surface a concrete reason instead of
// silently dropping the field. Cheap I/O — only runs on the empty path.
func classifySymbolMapEmpty(workspace, pkgArg, buildStatus string) string {
	pkgDir := strings.TrimPrefix(pkgArg, "./")
	absDir := filepath.Join(workspace, pkgDir)
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return fmt.Sprintf("directory unreadable: %v", err)
	}
	hasGo := false
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			hasGo = true
			break
		}
	}
	if !hasGo {
		return "no .go files in package directory"
	}
	if buildStatus != "ok" {
		return "parse errors in package (build failed)"
	}
	return "no exported symbols"
}

// collectStaleCertFiles reads the cert lock and returns files whose mtime > seal timestamp. [SRE-60.4]
func collectStaleCertFiles(workspace, target string) []string {
	lockPath := filepath.Join(workspace, ".neo", "db", "certified_state.lock")
	data, err := os.ReadFile(lockPath) //nolint:gosec // G304-WORKSPACE-CANON
	if err != nil {
		return nil // no lock file → can't determine staleness
	}

	// lockfile format: "/abs/path/file.go|unixTimestamp\n"
	sealed := make(map[string]int64)
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 {
			continue
		}
		var ts int64
		if _, scanErr := fmt.Sscan(parts[1], &ts); scanErr == nil {
			sealed[parts[0]] = ts
		}
	}

	pkgDir := strings.TrimPrefix(strings.TrimPrefix(target, "./"), "...")
	absDir := filepath.Join(workspace, pkgDir)

	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil
	}

	var stale []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		absFile := filepath.Join(absDir, e.Name())
		info, statErr := e.Info()
		if statErr != nil {
			continue
		}
		sealTS, ok := sealed[absFile]
		if !ok || info.ModTime().Unix() > sealTS {
			rel, _ := filepath.Rel(workspace, absFile)
			stale = append(stale, rel)
		}
	}
	return stale
}
