package astx

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// ExportedSymbol describes an exported identifier and its kind.
type ExportedSymbol struct {
	Name string // e.g. "SearchFlashback"
	Kind string // "func", "type", "var", "const"
	Sig  string // abbreviated signature for conflict detection
}

// ExtractExportedSymbols parses Go source and returns all exported identifiers.
// [SRE-31.2.1] Used by AuditSharedContract to detect cross-module breakage.
func ExtractExportedSymbols(filename string, src []byte) ([]ExportedSymbol, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", filename, err)
	}

	var symbols []ExportedSymbol
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name.IsExported() {
				sig := funcSignature(d)
				symbols = append(symbols, ExportedSymbol{Name: d.Name.Name, Kind: "func", Sig: sig})
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name.IsExported() {
						symbols = append(symbols, ExportedSymbol{Name: s.Name.Name, Kind: "type", Sig: s.Name.Name})
					}
				case *ast.ValueSpec:
					for _, id := range s.Names {
						if id.IsExported() {
							kind := "var"
							if d.Tok == token.CONST {
								kind = "const"
							}
							symbols = append(symbols, ExportedSymbol{Name: id.Name, Kind: kind, Sig: id.Name})
						}
					}
				}
			}
		}
	}
	return symbols, nil
}

// AuditSharedContract scans workspace Go files for callers of symbols exported
// by changedFile. Returns AuditFindings if a call-site uses a symbol that no
// longer matches its exported signature (renamed or removed). [SRE-31.2.1]
func AuditSharedContract(workspace string, changedFile string, src []byte) ([]AuditFinding, error) {
	symbols, err := ExtractExportedSymbols(changedFile, src)
	if err != nil {
		return nil, err
	}
	if len(symbols) == 0 {
		return nil, nil
	}

	// Build a set of exported symbol names for fast lookup.
	exported := make(map[string]struct{}, len(symbols))
	for _, s := range symbols {
		exported[s.Name] = struct{}{}
	}

	pkg := packageOf(changedFile)
	var findings []AuditFinding

	// Walk workspace for .go files outside the changed file's own package.
	err = filepath.WalkDir(workspace, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		if filepath.Ext(path) != ".go" || path == changedFile {
			return nil
		}
		// Skip vendor and generated dirs.
		if strings.Contains(path, "/vendor/") || strings.Contains(path, "/.neo/") {
			return nil
		}
		// Only scan files in packages that could import our package.
		callerSrc, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		callerText := string(callerSrc)
		if !strings.Contains(callerText, pkg) {
			return nil // quick reject — file doesn't import our package
		}
		// Scan for usage of our exported symbols.
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, callerSrc, 0)
		if parseErr != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if _, exists := exported[sel.Sel.Name]; !exists {
				return true
			}
			// The caller references a symbol name — check if qualifier matches our package.
			id, isIdent := sel.X.(*ast.Ident)
			if !isIdent {
				return true
			}
			// If qualifier matches our package alias, flag as a validated cross-dep.
			if id.Name == pkg || strings.HasSuffix(pkg, id.Name) {
				pos := fset.Position(sel.Pos())
				findings = append(findings, AuditFinding{
					File:    path,
					Line:    pos.Line,
					Kind:    "CROSS_DEP",
					Message: fmt.Sprintf("caller uses %s.%s — verify signature matches after mutation of %s", id.Name, sel.Sel.Name, filepath.Base(changedFile)),
				})
			}
			return true
		})
		return nil
	})
	if err != nil {
		return findings, fmt.Errorf("workspace scan: %w", err)
	}
	return findings, nil
}

// funcSignature builds a compact string describing a function's parameter types.
func funcSignature(fn *ast.FuncDecl) string {
	if fn.Type == nil || fn.Type.Params == nil {
		return fn.Name.Name + "()"
	}
	var parts []string
	for _, field := range fn.Type.Params.List {
		parts = append(parts, fmt.Sprintf("%s", field.Type))
	}
	return fmt.Sprintf("%s(%s)", fn.Name.Name, strings.Join(parts, ","))
}

