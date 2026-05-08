// Package state — daemon audit pipeline output types. [PILAR XXVII / 138.A.1]
//
// AuditVerdict represents the result of running the post-execution audit
// pipeline against a set of mutated files. The verdict drives the
// suggested-action logic in `neo_daemon` (138.C.5):
//
//	severity:pass               → auto-approve eligible at all tiers
//	severity:pass-with-warnings → auto-approve eligible only at L2/L3
//	severity:fail-recoverable   → operator review at any tier
//	severity:fail-fatal         → never auto-approve; reject + re-enqueue
//
// The struct is BoltDB-serializable (gob/json compatible) for persistence
// in the `daemon_results` bucket created by 138.C.4.
package state

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"
)

// FailureKind enumerates the audit pipeline steps that can fail.
// Each kind maps to one stage of RunAuditPipeline (138.A.2):
//
//	FailureBuild       — `go build ./...` failed (compile error)
//	FailureASTAudit    — neo_radar AST_AUDIT surfaced CC>15, shadow vars,
//	                     infinite loops, or other static analysis issues
//	FailureTests       — `go test -short` failed for an affected package
//	FailureCertify     — neo_sre_certify_mutation(dry_run) bouncer rejected
//	                     (zero-alloc violation, complexity intent mismatch, ...)
//	FailureMetrics     — MetricsDelta detected regression (CC up, tests
//	                     dropped, LOC bloated beyond threshold)
type FailureKind string

const (
	FailureBuild    FailureKind = "build"
	FailureASTAudit FailureKind = "ast_audit"
	FailureTests    FailureKind = "tests"
	FailureCertify  FailureKind = "certify"
	FailureMetrics  FailureKind = "metrics"
)

// AuditSeverity is the aggregate verdict computed from the failures slice.
// It drives the operator-vs-auto-approve decision in 138.C.5.
type AuditSeverity string

const (
	// SeverityPass — pipeline ran clean, no failures detected.
	SeverityPass AuditSeverity = "pass"

	// SeverityPassWithWarnings — only metrics regressions present
	// (FailureMetrics with no other failures). Trust tier L2/L3 may still
	// auto-approve; L0/L1 prompt the operator.
	SeverityPassWithWarnings AuditSeverity = "pass-with-warnings"

	// SeverityFailRecoverable — tests or AST audit failed. The change
	// compiled, certified, and didn't bloat the codebase, but a fix is
	// needed. Operator review required at all tiers.
	SeverityFailRecoverable AuditSeverity = "fail-recoverable"

	// SeverityFailFatal — build or certify failed. The mutation cannot
	// proceed; reject + re-enqueue with retries++ and never auto-approve
	// regardless of tier.
	SeverityFailFatal AuditSeverity = "fail-fatal"
)

