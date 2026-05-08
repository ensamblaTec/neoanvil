package cpg

import (
	"context"
	"fmt"
	"go/token"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// BuildConfig controls CPGBuilder behavior.
type BuildConfig struct {
	// Dir is the working directory for package resolution (default: "").
	Dir string
	// BuildFlags are extra flags forwarded to the Go toolchain (e.g. build tags).
	BuildFlags []string
	// WithTransitiveDeps adds NeedDeps to the packages.Load mode, loading all
	// transitive dependency packages into SSA. 142.D benchmark result: adds 1.18s
	// with zero change in node/edge count for cmd/neo-mcp (go/ssa resolves static
	// callees without NeedDeps). Default false (449ms) is the recommended mode.
	WithTransitiveDeps bool
}

// CPGBuilder constructs a Graph from a Go package pattern (e.g. "./cmd/neo-mcp").
type CPGBuilder struct {
	cfg      BuildConfig
	ssaFuncs []*ssa.Function // populated after Build(), used by SSAFunctions()
}

// NewBuilder returns a CPGBuilder with the given config.
func NewBuilder(cfg BuildConfig) *CPGBuilder {
	return &CPGBuilder{cfg: cfg}
}

// Build loads pkgPattern, constructs SSA, and returns the in-memory CPG.
// pkgPattern follows go/packages syntax: "./pkg/rag", "github.com/ensamblatec/neoanvil/pkg/rag", etc.
// ctx is forwarded to packages.Load — cancel it to abort a long build.
func (b *CPGBuilder) Build(ctx context.Context, pkgPattern string) (*Graph, error) {
	fset := token.NewFileSet()

	// Default mode: NeedDeps excluded — 142.D benchmark shows identical node/edge
	// counts without it (449ms vs 1.63s). Opt in via WithTransitiveDeps for deep analysis.
	mode := packages.NeedName |
		packages.NeedFiles |
		packages.NeedCompiledGoFiles |
		packages.NeedImports |
		packages.NeedTypes |
		packages.NeedTypesSizes |
		packages.NeedSyntax |
		packages.NeedTypesInfo
	if b.cfg.WithTransitiveDeps {
		mode |= packages.NeedDeps
	}

	cfg := &packages.Config{
		Mode:       mode,
		Fset:       fset,
		Dir:        b.cfg.Dir,
		BuildFlags: b.cfg.BuildFlags,
		Context:    ctx,
	}

	pkgs, err := packages.Load(cfg, pkgPattern)
	if err != nil {
		return nil, fmt.Errorf("cpg: load %q: %w", pkgPattern, err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("cpg: no packages matched %q", pkgPattern)
	}

	// Report any load errors but continue with what loaded successfully.
	var loadErrs []string
	for _, p := range pkgs {
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, e.Error())
		}
	}

	prog, _ := ssautil.AllPackages(pkgs, ssa.InstantiateGenerics)
	prog.Build()

	b.ssaFuncs = b.ssaFuncs[:0] // reset before collecting
	g := newGraph()
	for _, p := range pkgs {
		ssaPkg := prog.Package(p.Types)
		if ssaPkg == nil {
			continue
		}
		b.walkPackage(g, ssaPkg)
	}

	if len(g.Nodes) == 0 && len(loadErrs) > 0 {
		return nil, fmt.Errorf("cpg: load errors: %v", loadErrs)
	}
	return g, nil
}

// SSAFunctions returns the SSA functions collected during the most recent Build() call.
// Useful for computing McCabeCC on individual functions.
func (b *CPGBuilder) SSAFunctions() []*ssa.Function {
	return b.ssaFuncs
}

func (b *CPGBuilder) walkPackage(g *Graph, pkg *ssa.Package) {
	pkgName := pkg.Pkg.Path()

	for _, member := range pkg.Members {
		fn, ok := member.(*ssa.Function)
		if !ok {
			continue
		}
		b.walkFunction(g, pkgName, fn)
	}
}

func (b *CPGBuilder) walkFunction(g *Graph, pkgName string, fn *ssa.Function) {
	if fn.Blocks == nil {
		return // external / declared-only function
	}

	b.ssaFuncs = append(b.ssaFuncs, fn)

	pos := fn.Pos()
	line := 0
	file := ""
	if pos.IsValid() {
		p := fn.Prog.Fset.Position(pos)
		line = p.Line
		file = p.Filename
	}

	fnNode := g.addNode(Node{
		Kind:    NodeFunc,
		Package: pkgName,
		File:    file,
		Name:    fn.Name(),
		Line:    line,
	})

	// Walk basic blocks to build CFG edges.
	blockIDs := make(map[*ssa.BasicBlock]NodeID, len(fn.Blocks))
	for _, blk := range fn.Blocks {
		blkNode := g.addNode(Node{
			Kind:     NodeBlock,
			Package:  pkgName,
			Name:     fmt.Sprintf("%s$blk%d", fn.Name(), blk.Index),
			SSAValue: fmt.Sprintf("block %d", blk.Index),
		})
		g.addEdge(fnNode, blkNode, EdgeContain)
		blockIDs[blk] = blkNode
	}

	for _, blk := range fn.Blocks {
		fromID := blockIDs[blk]
		for _, succ := range blk.Succs {
			toID := blockIDs[succ]
			g.addEdge(fromID, toID, EdgeCFG)
		}

		// Emit call edges from Call instructions.
		for _, instr := range blk.Instrs {
			var callee *ssa.Function
			switch v := instr.(type) {
			case *ssa.Call:
				callee = v.Call.StaticCallee()
			case *ssa.Go:
				callee = v.Call.StaticCallee()
			case *ssa.Defer:
				callee = v.Call.StaticCallee()
			}
			if callee == nil || callee.Package() == nil {
				continue // skip builtins and synthetic wrappers
			}
			calleePos := callee.Pos()
			calleeLine := 0
			if calleePos.IsValid() {
				calleeLine = fn.Prog.Fset.Position(calleePos).Line
			}
			calleeNode := g.addNode(Node{
				Kind:    NodeFunc,
				Package: callee.Package().Pkg.Path(),
				Name:    callee.Name(),
				Line:    calleeLine,
			})
			g.addEdge(fnNode, calleeNode, EdgeCall)
		}
	}

	// Recurse into anonymous functions defined inside this one.
	for _, anon := range fn.AnonFuncs {
		b.walkFunction(g, pkgName, anon)
	}
}
