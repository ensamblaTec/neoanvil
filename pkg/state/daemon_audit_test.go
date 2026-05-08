package state

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeExecutor is a controllable AuditExecutor for pipeline tests.
// Each stage's outcome is set via the Stage* fields; Stage*Err overrides
// to simulate exec failure (returns non-nil error). Calls is a hit counter
// per stage so tests can assert short-circuit behavior.
type fakeExecutor struct {
	StageBuild       StageOutcome
	StageBuildErr    error
	StageAST         StageOutcome
	StageASTErr      error
	StageTests       StageOutcome
	StageTestsErr    error
	StageCertify     StageOutcome
	StageCertifyErr  error
	StageMetrics     StageOutcome
	StageMetricsErr  error
	BuildCalls       int
	ASTCalls         int
	TestsCalls       int
	CertifyCalls     int
	MetricsCalls     int
}

func (f *fakeExecutor) Build(_ context.Context, _ string, _ []string) (StageOutcome, error) {
	f.BuildCalls++
	return f.StageBuild, f.StageBuildErr
}
func (f *fakeExecutor) ASTAudit(_ context.Context, _ string, _ []string) (StageOutcome, error) {
	f.ASTCalls++
	return f.StageAST, f.StageASTErr
}
func (f *fakeExecutor) Tests(_ context.Context, _ string, _ []string) (StageOutcome, error) {
	f.TestsCalls++
	return f.StageTests, f.StageTestsErr
}
func (f *fakeExecutor) CertifyDryRun(_ context.Context, _ string, _ []string) (StageOutcome, error) {
	f.CertifyCalls++
	return f.StageCertify, f.StageCertifyErr
}
func (f *fakeExecutor) MetricsDelta(_ context.Context, _ string, _ []string) (StageOutcome, error) {
	f.MetricsCalls++
	return f.StageMetrics, f.StageMetricsErr
}

// allPassFake returns a fake executor where every stage passes — useful as
// the baseline for tests that mutate one stage at a time.
func allPassFake() *fakeExecutor {
	return &fakeExecutor{
		StageBuild:   StageOutcome{Passed: true, DurationMS: 320},
		StageAST:     StageOutcome{Passed: true, DurationMS: 80},
		StageTests:   StageOutcome{Passed: true, DurationMS: 1200, Extra: map[string]float64{"count_after": 87}},
		StageCertify: StageOutcome{Passed: true, DurationMS: 50},
		StageMetrics: StageOutcome{Passed: true, DurationMS: 30, Extra: map[string]float64{"cc_avg_after": 7.2}},
	}
}

// TestComputeSeverity_Pass: empty failures → SeverityPass. [138.A.4]
func TestComputeSeverity_Pass(t *testing.T) {
	got := computeSeverity(nil)
	if got != SeverityPass {
		t.Errorf("nil failures: want %q, got %q", SeverityPass, got)
	}
	got = computeSeverity([]FailureKind{})
	if got != SeverityPass {
		t.Errorf("empty failures: want %q, got %q", SeverityPass, got)
	}
}

// TestComputeSeverity_TableDriven covers every classification rule.
// Order matters: build/certify trump everything (fail-fatal), then
// tests/ast (fail-recoverable), then metrics-only (pass-with-warnings).
func TestComputeSeverity_TableDriven(t *testing.T) {
	cases := []struct {
		name     string
		failures []FailureKind
		want     AuditSeverity
	}{
		{"only metrics", []FailureKind{FailureMetrics}, SeverityPassWithWarnings},
		{"only ast", []FailureKind{FailureASTAudit}, SeverityFailRecoverable},
		{"only tests", []FailureKind{FailureTests}, SeverityFailRecoverable},
		{"ast + tests", []FailureKind{FailureASTAudit, FailureTests}, SeverityFailRecoverable},
		{"ast + metrics", []FailureKind{FailureASTAudit, FailureMetrics}, SeverityFailRecoverable},
		{"only build", []FailureKind{FailureBuild}, SeverityFailFatal},
		{"only certify", []FailureKind{FailureCertify}, SeverityFailFatal},
		{"build + tests (build trumps)", []FailureKind{FailureBuild, FailureTests}, SeverityFailFatal},
		{"build + metrics (build trumps)", []FailureKind{FailureBuild, FailureMetrics}, SeverityFailFatal},
		{"certify + ast (certify trumps)", []FailureKind{FailureCertify, FailureASTAudit}, SeverityFailFatal},
		{"all four bad", []FailureKind{FailureBuild, FailureCertify, FailureTests, FailureASTAudit}, SeverityFailFatal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeSeverity(tc.failures)
			if got != tc.want {
				t.Errorf("failures=%v: want %q, got %q", tc.failures, tc.want, got)
			}
		})
	}
}

