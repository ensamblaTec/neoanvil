package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/rag"
)

// TestPhase26_OneLineChange_SelectsCorrectTests is the Phase 2.6 regression
// from master_plan: "a 1-line change in tool_memory.go must select
// TestWithRemSleepDefaults and tests whose deps include tool_memory.go,
// NOT the whole cmd/neo-mcp suite." [SRE-PHASE-2.6 / Speed-First 2026-05-15]
//
// This is an end-to-end test of the full Phase 2.2+2.3+2.4 narrowing
// pipeline, but synthesized in a temp workspace so it's hermetic and fast.
// The real cmd/neo-mcp suite has 80+ test files; a faithful regression
// against the live tree would be slow and brittle. Instead we materialise
// the exact topology the spec describes (tool_memory.go + its test +
// unrelated noise files) and assert the regex narrows correctly.
//
// What this test guards against — silently regressing to "run all" mode:
//   · narrow regex drops TestWithRemSleepDefaults → tool_memory bug ships
//     uncaught (the most important thing the helper exists to prevent)
//   · narrow regex includes unrelated tests → narrowing claim is false
//   · empty impact set → DS Finding 1 fallback path
func TestPhase26_OneLineChange_SelectsCorrectTests(t *testing.T) {
	workspace := t.TempDir()

	// Materialize the spec scenario:
	//   pkg/neo-mcp/tool_memory.go            ← mutated file
	//   pkg/neo-mcp/tool_memory_test.go       ← MUST be selected (same-pkg)
	//     · TestWithRemSleepDefaults          (named in spec)
	//     · TestMemoryTool_StoreReturnsMCPEnvelope (also same-pkg)
	//   pkg/neo-mcp/unrelated_test.go         ← also same-pkg sibling
	//     · TestUnrelatedThing                (collateral, expected to be
	//                                          INCLUDED because same-pkg dir
	//                                          glob — Go compiles whole pkg;
	//                                          can't surgical-skip without
	//                                          symbol-level mapping, which
	//                                          the doc note in test_impact.go
	//                                          explicitly defers)
	//   pkg/other/other_test.go               ← cross-pkg, MUST NOT appear
	//     · TestOther                         (in the within-pkg regex)
	mkDir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(workspace, rel), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
	}
	writeFile := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(workspace, rel), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	mkDir("pkg/neo-mcp")
	mkDir("pkg/other")

	writeFile("pkg/neo-mcp/tool_memory.go", "package neomcp\n")
	writeFile("pkg/neo-mcp/tool_memory_test.go", `package neomcp

import "testing"

func TestWithRemSleepDefaults(t *testing.T)              {}
func TestMemoryTool_StoreReturnsMCPEnvelope(t *testing.T) {}
`)
	writeFile("pkg/neo-mcp/unrelated_test.go", `package neomcp

import "testing"

func TestUnrelatedThing(t *testing.T) {}
`)
	writeFile("pkg/other/other_test.go", `package other

import "testing"

func TestOther(t *testing.T) {}
`)

	// Wire the dep-graph: pkg/other/other_test.go has NO edge to
	// tool_memory.go (the cross-pkg test must NOT be impacted). Optional
	// edge from tool_memory_test.go → tool_memory.go would be redundant
	// (same-pkg dir glob already covers it) but we add it to mirror what
	// the real ingest pipeline produces.
	wal, err := rag.OpenWAL(filepath.Join(workspace, "wal.db"))
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	t.Cleanup(func() { _ = wal.Close() })
	if err := rag.InitGraphRAG(wal); err != nil {
		t.Fatalf("InitGraphRAG: %v", err)
	}
	edges := []rag.GraphEdge{
		{SourceNode: "pkg/neo-mcp/tool_memory_test.go", TargetNode: "pkg/neo-mcp/tool_memory.go"},
	}
	if err := rag.SaveGraphEdges(wal, edges); err != nil {
		t.Fatalf("SaveGraphEdges: %v", err)
	}

	// Simulate the certify pipeline narrowing flow.
	mutatedAbs := filepath.Join(workspace, "pkg/neo-mcp/tool_memory.go")
	samePkg := impactedSamePkgTestFiles(wal, workspace, mutatedAbs)
	if len(samePkg) == 0 {
		t.Fatalf("impacted same-pkg set must not be empty for a typical 1-line .go change with sibling tests")
	}

	// Build the regex via the Phase 2.4 helper (with empty allowlist —
	// the default path, exercising back-compat with v1 buildTestRunRegex).
	regex := buildTestRunRegexWithAllowlist(samePkg, nil)
	if regex == "" {
		t.Fatalf("regex must not be empty when same-pkg tests are present (DS Finding 1: empty regex would silently zero coverage)")
	}

	// Spec assertion 1: TestWithRemSleepDefaults MUST be selected.
	if !strings.Contains(regex, "TestWithRemSleepDefaults") {
		t.Errorf("Phase 2.6 spec violated: TestWithRemSleepDefaults must appear in narrowed regex, got: %s", regex)
	}

	// Spec assertion 2: TestMemoryTool_StoreReturnsMCPEnvelope (same-pkg
	// sibling) also must appear — `go test pkg` compiles the whole package,
	// and skipping a sibling test would require symbol-level dep tracking
	// which is explicitly deferred (see test_impact.go doc comment).
	if !strings.Contains(regex, "TestMemoryTool_StoreReturnsMCPEnvelope") {
		t.Errorf("same-pkg sibling test must appear in regex (whole-pkg compilation semantics): %s", regex)
	}

	// Spec assertion 3: cross-pkg TestOther must NOT appear. Phase 2.2 v1
	// only narrows within the mutated file's pkg; cross-pkg test inclusion
	// is a deliberate v2 epic. If this fires, the same-pkg filter in
	// impactedSamePkgTestFiles silently regressed.
	if strings.Contains(regex, "TestOther") {
		t.Errorf("cross-pkg TestOther leaked into within-pkg regex: %s", regex)
	}

	// Spec assertion 4: the regex shape is ^(A|B|...)$ — DS Finding 1
	// would have us shipping ^()$ which matches the empty string and runs
	// zero tests. Belt-and-suspenders.
	if !strings.HasPrefix(regex, "^(") || !strings.HasSuffix(regex, ")$") {
		t.Errorf("regex shape broken: %s", regex)
	}
	inner := strings.TrimPrefix(strings.TrimSuffix(regex, ")$"), "^(")
	if inner == "" || strings.HasPrefix(inner, "|") || strings.HasSuffix(inner, "|") {
		t.Errorf("regex inner content has empty alternative: %s", inner)
	}
}

