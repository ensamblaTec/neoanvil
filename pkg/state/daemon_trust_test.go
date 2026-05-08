package state

import (
	"encoding/json"
	"math"
	"sync"
	"testing"
	"time"
)

// TestNewTrustScore_Prior: a freshly created score sits on the uniform
// Beta(1,1) prior — no evidence accumulated, default L0 tier, manual_warmup
// off. Verifies the structural floor that lazy decay (138.B.2) must
// preserve forever.
func TestNewTrustScore_Prior(t *testing.T) {
	s := NewTrustScore("refactor", ".go:pkg/state")
	if s.Alpha != 1 || s.Beta != 1 {
		t.Errorf("prior: want α=1 β=1, got α=%v β=%v", s.Alpha, s.Beta)
	}
	if s.CurrentTier != TierL0 {
		t.Errorf("default tier: want L0, got %q", s.CurrentTier)
	}
	if s.TotalExecutions != 0 {
		t.Errorf("TotalExecutions should be 0, got %d", s.TotalExecutions)
	}
	if s.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures should be 0, got %d", s.ConsecutiveFailures)
	}
	if s.ManualWarmup {
		t.Error("ManualWarmup must default to false — only TrustWarmup sets it")
	}
	if s.LastUpdate.IsZero() {
		t.Error("LastUpdate should be set to the construction time")
	}
}

// TestTrustScore_Key: keys are "pattern:scope". Used as BoltDB key —
// must be stable for the same input forever.
func TestTrustScore_Key(t *testing.T) {
	s := NewTrustScore("refactor", ".go:pkg/state")
	if k := s.Key(); k != "refactor:.go:pkg/state" {
		t.Errorf("Key()=%q, want %q", k, "refactor:.go:pkg/state")
	}
}

// TestTrustScore_JSONRoundtrip: the persisted form survives marshal+unmarshal
// without dropping any field. Important because TrustScore is stored in
// BoltDB as JSON and the daemon may run for weeks across restarts —
// silent field drops would degrade trust math.
func TestTrustScore_JSONRoundtrip(t *testing.T) {
	src := TrustScore{
		Pattern:             "refactor",
		Scope:               ".go:pkg/state",
		Alpha:               42.5,
		Beta:                7.5,
		LastUpdate:          time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		TotalExecutions:     50,
		CurrentTier:         TierL2,
		ConsecutiveFailures: 1,
		ManualWarmup:        true,
	}
	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var dst TrustScore
	if err := json.Unmarshal(raw, &dst); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if dst != src {
		t.Errorf("roundtrip diverged:\nsrc=%+v\ndst=%+v", src, dst)
	}
}