// TestNewVerdict_PassedShortcut: Passed must mirror (Severity == pass).
// Catches a future regression where the convenience field drifts from the
// authoritative classification.
func TestNewVerdict_PassedShortcut(t *testing.T) {
	cases := []struct {
		name     string
		failures []FailureKind
		want     bool
	}{
		{"pass", nil, true},
		{"pass-with-warnings", []FailureKind{FailureMetrics}, false},
		{"fail-recoverable", []FailureKind{FailureTests}, false},
		{"fail-fatal", []FailureKind{FailureBuild}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := newVerdict([]string{"f.go"}, tc.failures, nil, nil, time.Now())
			if v.Passed != tc.want {
				t.Errorf("Passed shortcut: want %v, got %v (severity=%q)", tc.want, v.Passed, v.Severity)
			}
		})
	}
}

// TestNewVerdict_TimestampInvariants: StartedAt + DurationMS ≈ CompletedAt.
// DurationMS must be non-negative even when the call is instant.
func TestNewVerdict_TimestampInvariants(t *testing.T) {
	started := time.Now()
	// Tiny sleep so DurationMS becomes measurable in any timer resolution.
	time.Sleep(2 * time.Millisecond)
	v := newVerdict([]string{"f.go"}, nil, nil, nil, started)

	if v.DurationMS < 0 {
		t.Errorf("DurationMS must be non-negative, got %d", v.DurationMS)
	}
	if v.StartedAt != started {
		t.Errorf("StartedAt: want %v, got %v", started, v.StartedAt)
	}
	if v.CompletedAt.Before(started) {
		t.Errorf("CompletedAt (%v) cannot precede StartedAt (%v)", v.CompletedAt, started)
	}
	if !v.CompletedAt.After(v.StartedAt) {
		t.Errorf("CompletedAt must be strictly after StartedAt for non-zero duration")
	}
}

// TestNewVerdict_FilesPreserved: input files reach the verdict unchanged.
// Important for replay/debug — the verdict is self-describing for the
// trust scoring system (138.B) which keys per-file pattern history.
func TestNewVerdict_FilesPreserved(t *testing.T) {
	files := []string{"pkg/state/planner.go", "pkg/state/daemon_audit.go"}
	v := newVerdict(files, nil, nil, nil, time.Now())
	if len(v.Files) != len(files) {
		t.Fatalf("Files length: want %d, got %d", len(files), len(v.Files))
	}
	for i, f := range files {
		if v.Files[i] != f {
			t.Errorf("Files[%d]: want %q, got %q", i, f, v.Files[i])
		}
	}
}

// TestNewVerdict_MetricsCarried: when callers populate Metrics, the verdict
// preserves them verbatim. Empty/nil is OK (early-abort pipelines).
func TestNewVerdict_MetricsCarried(t *testing.T) {
	metrics := map[string]float64{
		"cc_avg_before":     8.2,
		"cc_avg_after":      7.5,
		"loc_delta":         -42,
		"build_duration_ms": 320,
	}
	v := newVerdict([]string{"x.go"}, nil, nil, metrics, time.Now())
	if len(v.Metrics) != len(metrics) {
		t.Fatalf("Metrics size: want %d, got %d", len(metrics), len(v.Metrics))
	}
	for k, want := range metrics {
		if got, ok := v.Metrics[k]; !ok || got != want {
			t.Errorf("Metrics[%q]: want %v, got %v (present=%v)", k, want, got, ok)
		}
	}
}

// TestNewVerdict_NilMetricsOK: Metrics nil is acceptable for fail-fatal
// verdicts where the pipeline aborted before MetricsDelta could run.
func TestNewVerdict_NilMetricsOK(t *testing.T) {
	v := newVerdict([]string{"x.go"}, []FailureKind{FailureBuild}, nil, nil, time.Now())
	if len(v.Metrics) != 0 {
		t.Errorf("nil metrics input should yield nil/empty Metrics, got %v", v.Metrics)
	}
	if v.Severity != SeverityFailFatal {
		t.Errorf("FailureBuild should be fail-fatal, got %q", v.Severity)
	}
	if v.Passed {
		t.Error("Passed must be false for fail-fatal")
	}
}

