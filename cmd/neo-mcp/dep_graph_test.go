package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/rag"
)

// mkFile creates an empty file at workspace-relative rel, making parent dirs.
func mkFile(t *testing.T, ws, rel string) {
	t.Helper()
	abs := filepath.Join(ws, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("// test\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceModulePath(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "go.mod"),
		[]byte("module example.com/foo\n\ngo 1.23\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := workspaceModulePath(ws); got != "example.com/foo" {
		t.Errorf("workspaceModulePath = %q, want example.com/foo", got)
	}
	// No go.mod → "" (non-Go workspace).
	if got := workspaceModulePath(t.TempDir()); got != "" {
		t.Errorf("workspaceModulePath(no go.mod) = %q, want \"\"", got)
	}
}

func TestGoImportToFiles(t *testing.T) {
	ws := t.TempDir()
	const mod = "example.com/foo"
	mkFile(t, ws, "pkg/bar/a.go")
	mkFile(t, ws, "pkg/bar/b.go")
	mkFile(t, ws, "pkg/bar/c_test.go") // must be excluded
	mkFile(t, ws, "pkg/bar/readme.md") // non-.go, excluded

	got := goImportToFiles(ws, mod, "example.com/foo/pkg/bar")
	sort.Strings(got)
	if len(got) != 2 || got[0] != "pkg/bar/a.go" || got[1] != "pkg/bar/b.go" {
		t.Errorf("goImportToFiles = %v, want [pkg/bar/a.go pkg/bar/b.go]", got)
	}

	// stdlib / third-party / module-root / missing-dir all resolve to nil.
	for _, imp := range []string{"fmt", "github.com/other/x", mod, "example.com/foo/pkg/missing"} {
		if got := goImportToFiles(ws, mod, imp); got != nil {
			t.Errorf("goImportToFiles(%q) = %v, want nil", imp, got)
		}
	}
	// Empty module path (non-Go workspace) → nil.
	if got := goImportToFiles(ws, "", "example.com/foo/pkg/bar"); got != nil {
		t.Errorf("goImportToFiles with empty module = %v, want nil", got)
	}
}

func TestFileDepEdges_Go(t *testing.T) {
	ws := t.TempDir()
	const mod = "example.com/foo"
	mkFile(t, ws, "pkg/bar/a.go")

	edges := fileDepEdges(ws, mod, "cmd/x/main.go", []string{"fmt", "example.com/foo/pkg/bar"})
	if len(edges) != 1 {
		t.Fatalf("fileDepEdges produced %d edges, want 1: %+v", len(edges), edges)
	}
	if edges[0].SourceNode != "cmd/x/main.go" || edges[0].TargetNode != "pkg/bar/a.go" ||
		edges[0].Relation != "imports" {
		t.Errorf("edge = %+v, want {cmd/x/main.go -> pkg/bar/a.go imports}", edges[0])
	}
}

func TestFileDepEdges_DropsSelfAndDupes(t *testing.T) {
	ws := t.TempDir()
	const mod = "example.com/foo"
	mkFile(t, ws, "pkg/bar/a.go")
	// main.go lives in pkg/bar itself → the a.go edge would include a self-edge
	// for pkg/bar/a.go; importing the same package twice must dedupe.
	edges := fileDepEdges(ws, mod, "pkg/bar/a.go",
		[]string{"example.com/foo/pkg/bar", "example.com/foo/pkg/bar"})
	if len(edges) != 0 {
		t.Errorf("fileDepEdges should drop self-edges + dupes, got %+v", edges)
	}
}

func TestRelativeImportToFile(t *testing.T) {
	ws := t.TempDir()
	mkFile(t, ws, "src/util.ts")
	mkFile(t, ws, "src/lib/index.ts")

	cases := []struct {
		name, srcRel, imp, ext, want string
	}{
		{"sibling ts", "src/app.ts", "./util", ".ts", "src/util.ts"},
		{"dir index", "src/app.ts", "./lib", ".ts", "src/lib/index.ts"},
		{"bare package", "src/app.ts", "react", ".ts", ""},
		{"unresolvable", "src/app.ts", "./missing", ".ts", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := relativeImportToFile(ws, tc.srcRel, tc.imp, tc.ext); got != tc.want {
				t.Errorf("relativeImportToFile(%q, %q) = %q, want %q", tc.srcRel, tc.imp, got, tc.want)
			}
		})
	}
}

// TestBackfillDepGraph covers the snapshot-boot safety net [1966953] + the
// cache-invalidation gen bump [71498e0]: a cold GRAPH_EDGES bucket must get
// populated from an import-only walk, graph.Gen must bump exactly once, and a
// second run over an already-populated graph must no-op (no re-bump).
func TestBackfillDepGraph(t *testing.T) {
	ws := t.TempDir()
	writeFile := func(rel, content string) {
		t.Helper()
		abs := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("go.mod", "module example.com/bf\n")
	writeFile("pkg/lib/lib.go", "package lib\n\nvar X = 1\n") // leaf — import target
	writeFile("cmd/app/main.go",
		"package main\n\nimport (\n\t\"fmt\"\n\t\"example.com/bf/pkg/lib\"\n)\n\nfunc main() { fmt.Println(lib.X) }\n")

	dbDir := filepath.Join(ws, ".neo", "db")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}
	wal, err := rag.OpenWAL(filepath.Join(dbDir, "hnsw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wal.Close() })
	if err := rag.InitGraphRAG(wal); err != nil {
		t.Fatal(err)
	}

	graph := &rag.Graph{}
	cfg := &config.NeoConfig{}
	cfg.Workspace.AllowedExtensions = []string{".go"}

	// First run: empty GRAPH_EDGES → backfill populates + bumps the gen.
	backfillDepGraph(ws, wal, graph, cfg)

	edges, err := rag.GetAllGraphEdges(wal)
	if err != nil {
		t.Fatal(err)
	}
	if deps := edges["cmd/app/main.go"]; len(deps) != 1 || deps[0] != "pkg/lib/lib.go" {
		t.Errorf("backfill edges for cmd/app/main.go = %v, want [pkg/lib/lib.go]", deps)
	}
	if g := graph.Gen.Load(); g != 1 {
		t.Errorf("graph.Gen after backfill = %d, want 1 (radar-cache invalidation bump)", g)
	}

	// Second run: graph already populated → no-op, gen must NOT re-bump.
	backfillDepGraph(ws, wal, graph, cfg)
	if g := graph.Gen.Load(); g != 1 {
		t.Errorf("graph.Gen after no-op backfill = %d, want 1 (must not re-bump a populated graph)", g)
	}
}
