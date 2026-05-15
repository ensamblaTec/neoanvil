package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationTaggedTestFiles_DetectsNewSyntax covers the modern
// `//go:build integration` form. Files with that tag must be returned;
// files without it must NOT.
func TestIntegrationTaggedTestFiles_DetectsNewSyntax(t *testing.T) {
	dir := t.TempDir()
	tagged := filepath.Join(dir, "integ_test.go")
	plain := filepath.Join(dir, "x_test.go")

	if err := os.WriteFile(tagged, []byte("//go:build integration\n\npackage foo\n\nimport \"testing\"\n\nfunc TestIntegPath(t *testing.T) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain, []byte("package foo\n\nimport \"testing\"\n\nfunc TestPlain(t *testing.T) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := integrationTaggedTestFiles("", dir)
	if len(got) != 1 || got[0] != tagged {
		t.Errorf("expected only %q in result, got %v", tagged, got)
	}
}

// TestIntegrationTaggedTestFiles_DetectsLegacySyntax covers the pre-1.17
// `// +build integration` form. Some operator codebases still ship it.
func TestIntegrationTaggedTestFiles_DetectsLegacySyntax(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy_test.go")
	if err := os.WriteFile(path, []byte("// +build integration\n\npackage foo\n\nimport \"testing\"\n\nfunc TestLegacy(t *testing.T) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := integrationTaggedTestFiles("", dir)
	if len(got) != 1 {
		t.Errorf("expected 1 hit on legacy build tag, got %d: %v", len(got), got)
	}
}

// TestIntegrationTaggedTestFiles_RejectsSuffixedTag covers the
// false-positive guard: only the bare `integration` token activates this
// path. `integrationdev`, `notintegration`, etc. must NOT trip it.
func TestIntegrationTaggedTestFiles_RejectsSuffixedTag(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fp1_test.go"),
		[]byte("//go:build integrationdev\n\npackage foo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fp2_test.go"),
		[]byte("//go:build notintegration\n\npackage foo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := integrationTaggedTestFiles("", dir); len(got) != 0 {
		t.Errorf("must reject suffixed tags as integration, got %v", got)
	}
}

// TestIntegrationTaggedTestFiles_NonexistentDir covers fail-soft: a
// missing pkg dir must return nil cleanly, not panic.
func TestIntegrationTaggedTestFiles_NonexistentDir(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("missing dir must not panic, got: %v", r)
		}
	}()
	got := integrationTaggedTestFiles("", "/nonexistent/path/xyz")
	if got != nil {
		t.Errorf("missing dir must return nil, got: %v", got)
	}
}

// TestBuildTestRunRegexWithAllowlist_UnionsBothSources covers the
// Phase 2.4 expand-only semantics: integration files contribute their
// test names AND the operator allowlist contributes additional names;
// the regex unions all of them, deduped + sorted.
func TestBuildTestRunRegexWithAllowlist_UnionsBothSources(t *testing.T) {
	dir := t.TempDir()
	depGraphFile := filepath.Join(dir, "a_test.go")
	if err := os.WriteFile(depGraphFile, []byte("package foo\n\nimport \"testing\"\n\nfunc TestFromGraph(t *testing.T) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	allowlist := []string{"TestFromYAML", "TestFromYAML2"}

	got := buildTestRunRegexWithAllowlist([]string{depGraphFile}, allowlist)
	want := "^(TestFromGraph|TestFromYAML|TestFromYAML2)$"
	if got != want {
		t.Errorf("union mismatch:\n  got:  %q\n  want: %q", got, want)
	}
}

// TestBuildTestRunRegexWithAllowlist_DedupAcrossSources covers the dedup
// guarantee: if the same test name appears in BOTH the dep-graph and the
// allowlist, it appears exactly once in the regex (otherwise `-run` would
// reject as duplicate alternation).
func TestBuildTestRunRegexWithAllowlist_DedupAcrossSources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a_test.go")
	if err := os.WriteFile(path, []byte("package foo\n\nimport \"testing\"\n\nfunc TestShared(t *testing.T) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := buildTestRunRegexWithAllowlist([]string{path}, []string{"TestShared", "TestExtra"})
	want := "^(TestExtra|TestShared)$"
	if got != want {
		t.Errorf("dedup mismatch:\n  got:  %q\n  want: %q", got, want)
	}
}

// TestBuildTestRunRegexWithAllowlist_EmptyAllowlist_SameAsBaseBuilder
// asserts the v2 helper preserves backward-compat with v1 (buildTestRunRegex)
// when the allowlist is empty/nil. Two callsites in runGoBouncer would
// drift apart silently otherwise.
func TestBuildTestRunRegexWithAllowlist_EmptyAllowlist_SameAsBaseBuilder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a_test.go")
	if err := os.WriteFile(path, []byte("package foo\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\nfunc TestB(t *testing.T) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v1 := buildTestRunRegex([]string{path})
	v2 := buildTestRunRegexWithAllowlist([]string{path}, nil)
	if v1 != v2 {
		t.Errorf("v1 and v2 must agree on empty-allowlist input: v1=%q v2=%q", v1, v2)
	}
}

// TestBuildTestRunRegexWithAllowlist_AllowlistAlone covers the
// graph-stale scenario: dep-graph returns nothing, but the operator's
// allowlist still produces a regex. This is the Phase 2.4 belt — it
// guarantees the always-run names run even when narrowing would
// otherwise have triggered Phase 2.3 fallback (full pkg test).
func TestBuildTestRunRegexWithAllowlist_AllowlistAlone(t *testing.T) {
	got := buildTestRunRegexWithAllowlist(nil, []string{"TestCriticalPath"})
	want := "^(TestCriticalPath)$"
	if got != want {
		t.Errorf("allowlist-alone:\n  got:  %q\n  want: %q", got, want)
	}
}

// TestBuildTestRunRegexWithAllowlist_AllowlistFiltersBlanks covers the
// defensive input cleanup: empty/whitespace entries in the yaml allowlist
// must be dropped before regex composition, otherwise we'd emit `^(|TestX)$`
// which matches the empty string — same DS Finding 1 trap as Phase 2.2.
func TestBuildTestRunRegexWithAllowlist_AllowlistFiltersBlanks(t *testing.T) {
	got := buildTestRunRegexWithAllowlist(nil, []string{"TestA", "", "  ", "TestB"})
	if strings.Contains(got, "||") || strings.Contains(got, "(|") || strings.Contains(got, "|)") {
		t.Errorf("regex must not contain empty alternatives, got: %q", got)
	}
	want := "^(TestA|TestB)$"
	if got != want {
		t.Errorf("blank-filter mismatch:\n  got:  %q\n  want: %q", got, want)
	}
}