// TestFailureCategory_AllConstants: catch typos in const declarations.
// If anyone renames a constant and forgets a usage site, this test
// fails before we hit BoltDB key drift in production.
func TestFailureCategory_AllConstants(t *testing.T) {
	cases := []struct {
		name string
		got  FailureCategory
		want string
	}{
		{"Success", OutcomeSuccess, "success"},
		{"Infra", OutcomeInfra, "infra"},
		{"SubOptimal", OutcomeSubOptimal, "sub_optimal"},
		{"Quality", OutcomeQuality, "quality"},
		{"OperatorOverride", OutcomeOperatorOverride, "operator_override"},
	}
	for _, tc := range cases {
		if string(tc.got) != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestTier_AllConstants: same rationale for tier strings — they are
// persisted and the daemon's promote/demote logic depends on exact match.
func TestTier_AllConstants(t *testing.T) {
	cases := []struct {
		got  Tier
		want string
	}{
		{TierL0, "L0"},
		{TierL1, "L1"},
		{TierL2, "L2"},
		{TierL3, "L3"},
	}
	for _, tc := range cases {
		if string(tc.got) != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// TestBayesianUpdate_AlphaBeta: success bumps α, failure bumps β, and
// LastUpdate refreshes. TotalExecutions counts every call regardless.
func TestBayesianUpdate_AlphaBeta(t *testing.T) {
	t0 := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)

	s := NewTrustScore("p", "s")
	s.LastUpdate = t0

	s.BayesianUpdate(true, t1)
	if s.Alpha != 2 || s.Beta != 1 {
		t.Errorf("after success: want α=2 β=1, got α=%v β=%v", s.Alpha, s.Beta)
	}
	if s.TotalExecutions != 1 || !s.LastUpdate.Equal(t1) {
		t.Errorf("totals/timestamp wrong: %+v", s)
	}

	s.BayesianUpdate(false, t1.Add(time.Hour))
	if s.Alpha != 2 || s.Beta != 2 {
		t.Errorf("after fail: want α=2 β=2, got α=%v β=%v", s.Alpha, s.Beta)
	}
	if s.TotalExecutions != 2 {
		t.Errorf("TotalExecutions=%d, want 2", s.TotalExecutions)
	}
}

// TestEffectiveAlphaBeta_PreservesPriorFloor: after a long idle window,
// accumulated evidence decays toward zero but α and β never drop below
// 1 (the uniform prior is structural, not evidence). This is the key
// invariant that keeps the math well-behaved across multi-month silences.
func TestEffectiveAlphaBeta_PreservesPriorFloor(t *testing.T) {
	t0 := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	s := TrustScore{Alpha: 50, Beta: 30, LastUpdate: t0}

	// 10000 hours later: factor ≈ 0.99^10000 ≈ 0
	a, b := s.EffectiveAlphaBeta(t0.Add(10000 * time.Hour))
	if a < 1 || b < 1 {
		t.Errorf("α/β must never drop below 1 prior, got α=%v β=%v", a, b)
	}
	if a > 1.01 || b > 1.01 {
		t.Errorf("expected near-prior after 10k h, got α=%v β=%v", a, b)
	}
}

// TestEffectiveAlphaBeta_NoDecayWhenFresh: hoursSince ≤ 0 returns the raw
// stored values unchanged. Important because clock skew or back-dated
// LastUpdate must never inflate evidence above what was stored.
func TestEffectiveAlphaBeta_NoDecayWhenFresh(t *testing.T) {
	t0 := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	s := TrustScore{Alpha: 5, Beta: 3, LastUpdate: t0}

	// Same instant.
	a, b := s.EffectiveAlphaBeta(t0)
	if a != 5 || b != 3 {
		t.Errorf("same instant: got α=%v β=%v, want α=5 β=3", a, b)
	}

	// "Now" before LastUpdate (clock skew).
	a, b = s.EffectiveAlphaBeta(t0.Add(-time.Hour))
	if a != 5 || b != 3 {
		t.Errorf("clock skew: got α=%v β=%v, want unchanged", a, b)
	}
}

// TestPointEstimate_HighEvidence: with a lot of evidence and matching
// success rate, the point estimate matches α/(α+β) closely.
func TestPointEstimate_HighEvidence(t *testing.T) {
	t0 := time.Now()
	s := TrustScore{Alpha: 81, Beta: 21, LastUpdate: t0} // prior + 80 succ + 20 fail
	pe := s.PointEstimate(t0)
	want := 81.0 / 102.0 // ≈ 0.794
	if math.Abs(pe-want) > 1e-9 {
		t.Errorf("PointEstimate=%v, want %v", pe, want)
	}
}

// TestLowerBound_PenalizesLowEvidence: with very little data, the lower
// bound sits well below the point estimate. With abundant data, it
// approaches the point estimate. This is the property the tier system
// (138.B.3) relies on to keep new patterns conservative.
func TestLowerBound_PenalizesLowEvidence(t *testing.T) {
	t0 := time.Now()

	// 1 success, 0 failures (just the prior). Mean is 2/3 but variance
	// is large, so lower bound must be much smaller.
	low := TrustScore{Alpha: 2, Beta: 1, LastUpdate: t0}
	pe1 := low.PointEstimate(t0)
	lb1 := low.LowerBound(t0)
	if lb1 >= pe1 {
		t.Errorf("low evidence: LowerBound(%v) should be below PointEstimate(%v)", lb1, pe1)
	}
	if pe1-lb1 < 0.2 {
		t.Errorf("low evidence: gap should be wide, got %v", pe1-lb1)
	}

	// 800 successes, 200 failures. Mean still 0.8, but variance is small.
	high := TrustScore{Alpha: 801, Beta: 201, LastUpdate: t0}
	pe2 := high.PointEstimate(t0)
	lb2 := high.LowerBound(t0)
	if pe2-lb2 > 0.05 {
		t.Errorf("high evidence: gap should be tight, got %v", pe2-lb2)
	}
}

// TestLowerBound_ClampedToUnitInterval: math is normal-approximation, so
// extreme α with tiny β can produce a "lower bound" above 1 in raw form.
// LowerBound clamps. Same on the negative side.
func TestLowerBound_Clamped(t *testing.T) {
	t0 := time.Now()
	// Pathological "all success" — clamped to ≤ 1.
	allSucc := TrustScore{Alpha: 1000, Beta: 1, LastUpdate: t0}
	if lb := allSucc.LowerBound(t0); lb < 0 || lb > 1 {
		t.Errorf("LowerBound out of [0,1]: %v", lb)
	}
	// Empty-ish — clamped to ≥ 0.
	low := TrustScore{Alpha: 1, Beta: 100, LastUpdate: t0}
	if lb := low.LowerBound(t0); lb < 0 {
		t.Errorf("LowerBound negative: %v", lb)
	}
}

// TestPointEstimate_DecayDoesNotMovePriorPattern: a pattern with no real
// evidence (just the (1,1) prior) returns 0.5 forever — decay only
// touches accumulated evidence, not the prior.
func TestPointEstimate_DecayDoesNotMovePriorPattern(t *testing.T) {
	t0 := time.Now()
	s := NewTrustScore("p", "s")
	s.LastUpdate = t0

	for _, h := range []float64{0, 1, 24, 168, 10000} {
		pe := s.PointEstimate(t0.Add(time.Duration(h) * time.Hour))
		if math.Abs(pe-0.5) > 1e-9 {
			t.Errorf("prior at t+%vh: PointEstimate=%v, want 0.5", h, pe)
		}
	}
}

// TestTierFor_NewScoreIsL0: a brand-new score sits at L0 regardless of
// any other state. Bayesian floor + the minExecsForPromote gate combine
// to keep new patterns conservative.
func TestTierFor_NewScoreIsL0(t *testing.T) {
	s := NewTrustScore("p", "s")
	if got := TierFor(s, time.Now()); got != TierL0 {
		t.Errorf("new score: TierFor=%q, want L0", got)
	}
}

// TestTierFor_MinExecsGate: a score with high LowerBound but TotalExecutions
// below minExecsForPromote stays at L0. Defends against a 5-success
// streak earning L1 auto-approval before the pattern is well-tested.
func TestTierFor_MinExecsGate(t *testing.T) {
	t0 := time.Now()
	// 49 successes, 0 failures — α=50, β=1, lb≈0.93 (would be L2).
	// But TotalExecutions=49 < 50, so gate forces L0.
	s := TrustScore{
		Alpha:           50,
		Beta:            1,
		LastUpdate:      t0,
		TotalExecutions: 49,
	}
	if got := TierFor(s, t0); got != TierL0 {
		t.Errorf("below min execs: TierFor=%q, want L0 (lb=%v)", got, s.LowerBound(t0))
	}
	// One more execution lifts the gate.
	s.TotalExecutions = 50
	if got := TierFor(s, t0); got == TierL0 {
		t.Errorf("at min execs: gate should release, TierFor=%q lb=%v", got, s.LowerBound(t0))
	}
}

// TestTierFor_ThresholdMapping: covers the four tier bands. Assumes the
// gate is satisfied (TotalExecutions ≥ 50). Each case picks α/β so the
// LowerBound lands in the target band; we then assert TierFor returns
// the expected tier.
func TestTierFor_ThresholdMapping(t *testing.T) {
	t0 := time.Now()
	cases := []struct {
		name  string
		alpha float64
		beta  float64
		want  Tier
	}{
		// 60 succ / 40 fail → mean 0.6, lb ≈ 0.504 → L0
		{"L0 lb<0.65", 60, 40, TierL0},
		// 75 succ / 25 fail → mean 0.75, lb ≈ 0.665 → L1
		{"L1 0.65≤lb<0.85", 75, 25, TierL1},
		// 470 succ / 30 fail → mean 0.94, lb ≈ 0.919 → L2
		{"L2 0.85≤lb<0.95", 470, 30, TierL2},
		// 1990 succ / 10 fail → mean 0.995, lb ≈ 0.992 → L3
		{"L3 lb≥0.95", 1990, 10, TierL3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := TrustScore{
				Alpha:           tc.alpha,
				Beta:            tc.beta,
				LastUpdate:      t0,
				TotalExecutions: 1000, // gate cleared
			}
			got := TierFor(s, t0)
			if got != tc.want {
				t.Errorf("α=%v β=%v lb=%v: TierFor=%q, want %q",
					tc.alpha, tc.beta, s.LowerBound(t0), got, tc.want)
			}
		})
	}
}

// TestRecordOutcome_Success: success bumps α, resets streak, ticks total.
func TestRecordOutcome_Success(t *testing.T) {
	t0 := time.Now()
	s := NewTrustScore("p", "s")
	s.ConsecutiveFailures = 2
	s.Alpha = 5
	s.Beta = 2

	s.RecordOutcome(OutcomeSuccess, t0)

	if s.Alpha != 6 || s.Beta != 2 {
		t.Errorf("after success: want α=6 β=2, got α=%v β=%v", s.Alpha, s.Beta)
	}
	if s.ConsecutiveFailures != 0 {
		t.Errorf("success must reset streak, got %d", s.ConsecutiveFailures)
	}
	if s.TotalExecutions != 1 {
		t.Errorf("TotalExecutions=%d, want 1", s.TotalExecutions)
	}
}

// TestRecordOutcome_Infra: infra is a no-op for α/β/streak. Only
// TotalExecutions and LastUpdate move. Critical: in-flight failure
// streaks are NOT reset by Infra (operator hasn't seen good behavior,
// just dodged a bullet). [DESIGN UPDATE 2026-04-30]
func TestRecordOutcome_Infra(t *testing.T) {
	t0 := time.Now()
	s := NewTrustScore("p", "s")
	s.ConsecutiveFailures = 2
	s.Alpha = 5
	s.Beta = 3

	s.RecordOutcome(OutcomeInfra, t0)

	if s.Alpha != 5 || s.Beta != 3 {
		t.Errorf("infra must not touch α/β, got α=%v β=%v", s.Alpha, s.Beta)
	}
	if s.ConsecutiveFailures != 2 {
		t.Errorf("infra must not touch streak, got %d", s.ConsecutiveFailures)
	}
	if s.TotalExecutions != 1 {
		t.Errorf("TotalExecutions=%d, want 1", s.TotalExecutions)
	}
}

// TestRecordOutcome_SubOptimal: β += 0.5, streak++.
func TestRecordOutcome_SubOptimal(t *testing.T) {
	t0 := time.Now()
	s := NewTrustScore("p", "s")
	s.RecordOutcome(OutcomeSubOptimal, t0)
	if s.Beta != 1.5 {
		t.Errorf("β=%v, want 1.5", s.Beta)
	}
	if s.ConsecutiveFailures != 1 {
		t.Errorf("streak=%d, want 1", s.ConsecutiveFailures)
	}
}

// TestRecordOutcome_Quality: β += 1, streak++.
func TestRecordOutcome_Quality(t *testing.T) {
	t0 := time.Now()
	s := NewTrustScore("p", "s")
	s.RecordOutcome(OutcomeQuality, t0)
	if s.Beta != 2 {
		t.Errorf("β=%v, want 2", s.Beta)
	}
	if s.ConsecutiveFailures != 1 {
		t.Errorf("streak=%d, want 1", s.ConsecutiveFailures)
	}
}

// TestRecordOutcome_OperatorOverride: β += 5 — strongest signal.
func TestRecordOutcome_OperatorOverride(t *testing.T) {
	t0 := time.Now()
	s := NewTrustScore("p", "s")
	s.RecordOutcome(OutcomeOperatorOverride, t0)
	if s.Beta != 6 {
		t.Errorf("β=%v, want 6", s.Beta)
	}
	if s.ConsecutiveFailures != 1 {
		t.Errorf("streak=%d, want 1", s.ConsecutiveFailures)
	}
}

// TestRecordOutcome_DemoteOn3ConsecutiveFailures: 3 quality fails in a
// row demote tier to L0 even if LowerBound math would say otherwise.
// Score starts at L2 with 1000 execs and 940 successes — well-loved
// pattern that just had a bad afternoon.
func TestRecordOutcome_DemoteOn3ConsecutiveFailures(t *testing.T) {
	t0 := time.Now()
	s := TrustScore{
		Alpha:           941, // 940 succ + prior
		Beta:            61,  // 60 fail + prior
		LastUpdate:      t0,
		TotalExecutions: 1000,
		CurrentTier:     TierL2,
	}

	s.RecordOutcome(OutcomeQuality, t0)
	if s.CurrentTier != TierL2 {
		t.Errorf("after 1 quality fail: tier=%q, want L2", s.CurrentTier)
	}

	s.RecordOutcome(OutcomeQuality, t0)
	if s.CurrentTier != TierL2 {
		t.Errorf("after 2 quality fails: tier=%q, want L2", s.CurrentTier)
	}

	s.RecordOutcome(OutcomeQuality, t0)
	if s.CurrentTier != TierL0 {
		t.Errorf("after 3 quality fails: tier=%q, want L0 (streak demote)", s.CurrentTier)
	}
}

// TestRecordOutcome_InfraDoesNotResetStreak: an Infra in the middle of
// a quality-failure streak must NOT reset the count. Otherwise a flaky
// network would mask consecutive model failures.
func TestRecordOutcome_InfraDoesNotResetStreak(t *testing.T) {
	t0 := time.Now()
	s := NewTrustScore("p", "s")

	s.RecordOutcome(OutcomeQuality, t0)
	s.RecordOutcome(OutcomeQuality, t0)
	if s.ConsecutiveFailures != 2 {
		t.Fatalf("setup: streak=%d, want 2", s.ConsecutiveFailures)
	}

	s.RecordOutcome(OutcomeInfra, t0)
	if s.ConsecutiveFailures != 2 {
		t.Errorf("infra must not reset streak, got %d", s.ConsecutiveFailures)
	}

	// One more quality fail should trip the demote.
	s.RecordOutcome(OutcomeQuality, t0)
	if s.ConsecutiveFailures != 3 {
		t.Errorf("expected 3 after second quality, got %d", s.ConsecutiveFailures)
	}
}

// TestRecordOutcome_SuccessResetsStreak: a single Success at any point
// in a streak resets the count. Demote-on-streak only fires when the
// failures are uninterrupted.
func TestRecordOutcome_SuccessResetsStreak(t *testing.T) {
	t0 := time.Now()
	s := NewTrustScore("p", "s")

	s.RecordOutcome(OutcomeQuality, t0)
	s.RecordOutcome(OutcomeQuality, t0)
	s.RecordOutcome(OutcomeSuccess, t0)
	if s.ConsecutiveFailures != 0 {
		t.Errorf("success must reset streak, got %d", s.ConsecutiveFailures)
	}

	// Two more failures should not trigger demote (streak restarted).
	s.RecordOutcome(OutcomeQuality, t0)
	s.RecordOutcome(OutcomeQuality, t0)
	if s.ConsecutiveFailures != 2 {
		t.Errorf("streak=%d, want 2 after restart", s.ConsecutiveFailures)
	}
}

// TestResolvePatternScope_StandardCase: the canonical example from the
// design doc — a "refactor" task targeting pkg/state/planner.go produces
// the expected key "refactor:.go:pkg/state".
func TestResolvePatternScope_StandardCase(t *testing.T) {
	task := SRETask{
		Description: "refactor logger en pkg/state/planner.go",
		TargetFile:  "pkg/state/planner.go",
	}
	pattern, scope := ResolvePatternScope(task)
	if pattern != "refactor" {
		t.Errorf("pattern=%q, want refactor", pattern)
	}
	if scope != ".go:pkg/state" {
		t.Errorf("scope=%q, want .go:pkg/state", scope)
	}
}

// TestResolvePatternScope_DescriptionPath: when TargetFile is empty,
// fall back to scanning the description for a path-like token.
func TestResolvePatternScope_DescriptionPath(t *testing.T) {
	task := SRETask{Description: "distill the logs from pkg/sre/oracle.go"}
	pattern, scope := ResolvePatternScope(task)
	if pattern != "distill" {
		t.Errorf("pattern=%q, want distill", pattern)
	}
	if scope != ".go:pkg/sre" {
		t.Errorf("scope=%q, want .go:pkg/sre", scope)
	}
}

// TestResolvePatternScope_UnknownPattern: a description with no keyword
// match still resolves to a key (just under "unknown" pattern).
func TestResolvePatternScope_UnknownPattern(t *testing.T) {
	task := SRETask{
		Description: "fix typo en pkg/state/planner.go",
		TargetFile:  "pkg/state/planner.go",
	}
	pattern, scope := ResolvePatternScope(task)
	if pattern != "unknown" {
		t.Errorf("pattern=%q, want unknown", pattern)
	}
	if scope != ".go:pkg/state" {
		t.Errorf("scope=%q, want .go:pkg/state", scope)
	}
}

// TestResolvePatternScope_EmptyTask: catch-all bucket when nothing can
// be extracted. Daemon always has a TrustScore key.
func TestResolvePatternScope_EmptyTask(t *testing.T) {
	task := SRETask{}
	pattern, scope := ResolvePatternScope(task)
	if pattern != "unknown" {
		t.Errorf("pattern=%q, want unknown", pattern)
	}
	if scope != "unknown:unknown" {
		t.Errorf("scope=%q, want unknown:unknown", scope)
	}
}

// TestResolvePatternScope_NoExtension: file with no .ext gets "noext".
func TestResolvePatternScope_NoExtension(t *testing.T) {
	task := SRETask{
		Description: "audit script",
		TargetFile:  "scripts/install/setup",
	}
	_, scope := ResolvePatternScope(task)
	if scope != "noext:scripts/install" {
		t.Errorf("scope=%q, want noext:scripts/install", scope)
	}
}

// TestResolvePatternScope_RootFile: a file at repo root has no parent
// directory, so the scope's dir component is "root".
func TestResolvePatternScope_RootFile(t *testing.T) {
	task := SRETask{
		Description: "document Makefile",
		TargetFile:  "Makefile",
	}
	pattern, scope := ResolvePatternScope(task)
	if pattern != "document" {
		t.Errorf("pattern=%q, want document", pattern)
	}
	if scope != "noext:root" {
		t.Errorf("scope=%q, want noext:root", scope)
	}
}

// TestResolvePatternScope_DeeplyNested: trim to last 2 dir components
// for stability across moves.
func TestResolvePatternScope_DeeplyNested(t *testing.T) {
	task := SRETask{
		Description: "rename method",
		TargetFile:  "cmd/plugin-deepseek/handlers/red_team/audit.go",
	}
	_, scope := ResolvePatternScope(task)
	if scope != ".go:handlers/red_team" {
		t.Errorf("scope=%q, want .go:handlers/red_team (last 2)", scope)
	}
}

// TestResolvePatternScope_FirstKeywordWins: when multiple keywords
// appear in the description, the first one in eligibilityPatterns order
// wins. "audit" comes before "refactor" — defends against a task
// description "refactor and audit X" being misclassified.
func TestResolvePatternScope_FirstKeywordWins(t *testing.T) {
	task := SRETask{
		Description: "refactor and audit pkg/state/planner.go",
		TargetFile:  "pkg/state/planner.go",
	}
	pattern, _ := ResolvePatternScope(task)
	if pattern != "audit" {
		t.Errorf("pattern=%q, want audit (audit precedes refactor in priority list)", pattern)
	}
}

// TestTrustScore_Convergence: simulating 1000 calls with a fixed 80%
// success rate, the point estimate must land within ±0.02 of 0.8 well
// before sample 200. Validates the end-to-end Bayesian update + decay
// path (decay should be near-zero across the simulation since calls
// happen at the same instant — we want pure convergence behavior).
//
// Without good convergence, tier promotion would be noisy and the
// daemon would oscillate between L1/L2 mid-session.
func TestTrustScore_Convergence(t *testing.T) {
	t0 := time.Now()
	s := NewTrustScore("p", "s")
	s.LastUpdate = t0

	// Deterministic 80% success: indices 0..7 success, 8..9 fail per block.
	const trials = 1000
	const targetRate = 0.8
	var converged bool
	var convergedAt int

	for i := range trials {
		if i%10 < 8 {
			s.RecordOutcome(OutcomeSuccess, t0)
		} else {
			s.RecordOutcome(OutcomeQuality, t0)
		}
		if !converged && i >= 50 {
			pe := s.PointEstimate(t0)
			if pe >= targetRate-0.02 && pe <= targetRate+0.02 {
				converged = true
				convergedAt = i + 1
			}
		}
	}

	if !converged {
		t.Fatalf("never converged in %d trials, final pe=%v", trials, s.PointEstimate(t0))
	}
	if convergedAt > 200 {
		t.Errorf("converged at sample %d, want ≤ 200", convergedAt)
	}
	final := s.PointEstimate(t0)
	if final < targetRate-0.02 || final > targetRate+0.02 {
		t.Errorf("final pe=%v drifted outside ±0.02 of %v", final, targetRate)
	}
}

// TestTrustScore_DecayOverTime: a pattern with 100 mixed outcomes is
// idle for progressively longer windows. PointEstimate stays near the
// recorded mean shortly after, but as decay kicks in over weeks/months
// the estimate drifts toward 0.5 (the prior). This validates that the
// daemon eventually re-evaluates stale patterns rather than trusting
// year-old evidence forever.
func TestTrustScore_DecayOverTime(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := NewTrustScore("p", "s")
	s.LastUpdate = t0

	// 80 successes + 20 failures (success rate 0.8). Pattern earned L2
	// in the past.
	for i := range 100 {
		if i%5 == 0 {
			s.RecordOutcome(OutcomeQuality, t0)
		} else {
			s.RecordOutcome(OutcomeSuccess, t0)
		}
	}
	freshMean := s.PointEstimate(t0)
	if freshMean < 0.78 || freshMean > 0.82 {
		t.Fatalf("setup: fresh mean=%v, want ≈0.8", freshMean)
	}

	// 1 hour later — virtually no decay.
	short := s.PointEstimate(t0.Add(time.Hour))
	if math.Abs(short-freshMean) > 0.01 {
		t.Errorf("1h decay too aggressive: %v → %v", freshMean, short)
	}

	// 1 month later — significant drift toward prior.
	month := s.PointEstimate(t0.Add(30 * 24 * time.Hour))
	if month >= freshMean {
		t.Errorf("1mo decay should pull toward 0.5, got %v ≥ fresh %v", month, freshMean)
	}
	if math.Abs(month-0.5) > math.Abs(freshMean-0.5) {
		t.Errorf("1mo: should be closer to prior 0.5, got %v (fresh %v)", month, freshMean)
	}

	// 1 year later — within 0.05 of the prior.
	year := s.PointEstimate(t0.Add(365 * 24 * time.Hour))
	if math.Abs(year-0.5) > 0.05 {
		t.Errorf("1yr decay: pe=%v should be within 0.05 of prior 0.5", year)
	}
}

// TestTrustScore_RecoveryAfterDemote: a streak of failures demotes a
// score to L0. After the operator manually validates a few correct
// outputs, the streak resets and TierFor can promote again — provided
// the gate of 50 execs is still satisfied (which it is, since
// TotalExecutions is monotonic).
func TestTrustScore_RecoveryAfterDemote(t *testing.T) {
	t0 := time.Now()
	// Start L2-worthy: 470 succ, 30 fail.
	s := TrustScore{
		Alpha: 471, Beta: 31,
		LastUpdate:      t0,
		TotalExecutions: 500,
		CurrentTier:     TierL2,
	}

	// 3 quality fails → demote to L0.
	for range 3 {
		s.RecordOutcome(OutcomeQuality, t0)
	}
	if s.CurrentTier != TierL0 {
		t.Fatalf("expected L0 after demote, got %q", s.CurrentTier)
	}

	// Now operator-approved successes — streak resets, tier recomputed.
	s.RecordOutcome(OutcomeSuccess, t0)
	if s.ConsecutiveFailures != 0 {
		t.Errorf("streak should reset, got %d", s.ConsecutiveFailures)
	}
	// LowerBound is still high enough for L2 (3 quality didn't move
	// the needle much against 470 successes).
	if s.CurrentTier == TierL0 {
		t.Errorf("after recovery: should re-promote past L0, got %q (lb=%v, total=%d)",
			s.CurrentTier, s.LowerBound(t0), s.TotalExecutions)
	}
}

// TestTierFor_DecayDemotesIdleScore: a pattern that earned L2 but has
// been idle for a year should drift back toward L0 via decay alone, even
// without new failures. The execution gate stays satisfied (count is
// historical), but LowerBound shrinks as evidence dampens.
func TestTierFor_DecayDemotesIdleScore(t *testing.T) {
	t0 := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	s := TrustScore{
		Alpha:           470,
		Beta:            30,
		LastUpdate:      t0,
		TotalExecutions: 500,
	}
	tier0 := TierFor(s, t0)
	if tier0 != TierL2 {
		t.Fatalf("baseline: TierFor=%q, want L2", tier0)
	}

	// 1 year later (≈ 8760 h) — accumulated evidence ≈ 0, returns to prior.
	tier1 := TierFor(s, t0.Add(365*24*time.Hour))
	if tier1 != TierL0 {
		t.Errorf("after 1y idle: TierFor=%q, want L0 (lb=%v)", tier1, s.LowerBound(t0.Add(365*24*time.Hour)))
	}
}

// TestTrustGet_FreshPriorWhenAbsent: reading a (pattern, scope) that
// was never written returns a (1,1) prior — daemon never sees a nil
// score, the lookup is total.
func TestTrustGet_FreshPriorWhenAbsent(t *testing.T) {
	setupTestPlanner(t)
	s, err := TrustGet("never-seen", "noext:nowhere")
	if err != nil {
		t.Fatalf("TrustGet: %v", err)
	}
	if s.Alpha != 1 || s.Beta != 1 {
		t.Errorf("absent score: want prior (1,1), got α=%v β=%v", s.Alpha, s.Beta)
	}
	if s.Pattern != "never-seen" || s.Scope != "noext:nowhere" {
		t.Errorf("absent score: identity not propagated, got %q:%q", s.Pattern, s.Scope)
	}
}

// TestTrustUpdate_RoundTrip: write then read recovers identical state.
// Smoke test for the JSON+BoltDB persistence path.
func TestTrustUpdate_RoundTrip(t *testing.T) {
	setupTestPlanner(t)
	const pattern = "refactor"
	const scope = ".go:pkg/state"

	err := TrustUpdate(pattern, scope, func(s *TrustScore) {
		s.RecordOutcome(OutcomeSuccess, time.Now())
		s.RecordOutcome(OutcomeSuccess, time.Now())
		s.RecordOutcome(OutcomeQuality, time.Now())
	})
	if err != nil {
		t.Fatalf("TrustUpdate: %v", err)
	}

	got, err := TrustGet(pattern, scope)
	if err != nil {
		t.Fatalf("TrustGet: %v", err)
	}
	if got.Alpha != 3 || got.Beta != 2 {
		t.Errorf("roundtrip: want α=3 β=2, got α=%v β=%v", got.Alpha, got.Beta)
	}
	if got.TotalExecutions != 3 {
		t.Errorf("TotalExecutions=%d, want 3", got.TotalExecutions)
	}
}

// TestTrustRecord_Concurrency: 100 goroutines call TrustRecord on the
// same (pattern, scope). Final TotalExecutions must equal 100 — bbolt
// serializes Update transactions, so no goroutine's increment is lost
// to a read+modify+write race. This is the central guarantee of B.8. [138.B.8]
func TestTrustRecord_Concurrency(t *testing.T) {
	setupTestPlanner(t)
	const pattern = "refactor"
	const scope = ".go:pkg/state"
	const goroutines = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)
	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			cat := OutcomeSuccess
			if idx%5 == 0 {
				cat = OutcomeQuality
			}
			if err := TrustRecord(pattern, scope, cat); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("TrustRecord goroutine error: %v", err)
	}

	got, err := TrustGet(pattern, scope)
	if err != nil {
		t.Fatalf("TrustGet: %v", err)
	}
	if got.TotalExecutions != goroutines {
		t.Errorf("TotalExecutions=%d, want %d (lost updates)", got.TotalExecutions, goroutines)
	}
	// 80 successes + 20 quality fails: α should be 81 (+ prior), β = 21 (+ prior).
	if got.Alpha != 81 {
		t.Errorf("α=%v, want 81 (80 succ + 1 prior)", got.Alpha)
	}
	if got.Beta != 21 {
		t.Errorf("β=%v, want 21 (20 fail + 1 prior)", got.Beta)
	}
}