// === RunAuditPipeline tests (138.A.2 / 138.A.4) ===========================

// TestRunAuditPipeline_AllPass: every stage passes → SeverityPass, every
// stage was called exactly once, metrics carry per-stage durations.
func TestRunAuditPipeline_AllPass(t *testing.T) {
	exec := allPassFake()
	v := RunAuditPipeline(context.Background(), "/ws", []string{"f.go"}, exec)

	if v.Severity != SeverityPass {
		t.Errorf("severity=%q want pass", v.Severity)
	}
	if !v.Passed {
		t.Error("Passed must be true on all-pass")
	}
	if len(v.Failures) != 0 {
		t.Errorf("Failures must be empty, got %v", v.Failures)
	}
	for stage, calls := range map[string]int{
		"Build": exec.BuildCalls, "AST": exec.ASTCalls, "Tests": exec.TestsCalls,
		"Certify": exec.CertifyCalls, "Metrics": exec.MetricsCalls,
	} {
		if calls != 1 {
			t.Errorf("%s stage called %d times, want 1", stage, calls)
		}
	}
	// Metrics must have per-stage durations.
	for _, key := range []string{"build_duration_ms", "ast_duration_ms", "tests_duration_ms", "certify_duration_ms", "metrics_duration_ms"} {
		if _, ok := v.Metrics[key]; !ok {
			t.Errorf("expected metric %q in verdict, got %v", key, v.Metrics)
		}
	}
	// Stage Extra must be merged with stage prefix.
	if v.Metrics["tests_count_after"] != 87 {
		t.Errorf("tests_count_after: want 87, got %v", v.Metrics["tests_count_after"])
	}
	if v.Metrics["metrics_cc_avg_after"] != 7.2 {
		t.Errorf("metrics_cc_avg_after: want 7.2, got %v", v.Metrics["metrics_cc_avg_after"])
	}
}

// TestRunAuditPipeline_BuildShortCircuit: build fails → downstream stages
// SKIPPED. Critical invariant — running tests on un-compilable code wastes
// time and produces noise in the verdict.
func TestRunAuditPipeline_BuildShortCircuit(t *testing.T) {
	exec := allPassFake()
	exec.StageBuild = StageOutcome{Passed: false, DurationMS: 200, Detail: "compile error: undefined: foo"}

	v := RunAuditPipeline(context.Background(), "/ws", []string{"f.go"}, exec)

	if v.Severity != SeverityFailFatal {
		t.Errorf("severity=%q want fail-fatal", v.Severity)
	}
	if exec.BuildCalls != 1 {
		t.Errorf("Build calls: want 1, got %d", exec.BuildCalls)
	}
	if exec.ASTCalls != 0 || exec.TestsCalls != 0 || exec.CertifyCalls != 0 || exec.MetricsCalls != 0 {
		t.Errorf("downstream stages must NOT run when Build fails: ast=%d tests=%d certify=%d metrics=%d",
			exec.ASTCalls, exec.TestsCalls, exec.CertifyCalls, exec.MetricsCalls)
	}
	if len(v.Failures) != 1 || v.Failures[0] != FailureBuild {
		t.Errorf("Failures: want [build], got %v", v.Failures)
	}
}

// TestRunAuditPipeline_BuildErrorTreatedAsFail: an exec error from Build
// (process spawn failed, ctx cancelled) treats the stage as fail-fatal —
// same short-circuit as outcome.Passed=false.
func TestRunAuditPipeline_BuildErrorTreatedAsFail(t *testing.T) {
	exec := allPassFake()
	exec.StageBuildErr = errors.New("exec.Cmd start: file not found")

	v := RunAuditPipeline(context.Background(), "/ws", []string{"f.go"}, exec)

	if v.Severity != SeverityFailFatal {
		t.Errorf("severity=%q want fail-fatal on Build err", v.Severity)
	}
	if exec.ASTCalls != 0 {
		t.Errorf("AST must skip when Build errored, got %d calls", exec.ASTCalls)
	}
}

