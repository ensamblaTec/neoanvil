package main

// blast_radius_lang.go — language-aware grep fallback for BLAST_RADIUS.
// PILAR XXIX: épicas 249-252.

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
)

// blastRadiusAutoFallbackThreshold is the index-coverage ratio below which
// BLAST_RADIUS skips the HNSW walk and uses the language-aware grep directly.
// [Épica 249.A]
const blastRadiusAutoFallbackThreshold = 0.20

// detectWorkspaceLang infers language from a target file's extension.
// Returns "go"|"python"|"rust"|"typescript"; defaults to "go". [Épica 249.B]
func detectWorkspaceLang(targetPath string) string {
	switch strings.ToLower(filepath.Ext(targetPath)) {
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".ts", ".tsx", ".js", ".jsx":
		return "typescript"
	default:
		return "go"
	}
}

// grepDependentsWithLinesLang dispatches to the language-specific grep fallback.
// Go callers pass lang:"go" and get the existing Go import scanner. [Épica 249.C]
func grepDependentsWithLinesLang(workspace, target, lang string) []dependentLine {
	switch lang {
	case "python":
		return grepDependentsWithLinesPython(workspace, target)
	case "rust":
		return grepDependentsWithLinesRust(workspace, target)
	case "typescript":
		return grepDependentsWithLinesTypeScript(workspace, target)
	default:
		return grepDependentsWithLines(workspace, target)
	}
}

// formatBlastRadiusAutoFallback renders the BLAST_RADIUS response when the
// auto-threshold fires (coverage < 20%) — skips HNSW entirely. [Épica 249.A]
func formatBlastRadiusAutoFallback(target string, hits []dependentLine, indexCoverage float64, lang string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "## BLAST_RADIUS: %s\n\n", target)
	sb.WriteString("**graph_status:** `not_indexed`  \n")
	fmt.Fprintf(&sb, "**index_coverage:** `%.0f%%`  \n", indexCoverage*100)
	sb.WriteString("**pagerank_used:** `false`  \n")
	sb.WriteString("**fallback:** `grep`  \n")
	fmt.Fprintf(&sb, "**fallback_trigger:** `auto (coverage < %.0f%%)`  \n", blastRadiusAutoFallbackThreshold*100)
	fmt.Fprintf(&sb, "**lang_detected:** `%s`  \n", lang)
	sb.WriteString("**confidence:** `medium`  \n")
	fmt.Fprintf(&sb, "**impacted_count:** %d  \n\n", len(hits))
	if len(hits) == 0 {
		sb.WriteString("_No callers found — workspace may have no files that import this target yet._\n")
		return sb.String()
	}
	sb.WriteString("### Callers _(auto-fallback grep — language-aware)_\n")
	for _, d := range hits {
		if d.CallerType != "" {
			fmt.Fprintf(&sb, "- %s:%d (%s)\n", d.File, d.Line, d.CallerType)
		} else if d.Line > 0 {
			fmt.Fprintf(&sb, "- %s:%d\n", d.File, d.Line)
		} else {
			fmt.Fprintf(&sb, "- %s\n", d.File)
		}
	}
	return sb.String()
}

// =============================================================================
// Python (Épica 250)
// =============================================================================

var pythonSkipDirs = map[string]bool{
	".neo": true, "vendor": true, "node_modules": true,
	"__pycache__": true, "site-packages": true,
	"venv": true, ".venv": true, "env": true,
	"data": true, "runs": true, "vision": true,
	".pytest_cache": true, ".mypy_cache": true,
	"htmlcov": true, ".ruff_cache": true,
	"lib": true, "build": true, "dist": true,
}

// pathToPythonModule converts a workspace-relative .py path to its Python
// module notation and basename. Edge case: __init__.py maps to the parent
// package. [Épica 250.B]
func pathToPythonModule(workspace, target string) (fullModule, basename string) {
	rel := target
	if filepath.IsAbs(target) {
		rel, _ = filepath.Rel(workspace, target)
	}
	rel = filepath.ToSlash(rel)

	if filepath.Base(rel) == "__init__.py" {
		dir := filepath.Dir(rel)
		basename = filepath.Base(dir)
		fullModule = strings.ReplaceAll(dir, "/", ".")
		return fullModule, basename
	}
	noExt := strings.TrimSuffix(rel, ".py")
	basename = filepath.Base(noExt)
	fullModule = strings.ReplaceAll(noExt, "/", ".")
	return fullModule, basename
}

