package astx

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
)

// LanguageAnalyzer runs static analysis on a source file.
// ctx should carry a deadline (recommended: 10s) for large files.
type LanguageAnalyzer interface {
	Analyze(ctx context.Context, path string, src []byte) ([]AuditFinding, error)
}

// LanguageRouter dispatches analysis to the registered LanguageAnalyzer for each file extension.
type LanguageRouter struct {
	m map[string]LanguageAnalyzer
}

// DefaultRouter is pre-configured with Go, Python, TypeScript/JS, and Rust analyzers.
var DefaultRouter = newDefaultRouter()

func newDefaultRouter() *LanguageRouter {
	r := &LanguageRouter{m: make(map[string]LanguageAnalyzer)}
	r.m[".go"] = GoAnalyzer{}
	r.m[".py"] = PythonAnalyzer{}
	r.m[".ts"] = TSAnalyzer{}
	r.m[".tsx"] = TSAnalyzer{}
	r.m[".js"] = TSAnalyzer{}
	r.m[".jsx"] = TSAnalyzer{}
	r.m[".rs"] = RustAnalyzer{}
	return r
}

// Register adds or replaces the analyzer for a file extension (include the dot, e.g. ".py").
func (r *LanguageRouter) Register(ext string, a LanguageAnalyzer) {
	r.m[ext] = a
}

// Lookup returns the analyzer for the file at path, keyed by extension. Returns false if none registered.
func (r *LanguageRouter) Lookup(path string) (LanguageAnalyzer, bool) {
	a, ok := r.m[filepath.Ext(path)]
	return a, ok
}

// globalCPGManager is the injection point for SSA-exact CC calculation in GoAnalyzer.
// Set once at boot via SetCPGManager; safe to read concurrently after that.
var globalCPGManager *cpg.Manager

// SetCPGManager injects the CPG manager so GoAnalyzer can refine CC findings
// with SSA-exact McCabe CC instead of the ast-regex approximation.
func SetCPGManager(m *cpg.Manager) { globalCPGManager = m }

// GoAnalyzer wraps AuditGoFile in the LanguageAnalyzer interface.
// When a CPG manager is available and ready, CC findings are replaced with
// SSA-exact McCabe CC (E-N+2). False positives (ast CC>15 but SSA CC≤15) are dropped.
// Each CC finding is annotated with [cc_method:ssa_exact] or [cc_method:ast_regex].
type GoAnalyzer struct{}

func (GoAnalyzer) Analyze(_ context.Context, path string, src []byte) ([]AuditFinding, error) {
	findings, err := AuditGoFile(path, src)
	if err != nil {
		return nil, err
	}

	mgr := globalCPGManager
	if mgr == nil {
		return findings, nil
	}
	ssaFuncs := mgr.SSAFunctions()
	if len(ssaFuncs) == 0 {
		return findings, nil
	}

	// Index SSA functions in this file by declaration line → exact McCabe CC.
	// Line-based matching avoids receiver-qualified name mismatches (e.g. SSA emits
	// "(*RadarTool).handleFoo" while ast emits "handleFoo").
	cleanPath := filepath.Clean(path)
	byLine := make(map[int]int) // start line → SSA McCabe CC
	for _, fn := range ssaFuncs {
		pos := fn.Pos()
		if !pos.IsValid() {
			continue
		}
		p := fn.Prog.Fset.Position(pos)
		if filepath.Clean(p.Filename) != cleanPath {
			continue
		}
		byLine[p.Line] = cpg.McCabeCC(fn)
	}
	if len(byLine) == 0 {
		return findings, nil
	}

	// Refine CC findings: replace ast-regex CC with SSA-exact CC.
	out := findings[:0]
	for _, f := range findings {
		if f.Kind != "COMPLEXITY" {
			out = append(out, f)
			continue
		}
		exactCC, hasSSA := byLine[f.Line]
		if hasSSA {
			if exactCC <= 15 {
				continue // ast-regex false positive — SSA confirms complexity is fine
			}
			name := parseCCFuncName(f.Message)
			f.Message = fmt.Sprintf("func %s: CC=%d (limit 15) [cc_method:ssa_exact]", name, exactCC)
		} else {
			f.Message += " [cc_method:ast_regex]"
		}
		out = append(out, f)
	}
	return out, nil
}

// parseCCFuncName extracts the function name from the standard CC finding message.
// Format: "func NAME: CC=N (limit 15)"
func parseCCFuncName(msg string) string {
	rest := strings.TrimPrefix(msg, "func ")
	name, _, _ := strings.Cut(rest, ":")
	return strings.TrimSpace(name)
}