// AuditVerdict captures the full output of one RunAuditPipeline invocation.
// Persisted in the daemon_results BoltDB bucket (138.C.4) so the trust
// scoring system (138.B) can replay verdicts when adjusting Beta-distribution
// counters.
type AuditVerdict struct {
	// Passed is true when severity == SeverityPass. Convenience shortcut for
	// callers that only want the boolean — full nuance lives in Severity.
	Passed bool `json:"passed"`

	// Severity is the aggregate classification of the verdict (see consts).
	Severity AuditSeverity `json:"severity"`

	// Failures lists every FailureKind that the pipeline detected. Empty
	// when Severity == SeverityPass. Order reflects pipeline stage order
	// (build → ast → tests → certify → metrics) so the first entry is the
	// earliest-stage failure when multiple stages tripped.
	Failures []FailureKind `json:"failures,omitempty"`

	// FailureDetails maps each failed FailureKind to the StageOutcome.Detail
	// string captured by the executor (compiler stderr first line, test
	// failure name, AST issue summary, etc.). Critical for the trust
	// scoring system (138.B) — the textual reason distinguishes "infra
	// failure" from "model quality failure" without re-running the
	// pipeline. Empty when Severity == SeverityPass. [DeepSeek red-team
	// finding 2026-04-30: Detail was previously dropped, losing debug
	// context across BoltDB persistence.]
	FailureDetails map[FailureKind]string `json:"failure_details,omitempty"`

	// Metrics carries quantitative deltas computed by MetricsDelta (138.A.3):
	//
	//	"cc_avg_before"      — average cyclomatic complexity pre-mutation
	//	"cc_avg_after"       — average CC post-mutation
	//	"cc_max_after"       — max CC across mutated files post-mutation
	//	"loc_delta"          — net lines added/removed
	//	"test_count_delta"   — change in test count for affected packages
	//	"build_duration_ms"  — wall-clock of `go build ./...`
	//	"tests_duration_ms"  — wall-clock of `go test -short`
	//
	// Empty map when the pipeline aborted early (e.g. fail-fatal at build).
	Metrics map[string]float64 `json:"metrics,omitempty"`

	// Files lists the mutated files that were submitted to the pipeline.
	// Mirrors the input to RunAuditPipeline; preserved here so the verdict
	// is self-describing for replay/debugging.
	Files []string `json:"files"`

	// DurationMS is the total wall-clock of the pipeline in milliseconds
	// (CompletedAt − StartedAt rounded to milliseconds).
	DurationMS int64 `json:"duration_ms"`

	// StartedAt is the wall-clock at which RunAuditPipeline began work.
	StartedAt time.Time `json:"started_at"`

	// CompletedAt is the wall-clock at which the pipeline emitted the verdict.
	// Equals StartedAt + DurationMS within rounding.
	CompletedAt time.Time `json:"completed_at"`
}

// computeSeverity derives the aggregate AuditSeverity from a Failures slice.
// Centralised so RunAuditPipeline (138.A.2) and unit tests share the same
// classification logic — keeps the operator-vs-auto-approve threshold
// consistent across pipeline updates.
//
// Conservative-by-default for unknown kinds: any FailureKind not explicitly
// recognized treats hasMetricsOnly=false. New kinds added in the future
// (e.g. FailureLinting) without updating this switch fall through to the
// final fail-recoverable return rather than silently producing a
// pass-with-warnings verdict. [DeepSeek red-team finding 2026-04-30]
func computeSeverity(failures []FailureKind) AuditSeverity {
	if len(failures) == 0 {
		return SeverityPass
	}
	hasBuild := false
	hasCertify := false
	hasTests := false
	hasAST := false
	hasMetricsOnly := true
	for _, f := range failures {
		switch f {
		case FailureBuild:
			hasBuild = true
			hasMetricsOnly = false
		case FailureCertify:
			hasCertify = true
			hasMetricsOnly = false
		case FailureTests:
			hasTests = true
			hasMetricsOnly = false
		case FailureASTAudit:
			hasAST = true
			hasMetricsOnly = false
		case FailureMetrics:
			// Recognized as metrics-only candidate — leave hasMetricsOnly as-is.
		default:
			// Unknown FailureKind — be conservative: do NOT treat as
			// metrics-only, so the final fall-through returns
			// SeverityFailRecoverable instead of pass-with-warnings.
			hasMetricsOnly = false
		}
	}
	switch {
	case hasBuild || hasCertify:
		return SeverityFailFatal
	case hasTests || hasAST:
		return SeverityFailRecoverable
	case hasMetricsOnly:
		return SeverityPassWithWarnings
	}
	return SeverityFailRecoverable
}

