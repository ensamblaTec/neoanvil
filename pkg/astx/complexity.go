package astx

import (
	"go/ast"
	"go/parser"
	"go/token"
)

// CalculateCC computes the Cyclomatic Complexity of an AST node.
func CalculateCC(node ast.Node) int {
	if node == nil {
		return 0
	}

	// Base complexity is 1 for a function/method
	complexity := 1

	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			// Don't recurse into closures — each has its own CC budget.
			// Avoids falsely inflating the parent function's score.
			if n != node {
				return false
			}
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.CaseClause, *ast.CommClause:
			complexity++
		case *ast.BinaryExpr:
			if x.Op == token.LAND || x.Op == token.LOR {
				complexity++
			}
		}
		return true
	})

	return complexity
}

// CalculateFileCC computes total Cyclomatic Complexity for an entire source code string.
func CalculateFileCC(src []byte) (int, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, 0)
	if err != nil {
		return 0, err
	}

	total := 0
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			total += CalculateCC(fn)
		}
	}
	return total, nil
}