// TestTrustWarmup_SeedsAndMarks: the operator imports a pre-validated
// score. Alpha/Beta replace the prior, ManualWarmup flips to true,
// CurrentTier reflects the seeded evidence (gate must clear).
func TestTrustWarmup_SeedsAndMarks(t *testing.T) {
	setupTestPlanner(t)
	const pattern = "boilerplate"
	const scope = ".go:cmd/plugin-deepseek"

	// Seed with 100 successes / 5 fails — well above gate, easily L2.
	if err := TrustWarmup(pattern, scope, 101, 6); err != nil {
		t.Fatalf("TrustWarmup: %v", err)
	}

	got, err := TrustGet(pattern, scope)
	if err != nil {
		t.Fatalf("TrustGet: %v", err)
	}
	if got.Alpha != 101 || got.Beta != 6 {
		t.Errorf("warmup α/β: want 101/6, got %v/%v", got.Alpha, got.Beta)
	}
	if !got.ManualWarmup {
		t.Error("ManualWarmup must be true after warmup")
	}
	if got.TotalExecutions != 105 {
		t.Errorf("TotalExecutions=%d, want 105 (101+6-2 prior)", got.TotalExecutions)
	}
	if got.CurrentTier == TierL0 {
		t.Errorf("CurrentTier=%q, want ≥ L1 (warmup should clear gate)", got.CurrentTier)
	}
}