// newVerdict builds a finalized AuditVerdict from raw pipeline state.
// Called once at the end of RunAuditPipeline (138.A.2) — the sole
// constructor for AuditVerdict so timestamp + severity invariants hold.
func newVerdict(files []string, failures []FailureKind, details map[FailureKind]string, metrics map[string]float64, started time.Time) AuditVerdict {
	now := time.Now()
	severity := computeSeverity(failures)
	return AuditVerdict{
		Passed:         severity == SeverityPass,
		Severity:       severity,
		Failures:       failures,
		FailureDetails: details,
		Metrics:        metrics,
		Files:          files,
		DurationMS:     now.Sub(started).Milliseconds(),
		StartedAt:      started,
		CompletedAt:    now,
	}
}

// StageOutcome is the per-stage result returned by AuditExecutor methods.
// It carries enough metadata for RunAuditPipeline to (a) decide whether to
// short-circuit downstream stages (e.g. skip tests when build failed) and
// (b) populate AuditVerdict.Metrics with quantitative per-stage data.
//
// Passed=false means the stage detected a fault — the caller appends the
// matching FailureKind to the verdict's Failures slice. Errors only fire
// when the stage itself could not run (e.g. exec.Cmd failed to spawn);
// they do NOT signal a quality failure — the pipeline records the stage
// as failed but the AuditExecutor implementation may distinguish the two.
type StageOutcome struct {
	Passed     bool
	DurationMS int64
	// Detail is a free-form string the executor may attach (e.g. compiler
	// stderr first line, test failure name, AST issue summary). Used by
	// the audit log + operator UI; not consumed by RunAuditPipeline logic.
	Detail string
	// Extra carries stage-specific numeric metrics that get merged into
	// AuditVerdict.Metrics under prefixed keys (e.g. tests stage emits
	// "tests_count_after"). Keys MUST be prefix-stable across runs so the
	// trust scoring system (138.B) can compute deltas.
	Extra map[string]float64
}

// AuditExecutor is the contract RunAuditPipeline calls into for each
// stage. The pkg/state package defines the interface and orchestration;
// concrete implementations live in cmd/neo-mcp (which has access to the
// in-process radar/certify tools) and in tests (mocks). This inversion
// keeps pkg/state low-level and avoids importing cmd/neo-mcp.
//
// All methods take a context — the caller passes a deadline or
// cancellation signal so any single stage cannot hang the pipeline.
// Implementations MUST honor ctx.Done() and return a non-nil error when
// the context fires.
type AuditExecutor interface {
	// Build runs `go build ./...` (or equivalent) against the workspace.
	// The mutated files list is passed for context — typical impls scope
	// the build to affected packages but a full build is acceptable.
	Build(ctx context.Context, workspace string, files []string) (StageOutcome, error)

	// ASTAudit invokes neo_radar AST_AUDIT on the mutated files. Failed
	// when CC>15, shadow vars, or other static-analysis issues surface.
	ASTAudit(ctx context.Context, workspace string, files []string) (StageOutcome, error)

	// Tests runs `go test -short` against packages affected by the
	// mutated files. Determining "affected" is the impl's job (typically
	// via BLAST_RADIUS or the CPG); naive impls may run the full suite.
	Tests(ctx context.Context, workspace string, files []string) (StageOutcome, error)

	// CertifyDryRun calls neo_sre_certify_mutation with dry_run=true to
	// exercise the bouncer (zero-alloc, complexity intent) without
	// writing the seal. The return value indicates whether the bouncer
	// would accept the change at certify time.
	CertifyDryRun(ctx context.Context, workspace string, files []string) (StageOutcome, error)

	// MetricsDelta computes pre/post quantitative deltas (CC, LOC,
	// test-count). It runs LAST so it sees the post-mutation state.
	// Outcome.Passed=false signals regression beyond an impl-defined
	// threshold (CC up by ≥3, tests dropped, LOC bloated by ≥30%).
	MetricsDelta(ctx context.Context, workspace string, files []string) (StageOutcome, error)
}

