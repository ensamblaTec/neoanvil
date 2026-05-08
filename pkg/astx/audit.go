package astx

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// AuditFinding represents a single SAST issue found in a source file.
type AuditFinding struct {
	File    string
	Line    int
	Kind    string // "COMPLEXITY", "INFINITE_LOOP", "SHADOW", "CC_SUMMARY"
	Message string
	CCValue int // Non-zero for COMPLEXITY and CC_SUMMARY findings; holds the CC estimate
}

// AuditGoFile runs static analysis on a Go source file and returns compact findings.
// Detects: cyclomatic complexity > 15, infinite loops, shadowed variables.
// [SRE-29.2.1/29.2.2/29.2.3]
func AuditGoFile(filename string, src []byte) ([]AuditFinding, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}

	var findings []AuditFinding

	// [SRE-29.2.1] Per-function cyclomatic complexity — warn if > 15.
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		cc := CalculateCC(fn)
		if cc > 15 {
			pos := fset.Position(fn.Pos())
			name := fn.Name.Name
			findings = append(findings, AuditFinding{
				File:    filename,
				Line:    pos.Line,
				Kind:    "COMPLEXITY",
				Message: fmt.Sprintf("func %s: CC=%d (limit 15)", name, cc),
			})
		}
	}

	// [SRE-29.2.2] Infinite loop detector: for {} body with no break/return/goto/panic at top level.
	ast.Inspect(f, func(n ast.Node) bool {
		forStmt, ok := n.(*ast.ForStmt)
		if !ok {
			return true
		}
		// A for {} has no Init, Cond, or Post.
		if forStmt.Init != nil || forStmt.Cond != nil || forStmt.Post != nil {
			return true
		}
		if forStmt.Body == nil {
			return true
		}
		if !bodyHasExit(forStmt.Body) {
			pos := fset.Position(forStmt.Pos())
			findings = append(findings, AuditFinding{
				File:    filename,
				Line:    pos.Line,
				Kind:    "INFINITE_LOOP",
				Message: "for{} with no break/return/goto/panic — potential livelock",
			})
		}
		return true
	})

	// [SRE-29.2.2] Shadowed variable detector: := in inner scope that shadows outer declaration.
	findings = append(findings, detectShadows(filename, fset, f)...)

	return findings, nil
}

// bodyHasExit checks if a block has at least one unconditional exit at top level.
func bodyHasExit(body *ast.BlockStmt) bool {
	if isForSelectBody(body) {
		return true
	}
	for _, stmt := range body.List {
		if stmtExits(stmt) {
			return true
		}
	}
	return false
}

// isForSelectBody returns true when the body is a single SelectStmt — a channel-driven event loop.
func isForSelectBody(body *ast.BlockStmt) bool {
	if len(body.List) != 1 {
		return false
	}
	_, ok := body.List[0].(*ast.SelectStmt)
	return ok
}

// stmtExits reports whether a single statement constitutes an exit or yield.
func stmtExits(stmt ast.Stmt) bool {
	switch s := stmt.(type) {
	case *ast.BranchStmt:
		return branchExits(s)
	case *ast.ReturnStmt:
		return true
	case *ast.ExprStmt:
		return isPanicCall(s) || isChannelReceive(s) || isSleepCall(s)
	case *ast.IfStmt:
		return ifHasExit(s)
	case *ast.SelectStmt:
		return selectHasExit(s)
	case *ast.AssignStmt:
		return isBlockingAccept(s)
	}
	return false
}

func branchExits(s *ast.BranchStmt) bool {
	return s.Tok == token.BREAK || s.Tok == token.RETURN || s.Tok == token.GOTO
}

func isPanicCall(s *ast.ExprStmt) bool {
	call, ok := s.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	ident, ok := call.Fun.(*ast.Ident)
	return ok && ident.Name == "panic"
}

// isChannelReceive returns true for `<-ch` expression statements (ticker/select yield — not a livelock).
func isChannelReceive(s *ast.ExprStmt) bool {
	u, ok := s.X.(*ast.UnaryExpr)
	return ok && u.Op == token.ARROW
}

// isSleepCall returns true for time.Sleep(...) — explicit yield, not a livelock.
func isSleepCall(s *ast.ExprStmt) bool {
	call, ok := s.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Sleep"
}