// TestTrustWarmup_RejectsBelowPrior: warmup must reject α<1 or β<1 —
// would corrupt the structural prior floor that lazy decay relies on.
func TestTrustWarmup_RejectsBelowPrior(t *testing.T) {
	setupTestPlanner(t)
	if err := TrustWarmup("p", "s", 0.5, 1); err == nil {
		t.Error("warmup α<1 must error")
	}
	if err := TrustWarmup("p", "s", 1, 0.5); err == nil {
		t.Error("warmup β<1 must error")
	}
}

// TestTrustWarmup_OverwritesExisting: re-warming the same key replaces
// the prior state. Useful for re-baselining after a major model upgrade
// (operator wants to wipe the slate without removing the bucket).
func TestTrustWarmup_OverwritesExisting(t *testing.T) {
	setupTestPlanner(t)
	const pattern = "audit"
	const scope = ".go:pkg/state"

	// First warmup: low evidence, would land L0.
	if err := TrustWarmup(pattern, scope, 5, 4); err != nil {
		t.Fatalf("first warmup: %v", err)
	}
	// Second warmup: high evidence, lands L2.
	if err := TrustWarmup(pattern, scope, 200, 30); err != nil {
		t.Fatalf("second warmup: %v", err)
	}

	got, err := TrustGet(pattern, scope)
	if err != nil {
		t.Fatalf("TrustGet: %v", err)
	}
	if got.Alpha != 200 || got.Beta != 30 {
		t.Errorf("after overwrite: α/β = %v/%v, want 200/30", got.Alpha, got.Beta)
	}
}

