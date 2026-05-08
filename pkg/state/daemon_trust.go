// Package state — daemon trust scoring system. [PILAR XXVII / 138.B.1]
//
// TrustScore is a per-(pattern, scope) Beta-distribution that tracks how
// reliably a model+task combination produces audit-passing outcomes. The
// daemon (138.C) consults the score before deciding whether to auto-approve
// a mutation or escalate to the operator.
//
// The Beta distribution is the natural conjugate prior for a Bernoulli
// process (each task either passes audit or doesn't). Alpha counts
// successes plus a uniform prior of 1; Beta counts failures plus a prior
// of 1. The point estimate of trust is α/(α+β); the lower bound at 95%
// confidence is what the daemon uses for tiering decisions — that way
// new patterns with little data sit in the most cautious tier even if
// their first few outcomes look good.
//
// Decay is lazy on read (138.B.2): the persisted struct stores raw α/β
// counts, and any read computes effective values by dampening the
// evidence with 0.99^hoursSinceLastUpdate. The prior of 1 never decays —
// it's a structural floor, not evidence.
//
// FailureCategory (138.B.4) splits "outcome != success" into four classes
// so infrastructure bugs don't poison the model's score. This is the
// design correction discovered during 138.A.1 hands-on, where a missing
// MkdirAll in plugin-deepseek would have penalized DeepSeek's β counter
// despite the model itself being correct.
package state