// RunAuditPipeline executes the post-mutation audit pipeline sequentially
// and returns an AuditVerdict capturing every stage outcome. Stage order
// (build → ast → tests → certify → metrics) is intentional:
//
//   - build first because nothing else makes sense if the code doesn't
//     compile (downstream stages would fail noisily, not informatively);
//   - ast next because static analysis is fast and surfaces issues that
//     test runs would miss;
//   - tests + certify exercise the runtime + bouncer respectively —
//     longer wall-clocks, run only when earlier stages didn't fail-fatal;
//   - metrics last because it needs the full post-mutation state.
//
// Short-circuit policy: if Build fails, all downstream stages are skipped
// (their outcomes will be misleading). If any other stage fails, later
// stages still run — operator wants the full picture for decision-making.
//
// Error vs. !Passed semantics: an executor returning a non-nil error
// indicates the stage couldn't execute (process spawn failed, ctx
// cancelled). The pipeline treats this the same as Passed=false (records
// the FailureKind) but appends the error message to the stage Detail so
// operators can distinguish "stage ran and failed" from "stage didn't
// run at all" via the detail string.
func RunAuditPipeline(ctx context.Context, workspace string, files []string, exec AuditExecutor) AuditVerdict {
	started := time.Now()
	if exec == nil || workspace == "" || len(files) == 0 {
		// Defensive — these are caller bugs, not pipeline failures.
		// Return a fail-fatal verdict with a synthetic build failure so
		// the suggested-action logic (138.C.5) routes to the operator.
		return newVerdict(files, []FailureKind{FailureBuild}, nil, nil, started)
	}

	var failures []FailureKind
	details := map[FailureKind]string{}
	metrics := map[string]float64{}

	// Stage 1: Build — short-circuit guard for everything else.
	if outcome, err := exec.Build(ctx, workspace, files); !outcome.Passed || err != nil {
		failures = append(failures, FailureBuild)
		recordFailureDetail(details, FailureBuild, outcome.Detail, err)
		mergeStageMetrics(metrics, "build", outcome)
		return newVerdict(files, failures, details, metrics, started)
	} else {
		mergeStageMetrics(metrics, "build", outcome)
	}

	// Stage 2: AST audit.
	if outcome, err := exec.ASTAudit(ctx, workspace, files); !outcome.Passed || err != nil {
		failures = append(failures, FailureASTAudit)
		recordFailureDetail(details, FailureASTAudit, outcome.Detail, err)
		mergeStageMetrics(metrics, "ast", outcome)
	} else {
		mergeStageMetrics(metrics, "ast", outcome)
	}

	// Stage 3: Tests.
	if outcome, err := exec.Tests(ctx, workspace, files); !outcome.Passed || err != nil {
		failures = append(failures, FailureTests)
		recordFailureDetail(details, FailureTests, outcome.Detail, err)
		mergeStageMetrics(metrics, "tests", outcome)
	} else {
		mergeStageMetrics(metrics, "tests", outcome)
	}

	// Stage 4: Certify dry-run.
	if outcome, err := exec.CertifyDryRun(ctx, workspace, files); !outcome.Passed || err != nil {
		failures = append(failures, FailureCertify)
		recordFailureDetail(details, FailureCertify, outcome.Detail, err)
		mergeStageMetrics(metrics, "certify", outcome)
	} else {
		mergeStageMetrics(metrics, "certify", outcome)
	}

	// Stage 5: Metrics delta.
	if outcome, err := exec.MetricsDelta(ctx, workspace, files); !outcome.Passed || err != nil {
		failures = append(failures, FailureMetrics)
		recordFailureDetail(details, FailureMetrics, outcome.Detail, err)
		mergeStageMetrics(metrics, "metrics", outcome)
	} else {
		mergeStageMetrics(metrics, "metrics", outcome)
	}

	return newVerdict(files, failures, details, metrics, started)
}

