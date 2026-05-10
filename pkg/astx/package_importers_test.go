package astx

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestPackageImporters_FixtureDemonstratesScopeMismatch builds a small
// workspace fixture that captures the exact bug we found in production:
// a package has two files, callers use symbols from EACH, and we want
// PackageImporters to return the union — not just callers of one
// file's symbols (which is what AuditSharedContract does).
//
// Layout:
//
//	fixture_root/
//	├─ go.mod (synthetic — fixture path doesn't matter)
//	├─ pkg/foo/
//	│  ├─ keystore.go  (target — exports Load, Save)
//	│  └─ another.go   (sibling — exports OpenAuditLog)
//	├─ cmd/uses_keystore/main.go   (uses foo.Load) ← AuditSharedContract finds
//	├─ cmd/uses_another/main.go    (uses foo.OpenAuditLog only) ← MISSED by AuditSharedContract
//	└─ cmd/no_import/main.go       (does not import foo) ← never found
//
// Expectations:
//   - AuditSharedContract(target=keystore.go) finds 1 caller (uses_keystore)
//   - PackageImporters(target=keystore.go) finds 2 callers (uses_keystore + uses_another)
//   - The new sprint behaviour returns the union via mergedASTImpact.
//
// This is the regression gate: if anyone narrows PackageImporters back
// to symbol-scope, this test catches it.
func TestPackageImporters_FixtureDemonstratesScopeMismatch(t *testing.T) {
	root := t.TempDir()

	mustWrite := func(path, body string) {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}

	// Synthetic go.mod so go tooling treats this as a real module.
	mustWrite("go.mod", "module fixture/foo\n\ngo 1.26\n")

	// Target package with TWO files. keystore.go exports Load. another.go
	// exports OpenAuditLog. Same package "foo".
	mustWrite("pkg/foo/keystore.go", `package foo

func Load(path string) error {
	return nil
}

func Save(path string) error {
	return nil
}
`)
	mustWrite("pkg/foo/another.go", `package foo

import "errors"

func OpenAuditLog(path string) error {
	return errors.New("not implemented")
}
`)

	// Caller A: uses foo.Load (symbol exported BY keystore.go).
	mustWrite("cmd/uses_keystore/main.go", `package main

import "fixture/foo/pkg/foo"

func main() {
	_ = foo.Load("/etc/passwd")
}
`)

	// Caller B: uses foo.OpenAuditLog (symbol exported by ANOTHER file
	// of the same package). This is the case AuditSharedContract misses.
	mustWrite("cmd/uses_another/main.go", `package main

import "fixture/foo/pkg/foo"

func main() {
	_ = foo.OpenAuditLog("/var/log")
}
`)

	// Caller C: does NOT import foo. Should never appear.
	mustWrite("cmd/no_import/main.go", `package main

import "fmt"

func main() {
	fmt.Println("hi")
}
`)

	// --- AuditSharedContract: symbol-scoped (legacy behaviour) ---
	target := filepath.Join(root, "pkg/foo/keystore.go")
	src, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	findings, err := AuditSharedContract(root, target, src)
	if err != nil {
		t.Fatalf("AuditSharedContract: %v", err)
	}
	symbolCallers := uniqFiles(findings, root)

	// --- PackageImporters: package-scoped (sprint fix) ---
	pkgImps, err := PackageImporters(root, target)
	if err != nil {
		t.Fatalf("PackageImporters: %v", err)
	}
	sort.Strings(pkgImps)

	// Assertion 1: AuditSharedContract returns exactly the callers of
	// keystore.go's symbols (uses_keystore only).
	if len(symbolCallers) != 1 {
		t.Errorf("AuditSharedContract: want 1 caller (uses_keystore), got %d: %v",
			len(symbolCallers), symbolCallers)
	}
	if len(symbolCallers) > 0 && !contains(symbolCallers[0], "uses_keystore") {
		t.Errorf("AuditSharedContract: want uses_keystore, got %v", symbolCallers)
	}

	// Assertion 2 (the regression gate): PackageImporters returns BOTH
	// uses_keystore AND uses_another. cmd/no_import is excluded.
	if len(pkgImps) != 2 {
		t.Fatalf("PackageImporters: want 2 importers (uses_keystore + uses_another), got %d: %v",
			len(pkgImps), pkgImps)
	}
	hasKeystore := false
	hasAnother := false
	hasNoImport := false
	for _, p := range pkgImps {
		switch {
		case contains(p, "uses_keystore"):
			hasKeystore = true
		case contains(p, "uses_another"):
			hasAnother = true
		case contains(p, "no_import"):
			hasNoImport = true
		}
	}
	if !hasKeystore || !hasAnother {
		t.Errorf("PackageImporters: missing one of [uses_keystore, uses_another]: %v", pkgImps)
	}
	if hasNoImport {
		t.Errorf("PackageImporters: false positive — no_import shouldn't appear: %v", pkgImps)
	}
}

// TestPackageImporters_PointsToSelfPackage_Excluded confirms that files
// in the SAME package as the target are not reported as importers.
// Without this guard, every internal sibling would "import itself" via
// implicit package scope.
func TestPackageImporters_PointsToSelfPackage_Excluded(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(path, body string) {
		full := filepath.Join(root, path)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, []byte(body), 0o644)
	}
	mustWrite("go.mod", "module fixture\n\ngo 1.26\n")
	mustWrite("pkg/p/a.go", "package p\n\nfunc A() {}\n")
	mustWrite("pkg/p/b.go", "package p\n\nfunc B() { A() }\n") // sibling — NO external import

	target := filepath.Join(root, "pkg/p/a.go")
	imps, err := PackageImporters(root, target)
	if err != nil {
		t.Fatalf("PackageImporters: %v", err)
	}
	if len(imps) != 0 {
		t.Errorf("expected 0 importers (sibling in same package), got %d: %v", len(imps), imps)
	}
}

// TestPackageImporters_VendorAndDotNeoSkipped ensures the walker skips
// the two opt-out dirs we never want to scan.
func TestPackageImporters_VendorAndDotNeoSkipped(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(path, body string) {
		full := filepath.Join(root, path)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, []byte(body), 0o644)
	}
	mustWrite("go.mod", "module fixture\n\ngo 1.26\n")
	mustWrite("pkg/foo/foo.go", "package foo\n\nfunc Hi() {}\n")
	// In vendor/ — should be skipped.
	mustWrite("vendor/external/x.go", `package external
import "fixture/pkg/foo"
func Use() { foo.Hi() }
`)
	// In .neo/ — also skipped.
	mustWrite(".neo/snapshot/y.go", `package snapshot
import "fixture/pkg/foo"
func Use() { foo.Hi() }
`)
	// Real importer — must be the only result.
	mustWrite("cmd/real/main.go", `package main
import "fixture/pkg/foo"
func main() { foo.Hi() }
`)

	target := filepath.Join(root, "pkg/foo/foo.go")
	imps, err := PackageImporters(root, target)
	if err != nil {
		t.Fatalf("PackageImporters: %v", err)
	}
	if len(imps) != 1 {
		t.Fatalf("want 1 importer (cmd/real), got %d: %v", len(imps), imps)
	}
	if !contains(imps[0], "cmd/real") {
		t.Errorf("want cmd/real, got %v", imps)
	}
}

// helpers ----------------------------------------------------------------

func uniqFiles(findings []AuditFinding, workspace string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, f := range findings {
		rel, _ := filepath.Rel(workspace, f.File)
		if _, ok := seen[rel]; ok {
			continue
		}
		seen[rel] = struct{}{}
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || stringsContain(haystack, needle))
}

func stringsContain(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