import (
	"encoding/json"
	"errors"
	"log"
	"math"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

// daemonTrustBucket is the BoltDB bucket holding TrustScore records,
// keyed by `pattern:scope` (the same string TrustScore.Key() emits).
const daemonTrustBucket = "daemon_trust"

// errTrustOffline is returned when the package-level plannerDB is nil
// (typical during tests that didn't call InitPlanner). Callers should
// treat trust unavailability as "default to L0 conservative" rather
// than failing the daemon outright.
var errTrustOffline = errors.New("trust store offline (plannerDB nil)")

// decayPerHour is the multiplicative dampening applied per elapsed hour
// since LastUpdate. After 24h, evidence weight = 0.99^24 ≈ 0.786.
// After 168h (1 week), ≈ 0.184. Patterns the daemon stops exercising
// drift back toward the uniform prior over weeks rather than seconds.
const decayPerHour = 0.99

// confidence95Z is the standard normal quantile for the 95% lower bound.
const confidence95Z = 1.96

// Tier promotion thresholds operate on LowerBound(now). Boundaries are
// inclusive on the lower side: lb=0.85 promotes to L2.
const (
	tierL1Threshold = 0.65
	tierL2Threshold = 0.85
	tierL3Threshold = 0.95
)

// minExecsForPromote is the hard sample-count floor below which a score
// stays at L0 regardless of LowerBound. Defends against a 5-success
// streak earning auto-approval on a brand-new pattern: even if Bayesian
// math gives lb≥0.65 with very low evidence, business rules say "no auto-
// approve until we've seen this pattern at least 50 times". [138.B.3]
const minExecsForPromote = 50

// Tier classifies a TrustScore into an auto-approval bucket. Tier values
// are ordered: L0 < L1 < L2 < L3. Higher tier = more autonomy granted to
// the daemon for that (pattern, scope).
type Tier string

const (
	// TierL0 — manual review required at any tier. Default for new
	// patterns until they accumulate evidence.
	TierL0 Tier = "L0"
	// TierL1 — auto-approve trivial mutations (BUG_FIX, doc updates).
	TierL1 Tier = "L1"
	// TierL2 — auto-approve standard mutations (FEATURE_ADD with passing
	// tests, refactors with no CC regression).
	TierL2 Tier = "L2"
	// TierL3 — auto-approve aggressive mutations (multi-file refactors,
	// dependency upgrades). Reserved for patterns with >0.95 confidence.
	TierL3 Tier = "L3"
)

// FailureCategory classifies non-success outcomes for the trust score
// update logic. The four categories carry different weights:
//
//	Success           — α += 1
//	Infra             — no-op (bug in plugin/transport, not the model)
//	SubOptimal        — β += 0.5 (output correct but noisy/redundant)
//	Quality           — β += 1   (output incorrect or hallucinated)
//	OperatorOverride  — β += 5   (operator rejected; strong signal)
//
// Infra failures also do NOT count toward the consecutive-failure streak
// that demotes a score to L0 (138.B.4).
type FailureCategory string

const (
	OutcomeSuccess          FailureCategory = "success"
	OutcomeInfra            FailureCategory = "infra"
	OutcomeSubOptimal       FailureCategory = "sub_optimal"
	OutcomeQuality          FailureCategory = "quality"
	OutcomeOperatorOverride FailureCategory = "operator_override"
)

// TrustScore is the persisted state for one (Pattern, Scope) bucket.
// Stored in the `daemon_trust` BoltDB bucket keyed by Pattern+":"+Scope.
//
// Alpha and Beta are stored as float64 — not int — because OutcomeSubOptimal
// adds 0.5 to Beta. Even though the prior is integer (1, 1), accumulated
// evidence may not be.
//
// LastUpdate is the timestamp of the most recent RecordOutcome that
// changed Alpha or Beta. Lazy decay reads this to compute hoursSince.
//
// TotalExecutions is the raw count of all outcomes recorded (including
// Infra). Used as the gate for tier promotion (min 50 execs before L0→L1
// is allowed, 138.B.3).
//
// CurrentTier is denormalized from TierFor(score) — recomputed on every
// RecordOutcome and persisted so the daemon can read tier without
// recomputing. Acts as a cache; the source of truth is TierFor.
//
// ConsecutiveFailures counts non-Infra non-Success outcomes since the
// last Success or Infra. Resets on Success, untouched on Infra. When it
// reaches 3, RecordOutcome demotes CurrentTier to L0 immediately
// regardless of LowerBound.
//
// ManualWarmup is set by 138.B.7 when the operator pre-approves a pattern
// with synthetic evidence. Audit trail: TrustScore-derived decisions for
// patterns with ManualWarmup:true are flagged in the daemon log so the
// operator can distinguish learned trust from imported trust.
type TrustScore struct {
	Pattern             string    `json:"pattern"`
	Scope               string    `json:"scope"`
	Alpha               float64   `json:"alpha"`
	Beta                float64   `json:"beta"`
	LastUpdate          time.Time `json:"last_update"`
	TotalExecutions     int       `json:"total_executions"`
	CurrentTier         Tier      `json:"current_tier"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	ManualWarmup        bool      `json:"manual_warmup,omitempty"`
}

// NewTrustScore returns a fresh TrustScore with the uniform Beta(1,1)
// prior. CurrentTier is L0 — new patterns earn their tier through
// evidence, never start higher.
func NewTrustScore(pattern, scope string) TrustScore {
	return TrustScore{
		Pattern:     pattern,
		Scope:       scope,
		Alpha:       1,
		Beta:        1,
		LastUpdate:  time.Now(),
		CurrentTier: TierL0,
	}
}

// Key returns the BoltDB key for this score: "pattern:scope".
// Stable, deterministic, used by both RecordOutcome and TrustWarmup.
func (s TrustScore) Key() string {
	return s.Pattern + ":" + s.Scope
}

// unknownScope is the fallback (pattern, scope) when extraction fails.
// Tasks without a recognizable keyword or path still need a key in the
// trust BoltDB — they all bucket together under "unknown:unknown:unknown".
const unknownToken = "unknown"

// ResolvePatternScope extracts a (pattern, scope) tuple from an SRETask
// for trust score lookup. Pattern is a 132.F eligibility keyword
// (refactor, distill, audit, ...); scope describes where the task lives
// as "file_ext:dir_root", where dir_root is the last two path components
// of TargetFile's directory.
//
// Examples:
//
//	{Desc:"refactor logger en pkg/state/planner.go", TargetFile:"pkg/state/planner.go"}
//	  → pattern="refactor", scope=".go:pkg/state"
//
//	{Desc:"audit handler", TargetFile:"cmd/neo-mcp/main.go"}
//	  → pattern="audit", scope=".go:cmd/neo-mcp"
//
//	{Desc:"fix typo"} (no keyword, no file)
//	  → pattern="unknown", scope="unknown:unknown"
//
// Both fields normalize to "unknown" when extraction fails — the daemon
// always has a key, and tasks that defy classification accumulate under
// the catch-all bucket. [138.B.5]
func ResolvePatternScope(task SRETask) (pattern, scope string) {
	return extractTrustPattern(task.Description), extractTrustScope(task.TargetFile, task.Description)
}

func extractTrustPattern(description string) string {
	if description == "" {
		return unknownToken
	}
	lower := strings.ToLower(description)
	for _, p := range eligibilityPatterns {
		if strings.Contains(lower, p.keyword) {
			return p.keyword
		}
	}
	return unknownToken
}

func extractTrustScope(targetFile, description string) string {
	path := targetFile
	if path == "" {
		path = extractPathFromText(description)
	}
	if path == "" {
		return unknownToken + ":" + unknownToken
	}
	ext := filepath.Ext(path)
	if ext == "" {
		ext = "noext"
	}
	dir := filepath.Dir(path)
	if dir == "." || dir == "/" || dir == "" {
		dir = "root"
	} else {
		dir = trimDirToLast2(dir)
	}
	return ext + ":" + dir
}

// extractPathFromText scans free-form description for the first token
// that looks like a relative path: contains "/" and ends with ".<ext>".
// Trims surrounding punctuation. Returns "" when no candidate found.
func extractPathFromText(text string) string {
	for f := range strings.FieldsSeq(text) {
		clean := strings.Trim(f, ".,;:!?\"'()[]{}")
		if strings.Contains(clean, "/") && filepath.Ext(clean) != "" {
			return clean
		}
	}
	return ""
}

// trimDirToLast2 keeps only the trailing two components of a directory
// path. Stable scope keys across nested directory moves (e.g. moving a
// file from cmd/plugin-deepseek/handlers/audit.go to
// cmd/plugin-deepseek/v2/handlers/audit.go preserves "v2/handlers" or
// "handlers/audit" — close enough for trust grouping).
func trimDirToLast2(dir string) string {
	parts := strings.Split(filepath.ToSlash(dir), "/")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, "/")
}

// TrustGet returns the score for (pattern, scope) or a fresh (1,1)
// prior if no record exists yet. Read-only — does not create the
// bucket or touch state. Safe for concurrent calls.
func TrustGet(pattern, scope string) (TrustScore, error) {
	if plannerDB == nil {
		return NewTrustScore(pattern, scope), errTrustOffline
	}
	var s TrustScore
	found := false
	err := plannerDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(daemonTrustBucket))
		if b == nil {
			return nil
		}
		raw := b.Get([]byte(pattern + ":" + scope))
		if raw == nil {
			return nil
		}
		found = true
		return json.Unmarshal(raw, &s)
	})
	if err != nil {
		return TrustScore{}, err
	}
	if !found {
		return NewTrustScore(pattern, scope), nil
	}
	return s, nil
}

// TrustUpdate applies fn to the score for (pattern, scope) atomically:
// read existing record (or fresh prior) → fn(&s) → write back. Wraps the
// whole sequence in a single bbolt write transaction so 100 goroutines
// updating the same key never lose an update — bbolt serializes Update()
// calls. Mirrors the pattern from daemon_budget.go::updateBudget. [138.B.8]
func TrustUpdate(pattern, scope string, fn func(*TrustScore)) error {
	if plannerDB == nil {
		return errTrustOffline
	}
	return plannerDB.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(daemonTrustBucket))
		if err != nil {
			return err
		}
		key := []byte(pattern + ":" + scope)
		var s TrustScore
		if raw := b.Get(key); raw != nil {
			if jerr := json.Unmarshal(raw, &s); jerr != nil {
				return jerr
			}
		} else {
			s = NewTrustScore(pattern, scope)
		}
		fn(&s)
		out, err := json.Marshal(s)
		if err != nil {
			return err
		}
		return b.Put(key, out)
	})
}

// TrustRecord is the daemon's primary entry point. Resolves the (pattern,
// scope) atomically and applies one outcome with the FailureCategory
// weights from RecordOutcome. Concurrency-safe via TrustUpdate. [138.B.8]
func TrustRecord(pattern, scope string, category FailureCategory) error {
	return TrustUpdate(pattern, scope, func(s *TrustScore) {
		s.RecordOutcome(category, time.Now())
	})
}

// SuggestedAction is what the daemon recommends after running the audit
// pipeline against a mutated set of files. The operator either accepts
// the suggestion (and optional auto-approval policy applies) or
// overrides it. [138.C.5]
type SuggestedAction string

const (
	// ActionAutoApprove — daemon proceeds without asking. Reserved for
	// high-tier patterns whose audit verdict cleared all gates.
	ActionAutoApprove SuggestedAction = "auto-approve"
	// ActionPromptOperator — daemon waits for the operator to decide.
	// Default conservative outcome for L0 + any unclear case.
	ActionPromptOperator SuggestedAction = "prompt-operator"
	// ActionReject — daemon refuses outright. Reserved for L3 patterns
	// whose audit returned fail-fatal: even the most-trusted patterns
	// shouldn't auto-merge a build break.
	ActionReject SuggestedAction = "reject"
)

// l1SpotCheckRate is the probability that an L1 suggestion drops to
// prompt-operator instead of auto-approve. Designed to keep the
// operator in the loop occasionally even on patterns the system thinks
// it has figured out — catches drift early. [138.C.5]
const l1SpotCheckRate = 0.20

// SuggestAction returns the daemon's recommended action given the
// current tier and the audit verdict from RunAuditPipeline.
//
// Decision matrix:
//
//	tier=L0                     → prompt-operator (always)
//	tier=L1 + 20% chance        → prompt-operator (spot-check)
//	tier=L1 + 80% chance        → auto-approve
//	tier=L2 + audit not pass    → prompt-operator
//	tier=L2 + audit pass        → auto-approve
//	tier=L3 + audit fail-fatal  → reject (even L3 won't auto-merge a build break)
//	tier=L3 + anything else     → auto-approve
//
// Empty AuditVerdict.Severity (skeleton path or unrun pipeline) is
// treated as "not pass" — the conservative default. [138.C.5]
func SuggestAction(tier Tier, audit AuditVerdict) SuggestedAction {
	return suggestActionWithRand(tier, audit, rand.Float64)
}

// suggestActionWithRand exposes the decision matrix with an injectable
// RNG for deterministic tests. The exported entry point uses the
// package-level rand.
func suggestActionWithRand(tier Tier, audit AuditVerdict, randFn func() float64) SuggestedAction {
	// Empty severity = audit pipeline never ran (skeleton path or partial
	// wiring). Conservative default at EVERY tier — never auto-approve
	// without evidence, even at L3. Defense in depth against future
	// callers forgetting to populate Severity. [DeepSeek TRUST-LOGIC-001]
	if audit.Severity == "" {
		return ActionPromptOperator
	}
	switch tier {
	case TierL3:
		// L3 still escalates test/AST failures even though it's the most
		// trusted tier — auto-approving a recoverable regression is too
		// risky. Only Pass and PassWithWarnings auto-approve; FailFatal
		// rejects outright; FailRecoverable goes to operator.
		// [DeepSeek TRUST-LOGIC-002]
		switch audit.Severity {
		case SeverityFailFatal:
			return ActionReject
		case SeverityFailRecoverable:
			return ActionPromptOperator
		}
		return ActionAutoApprove
	case TierL2:
		if audit.Severity != SeverityPass {
			return ActionPromptOperator
		}
		return ActionAutoApprove
	case TierL1:
		if randFn() < l1SpotCheckRate {
			return ActionPromptOperator
		}
		return ActionAutoApprove
	}
	// L0 + any unknown tier: conservative default.
	return ActionPromptOperator
}

// ListTrustScores returns every score in the daemon_trust bucket plus
// a count of corrupt entries that had to be skipped. Pure read; no
// mutations. Sorting + filtering is the caller's responsibility.
//
// Returns an empty slice + 0 skipped + nil error when the bucket
// doesn't exist yet (no scores recorded yet) or plannerDB is offline.
// The daemon's trust_status action surfaces "no trust data yet" in
// that case rather than treating it as a failure.
//
// Corrupt entries (JSON unmarshal failure) are logged with their key
// and counted in skipped — silent loss would mask data integrity
// issues from the operator. [138.C.6 + DeepSeek TRUST-STATUS-001/008]
func ListTrustScores() (scores []TrustScore, skipped int, err error) {
	if plannerDB == nil {
		return nil, 0, nil
	}
	err = plannerDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(daemonTrustBucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var s TrustScore
			if jerr := json.Unmarshal(v, &s); jerr != nil {
				log.Printf("[TRUST-STORE] skipping corrupt entry key=%q: %v", string(k), jerr)
				skipped++
				return nil
			}
			scores = append(scores, s)
			return nil
		})
	})
	return scores, skipped, err
}

// TrustWarmup imports operator-validated evidence for a (pattern, scope).
// Sets Alpha and Beta directly, marks ManualWarmup:true for audit trail,
// and recomputes CurrentTier. Useful when migrating from legacy
// observation logs or when the operator pre-approves a class of tasks
// after manual verification.
//
// TotalExecutions is reconstructed from the synthetic evidence:
// (alpha-1) + (beta-1). When the imported counts already include the
// uniform prior, this preserves the meaning of TotalExecutions as
// "real outcomes seen". [138.B.7]
func TrustWarmup(pattern, scope string, alpha, beta float64) error {
	if alpha < 1 || beta < 1 {
		return errors.New("trust warmup: α and β must be ≥ 1 (uniform prior floor)")
	}
	now := time.Now()
	return TrustUpdate(pattern, scope, func(s *TrustScore) {
		s.Alpha = alpha
		s.Beta = beta
		s.LastUpdate = now
		s.ManualWarmup = true
		realEvidence := max(
			// strip prior of (1, 1)
			int(alpha+beta)-2, 0)
		s.TotalExecutions = realEvidence
		s.ConsecutiveFailures = 0
		s.CurrentTier = TierFor(*s, now)
	})
}

// EffectiveAlphaBeta returns the decayed (α, β) values relative to `now`.
// The uniform prior of 1 is preserved — only the accumulated evidence
// (α−1, β−1) is dampened by decayPerHour^hoursSinceLastUpdate.
//
// Lazy on-read: persisted state is unchanged; the daemon computes this
// at read time. That way a process running for weeks doesn't need a
// background goroutine to age every score in BoltDB. [138.B.2]
func (s TrustScore) EffectiveAlphaBeta(now time.Time) (alpha, beta float64) {
	hours := now.Sub(s.LastUpdate).Hours()
	if hours <= 0 {
		return s.Alpha, s.Beta
	}
	factor := math.Pow(decayPerHour, hours)
	alpha = 1 + (s.Alpha-1)*factor
	beta = 1 + (s.Beta-1)*factor
	return alpha, beta
}

// PointEstimate returns the Beta-distribution mean α/(α+β) using decayed
// effective values. This is the daemon's current estimate of the
// probability that a call against this (pattern, scope) succeeds.
func (s TrustScore) PointEstimate(now time.Time) float64 {
	a, b := s.EffectiveAlphaBeta(now)
	n := a + b
	if n == 0 {
		return 0
	}
	return a / n
}

// LowerBound returns the 95%-confidence lower bound of the trust estimate
// using the normal approximation: mean − 1.96·σ. With little evidence the
// variance is large, pushing the lower bound far below the point estimate
// — that's intentional. New patterns sit in conservative tiers until they
// accumulate samples. Result is clamped to [0, 1]. [138.B.2]
func (s TrustScore) LowerBound(now time.Time) float64 {
	a, b := s.EffectiveAlphaBeta(now)
	n := a + b
	if n == 0 {
		return 0
	}
	mean := a / n
	variance := (a * b) / (n * n * (n + 1))
	lb := mean - confidence95Z*math.Sqrt(variance)
	switch {
	case lb < 0:
		return 0
	case lb > 1:
		return 1
	}
	return lb
}

// TierFor maps a TrustScore to its tier based on the 95% lower bound
// and a minimum-execution gate. Patterns with fewer than
// minExecsForPromote samples stay at L0 regardless of score —
// Bayesian variance already penalizes low evidence, but this is an
// extra hard floor that stops a single lucky run from earning auto-
// approval. [138.B.3]
//
// TierFor is the source of truth; CurrentTier on the persisted struct
// is a denormalized cache, recomputed by RecordOutcome (138.B.4).
func TierFor(s TrustScore, now time.Time) Tier {
	if s.TotalExecutions < minExecsForPromote {
		return TierL0
	}
	lb := s.LowerBound(now)
	switch {
	case lb >= tierL3Threshold:
		return TierL3
	case lb >= tierL2Threshold:
		return TierL2
	case lb >= tierL1Threshold:
		return TierL1
	}
	return TierL0
}

// consecutiveFailureLimit demotes a score to L0 when reached. Infra
// failures don't count toward this streak — they're operational bugs,
// not model quality regressions. [138.B.4]
const consecutiveFailureLimit = 3

// RecordOutcome applies one outcome with weight per FailureCategory and
// updates CurrentTier. Caller is responsible for persisting the score
// after this returns.
//
// Weights:
//
//	Success           → α += 1, streak reset to 0
//	Infra             → no-op on α/β/streak (model not at fault)
//	SubOptimal        → β += 0.5, streak++
//	Quality           → β += 1, streak++
//	OperatorOverride  → β += 5, streak++  (operator reject = strong signal)
//
// TotalExecutions counts every call regardless of category. LastUpdate
// is set to `now` so subsequent reads see fresh evidence.
//
// When the consecutive non-Infra non-Success streak reaches
// consecutiveFailureLimit (3), CurrentTier is forced to L0 immediately
// regardless of LowerBound — the streak is policy that overrides math.
// Otherwise CurrentTier is recomputed via TierFor.
//
// [138.B.4]
func (s *TrustScore) RecordOutcome(category FailureCategory, now time.Time) {
	s.TotalExecutions++
	s.LastUpdate = now

	switch category {
	case OutcomeSuccess:
		s.Alpha++
		s.ConsecutiveFailures = 0
	case OutcomeInfra:
		// No-op on α/β/streak. Timestamp + total still tick.
	case OutcomeSubOptimal:
		s.Beta += 0.5
		s.ConsecutiveFailures++
	case OutcomeQuality:
		s.Beta++
		s.ConsecutiveFailures++
	case OutcomeOperatorOverride:
		s.Beta += 5
		s.ConsecutiveFailures++
	}

	if s.ConsecutiveFailures >= consecutiveFailureLimit {
		s.CurrentTier = TierL0
		return
	}
	s.CurrentTier = TierFor(*s, now)
}

// BayesianUpdate applies a single binary outcome. Increments Alpha on
// success (audit passed), Beta on failure. Updates LastUpdate so
// subsequent reads see fresh evidence and TotalExecutions for the tier
// promotion gate (138.B.3).
//
// This is the primitive update. RecordOutcome (138.B.4) calls
// BayesianUpdate after applying FailureCategory weights. Direct callers
// should be rare — used for the warmup path (138.B.7) and tests. [138.B.2]
func (s *TrustScore) BayesianUpdate(success bool, now time.Time) {
	if success {
		s.Alpha++
	} else {
		s.Beta++
	}
	s.TotalExecutions++
	s.LastUpdate = now
}
