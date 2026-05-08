package cpg

import "golang.org/x/tools/go/ssa"

// McCabeCC computes the exact McCabe Cyclomatic Complexity of an SSA function
// using the formula CC = E - N + 2, where E = number of CFG edges and N = number of blocks.
// Returns 1 for functions with no blocks (vacuous complexity).
func McCabeCC(fn *ssa.Function) int {
	if fn == nil || len(fn.Blocks) == 0 {
		return 1
	}
	n := len(fn.Blocks)
	e := 0
	for _, b := range fn.Blocks {
		e += len(b.Succs)
	}
	cc := e - n + 2
	if cc < 1 {
		return 1
	}
	return cc
}