// TestRunAuditPipeline_RecoverableContinues: tests fail but build OK →
// downstream stages still run (operator wants full picture). Severity
// fail-recoverable.
func TestRunAuditPipeline_RecoverableContinues(t *testing.T) {
	exec := allPassFake()
	exec.StageTests = StageOutcome{Passed: false, DurationMS: 800, Detail: "TestX failed"}

	v := RunAuditPipeline(context.Background(), "/ws", []string{"f.go"}, exec)

	if v.Severity != SeverityFailRecoverable {
		t.Errorf("severity=%q want fail-recoverable", v.Severity)
	}
	if exec.CertifyCalls != 1 || exec.MetricsCalls != 1 {
		t.Errorf("downstream stages must run after non-fatal failure: certify=%d metrics=%d",
			exec.CertifyCalls, exec.MetricsCalls)
	}
}

// TestRunAuditPipeline_MetricsOnlyWarnings: only the metrics stage flags →
// pass-with-warnings (operator can auto-approve at higher trust tiers).
func TestRunAuditPipeline_MetricsOnlyWarnings(t *testing.T) {
	exec := allPassFake()
	exec.StageMetrics = StageOutcome{Passed: false, DurationMS: 30, Detail: "CC regression: avg 8.2 → 11.4"}

	v := RunAuditPipeline(context.Background(), "/ws", []string{"f.go"}, exec)

	if v.Severity != SeverityPassWithWarnings {
		t.Errorf("severity=%q want pass-with-warnings", v.Severity)
	}
	if v.Passed {
		t.Error("Passed must be false even for pass-with-warnings (only true SeverityPass)")
	}
}

// TestRunAuditPipeline_DefensiveGuards: nil executor or empty inputs return
// fail-fatal verdict (caller bug — surface it via operator review queue).
func TestRunAuditPipeline_DefensiveGuards(t *testing.T) {
	cases := []struct {
		name      string
		workspace string
		files     []string
		exec      AuditExecutor
	}{
		{"nil exec", "/ws", []string{"f.go"}, nil},
		{"empty workspace", "", []string{"f.go"}, allPassFake()},
		{"nil files", "/ws", nil, allPassFake()},
		{"empty files", "/ws", []string{}, allPassFake()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := RunAuditPipeline(context.Background(), tc.workspace, tc.files, tc.exec)
			if v.Severity != SeverityFailFatal {
				t.Errorf("severity=%q want fail-fatal for caller bug", v.Severity)
			}
			if v.Passed {
				t.Error("defensive guard verdict must not be Passed")
			}
		})
	}
}

// TestMetricsDelta_EmptySnapshots: zero-value snapshots produce a passing
// report with all deltas at zero. Guards the trivial case where the audit
// runs against files with no measurable functions or tests yet.
func TestMetricsDelta_EmptySnapshots(t *testing.T) {
	rpt := MetricsDelta(MetricsSnapshot{}, MetricsSnapshot{})
	if !rpt.Passed {
		t.Fatalf("empty snapshots should pass, got reasons=%v", rpt.Reasons)
	}
	if rpt.CCAvgDelta != 0 || rpt.LOCDelta != 0 || rpt.TestsDelta != 0 {
		t.Errorf("non-zero delta on empty input: %+v", rpt)
	}
}

// TestMetricsDelta_PureImprovement: CC drops, LOC shrinks, tests grow.
// Healthy refactor — must Pass.
func TestMetricsDelta_PureImprovement(t *testing.T) {
	before := MetricsSnapshot{
		CCByFunc:   map[string]int{"pkg.A": 18, "pkg.B": 12},
		LOCByFile:  map[string]int{"a.go": 200},
		TestsByPkg: map[string]int{"pkg": 5},
	}
	after := MetricsSnapshot{
		CCByFunc:   map[string]int{"pkg.A": 8, "pkg.B": 12},
		LOCByFile:  map[string]int{"a.go": 150},
		TestsByPkg: map[string]int{"pkg": 7},
	}
	rpt := MetricsDelta(before, after)
	if !rpt.Passed {
		t.Errorf("pure improvement should pass, reasons=%v", rpt.Reasons)
	}
	if rpt.CCAvgDelta >= 0 {
		t.Errorf("CCAvgDelta should be negative, got %v", rpt.CCAvgDelta)
	}
	if rpt.LOCDelta != -50 {
		t.Errorf("LOCDelta=%d, want -50", rpt.LOCDelta)
	}
	if rpt.TestsDelta != 2 {
		t.Errorf("TestsDelta=%d, want 2", rpt.TestsDelta)
	}
}