// recordFailureDetail captures a stage's textual context into the verdict's
// FailureDetails map. Prefers outcome.Detail; falls back to err.Error() so
// the operator + trust scoring system always see SOMETHING. [DeepSeek
// red-team finding 2026-04-30: detail was previously dropped.]
func recordFailureDetail(dst map[FailureKind]string, kind FailureKind, detail string, err error) {
	switch {
	case detail != "":
		dst[kind] = detail
	case err != nil:
		dst[kind] = "exec error: " + err.Error()
	default:
		dst[kind] = "(no detail provided by executor)"
	}
}

// mergeStageMetrics folds a StageOutcome's per-stage data into the
// pipeline-level metrics map. Stage prefix prevents key collisions when
// multiple stages report similar concepts.
//
// Hardenings (DeepSeek red-team findings 2026-04-30):
//   - Reserved suffix "duration_ms" — Extra entries with this key are
//     ignored to prevent a buggy/malicious executor from overwriting the
//     canonical timing metric written from outcome.DurationMS.
//   - NaN/Inf rejection — float64 values that fail json.Marshal are
//     skipped. Without this, a single NaN poisons BoltDB persistence
//     because json.Marshal returns "json: unsupported value: NaN".
const reservedExtraSuffix = "duration_ms"

func mergeStageMetrics(dst map[string]float64, stage string, outcome StageOutcome) {
	dst[stage+"_"+reservedExtraSuffix] = float64(outcome.DurationMS)
	for k, v := range outcome.Extra {
		if k == reservedExtraSuffix {
			continue // protect canonical timing metric
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue // protect downstream JSON serialization
		}
		dst[stage+"_"+k] = v
	}
}

// MetricsSnapshot is a frozen view of code-quality metrics at one point.
// Captured before and after a mutation; fed into MetricsDelta to detect
// regressions that pass build+tests but degrade the codebase silently.
//
// Keys are not constrained to the mutated set — callers may include
// transitively affected functions/files when relevant.
type MetricsSnapshot struct {
	CCByFunc   map[string]int `json:"cc_by_func"`   // "pkg.Func" → cyclomatic complexity
	LOCByFile  map[string]int `json:"loc_by_file"`  // file path → physical LOC
	TestsByPkg map[string]int `json:"tests_by_pkg"` // pkg path → Test* count
	CapturedAt time.Time      `json:"captured_at"`
}

// MetricsDeltaReport summarizes the regression analysis between two snapshots.
//
// Passed is false when any per-function CC went up, any file exceeded the LOC
// bloat threshold, or any package's test count dropped. Reasons holds a
// human-readable list suitable for AuditVerdict.FailureDetails[FailureMetrics].
type MetricsDeltaReport struct {
	Passed bool `json:"passed"`

	CCAvgBefore    float64  `json:"cc_avg_before"`
	CCAvgAfter     float64  `json:"cc_avg_after"`
	CCAvgDelta     float64  `json:"cc_avg_delta"`
	CCRegressedFns []string `json:"cc_regressed_fns,omitempty"`

	LOCBefore       int      `json:"loc_before"`
	LOCAfter        int      `json:"loc_after"`
	LOCDelta        int      `json:"loc_delta"`
	LOCBloatedFiles []string `json:"loc_bloated_files,omitempty"`

	TestsBefore      int      `json:"tests_before"`
	TestsAfter       int      `json:"tests_after"`
	TestsDelta       int      `json:"tests_delta"`
	TestsDroppedPkgs []string `json:"tests_dropped_pkgs,omitempty"`

	Reasons []string `json:"reasons,omitempty"`
}

// LOC bloat threshold: a file ≥ locBloatMinLines that grows by more than
// locBloatGrowthPct between snapshots is flagged as bloated. Tiny files
// (helpers, generated stubs) are excluded so a 10→16 LOC change doesn't
// trip a 60% growth alarm.
const (
	locBloatMinLines  = 50
	locBloatGrowthPct = 0.50
)