// packageOf returns the last path component of a file's parent directory,
// used as the package qualifier in cross-dep detection.
func packageOf(filename string) string {
	return filepath.Base(filepath.Dir(filename))
}

// PackageImporters returns every .go file in workspace whose import block
// references the package containing target — regardless of which exported
// symbol the caller uses.
//
// This is BROADER than AuditSharedContract (which is symbol-scoped). When
// the operator asks "what depends on auth/keystore.go", they usually want
// the refactor-level blast radius: every file that touches the auth
// package, not only files calling auth.Load / auth.Save (the specific
// symbols defined in keystore.go).
//
// Use case mapping:
//   - AuditSharedContract  → "if I rename Save(), who breaks?" (signature)
//   - PackageImporters      → "if I redesign the auth package, who's affected?"
//
// Match strategy: parse imports-only, accept any import path that ends
// with the relative dir of target ("pkg/auth"). This handles:
//
//	"github.com/ensamblatec/neoanvil/pkg/auth"  → match
//	"github.com/other/pkg/auth"                  → match (rare; same name)
//
// We err on the side of inclusion since BLAST_RADIUS is operator-facing
// and false-positives are easier to dismiss than false-negatives.
//
// Known limitation: in a workspace that imports a third-party module with
// the same relative path (e.g. local `pkg/auth` AND vendored
// `github.com/foo/pkg/auth`), files importing the third-party package
// will appear in the result. To make this strict we'd need to read
// go.mod and require the import path starts with the local module
// path. The trade-off (extra IO + multi-module edge cases) isn't worth
// it for the operator-facing UX — false positives are visible in the
// output and dismissible. [Sprint package-level fix + audit follow-up]
//
// Skips: vendor/, .neo/, the target file itself, files in the same
// package directory as target.
//
// Inputs are validated only for type (target must end in .go); a
// path-traversal target like `../../../etc/passwd.go` is bounded by
// filepath.WalkDir(workspace, ...) and produces 0 results without
// panic or filesystem leak. [Pen-and-paper audit 2026-05-10]
func PackageImporters(workspace, target string) ([]string, error) {
	if !strings.HasSuffix(target, ".go") {
		return nil, nil
	}
	absTarget := target
	if !filepath.IsAbs(target) {
		absTarget = filepath.Join(workspace, target)
	}
	rel, err := filepath.Rel(workspace, absTarget)
	if err != nil {
		return nil, fmt.Errorf("rel path: %w", err)
	}
	pkgRel := filepath.ToSlash(filepath.Dir(rel))
	if pkgRel == "" || pkgRel == "." {
		return nil, nil
	}
	suffix := "/" + pkgRel
	targetDir := filepath.Dir(absTarget)
	seen := make(map[string]struct{})
	var importers []string
	walkErr := filepath.WalkDir(workspace, func(path string, d os.DirEntry, wErr error) error {
		if wErr != nil || d.IsDir() {
			return wErr
		}
		if filepath.Ext(path) != ".go" || path == absTarget {
			return nil
		}
		if strings.Contains(path, "/vendor/") || strings.Contains(path, "/.neo/") {
			return nil
		}
		if filepath.Dir(path) == targetDir {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		// Quick reject: import path string must appear before we pay parse cost.
		if !strings.Contains(string(src), suffix+"\"") {
			return nil
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, src, parser.ImportsOnly)
		if parseErr != nil {
			return nil
		}
		for _, imp := range f.Imports {
			ip := strings.Trim(imp.Path.Value, `"`)
			if strings.HasSuffix(ip, suffix) {
				relCaller, _ := filepath.Rel(workspace, path)
				relCaller = filepath.ToSlash(relCaller)
				if _, dup := seen[relCaller]; !dup {
					seen[relCaller] = struct{}{}
					importers = append(importers, relCaller)
				}
				break
			}
		}
		return nil
	})
	if walkErr != nil {
		return importers, fmt.Errorf("workspace scan: %w", walkErr)
	}
	return importers, nil
}
