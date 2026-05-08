// brain_test.go — happy-path tests for the `neo brain` CLI commands.
// Tests use the local:// driver against t.TempDir() so they run without
// network or credentials. Live R2 testing belongs in a separate smoke
// runbook outside `go test ./...`.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/brain"
)

// withWorkspaceRegistry sets HOME to t.TempDir() so LoadRegistry()
// touches a sandbox file rather than the operator's real registry.
// Returns the absolute path of the sandbox HOME so the caller can
// pre-populate ~/.neo/workspaces.json.
func withWorkspaceRegistry(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".neo"), 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

// mustWriteFile writes content to path with 0o644; calls t.Fatal on error.
func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// brainOp runs fn and calls t.Fatalf with label on error.
func brainOp(t *testing.T, label string, fn func() error) {
	t.Helper()
	if err := fn(); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
}

// assertOutput checks that buf contains want, then resets buf.
func assertOutput(t *testing.T, buf *bytes.Buffer, label, want string) {
	t.Helper()
	if !strings.Contains(buf.String(), want) {
		t.Errorf("%s: got %q, want substring %q", label, buf.String(), want)
	}
	buf.Reset()
}

// TestSlugify — alphabet-only characters survive; everything else → "-".
func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello", "hello"},
		{"hello world", "hello-world"},
		{"v1.0/release", "v1-0-release"},
		{"a:b:c", "a-b-c"},
		{"", ""},
	}
	for _, c := range cases {
		if got := slugify(c.in); got != c.want {
			t.Errorf("slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseCSV — splits and trims, dropping empties.
func TestParseCSV(t *testing.T) {
	if got := parseCSV(""); got != nil {
		t.Errorf("empty input should return nil, got %v", got)
	}
	got := parseCSV("a, b ,,c")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("idx %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestSnapshotPrefix — with and without tag.
func TestSnapshotPrefix(t *testing.T) {
	hlc := brain.HLC{WallMS: 12345, LogicalCounter: 7}
	if got := snapshotPrefix(hlc, ""); got != "snapshots/12345.7" {
		t.Errorf("untagged = %q", got)
	}
	if got := snapshotPrefix(hlc, "release v1.0"); got != "snapshots/12345.7-release-v1-0" {
		t.Errorf("tagged = %q", got)
	}
}

// TestReadPassphrase — env var present + missing.
func TestReadPassphrase(t *testing.T) {
	t.Setenv("BRAIN_TEST_PASS", "hunter2")
	got, err := readPassphrase("BRAIN_TEST_PASS")
	if err != nil || got != "hunter2" {
		t.Errorf("got %q, %v; want hunter2, nil", got, err)
	}
	if _, err := readPassphrase("BRAIN_TEST_MISSING"); err == nil {
		t.Error("missing env var should error")
	}
	if _, err := readPassphrase(""); err == nil {
		t.Error("empty env name should error")
	}
}

// TestOpenBrainStore_Local — local:// remote opens LocalStore.
func TestOpenBrainStore_Local(t *testing.T) {
	dir := t.TempDir()
	store, err := openBrainStore("local://" + dir)
	if err != nil {
		t.Fatalf("openBrainStore: %v", err)
	}
	defer store.Close()
}

// TestOpenBrainStore_BadScheme — unknown scheme is rejected with a clear
// error mentioning the supported schemes.
func TestOpenBrainStore_BadScheme(t *testing.T) {
	_, err := openBrainStore("ftp://nope")
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("unsupported scheme: got %v", err)
	}
}

// TestBrainPushPull_Roundtrip — full flow:
//
//   1. Bootstrap a fake workspace + registry under HOME=t.TempDir()
//   2. push to local:// remote
//   3. pull --dry-run lists files
//   4. pull (without --dry-run) restores under --dest
//   5. confirm a known file content survived
func TestBrainPushPull_Roundtrip(t *testing.T) {
	t.Setenv("NEO_BRAIN_PASS", "test-passphrase-not-real")

	homeDir := withWorkspaceRegistry(t)
	wsRoot := filepath.Join(homeDir, "fake-workspace")
	if err := os.MkdirAll(wsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(wsRoot, "README.md"), "# fake repo\n")
	mustWriteFile(t, filepath.Join(wsRoot, "main.go"), "package main\n")

	registryJSON := `{"workspaces":[{"id":"fake-1","path":"` + wsRoot + `","name":"fake-workspace","dominant_lang":"go","health":"unknown","added_at":"2026-01-01T00:00:00Z","transport":"sse","type":"workspace"}],"active_id":"fake-1"}`
	if err := os.WriteFile(filepath.Join(homeDir, ".neo", "workspaces.json"), []byte(registryJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	remoteDir := t.TempDir()
	remote := "local://" + remoteDir
	var buf bytes.Buffer

	brainOp(t, "push", func() error { return runBrainPushWithLock(&buf, remote, "", "test", "NEO_BRAIN_PASS", false) })
	assertOutput(t, &buf, "push", "✓ pushed snapshot")

	brainOp(t, "log", func() error { return runBrainLog(&buf, remote, 10) })
	assertOutput(t, &buf, "log", "snapshots/")

	brainOp(t, "verify", func() error { return runBrainVerify(&buf, remote, "test") })
	assertOutput(t, &buf, "verify", "OK")

	dest := t.TempDir()
	brainOp(t, "pull dry-run", func() error { return runBrainPull(&buf, remote, "test", dest, "NEO_BRAIN_PASS", true) })
	assertOutput(t, &buf, "dry-run", "would write")
	if entries, _ := os.ReadDir(dest); len(entries) != 0 {
		t.Errorf("dry-run wrote %d entries", len(entries))
	}

	brainOp(t, "pull", func() error { return runBrainPull(&buf, remote, "test", dest, "NEO_BRAIN_PASS", false) })
	assertOutput(t, &buf, "pull", "wrote")

	got, err := os.ReadFile(filepath.Join(dest, "workspace", "fake-1", "README.md"))
	if err != nil {
		t.Fatalf("read restored README: %v", err)
	}
	if string(got) != "# fake repo\n" {
		t.Errorf("restored content drift: %q", got)
	}
}

// TestBrainPush_RequiresRemote — empty --remote → typed error.
func TestBrainPush_RequiresRemote(t *testing.T) {
	if err := runBrainPushWithLock(&bytes.Buffer{}, "", "", "", "NEO_BRAIN_PASS", false); err == nil {
		t.Error("empty remote should error")
	}
}

// TestBrainPull_RequiresRemoteAndDest — both flags required.
func TestBrainPull_RequiresRemoteAndDest(t *testing.T) {
	if err := runBrainPull(&bytes.Buffer{}, "", "latest", "", "NEO_BRAIN_PASS", false); err == nil {
		t.Error("empty remote should error")
	}
	if err := runBrainPull(&bytes.Buffer{}, "local:///tmp", "latest", "", "NEO_BRAIN_PASS", false); err == nil {
		t.Error("empty dest should error")
	}
}

// TestBrainStatus_EmptyRemote — well-handled, doesn't panic.
func TestBrainStatus_EmptyRemote(t *testing.T) {
	dir := t.TempDir()
	withWorkspaceRegistry(t)
	var buf bytes.Buffer
	if err := runBrainStatus(&buf, "local://"+dir); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "empty") {
		t.Errorf("expected 'empty' in output: %q", buf.String())
	}
}