// MetricsDelta compares two MetricsSnapshots and returns a regression report.
// Pure function — no I/O, deterministic. Callers (typically the audit
// executor in cmd/neo-mcp) compute snapshots via AST_AUDIT/file walks and
// hand them in. [138.A.3]
func MetricsDelta(before, after MetricsSnapshot) MetricsDeltaReport {
	rpt := MetricsDeltaReport{Passed: true}
	diffCC(before, after, &rpt)
	diffLOC(before, after, &rpt)
	diffTests(before, after, &rpt)
	collectReasons(&rpt)
	return rpt
}

func diffCC(before, after MetricsSnapshot, rpt *MetricsDeltaReport) {
	rpt.CCAvgBefore = avgCC(before.CCByFunc)
	rpt.CCAvgAfter = avgCC(after.CCByFunc)
	rpt.CCAvgDelta = rpt.CCAvgAfter - rpt.CCAvgBefore
	for fn, ccBefore := range before.CCByFunc {
		if ccAfter, ok := after.CCByFunc[fn]; ok && ccAfter > ccBefore {
			rpt.CCRegressedFns = append(rpt.CCRegressedFns, fn)
		}
	}
	sort.Strings(rpt.CCRegressedFns)
}

func diffLOC(before, after MetricsSnapshot, rpt *MetricsDeltaReport) {
	for _, n := range before.LOCByFile {
		rpt.LOCBefore += n
	}
	for _, n := range after.LOCByFile {
		rpt.LOCAfter += n
	}
	rpt.LOCDelta = rpt.LOCAfter - rpt.LOCBefore
	for f, locBefore := range before.LOCByFile {
		locAfter, ok := after.LOCByFile[f]
		if !ok || locBefore < locBloatMinLines {
			continue
		}
		if float64(locAfter-locBefore)/float64(locBefore) > locBloatGrowthPct {
			rpt.LOCBloatedFiles = append(rpt.LOCBloatedFiles, f)
		}
	}
	sort.Strings(rpt.LOCBloatedFiles)
}

func diffTests(before, after MetricsSnapshot, rpt *MetricsDeltaReport) {
	for _, n := range before.TestsByPkg {
		rpt.TestsBefore += n
	}
	for _, n := range after.TestsByPkg {
		rpt.TestsAfter += n
	}
	rpt.TestsDelta = rpt.TestsAfter - rpt.TestsBefore
	for pkg, testsBefore := range before.TestsByPkg {
		if after.TestsByPkg[pkg] < testsBefore {
			rpt.TestsDroppedPkgs = append(rpt.TestsDroppedPkgs, pkg)
		}
	}
	sort.Strings(rpt.TestsDroppedPkgs)
}

func collectReasons(rpt *MetricsDeltaReport) {
	if len(rpt.CCRegressedFns) > 0 {
		rpt.Passed = false
		rpt.Reasons = append(rpt.Reasons, fmt.Sprintf("cc regressed in %d function(s)", len(rpt.CCRegressedFns)))
	}
	if len(rpt.LOCBloatedFiles) > 0 {
		rpt.Passed = false
		rpt.Reasons = append(rpt.Reasons, fmt.Sprintf("loc bloated in %d file(s)", len(rpt.LOCBloatedFiles)))
	}
	if len(rpt.TestsDroppedPkgs) > 0 {
		rpt.Passed = false
		// Mention raw drop magnitude when total also went down; otherwise
		// the per-pkg list is the headline (total may still be positive
		// because a new package was added).
		if rpt.TestsDelta < 0 {
			rpt.Reasons = append(rpt.Reasons, fmt.Sprintf("test count dropped by %d", -rpt.TestsDelta))
		} else {
			rpt.Reasons = append(rpt.Reasons, fmt.Sprintf("tests dropped in %d package(s)", len(rpt.TestsDroppedPkgs)))
		}
	}
}

func avgCC(m map[string]int) float64 {
	if len(m) == 0 {
		return 0
	}
	sum := 0
	for _, cc := range m {
		sum += cc
	}
	return float64(sum) / float64(len(m))
}
