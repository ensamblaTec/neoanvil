package brain

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// makeFakeWorkspace populates dir with a small synthetic file tree:
// some included files, some excluded directories, one big file we can
// use to test caps. Returns the included rel paths the test should see.
func makeFakeWorkspace(t *testing.T, dir string) []string {
	t.Helper()
	create := func(rel string, body []byte) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	create("README.md", []byte("# fake repo\n"))
	create("main.go", []byte("package main\nfunc main(){}\n"))
	create("docs/intro.md", []byte("intro text\n"))

	// Excluded by directory rule (.git, bin, node_modules)
	create(".git/config", []byte("[core]\n"))
	create("bin/neo", []byte("BINARY"))
	create("node_modules/foo/index.js", []byte("module.exports={};"))

	// Excluded by suffix (.log)
	create("server.log", []byte("ERROR something\n"))

	// Excluded by path fragment (.neo/pki/)
	create(".neo/pki/server.crt", []byte("CERT"))
	create(".neo/db/hnsw.db", []byte("HNSW"))
	// .neo/master_plan.md should NOT be excluded (only db/, pki/, logs/ are)
	create(".neo/master_plan.md", []byte("plan\n"))

	// Sort for deterministic comparison.
	expected := []string{
		".neo/master_plan.md",
		"README.md",
		"docs/intro.md",
		"main.go",
	}
	slices.Sort(expected)
	return expected
}

// TestBuildArchive_IncludesAndExcludes — round-trip through tar+zstd:
// build, extract, verify the file set matches expectations. Excluded
// paths must NOT appear; included paths must.
func TestBuildArchive_IncludesAndExcludes(t *testing.T) {
	src := t.TempDir()
	expected := makeFakeWorkspace(t, src)

	m := NewManifest(
		[]WalkedWorkspace{{
			ID:          "fake-1",
			Path:        src,
			Name:        "fake",
			CanonicalID: "github.com/x/fake",
		}},
		nil, nil,
	)

	var buf bytes.Buffer
	written, err := BuildArchive(m, &buf)
	if err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}
	if written == 0 {
		t.Fatal("BuildArchive wrote zero bytes")
	}
	// Manifest.Files is rewritten in place — verify the manifest reflects
	// what got included.
	got := m.Workspaces[0].Files
	slices.Sort(got)
	if !slices.Equal(got, expected) {
		t.Errorf("manifest Files = %v, want %v", got, expected)
	}
	// None of the excluded paths leaked into the manifest.
	for _, banned := range []string{".git/config", "bin/neo", "node_modules/foo/index.js", "server.log", ".neo/pki/server.crt", ".neo/db/hnsw.db"} {
		if slices.Contains(got, banned) {
			t.Errorf("banned path %q leaked into archive", banned)
		}
	}

	// Now extract and verify file contents.
	dest := t.TempDir()
	wrote, err := ApplyArchive(&buf, m, HLC{}, dest, false)
	if err != nil {
		t.Fatalf("ApplyArchive: %v", err)
	}
	if len(wrote) != len(expected) {
		t.Errorf("extracted %d files, want %d", len(wrote), len(expected))
	}
	// Spot-check one file to confirm content survived.
	got1, err := os.ReadFile(filepath.Join(dest, "workspace", "fake-1", "main.go"))
	if err != nil {
		t.Fatalf("read extracted: %v", err)
	}
	if !bytes.Contains(got1, []byte("func main()")) {
		t.Errorf("extracted main.go missing content: %q", got1)
	}
}

// TestBuildArchive_EmptyManifest — works on a manifest with no workspaces
// (just validates archive container is well-formed).
func TestBuildArchive_EmptyManifest(t *testing.T) {
	m := NewManifest(nil, nil, nil)
	var buf bytes.Buffer
	written, err := BuildArchive(m, &buf)
	if err != nil {
		t.Fatalf("BuildArchive empty: %v", err)
	}
	if written != 0 {
		t.Errorf("written = %d, want 0 for empty manifest", written)
	}
	if buf.Len() == 0 {
		t.Error("zstd container should still have a few bytes (header)")
	}
}

// TestBuildArchive_NilArguments — defensive — nil manifest or writer
// returns an error rather than panicking.
func TestBuildArchive_NilArguments(t *testing.T) {
	if _, err := BuildArchive(nil, &bytes.Buffer{}); err == nil {
		t.Error("nil manifest should error")
	}
	m := NewManifest(nil, nil, nil)
	if _, err := BuildArchive(m, nil); err == nil {
		t.Error("nil writer should error")
	}
}