// TestSuggestAction_TierL0AlwaysPrompt: L0 means "no evidence yet"
// (or demoted via streak). Daemon must never auto-approve at L0
// regardless of what the audit pipeline says.
func TestSuggestAction_TierL0AlwaysPrompt(t *testing.T) {
	cases := []AuditSeverity{
		SeverityPass,
		SeverityPassWithWarnings,
		SeverityFailRecoverable,
		SeverityFailFatal,
	}
	for _, sev := range cases {
		v := AuditVerdict{Severity: sev}
		got := suggestActionWithRand(TierL0, v, func() float64 { return 0.0 })
		if got != ActionPromptOperator {
			t.Errorf("L0 + %q: got %q, want prompt-operator", sev, got)
		}
	}
}

// TestSuggestAction_TierL1SpotCheck: L1 lands on prompt-operator at the
// configured spot-check rate, auto-approve otherwise. Pure-RNG path —
// the audit verdict is irrelevant at this tier (audit has already
// gated entry to the trust system).
func TestSuggestAction_TierL1SpotCheck(t *testing.T) {
	v := AuditVerdict{Severity: SeverityPass}

	// RNG returns 0.0 — below threshold → prompt-operator.
	if got := suggestActionWithRand(TierL1, v, func() float64 { return 0.0 }); got != ActionPromptOperator {
		t.Errorf("L1 RNG=0.0: got %q, want prompt-operator", got)
	}
	// RNG just below 0.2 — still spot-check.
	if got := suggestActionWithRand(TierL1, v, func() float64 { return 0.199 }); got != ActionPromptOperator {
		t.Errorf("L1 RNG=0.199: got %q, want prompt-operator", got)
	}
	// RNG at 0.2 (exactly the threshold, since `<` is strict) — auto.
	if got := suggestActionWithRand(TierL1, v, func() float64 { return 0.2 }); got != ActionAutoApprove {
		t.Errorf("L1 RNG=0.2: got %q, want auto-approve", got)
	}
	// RNG above threshold — auto.
	if got := suggestActionWithRand(TierL1, v, func() float64 { return 0.5 }); got != ActionAutoApprove {
		t.Errorf("L1 RNG=0.5: got %q, want auto-approve", got)
	}
}

