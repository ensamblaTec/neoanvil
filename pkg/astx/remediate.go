package astx

// [SRE-88.A] AST Auto-Remediation — detects zero-alloc violations and generates
// patch suggestions. Does NOT modify files directly — returns a textual diff
// that the agent (pair mode) or daemon can apply.
//
// Supported transformations:
//   - make() in for/range loops → sync.Pool suggestion
//   - interface{}/any in func params where callers use concrete type → type hint
//
// This is heuristic, not guaranteed correct. Confidence is always "medium".

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// Remediation is a suggested AST fix for a zero-alloc violation.
type Remediation struct {
	File       string `json:"file"`
	Line       int    `json:"line"`
	Rule       string `json:"rule"`        // e.g. "MAKE_IN_LOOP", "INTERFACE_ANY"
	Message    string `json:"message"`
	Suggestion string `json:"suggestion"`  // human-readable fix description
	Confidence string `json:"confidence"`  // "high" | "medium" | "low"
}

// DetectRemediations scans a Go source file and returns suggested zero-alloc fixes.
// This is a static analysis pass — no compilation or execution.
func DetectRemediations(filename string, src []byte) ([]Remediation, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.AllErrors)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", filename, err)
	}

	var remediations []Remediation
	remediations = append(remediations, detectMakeInLoops(filename, fset, f)...)
	remediations = append(remediations, detectBareInterface(filename, fset, f)...)
	return remediations, nil
}

// FormatRemediationReport produces a Markdown summary of suggested fixes.
func FormatRemediationReport(rems []Remediation) string {
	if len(rems) == 0 {
		return "No zero-alloc violations detected."
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("## Auto-Remediation: %d suggestion(s)\n\n", len(rems)))
	for i, r := range rems {
		b.WriteString(fmt.Sprintf("### %d. [%s] %s:%d\n", i+1, r.Rule, r.File, r.Line))
		b.WriteString(fmt.Sprintf("**Issue:** %s\n", r.Message))
		b.WriteString(fmt.Sprintf("**Fix:** %s\n", r.Suggestion))
		b.WriteString(fmt.Sprintf("**Confidence:** %s\n\n", r.Confidence))
	}
	return b.String()
}

// detectMakeInLoops finds make() calls inside for/range bodies. [SRE-88.A.1]
func detectMakeInLoops(filename string, fset *token.FileSet, f *ast.File) []Remediation {
	var rems []Remediation

	ast.Inspect(f, func(n ast.Node) bool {
		// Look for for-statements and range-statements.
		var body *ast.BlockStmt
		switch stmt := n.(type) {
		case *ast.ForStmt:
			body = stmt.Body
		case *ast.RangeStmt:
			body = stmt.Body
		default:
			return true
		}
		if body == nil {
			return true
		}

		// Walk the loop body looking for make() calls.
		ast.Inspect(body, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "make" {
				return true
			}

			pos := fset.Position(call.Pos())
			typeArg := "[]T"
			if len(call.Args) > 0 {
				typeArg = formatNode(fset, call.Args[0])
			}

			rems = append(rems, Remediation{
				File:       filename,
				Line:       pos.Line,
				Rule:       "MAKE_IN_LOOP",
				Message:    fmt.Sprintf("make(%s) inside loop body — allocates on every iteration", typeArg),
				Suggestion: fmt.Sprintf("Use sync.Pool: declare `var pool = sync.Pool{New: func() any { return make(%s, 0, cap) }}` outside the loop. Inside: `buf := pool.Get().(%s)[:0]` ... `defer pool.Put(buf)`", typeArg, typeArg),
				Confidence: "medium",
			})
			return true
		})
		return true
	})
	return rems
}

// detectBareInterface finds interface{} or any in function parameters. [SRE-88.A.2]
func detectBareInterface(filename string, fset *token.FileSet, f *ast.File) []Remediation {
	var rems []Remediation

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Type != nil && fn.Type.Params != nil {
			for _, field := range fn.Type.Params.List {
				if isInterfaceType(field.Type) {
					pos := fset.Position(field.Pos())
					name := ""
					if len(field.Names) > 0 {
						name = field.Names[0].Name
					}
					rems = append(rems, Remediation{
						File:       filename,
						Line:       pos.Line,
						Rule:       "INTERFACE_ANY",
						Message:    fmt.Sprintf("Parameter '%s' uses interface{}/any — weakens type safety", name),
						Suggestion: "If all callers pass the same concrete type, replace with that type. Check callers with BLAST_RADIUS first.",
						Confidence: "low",
					})
				}
			}
		}
	}
	return rems
}

// isInterfaceType checks if an AST type expression is interface{} or any.
func isInterfaceType(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.InterfaceType:
		return t.Methods == nil || len(t.Methods.List) == 0
	case *ast.Ident:
		return t.Name == "any"
	}
	return false
}

// formatNode returns a short string representation of an AST node.
func formatNode(fset *token.FileSet, n ast.Node) string {
	pos := fset.Position(n.Pos())
	end := fset.Position(n.End())
	if pos.Filename == "" {
		return "T"
	}
	_ = end
	switch t := n.(type) {
	case *ast.ArrayType:
		return "[]" + formatNode(fset, t.Elt)
	case *ast.MapType:
		return "map[" + formatNode(fset, t.Key) + "]" + formatNode(fset, t.Value)
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return formatNode(fset, t.X) + "." + t.Sel.Name
	default:
		return "T"
	}
}