// TestApplyArchive_DryRun — dry_run=true does not write files, only
// returns the list of paths that would be touched.
func TestApplyArchive_DryRun(t *testing.T) {
	src := t.TempDir()
	makeFakeWorkspace(t, src)

	m := NewManifest(
		[]WalkedWorkspace{{ID: "fake-1", Path: src, Name: "fake", CanonicalID: "github.com/x/fake"}},
		nil, nil,
	)
	var buf bytes.Buffer
	if _, err := BuildArchive(m, &buf); err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}

	dest := t.TempDir()
	would, err := ApplyArchive(&buf, m, HLC{}, dest, true)
	if err != nil {
		t.Fatalf("ApplyArchive dryRun: %v", err)
	}
	if len(would) == 0 {
		t.Fatal("dry_run reported zero files; expected ≥1")
	}
	// Confirm NO file was actually written.
	for _, p := range would {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("dry_run wrote %q (stat err = %v); should not exist", p, err)
		}
	}
}

// TestApplyArchive_RejectsPathTraversal — a malicious archive containing
// "../escape.txt" must be refused, not extracted to destRoot/../escape.txt.
func TestApplyArchive_RejectsPathTraversal(t *testing.T) {
	// Hand-craft a tar+zstd archive with one entry whose name is "../pwned".
	var buf bytes.Buffer
	zw, _ := zstdNewWriterForTest(&buf)
	tw := tarNewWriterForTest(zw)
	hdr := tarHeaderRegular("../pwned", []byte("DATA"))
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("DATA")); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = zw.Close()

	m := NewManifest([]WalkedWorkspace{{ID: "x", CanonicalID: "y"}}, nil, nil)
	dest := t.TempDir()
	_, err := ApplyArchive(&buf, m, HLC{}, dest, false)
	if err == nil || !strings.Contains(err.Error(), "path traversal") {
		t.Errorf("expected path traversal rejection, got %v", err)
	}
}

// TestSanitizeID — key edge cases for the archive-prefix sanitizer.
func TestSanitizeID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"github.com/x/y", "github.com_x_y"},
		{"project:planifier:_root", "project_planifier__root"},
		{"local:abc123", "local_abc123"},
		{"safe-id-1.0", "safe-id-1.0"},
	}
	for _, c := range cases {
		if got := sanitizeID(c.in); got != c.want {
			t.Errorf("sanitizeID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestApplyArchive_HLCGuard verifies that a manifest whose HLC is not strictly
// greater than the supplied currentHLC is rejected with ErrHLCRollback.
func TestApplyArchive_HLCGuard(t *testing.T) {
	src := t.TempDir()
	makeFakeWorkspace(t, src)

	m := NewManifest(
		[]WalkedWorkspace{{ID: "fake-1", Path: src, Name: "fake", CanonicalID: "github.com/x/fake"}},
		nil, nil,
	)
	// m.HLC is set by NewManifest via NextHLC(); advance the clock so that
	// newerHLC is strictly after m.HLC.
	newerHLC := NextHLC()

	var buf bytes.Buffer
	if _, err := BuildArchive(m, &buf); err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}

	dest := t.TempDir()

	// Replay: pass newerHLC as currentHLC → manifest HLC is older → reject.
	_, err := ApplyArchive(&buf, m, newerHLC, dest, false)
	if err == nil {
		t.Fatal("expected ErrHLCRollback, got nil")
	}
	if !errors.Is(err, ErrHLCRollback) {
		t.Errorf("expected ErrHLCRollback, got: %v", err)
	}

	// Sanity: zero currentHLC (first pull) must succeed.
	buf.Reset()
	if _, err := BuildArchive(m, &buf); err != nil {
		t.Fatalf("BuildArchive second: %v", err)
	}
	if _, err := ApplyArchive(&buf, m, HLC{}, dest, true); err != nil {
		t.Errorf("zero currentHLC should not trigger HLC guard: %v", err)
	}

	// Sanity: older currentHLC than manifest must also succeed.
	olderHLC := HLC{WallMS: 1, LogicalCounter: 0}
	buf.Reset()
	if _, err := BuildArchive(m, &buf); err != nil {
		t.Fatalf("BuildArchive third: %v", err)
	}
	if _, err := ApplyArchive(&buf, m, olderHLC, dest, true); err != nil {
		t.Errorf("older currentHLC should not trigger HLC guard: %v", err)
	}
}

// TestExcludedByPath — directly exercises the path predicate.
func TestExcludedByPath(t *testing.T) {
	cases := []struct {
		path     string
		excluded bool
	}{
		{"main.go", false},
		{"docs/intro.md", false},
		{".neo/master_plan.md", false},
		{".neo/pki/server.crt", true},
		{".neo/db/hnsw.db", true},
		{".neo/logs/x.log", true},
		{"server.log", true},
		{"path/to/foo.log", true},
		{"build.pid", true},
	}
	for _, c := range cases {
		if got := excludedByPath(c.path); got != c.excluded {
			t.Errorf("excludedByPath(%q) = %v, want %v", c.path, got, c.excluded)
		}
	}
}