// TestSuggestAction_TierL2GatesOnAudit: L2 auto-approves only when the
// audit returns SeverityPass. Anything else (warnings, recoverable,
// fatal) routes back to operator.
func TestSuggestAction_TierL2GatesOnAudit(t *testing.T) {
	cases := []struct {
		sev  AuditSeverity
		want SuggestedAction
	}{
		{SeverityPass, ActionAutoApprove},
		{SeverityPassWithWarnings, ActionPromptOperator},
		{SeverityFailRecoverable, ActionPromptOperator},
		{SeverityFailFatal, ActionPromptOperator},
		{"", ActionPromptOperator}, // empty severity = skeleton path = conservative
	}
	for _, tc := range cases {
		v := AuditVerdict{Severity: tc.sev}
		// RNG=0.0 — proves L2 ignores RNG (no spot-check at L2).
		got := suggestActionWithRand(TierL2, v, func() float64 { return 0.0 })
		if got != tc.want {
			t.Errorf("L2 + %q: got %q, want %q", tc.sev, got, tc.want)
		}
	}
}

// TestSuggestAction_TierL3GatesOnSeverity: L3 auto-approves Pass and
// PassWithWarnings, escalates FailRecoverable to operator (test
// regressions need review even from trusted patterns), rejects
// FailFatal outright. [DeepSeek TRUST-LOGIC-002]
func TestSuggestAction_TierL3GatesOnSeverity(t *testing.T) {
	cases := []struct {
		sev  AuditSeverity
		want SuggestedAction
	}{
		{SeverityPass, ActionAutoApprove},
		{SeverityPassWithWarnings, ActionAutoApprove},
		{SeverityFailRecoverable, ActionPromptOperator}, // 138.C.5+DS-001: escalate, don't auto-merge regressions
		{SeverityFailFatal, ActionReject},
	}
	for _, tc := range cases {
		v := AuditVerdict{Severity: tc.sev}
		got := suggestActionWithRand(TierL3, v, func() float64 { return 0.0 })
		if got != tc.want {
			t.Errorf("L3 + %q: got %q, want %q", tc.sev, got, tc.want)
		}
	}
}