// isBlockingAccept returns true for `conn, err := ln.Accept()` or `conn, err := net.Dial(...)`
// — blocking I/O server loops; the goroutine yields to the OS on every iteration.
func isBlockingAccept(s *ast.AssignStmt) bool {
	if len(s.Rhs) != 1 {
		return false
	}
	call, ok := s.Rhs[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	name := sel.Sel.Name
	return name == "Accept" || name == "Dial"
}

func ifHasExit(s *ast.IfStmt) bool {
	if blockHasExit(s.Body) {
		return true
	}
	if s.Else == nil {
		return false
	}
	switch e := s.Else.(type) {
	case *ast.BlockStmt:
		return blockHasExit(e)
	case *ast.IfStmt:
		return bodyHasExit(&ast.BlockStmt{List: []ast.Stmt{e}})
	}
	return false
}

func selectHasExit(s *ast.SelectStmt) bool {
	for _, cc := range s.Body.List {
		clause, ok := cc.(*ast.CommClause)
		if !ok {
			continue
		}
		if clauseBodyHasExit(clause.Body) {
			return true
		}
	}
	return false
}

func clauseBodyHasExit(stmts []ast.Stmt) bool {
	for _, cstmt := range stmts {
		switch cs := cstmt.(type) {
		case *ast.ReturnStmt:
			return true
		case *ast.BranchStmt:
			if branchExits(cs) {
				return true
			}
		}
	}
	return false
}

func blockHasExit(b *ast.BlockStmt) bool {
	if b == nil {
		return false
	}
	return bodyHasExit(b)
}

// detectShadows finds variables declared with := in an inner scope that shadow an outer name.
func detectShadows(filename string, fset *token.FileSet, f *ast.File) []AuditFinding {
	var findings []AuditFinding

	// Walk each function independently.
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		// Collect top-level parameter names as outer scope.
		outerNames := map[string]bool{}
		if fn.Type.Params != nil {
			for _, field := range fn.Type.Params.List {
				for _, name := range field.Names {
					outerNames[name.Name] = true
				}
			}
		}
		walkBlockForShadows(filename, fset, fn.Body, outerNames, &findings)
	}
	return findings
}

// walkBlockForShadows recursively walks blocks looking for := that shadow names in outerScope.
func walkBlockForShadows(filename string, fset *token.FileSet, block *ast.BlockStmt, outerScope map[string]bool, findings *[]AuditFinding) {
	innerScope := map[string]bool{}
	for k, v := range outerScope {
		innerScope[k] = v
	}
	for _, stmt := range block.List {
		switch s := stmt.(type) {
		case *ast.AssignStmt:
			checkAssignShadows(filename, fset, s, outerScope, innerScope, findings)
		case *ast.BlockStmt:
			walkBlockForShadows(filename, fset, s, innerScope, findings)
		case *ast.IfStmt:
			walkIfShadows(filename, fset, s, innerScope, findings)
		case *ast.ForStmt:
			walkForShadows(filename, fset, s, outerScope, innerScope, findings)
		case *ast.RangeStmt:
			walkRangeShadows(filename, fset, s, outerScope, innerScope, findings)
		}
	}
}

func checkAssignShadows(filename string, fset *token.FileSet, s *ast.AssignStmt, outerScope, innerScope map[string]bool, findings *[]AuditFinding) {
	if s.Tok != token.DEFINE {
		return
	}
	for _, lhs := range s.Lhs {
		ident, ok := lhs.(*ast.Ident)
		if !ok || ident.Name == "_" {
			continue
		}
		if outerScope[ident.Name] {
			pos := fset.Position(ident.Pos())
			*findings = append(*findings, AuditFinding{
				File:    filename,
				Line:    pos.Line,
				Kind:    "SHADOW",
				Message: fmt.Sprintf("variable '%s' shadows outer scope declaration", ident.Name),
			})
		}
		innerScope[ident.Name] = true
	}
}

// walkIfShadows handles IfStmt shadow detection.
// Init-declared vars (if err := f(); ...) are added to scope but never flagged —
// idiomatic Go, consumed immediately by the condition.
func walkIfShadows(filename string, fset *token.FileSet, s *ast.IfStmt, innerScope map[string]bool, findings *[]AuditFinding) {
	ifScope := make(map[string]bool, len(innerScope))
	for k, v := range innerScope {
		ifScope[k] = v
	}
	if s.Init != nil {
		if assign, ok := s.Init.(*ast.AssignStmt); ok && assign.Tok == token.DEFINE {
			for _, lhs := range assign.Lhs {
				if ident, ok2 := lhs.(*ast.Ident); ok2 && ident.Name != "_" {
					ifScope[ident.Name] = true
				}
			}
		}
	}
	if s.Body != nil {
		walkBlockForShadows(filename, fset, s.Body, ifScope, findings)
	}
	if s.Else != nil {
		if elseBlock, ok := s.Else.(*ast.BlockStmt); ok {
			walkBlockForShadows(filename, fset, elseBlock, ifScope, findings)
		}
	}
}

