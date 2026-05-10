//go:build cpg_phases

// Phase-instrumented build benchmark — splits time into Load / SSA / Walk.
// Run with: go test -tags cpg_phases -v ./pkg/cpg/ -run TestPhases -timeout 5m

package cpg

import (
	"context"
	"fmt"
	"testing"
	"time"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// TestPhases_NeoanvilSelf builds the CPG for this repo (~700 .go files),
// reports the time spent in each phase. The Walk phase is the only one
// fully sequential in our code — Load + prog.Build() use Go's internal
// parallelism via GOMAXPROCS.
func TestPhases_NeoanvilSelf(t *testing.T) {
	dir := "/home/ensamblatec/go/src/github.com/ensamblatec/neoanvil"
	pkgPattern := "./cmd/neo-mcp" // production scope (cfg.CPG.PackagePath default)

	mode := packages.NeedName |
		packages.NeedFiles |
		packages.NeedCompiledGoFiles |
		packages.NeedImports |
		packages.NeedTypes |
		packages.NeedTypesSizes |
		packages.NeedSyntax |
		packages.NeedTypesInfo

	cfg := &packages.Config{
		Mode:    mode,
		Dir:     dir,
		Context: context.Background(),
	}

	start := time.Now()
	pkgs, err := packages.Load(cfg, pkgPattern)
	loadTime := time.Since(start)
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}

	start = time.Now()
	prog, _ := ssautil.AllPackages(pkgs, ssa.InstantiateGenerics)
	prog.Build()
	ssaTime := time.Since(start)

	b := NewBuilder(BuildConfig{Dir: dir})

	start = time.Now()
	g := newGraph()
	for _, p := range pkgs {
		ssaPkg := prog.Package(p.Types)
		if ssaPkg == nil {
			continue
		}
		b.walkPackage(g, ssaPkg)
	}
	walkTime := time.Since(start)

	total := loadTime + ssaTime + walkTime
	fmt.Println("\n┌──── CPG build phase breakdown (neoanvil self ./...) ───────────────┐")
	fmt.Printf("│ Phase                      │ Time         │ Share              │\n")
	fmt.Println("├────────────────────────────┼──────────────┼────────────────────┤")
	fmt.Printf("│ packages.Load              │ %-12v │ %5.1f%% (Go-parallel)│\n", loadTime.Round(time.Millisecond), 100*float64(loadTime)/float64(total))
	fmt.Printf("│ ssautil.AllPackages+Build  │ %-12v │ %5.1f%% (Go-parallel)│\n", ssaTime.Round(time.Millisecond), 100*float64(ssaTime)/float64(total))
	fmt.Printf("│ Walk packages (sequential) │ %-12v │ %5.1f%% (parallelizable)│\n", walkTime.Round(time.Millisecond), 100*float64(walkTime)/float64(total))
	fmt.Printf("│ TOTAL                      │ %-12v │                    │\n", total.Round(time.Millisecond))
	fmt.Println("└────────────────────────────┴──────────────┴────────────────────┘")
	fmt.Printf("Built: %d nodes / %d edges across %d packages\n", len(g.Nodes), len(g.Edges), len(pkgs))
}