// TestPhase26_EmptyImpactSet_TriggersSafeFallback covers the other half of
// the spec: when dep-graph is empty (workspace not yet indexed) AND no
// same-pkg test files exist, narrowing must produce empty regex →
// runGoBouncer falls through to full pkg test. This is the "never
// silently drop coverage" guarantee from Phase 2.3.
func TestPhase26_EmptyImpactSet_TriggersSafeFallback(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "pkg/lonely"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "pkg/lonely/x.go"),
		[]byte("package lonely\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Note: NO _test.go in pkg/lonely, NO dep-graph entries.

	wal, err := rag.OpenWAL(filepath.Join(workspace, "wal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wal.Close() })
	if err := rag.InitGraphRAG(wal); err != nil {
		t.Fatal(err)
	}

	samePkg := impactedSamePkgTestFiles(wal, workspace, filepath.Join(workspace, "pkg/lonely/x.go"))
	regex := buildTestRunRegexWithAllowlist(samePkg, nil)
	if regex != "" {
		t.Errorf("empty impact set must produce empty regex → runGoBouncer fallback to full pkg test, got: %q", regex)
	}
}

// TestPhase26_AllowlistRescuesFromFallback covers the Phase 2.4 belt:
// even with empty dep-graph + no same-pkg tests, if the operator's
// always_run allowlist names a test, the regex still narrows. Critical
// for "this test must ALWAYS run" workflows like schema migration regressions.
func TestPhase26_AllowlistRescuesFromFallback(t *testing.T) {
	regex := buildTestRunRegexWithAllowlist(nil, []string{"TestSchemaMigration_v117"})
	if regex == "" {
		t.Fatal("operator allowlist must produce non-empty regex even when dep-graph empty")
	}
	if !strings.Contains(regex, "TestSchemaMigration_v117") {
		t.Errorf("allowlist name dropped: %s", regex)
	}
}
