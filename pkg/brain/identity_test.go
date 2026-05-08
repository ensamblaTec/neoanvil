// Tests for ResolveCanonicalID — covers the four resolution rules and
// their edge cases. All tests use t.TempDir() so they parallel safely
// and don't pollute the operator's $HOME.

package brain

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNormalizeGitRemote_HappyPaths covers the SSH and HTTPS shapes the
// resolver promises to handle. Each entry round-trips a real-looking
// remote URL to its canonical form.
func TestNormalizeGitRemote_HappyPaths(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ssh github", "git@github.com:foo/bar.git", "github.com/foo/bar"},
		{"ssh github no .git", "git@github.com:foo/bar", "github.com/foo/bar"},
		{"https github .git", "https://github.com/foo/bar.git", "github.com/foo/bar"},
		{"https github no .git", "https://github.com/foo/bar", "github.com/foo/bar"},
		{"http enterprise port", "http://gitlab.local:8443/x/y.git", "gitlab.local:8443/x/y"},
		{"ssh:// scheme", "ssh://git@host/path/to/repo.git", "host/path/to/repo"},
		{"git:// scheme", "git://kernel.org/pub/scm/linux.git", "kernel.org/pub/scm/linux"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeGitRemote(c.in)
			if got != c.want {
				t.Errorf("normalizeGitRemote(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestNormalizeGitRemote_Unknown returns "" for shapes the parser doesn't
// recognise — caller falls through to the next resolution rule.
func TestNormalizeGitRemote_Unknown(t *testing.T) {
	cases := []string{
		"",
		"file:///local/repo",
		"not a url at all",
		"github.com/foo/bar", // no scheme prefix
	}
	for _, in := range cases {
		if got := normalizeGitRemote(in); got != "" {
			t.Errorf("normalizeGitRemote(%q) = %q, want empty", in, got)
		}
	}
}

// TestPathHash_Stable two calls on the same absolute path yield the same
// hash. Different paths produce different hashes (collision-resistance is
// the whole point of using sha256).
func TestPathHash_Stable(t *testing.T) {
	a := pathHash("/foo/bar")
	b := pathHash("/foo/bar")
	if a != b {
		t.Errorf("pathHash unstable: %q != %q", a, b)
	}
	if !strings.HasPrefix(a, "local:") {
		t.Errorf("pathHash missing local: prefix, got %q", a)
	}
	if pathHash("/foo/baz") == a {
		t.Error("pathHash collision between distinct paths")
	}
}

// TestResolveCanonicalID_Fallback — no neo.yaml, no .git, no
// .neo-project — falls through to device key (preferred) or path hash.
// [146.J] The fallback is now SourceDeviceKey when ~/.neo/identity.key is
// available (or can be created); SourcePathHash only fires when key gen fails.
func TestResolveCanonicalID_Fallback(t *testing.T) {
	dir := t.TempDir()
	got := ResolveCanonicalID(dir)
	switch got.Source {
	case SourceDeviceKey:
		if !strings.HasPrefix(got.ID, "dev:") {
			t.Errorf("device key ID = %q, want dev: prefix", got.ID)
		}
	case SourcePathHash:
		// Fallback when home dir or key gen unavailable (e.g. locked-down CI).
		if !strings.HasPrefix(got.ID, "local:") {
			t.Errorf("path hash ID = %q, want local: prefix", got.ID)
		}
	default:
		t.Errorf("unexpected source %q — want device_key or path_hash", got.Source)
	}
}

// TestResolveCanonicalID_GitRemoteSSH — write a fake .git/config with an
// SSH-form remote URL and verify the resolver picks SourceGitRemote and
// produces the normalized form. Skips if `git` is not on PATH.
func TestResolveCanonicalID_GitRemoteSSH(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := exec.Command("git", "-C", dir, "remote", "add", "origin", "git@github.com:foo/bar.git").Run(); err != nil {
		t.Fatalf("git remote add: %v", err)
	}
	got := ResolveCanonicalID(dir)
	if got.Source != SourceGitRemote {
		t.Errorf("source = %q, want %q", got.Source, SourceGitRemote)
	}
	if got.ID != "github.com/foo/bar" {
		t.Errorf("ID = %q, want github.com/foo/bar", got.ID)
	}
}

// TestResolveCanonicalID_GitRemoteHTTPS — same as SSH variant but with
// HTTPS form. Also covers walk-up: workspacePath is a subdir of the git
// root, the resolver should still find it.
func TestResolveCanonicalID_GitRemoteHTTPS(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := exec.Command("git", "-C", dir, "remote", "add", "origin", "https://github.com/foo/bar").Run(); err != nil {
		t.Fatalf("git remote add: %v", err)
	}
	// Resolve from a subdirectory — verifies walk-up works.
	sub := filepath.Join(dir, "internal", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got := ResolveCanonicalID(sub)
	if got.Source != SourceGitRemote {
		t.Errorf("source = %q, want %q", got.Source, SourceGitRemote)
	}
	if got.ID != "github.com/foo/bar" {
		t.Errorf("ID = %q, want github.com/foo/bar", got.ID)
	}
}

// TestResolveCanonicalID_ConfigOverride — config workspace.canonical_id
// wins over git remote even when both are present. Establishes precedence.
func TestResolveCanonicalID_ConfigOverride(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := exec.Command("git", "-C", dir, "remote", "add", "origin", "git@github.com:foo/bar.git").Run(); err != nil {
		t.Fatalf("git remote add: %v", err)
	}
	yaml := []byte("workspace:\n  canonical_id: explicit-override-id\n")
	if err := os.WriteFile(filepath.Join(dir, "neo.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	got := ResolveCanonicalID(dir)
	if got.Source != SourceConfigOverride {
		t.Errorf("source = %q, want %q (config must win over git)", got.Source, SourceConfigOverride)
	}
	if got.ID != "explicit-override-id" {
		t.Errorf("ID = %q, want explicit-override-id", got.ID)
	}
}

// TestResolveCanonicalID_ProjectName — no git, no override; .neo-project
// with project_name produces "project:<name>:<basename>". Walk-up applies:
// .neo-project lives at the parent of the workspace dir.
func TestResolveCanonicalID_ProjectName(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, ".neo-project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := []byte("project_name: planifier\nmember_workspaces: []\ndominant_lang: go\n")
	if err := os.WriteFile(filepath.Join(projectDir, "neo.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	// workspacePath is a sibling dir of .neo-project.
	wsPath := filepath.Join(root, "backend")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	got := ResolveCanonicalID(wsPath)
	if got.Source != SourceProjectName {
		t.Errorf("source = %q, want %q", got.Source, SourceProjectName)
	}
	want := "project:planifier:backend"
	if got.ID != want {
		t.Errorf("ID = %q, want %q", got.ID, want)
	}
}

// TestWalkUpFor_BoundedRecursion — synthesize a sibling-only path that
// will never find the target, confirms we don't loop forever.
func TestWalkUpFor_NotFound(t *testing.T) {
	dir := t.TempDir()
	got := walkUpFor(dir, ".does_not_exist")
	if got != "" {
		t.Errorf("walkUpFor not-found returned %q, want empty", got)
	}
}