// TestSuggestAction_EmptySeverityNeverAutoApproves: an empty Severity
// (skeleton path or partial wiring) routes to prompt-operator regardless
// of tier — defense in depth against future callers forgetting to
// populate the audit verdict. [DeepSeek TRUST-LOGIC-001]
func TestSuggestAction_EmptySeverityNeverAutoApproves(t *testing.T) {
	v := AuditVerdict{Severity: ""}
	tiers := []Tier{TierL0, TierL1, TierL2, TierL3}
	for _, tier := range tiers {
		// RNG=0.99 — at L1 would normally auto-approve, but empty severity
		// short-circuits before the spot-check.
		got := suggestActionWithRand(tier, v, func() float64 { return 0.99 })
		if got != ActionPromptOperator {
			t.Errorf("tier=%q + empty severity: got %q, want prompt-operator", tier, got)
		}
	}
}

// TestSuggestAction_DefaultUsesPackageRand: smoke test that the public
// SuggestAction returns one of the three valid values without panicking.
// The actual decision is non-deterministic for L1 — we just check the
// shape.
func TestSuggestAction_DefaultUsesPackageRand(t *testing.T) {
	v := AuditVerdict{Severity: SeverityPass}
	got := SuggestAction(TierL2, v)
	switch got {
	case ActionAutoApprove, ActionPromptOperator, ActionReject:
		// fine
	default:
		t.Errorf("SuggestAction returned unexpected value %q", got)
	}
}

