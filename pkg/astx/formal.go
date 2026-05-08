package astx

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// DeadlockChecker implements a Resource Allocation Graph (RAG) model checker
type DeadlockChecker struct {
	Edges       map[string][]string
	ActiveLocks []string

	// WaitGroup Balance
	WgAddCount  int
	WgDoneCount int
}

// DetectDeadlockCycles constructs an AST topological graph of Lock sequences and runs a DFS for cycles.
func DetectDeadlockCycles(code string) error {
	source := code
	if !strings.Contains(code, "package ") {
		source = "package shadow_prm\n" + code
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", source, 0)
	if err != nil {
		return nil // Delegated to S1 Parses
	}

	checker := &DeadlockChecker{Edges: make(map[string][]string)}

	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if ok && fn.Body != nil {
			checker.ActiveLocks = []string{} // Reset per goroutine/func scope
			checker.scanBlock(fn.Body)

			// Si una función desbalancea localmente WaitGroups asintóticamente
			if checker.WgAddCount > 0 && checker.WgDoneCount == 0 {
				checker.WgAddCount = -1 // Señal de error
			}
		}
		return true // Traverse everything
	})

	if checker.WgAddCount == -1 {
		return fmt.Errorf("S9 WAITGROUP VETO: Asimetría Lineal Algebraica. Un wg.Add carece de alcance equivalente a wg.Done()")
	}

	return checker.findCycle()
}

func exprToString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprToString(e.X) + "." + e.Sel.Name
	}
	return ""
}

func (c *DeadlockChecker) scanBlock(block *ast.BlockStmt) {
	if block == nil {
		return
	}
	for _, stmt := range block.List {
		switch s := stmt.(type) {
		case *ast.DeferStmt:
			c.scanDeferStmt(s)
		case *ast.ExprStmt:
			c.scanExprStmt(s)
		case *ast.BlockStmt:
			c.scanBlock(s)
		case *ast.IfStmt:
			c.scanIfStmt(s)
		case *ast.ForStmt:
			c.scanBlock(s.Body)
		case *ast.RangeStmt:
			c.scanBlock(s.Body)
		case *ast.GoStmt:
			if lit, ok := s.Call.Fun.(*ast.FuncLit); ok {
				c.scanBlock(lit.Body)
			}
		}
	}
}

func (c *DeadlockChecker) scanDeferStmt(s *ast.DeferStmt) {
	if call, ok := s.Call.Fun.(*ast.SelectorExpr); ok {
		if call.Sel.Name == "Done" {
			c.WgDoneCount++
		}
	}
}

func (c *DeadlockChecker) scanExprStmt(s *ast.ExprStmt) {
	call, ok := s.X.(*ast.CallExpr)
	if !ok {
		return
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	lockName := exprToString(sel.X)
	if lockName == "" {
		return
	}
	c.applyLockOp(lockName, sel.Sel.Name)
}

func (c *DeadlockChecker) applyLockOp(lockName, op string) {
	switch op {
	case "Lock":
		for _, held := range c.ActiveLocks {
			c.Edges[held] = append(c.Edges[held], lockName)
		}
		c.ActiveLocks = append(c.ActiveLocks, lockName)
	case "Unlock":
		for i, held := range c.ActiveLocks {
			if held == lockName {
				c.ActiveLocks = append(c.ActiveLocks[:i], c.ActiveLocks[i+1:]...)
				break
			}
		}
	case "Add":
		if strings.Contains(lockName, "wg") {
			c.WgAddCount++
		}
	case "Done":
		c.WgDoneCount++
	}
}

func (c *DeadlockChecker) scanIfStmt(s *ast.IfStmt) {
	c.scanBlock(s.Body)
	if s.Else != nil {
		if elseBlock, ok := s.Else.(*ast.BlockStmt); ok {
			c.scanBlock(elseBlock)
		}
	}
}

// findCycle executes DFS finding topological Back-Edges mapping strict Deadlocks
func (c *DeadlockChecker) findCycle() error {
	visited := make(map[string]bool)
	stack := make(map[string]bool)

	var dfs func(node string) error
	dfs = func(node string) error {
		visited[node] = true
		stack[node] = true

		for _, neighbor := range c.Edges[node] {
			if !visited[neighbor] {
				if err := dfs(neighbor); err != nil {
					return err
				}
			} else if stack[neighbor] {
				return fmt.Errorf("S9 DEADLOCK VETO: Matriz interbloqueo %s -> %s confirmada sin simulación", node, neighbor)
			}
		}
		stack[node] = false
		return nil
	}

	for node := range c.Edges {
		if !visited[node] {
			if err := dfs(node); err != nil {
				return err
			}
		}
	}
	return nil
}