// grepDependentsWithLinesPython finds Python callers of target by scanning
// import/from statements. Max 500 .py files to avoid timeouts. [Épica 250.A]
func grepDependentsWithLinesPython(workspace, target string) []dependentLine {
	const maxPyFiles = 500

	fullModule, basename := pathToPythonModule(workspace, target)
	targetRel := toRelSlash(workspace, target)

	// Build import patterns for this module.
	patterns := []string{
		"import " + fullModule,
		"from " + fullModule + " import",
	}
	parts := strings.Split(fullModule, ".")
	if len(parts) >= 2 {
		parentModule := strings.Join(parts[:len(parts)-1], ".")
		patterns = append(patterns, "from "+parentModule+" import "+basename)
		if len(parts) >= 3 {
			directParent := parts[len(parts)-2]
			patterns = append(patterns, "from .."+directParent+" import "+basename)
		}
	}
	patterns = append(patterns, "from ."+basename+" import")

	var results []dependentLine
	scanned := 0
	walkErr := filepath.WalkDir(workspace, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if pythonSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".py") {
			return nil
		}
		if scanned >= maxPyFiles {
			return filepath.SkipAll
		}
		rel := toRelSlash(workspace, path)
		if rel == targetRel {
			return nil
		}
		data, rErr := os.ReadFile(path) //nolint:gosec // G304-DIR-WALK
		if rErr != nil {
			return nil
		}
		scanned++
		for lineNum, line := range strings.Split(string(data), "\n") {
			for _, pat := range patterns {
				if strings.Contains(line, pat) {
					results = append(results, dependentLine{File: rel, Line: lineNum + 1})
					return nil // one entry per file
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		log.Printf("[BLAST-WARN] grepDependentsWithLinesPython: walk %s failed: %v", workspace, walkErr)
	}
	return results
}

// =============================================================================
// Rust (Épica 251)
// =============================================================================

var rustSkipDirs = map[string]bool{
	".neo": true, "vendor": true, "node_modules": true,
	"target": true,
}

// pathToRustModule converts a workspace-relative .rs path to its Rust crate
// path and basename. Edge case: mod.rs → parent dir is the module. [Épica 251.B]
func pathToRustModule(workspace, target string) (cratePath, basename string) {
	rel := target
	if filepath.IsAbs(target) {
		rel, _ = filepath.Rel(workspace, target)
	}
	rel = filepath.ToSlash(rel)

	if filepath.Base(rel) == "mod.rs" {
		dir := filepath.Dir(rel)
		basename = filepath.Base(dir)
		dir = strings.TrimPrefix(dir, "src/")
		cratePath = strings.ReplaceAll(dir, "/", "::")
		return cratePath, basename
	}
	noExt := strings.TrimSuffix(rel, ".rs")
	basename = filepath.Base(noExt)
	noExt = strings.TrimPrefix(noExt, "src/")
	cratePath = strings.ReplaceAll(noExt, "/", "::")
	return cratePath, basename
}

// grepDependentsWithLinesRust finds Rust callers of target by scanning use
// and mod declarations. mod declarations are tagged as "mod_decl". [Épica 251.A]
func grepDependentsWithLinesRust(workspace, target string) []dependentLine {
	cratePath, basename := pathToRustModule(workspace, target)
	targetRel := toRelSlash(workspace, target)

	usePatterns := []string{
		"use crate::" + cratePath,
		"use super::" + basename,
		"::" + basename,
	}
	modPatterns := []string{
		"mod " + basename + ";",
		"pub mod " + basename + ";",
	}

	var results []dependentLine
	walkErr := filepath.WalkDir(workspace, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if rustSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".rs") {
			return nil
		}
		rel := toRelSlash(workspace, path)
		if rel == targetRel {
			return nil
		}
		data, rErr := os.ReadFile(path) //nolint:gosec // G304-DIR-WALK
		if rErr != nil {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		for lineNum, line := range lines {
			for _, pat := range modPatterns {
				if strings.Contains(line, pat) {
					results = append(results, dependentLine{
						File:       rel,
						Line:       lineNum + 1,
						CallerType: "mod_decl",
					})
					return nil
				}
			}
			for _, pat := range usePatterns {
				if strings.Contains(line, pat) {
					results = append(results, dependentLine{File: rel, Line: lineNum + 1})
					return nil
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		log.Printf("[BLAST-WARN] grepDependentsWithLinesRust: walk %s failed: %v", workspace, walkErr)
	}
	return results
}

// =============================================================================
// TypeScript / JavaScript (Épica 252)
// =============================================================================

var tsSkipDirs = map[string]bool{
	".neo": true, "vendor": true, "node_modules": true,
	".next": true, "dist": true, "out": true, "build": true,
	".turbo": true, ".cache": true,
}

var tsFileExts = map[string]bool{
	".ts": true, ".tsx": true, ".js": true, ".jsx": true,
}

// readTSConfigPaths delegates to pkg/cpg.ReadTSConfigPaths (moved in Épica 257.A
// so bridge.go can share the alias resolver without circular imports).
func readTSConfigPaths(workspace string) map[string]string {
	return cpg.ReadTSConfigPaths(workspace)
}

// grepDependentsWithLinesTypeScript finds TypeScript/React callers of target
// by scanning import/from/require statements. [Épica 252.A]
// Resolves tsconfig aliases and relative paths. False-positive filter: only
// lines that start with import/from/require are considered. [Épica 252.C]
func grepDependentsWithLinesTypeScript(workspace, target string) []dependentLine {
	ext := filepath.Ext(target)
	basename := strings.TrimSuffix(filepath.Base(target), ext)
	targetRel := toRelSlash(workspace, target)
	targetNoExt := strings.TrimSuffix(targetRel, ext)

	// Compute alias-resolved path suffixes to search for in import strings.
	aliases := readTSConfigPaths(workspace)
	// Any import path ending with "/basename" (quote-terminated) matches.
	searchSuffixes := []string{
		"/" + basename + `"`,
		"/" + basename + `'`,
	}
	// Also add full alias paths (e.g. "@/components/Modal" → "@/components/Modal\"").
	for aliasPrefix, srcPrefix := range aliases {
		if strings.HasPrefix(targetNoExt, srcPrefix) {
			ap := aliasPrefix + strings.TrimPrefix(targetNoExt, srcPrefix)
			searchSuffixes = append(searchSuffixes, ap+`"`, ap+`'`)
		}
	}

	var results []dependentLine
	walkErr := filepath.WalkDir(workspace, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if tsSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !tsFileExts[filepath.Ext(path)] {
			return nil
		}
		rel := toRelSlash(workspace, path)
		if rel == targetRel {
			return nil
		}
		data, rErr := os.ReadFile(path) //nolint:gosec // G304-DIR-WALK
		if rErr != nil {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		for lineNum, line := range lines {
			trimmed := strings.TrimSpace(line)
			// [Épica 252.C] Reject non-import lines.
			if !strings.HasPrefix(trimmed, "import ") &&
				!strings.HasPrefix(trimmed, "from ") &&
				!strings.HasPrefix(trimmed, "require(") {
				continue
			}
			for _, suf := range searchSuffixes {
				if strings.Contains(line, suf) {
					results = append(results, dependentLine{File: rel, Line: lineNum + 1})
					return nil
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		log.Printf("[BLAST-WARN] grepDependentsWithLinesTypeScript: walk %s failed: %v", workspace, walkErr)
	}
	return results
}

// =============================================================================
// Shared helpers
// =============================================================================

// toRelSlash returns the workspace-relative slash path for an absolute or
// already-relative path. Used by all lang-specific grep functions.
func toRelSlash(workspace, path string) string {
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(workspace, path)
		if err != nil {
			return filepath.ToSlash(path)
		}
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(path)
}