// TestListTrustScores_EmptyBucket: bucket doesn't exist yet → empty
// slice + nil error. Daemon's trust_status surfaces "no data yet"
// without treating absence as failure. [138.C.6]
func TestListTrustScores_EmptyBucket(t *testing.T) {
	setupTestPlanner(t)
	scores, skipped, err := ListTrustScores()
	if err != nil {
		t.Fatalf("ListTrustScores: %v", err)
	}
	if len(scores) != 0 {
		t.Errorf("empty bucket: got %d scores, want 0", len(scores))
	}
	if skipped != 0 {
		t.Errorf("empty bucket: skipped=%d, want 0", skipped)
	}
}

// TestListTrustScores_ReturnsAll: write 3 scores, list returns all 3
// with intact identity + counters. Order is bucket-iteration order
// (not guaranteed) — caller is responsible for sorting.
func TestListTrustScores_ReturnsAll(t *testing.T) {
	setupTestPlanner(t)
	now := time.Now()
	for _, p := range []struct{ pat, scope string }{
		{"refactor", ".go:pkg/state"},
		{"audit", ".go:pkg/sre"},
		{"distill", ".md:docs"},
	} {
		if err := TrustUpdate(p.pat, p.scope, func(s *TrustScore) {
			s.Alpha += 5
			s.LastUpdate = now
		}); err != nil {
			t.Fatalf("seed %s/%s: %v", p.pat, p.scope, err)
		}
	}

	scores, skipped, err := ListTrustScores()
	if err != nil {
		t.Fatalf("ListTrustScores: %v", err)
	}
	if len(scores) != 3 {
		t.Fatalf("got %d scores, want 3", len(scores))
	}
	if skipped != 0 {
		t.Errorf("clean bucket: skipped=%d, want 0", skipped)
	}
	keys := map[string]bool{}
	for _, s := range scores {
		keys[s.Key()] = true
		if s.Alpha != 6 { // prior 1 + 5 from update
			t.Errorf("%s: α=%v, want 6", s.Key(), s.Alpha)
		}
	}
	for _, want := range []string{"refactor:.go:pkg/state", "audit:.go:pkg/sre", "distill:.md:docs"} {
		if !keys[want] {
			t.Errorf("missing key %s in result", want)
		}
	}
}

// TestTrustGet_OfflineReturnsPrior: when plannerDB is nil (test harness
// or boot-failure), TrustGet returns a usable prior + sentinel error so
// the daemon can degrade to "default L0 conservative" instead of
// crashing.
func TestTrustGet_OfflineReturnsPrior(t *testing.T) {
	// Force offline state — no setupTestPlanner.
	prevDB := plannerDB
	plannerDB = nil
	t.Cleanup(func() { plannerDB = prevDB })

	s, err := TrustGet("p", "s")
	if err == nil {
		t.Error("expected sentinel error when offline")
	}
	if s.Alpha != 1 || s.Beta != 1 {
		t.Errorf("offline: should return fresh prior, got α=%v β=%v", s.Alpha, s.Beta)
	}
}