// walkForShadows handles shadow detection for 3-clause `for` statements.
// The init stmt `for i := 0; ...` declares a new `i` that can shadow outer
// scope names. Go 1.22+ per-iteration semantics don't alter this rule. [330.D]
func walkForShadows(filename string, fset *token.FileSet, s *ast.ForStmt, outerScope, innerScope map[string]bool, findings *[]AuditFinding) {
	bodyOuter := copyScope(outerScope)
	if s.Init != nil {
		if assign, ok := s.Init.(*ast.AssignStmt); ok && assign.Tok == token.DEFINE {
			for _, lhs := range assign.Lhs {
				ident, ok2 := lhs.(*ast.Ident)
				if !ok2 || ident.Name == "_" {
					continue
				}
				if outerScope[ident.Name] {
					pos := fset.Position(ident.Pos())
					*findings = append(*findings, AuditFinding{
						File:    filename,
						Line:    pos.Line,
						Kind:    "SHADOW",
						Message: fmt.Sprintf("variable '%s' shadows outer scope declaration", ident.Name),
					})
				}
				bodyOuter[ident.Name] = true
			}
		}
	}
	if s.Body != nil {
		walkBlockForShadows(filename, fset, s.Body, bodyOuter, findings)
	}
	_ = innerScope // retained for API symmetry with walkRangeShadows
}

// walkRangeShadows handles shadow detection for range statements. [160.A]
// Range vars are checked against outerScope (params). The body's shadow-check scope starts
// from outerScope + range vars — NOT from the accumulated innerScope. This prevents FP
// when a loop body re-declares a function-body variable with := (common Go pattern).
// Sequential loops no longer pollute each other's shadow scope. [160.C]
func walkRangeShadows(filename string, fset *token.FileSet, s *ast.RangeStmt, outerScope, innerScope map[string]bool, findings *[]AuditFinding) {
	// bodyOuter: the set of names that count as "outer scope" for the loop body.
	// Starts from outerScope (params) + range vars declared by this loop.
	// Intentionally excludes function-body vars accumulated in innerScope.
	bodyOuter := copyScope(outerScope)

	if s.Tok == token.DEFINE {
		for _, lhs := range []ast.Expr{s.Key, s.Value} {
			if lhs == nil {
				continue
			}
			ident, ok := lhs.(*ast.Ident)
			if !ok || ident.Name == "_" {
				continue
			}
			if outerScope[ident.Name] {
				pos := fset.Position(ident.Pos())
				*findings = append(*findings, AuditFinding{
					File:    filename,
					Line:    pos.Line,
					Kind:    "SHADOW",
					Message: fmt.Sprintf("range variable '%s' shadows outer scope declaration", ident.Name),
				})
			}
			bodyOuter[ident.Name] = true
			// Range vars are scoped to the loop body in Go (per-iteration in 1.22+).
			// They do NOT leak to sibling stmts. Leaking into innerScope caused FP when a
			// later if-block copied it into its own scope and flagged an inner range reusing
			// the same name. [AUDIT-2026-04-23]
		}
	}
	if s.Body != nil {
		walkBlockForShadows(filename, fset, s.Body, bodyOuter, findings)
	}
	// Unused parameter retained for API compat with walkBlockForShadows callsite.
	_ = innerScope
}

// copyScope returns a shallow copy of a scope map.
func copyScope(src map[string]bool) map[string]bool {
	dst := make(map[string]bool, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// FormatAuditReport renders findings as ultra-compact plain text (max 15 lines). [SRE-29.2.3]
// CC_SUMMARY findings (Python cc_detail) are shown separately and not counted as issues.
func FormatAuditReport(findings []AuditFinding) string {
	var real, summaries []AuditFinding
	for _, f := range findings {
		if f.Kind == "CC_SUMMARY" {
			summaries = append(summaries, f)
		} else {
			real = append(real, f)
		}
	}

	var sb strings.Builder
	if len(real) == 0 {
		sb.WriteString("✅ AST_AUDIT: No issues found.")
	} else {
		sb.WriteString(fmt.Sprintf("⚠️  AST_AUDIT: %d issue(s)\n", len(real)))
		limit := len(real)
		truncated := false
		if limit > 14 {
			limit = 14
			truncated = true
		}
		for _, f := range real[:limit] {
			sb.WriteString(fmt.Sprintf("  [%s] %s:%d — %s\n", f.Kind, f.File, f.Line, f.Message))
		}
		if truncated {
			sb.WriteString(fmt.Sprintf("  ... and %d more (fix above first)\n", len(real)-14))
		}
	}

	// Append CC_SUMMARY section so agents can verify post-refactor CC without re-running.
	for _, s := range summaries {
		sb.WriteString(fmt.Sprintf("\n  📊 %s\n", s.Message))
	}

	return sb.String()
}
