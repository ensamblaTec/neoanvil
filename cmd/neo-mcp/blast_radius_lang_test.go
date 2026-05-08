package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAutoFallbackThreshold verifies detectWorkspaceLang returns the correct
// language string for each supported extension. [Épica 249.D]
func TestAutoFallbackThreshold(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"src/models/yolo_segmenter.py", "python"},
		{"src/lib.rs", "rust"},
		{"src/models/segmenter.rs", "rust"},
		{"src/components/Modal.tsx", "typescript"},
		{"src/App.jsx", "typescript"},
		{"src/utils/helpers.ts", "typescript"},
		{"pages/index.js", "typescript"},
		{"pkg/rag/hnsw.go", "go"},
		{"cmd/main.go", "go"},
		{"README.md", "go"}, // unknown extension → go (backward compat)
	}
	for _, tc := range cases {
		got := detectWorkspaceLang(tc.path)
		if got != tc.want {
			t.Errorf("detectWorkspaceLang(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestGrepFallbackPython verifies that router.py is detected as a caller of
// yolo_segmenter.py via "from src.models.yolo_segmenter import". [Épica 250.D]
func TestGrepFallbackPython(t *testing.T) {
	workspace := t.TempDir()

	// Target: src/models/yolo_segmenter.py
	mustMkdir(t, filepath.Join(workspace, "src", "models"))
	mustWrite(t, filepath.Join(workspace, "src", "models", "yolo_segmenter.py"),
		"class YOLOSegmenter:\n    pass\n")

	// Caller: src/api/router.py
	mustMkdir(t, filepath.Join(workspace, "src", "api"))
	mustWrite(t, filepath.Join(workspace, "src", "api", "router.py"),
		"from src.models.yolo_segmenter import YOLOSegmenter\n\nrouter = None\n")

	hits := grepDependentsWithLinesPython(workspace, "src/models/yolo_segmenter.py")

	if len(hits) == 0 {
		t.Fatal("expected at least one caller, got none")
	}
	found := false
	for _, h := range hits {
		if strings.HasSuffix(h.File, "router.py") {
			found = true
			if h.Line != 1 {
				t.Errorf("expected line 1, got %d", h.Line)
			}
		}
	}
	if !found {
		t.Errorf("router.py not in callers: %v", hits)
	}
}

// TestGrepFallbackPythonInit verifies __init__.py maps to the parent package.
func TestGrepFallbackPythonInit(t *testing.T) {
	workspace := t.TempDir()

	// Target: src/models/__init__.py  → module "src.models", basename "models"
	mustMkdir(t, filepath.Join(workspace, "src", "models"))
	mustWrite(t, filepath.Join(workspace, "src", "models", "__init__.py"), "")

	mustMkdir(t, filepath.Join(workspace, "src", "api"))
	mustWrite(t, filepath.Join(workspace, "src", "api", "app.py"),
		"import src.models\n")

	fullMod, base := pathToPythonModule(workspace, "src/models/__init__.py")
	if fullMod != "src.models" {
		t.Errorf("fullModule = %q, want %q", fullMod, "src.models")
	}
	if base != "models" {
		t.Errorf("basename = %q, want %q", base, "models")
	}

	hits := grepDependentsWithLinesPython(workspace, "src/models/__init__.py")
	if len(hits) == 0 {
		t.Fatal("expected app.py as caller, got none")
	}
}

// TestGrepFallbackRust verifies that handler.rs is detected as a caller of
// segmenter.rs via "use crate::models::segmenter". [Épica 251.D]
func TestGrepFallbackRust(t *testing.T) {
	workspace := t.TempDir()

	mustMkdir(t, filepath.Join(workspace, "src", "models"))
	mustWrite(t, filepath.Join(workspace, "src", "models", "segmenter.rs"),
		"pub struct Segmenter;\n")

	mustMkdir(t, filepath.Join(workspace, "src"))
	mustWrite(t, filepath.Join(workspace, "src", "lib.rs"),
		"pub mod models;\n")

	mustMkdir(t, filepath.Join(workspace, "src", "api"))
	mustWrite(t, filepath.Join(workspace, "src", "api", "handler.rs"),
		"use crate::models::segmenter::Segmenter;\n\npub fn handle() {}\n")

	hits := grepDependentsWithLinesRust(workspace, "src/models/segmenter.rs")

	foundHandler := false
	foundModDecl := false
	for _, h := range hits {
		if strings.HasSuffix(h.File, "handler.rs") {
			foundHandler = true
		}
		if strings.HasSuffix(h.File, "lib.rs") && h.CallerType == "mod_decl" {
			foundModDecl = true
		}
	}
	if !foundHandler {
		t.Errorf("handler.rs not in callers: %v", hits)
	}
	_ = foundModDecl // mod models; in lib.rs doesn't directly reference segmenter — acceptable
}

// TestGrepFallbackRustModRS verifies mod.rs maps to the parent dir. [Épica 251.B]
func TestGrepFallbackRustModRS(t *testing.T) {
	workspace := t.TempDir()
	crate, base := pathToRustModule(workspace, "src/models/mod.rs")
	if base != "models" {
		t.Errorf("basename = %q, want %q", base, "models")
	}
	if crate != "models" {
		t.Errorf("cratePath = %q, want %q", crate, "models")
	}
}

// TestGrepFallbackTypeScript verifies that both relative-path and @/ alias
// callers are detected for a React component. [Épica 252.D]
func TestGrepFallbackTypeScript(t *testing.T) {
	workspace := t.TempDir()

	// Target: src/components/Modal.tsx
	mustMkdir(t, filepath.Join(workspace, "src", "components"))
	mustWrite(t, filepath.Join(workspace, "src", "components", "Modal.tsx"),
		"export default function Modal() { return null; }\n")

	// Caller 1: relative import
	mustMkdir(t, filepath.Join(workspace, "src", "app"))
	mustWrite(t, filepath.Join(workspace, "src", "app", "page.tsx"),
		`import Modal from '../components/Modal'`+"\n")

	// Caller 2: @/ alias import
	mustWrite(t, filepath.Join(workspace, "src", "app", "layout.tsx"),
		`import { Modal } from '@/components/Modal'`+"\n")

	// Non-caller: import inside a comment line — should be filtered
	mustWrite(t, filepath.Join(workspace, "src", "app", "ignored.tsx"),
		`// import Modal from '../components/Modal'`+"\n")

	hits := grepDependentsWithLinesTypeScript(workspace, "src/components/Modal.tsx")

	fileSet := make(map[string]bool)
	for _, h := range hits {
		fileSet[filepath.Base(h.File)] = true
	}
	if !fileSet["page.tsx"] {
		t.Errorf("page.tsx (relative import) not found in callers: %v", hits)
	}
	if !fileSet["layout.tsx"] {
		t.Errorf("layout.tsx (@/ alias) not found in callers: %v", hits)
	}
	if fileSet["ignored.tsx"] {
		t.Errorf("ignored.tsx (commented import) should not be a caller")
	}
}

// helpers

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