// TestMetricsDelta_CCRegression: a single function whose CC went up trips
// the report regardless of LOC/test stability.
func TestMetricsDelta_CCRegression(t *testing.T) {
	before := MetricsSnapshot{CCByFunc: map[string]int{"pkg.A": 5, "pkg.B": 10}}
	after := MetricsSnapshot{CCByFunc: map[string]int{"pkg.A": 12, "pkg.B": 10}}
	rpt := MetricsDelta(before, after)
	if rpt.Passed {
		t.Error("CC regression must fail")
	}
	if len(rpt.CCRegressedFns) != 1 || rpt.CCRegressedFns[0] != "pkg.A" {
		t.Errorf("CCRegressedFns=%v, want [pkg.A]", rpt.CCRegressedFns)
	}
}

// TestMetricsDelta_LOCBloat: a file ≥ locBloatMinLines that grows beyond
// locBloatGrowthPct is flagged. Tiny files are excluded by design.
func TestMetricsDelta_LOCBloat(t *testing.T) {
	before := MetricsSnapshot{LOCByFile: map[string]int{
		"big.go":   100, // → 200, +100% bloat
		"tiny.go":  10,  // → 30, +200% but below min
		"stable.go": 100,
	}}
	after := MetricsSnapshot{LOCByFile: map[string]int{
		"big.go":   200,
		"tiny.go":  30,
		"stable.go": 105,
	}}
	rpt := MetricsDelta(before, after)
	if rpt.Passed {
		t.Error("LOC bloat must fail")
	}
	if len(rpt.LOCBloatedFiles) != 1 || rpt.LOCBloatedFiles[0] != "big.go" {
		t.Errorf("LOCBloatedFiles=%v, want [big.go]", rpt.LOCBloatedFiles)
	}
}

// TestMetricsDelta_TestDrop: any package's test count dropping fails the
// report — even if total test count went up due to a new package.
func TestMetricsDelta_TestDrop(t *testing.T) {
	before := MetricsSnapshot{TestsByPkg: map[string]int{"pkg/a": 10, "pkg/b": 5}}
	after := MetricsSnapshot{TestsByPkg: map[string]int{"pkg/a": 8, "pkg/b": 5, "pkg/c": 4}}
	rpt := MetricsDelta(before, after)
	if rpt.Passed {
		t.Error("test count drop must fail even when total grew")
	}
	if len(rpt.TestsDroppedPkgs) != 1 || rpt.TestsDroppedPkgs[0] != "pkg/a" {
		t.Errorf("TestsDroppedPkgs=%v, want [pkg/a]", rpt.TestsDroppedPkgs)
	}
	// Total still went up overall: 15 → 17.
	if rpt.TestsDelta != 2 {
		t.Errorf("TestsDelta=%d, want 2", rpt.TestsDelta)
	}
}

// TestMetricsDelta_NewFunctionsIgnored: a function present in `after` but
// not in `before` is not a regression — it's a new function. CC absent
// from `before` means we have no baseline to compare against.
func TestMetricsDelta_NewFunctionsIgnored(t *testing.T) {
	before := MetricsSnapshot{CCByFunc: map[string]int{"pkg.A": 5}}
	after := MetricsSnapshot{CCByFunc: map[string]int{"pkg.A": 5, "pkg.NewlyAdded": 14}}
	rpt := MetricsDelta(before, after)
	if !rpt.Passed {
		t.Errorf("new function should not be flagged as regression, reasons=%v", rpt.Reasons)
	}
	if len(rpt.CCRegressedFns) != 0 {
		t.Errorf("CCRegressedFns should be empty, got %v", rpt.CCRegressedFns)
	}
}

// TestMetricsDelta_ReasonsAreHumanReadable: when multiple regressions
// stack, all of them appear in Reasons for the AuditVerdict.FailureDetails
// surface used by the trust scoring system (138.B).
func TestMetricsDelta_ReasonsAreHumanReadable(t *testing.T) {
	before := MetricsSnapshot{
		CCByFunc:   map[string]int{"pkg.A": 5},
		LOCByFile:  map[string]int{"big.go": 100},
		TestsByPkg: map[string]int{"pkg": 10},
	}
	after := MetricsSnapshot{
		CCByFunc:   map[string]int{"pkg.A": 9},
		LOCByFile:  map[string]int{"big.go": 200},
		TestsByPkg: map[string]int{"pkg": 6},
	}
	rpt := MetricsDelta(before, after)
	if rpt.Passed {
		t.Fatal("triple regression must fail")
	}
	if len(rpt.Reasons) != 3 {
		t.Errorf("expected 3 reasons (cc, loc, tests), got %d: %v", len(rpt.Reasons), rpt.Reasons)
	}
}
