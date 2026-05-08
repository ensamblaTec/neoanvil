// pkg/state/pair_audit_events_test.go — tests for the Pair-mode
// audit event persistence layer. [138.E.1]
package state

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// TestEmitPairAuditEvent_RoundTrip — emit then list returns the event
// with all fields preserved.
func TestEmitPairAuditEvent_RoundTrip(t *testing.T) {
	setupTestPlanner(t)

	id, err := EmitPairAuditEvent(
		"refactor:.go:pkg/state",
		"TRUST-LOGIC-001",
		"empty severity routes to prompt-operator",
		8,
		[]string{"pkg/state/daemon_trust.go", "pkg/state/daemon_audit.go"},
	)
	if err != nil {
		t.Fatalf("EmitPairAuditEvent: %v", err)
	}
	if !strings.HasPrefix(id, "evt-") {
		t.Errorf("EventID=%q, want evt-<unixnano>", id)
	}

	events, _, err := ListUnresolvedPairEvents()
	if err != nil {
		t.Fatalf("ListUnresolvedPairEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	got := events[0]
	if got.EventID != id {
		t.Errorf("EventID=%q, want %q", got.EventID, id)
	}
	if got.Scope != "refactor:.go:pkg/state" {
		t.Errorf("Scope=%q", got.Scope)
	}
	if got.Severity != 8 {
		t.Errorf("Severity=%d, want 8", got.Severity)
	}
	if len(got.Files) != 2 {
		t.Errorf("Files len=%d, want 2", len(got.Files))
	}
	if got.Resolved {
		t.Error("event should be unresolved on emit")
	}
	if got.Outcome != "" {
		t.Errorf("Outcome=%q, want empty pre-resolve", got.Outcome)
	}
}

// TestEmitPairAuditEvent_RejectsBadInput — empty scope, invalid
// severity surface as errors at emit time.
func TestEmitPairAuditEvent_RejectsBadInput(t *testing.T) {
	setupTestPlanner(t)

	if _, err := EmitPairAuditEvent("", "F1", "...", 5, nil); err == nil {
		t.Error("empty scope should error")
	}
	if _, err := EmitPairAuditEvent("scope", "F1", "...", 0, nil); err == nil {
		t.Error("severity=0 should error")
	}
	if _, err := EmitPairAuditEvent("scope", "F1", "...", 11, nil); err == nil {
		t.Error("severity=11 should error")
	}
}

// TestEmitPairAuditEvent_TruncatesLongClaim — claim text >240 chars
// gets trimmed with ellipsis to keep the bucket small.
func TestEmitPairAuditEvent_TruncatesLongClaim(t *testing.T) {
	setupTestPlanner(t)
	long := strings.Repeat("x", 500)
	id, err := EmitPairAuditEvent("scope", "F1", long, 5, nil)
	if err != nil {
		t.Fatalf("EmitPairAuditEvent: %v", err)
	}
	events, _, _ := ListUnresolvedPairEvents()
	for _, e := range events {
		if e.EventID == id {
			if !strings.HasSuffix(e.ClaimText, "…") {
				t.Errorf("expected ellipsis suffix, got %q", e.ClaimText[len(e.ClaimText)-10:])
			}
			if len(e.ClaimText) > 250 {
				t.Errorf("claim too long: %d chars", len(e.ClaimText))
			}
		}
	}
}

// TestMarkPairEventResolved_TransitionsState — resolve transitions
// the event with outcome + ResolvedAt timestamp.
func TestMarkPairEventResolved_TransitionsState(t *testing.T) {
	setupTestPlanner(t)
	id, _ := EmitPairAuditEvent("scope", "F1", "claim", 7, []string{"f.go"})

	if err := MarkPairEventResolved(id, OutcomeSuccess); err != nil {
		t.Fatalf("MarkPairEventResolved: %v", err)
	}

	events, _, _ := ListUnresolvedPairEvents()
	if len(events) != 0 {
		t.Errorf("event should be resolved, but ListUnresolvedPairEvents returned %d", len(events))
	}
}

// TestMarkPairEventResolved_Idempotent — second resolve is a no-op
// without mutating Outcome or ResolvedAt.
func TestMarkPairEventResolved_Idempotent(t *testing.T) {
	setupTestPlanner(t)
	id, _ := EmitPairAuditEvent("scope", "F1", "claim", 7, nil)

	if err := MarkPairEventResolved(id, OutcomeSuccess); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	// Second resolve with a DIFFERENT outcome — should NOT overwrite.
	if err := MarkPairEventResolved(id, OutcomeQuality); err != nil {
		t.Errorf("second resolve should be idempotent no-op, got error: %v", err)
	}
}

// TestMarkPairEventResolved_AbsentErrors — resolving a non-existent
// event ID returns a clear error rather than silently creating one.
func TestMarkPairEventResolved_AbsentErrors(t *testing.T) {
	setupTestPlanner(t)
	if err := MarkPairEventResolved("evt-never-existed", OutcomeSuccess); err == nil {
		t.Error("resolving absent event should error")
	}
}

// TestPairAuditEvent_FilesIntersect — exact path equality only;
// substring matches don't count.
func TestPairAuditEvent_FilesIntersect(t *testing.T) {
	e := PairAuditEvent{Files: []string{"pkg/state/daemon_trust.go", "pkg/state/daemon_audit.go"}}

	if !e.FilesIntersect([]string{"pkg/state/daemon_trust.go"}) {
		t.Error("exact match should intersect")
	}
	if e.FilesIntersect([]string{"pkg/state/daemon_OTHER.go"}) {
		t.Error("non-matching file should not intersect")
	}
	if e.FilesIntersect([]string{"pkg/state"}) {
		t.Error("substring/dirname should not intersect (exact-equality only)")
	}
	if e.FilesIntersect(nil) {
		t.Error("empty mutated should not intersect")
	}

	// Empty Files slice in event — never matches.
	e2 := PairAuditEvent{Files: nil}
	if e2.FilesIntersect([]string{"any.go"}) {
		t.Error("event with no Files should never intersect")
	}
}

// TestListUnresolvedPairEvents_FiltersResolved — only unresolved
// events come back. Resolved ones stay in the bucket for audit trail
// but don't reappear in list.
func TestListUnresolvedPairEvents_FiltersResolved(t *testing.T) {
	setupTestPlanner(t)

	id1, _ := EmitPairAuditEvent("scope1", "F1", "...", 5, nil)
	id2, _ := EmitPairAuditEvent("scope2", "F2", "...", 6, nil)
	_, _ = EmitPairAuditEvent("scope3", "F3", "...", 7, nil)

	if err := MarkPairEventResolved(id1, OutcomeSuccess); err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	if err := MarkPairEventResolved(id2, OutcomeQuality); err != nil {
		t.Fatalf("resolve 2: %v", err)
	}

	events, _, _ := ListUnresolvedPairEvents()
	if len(events) != 1 {
		t.Errorf("got %d unresolved, want 1", len(events))
	}
}

// TestPairAuditEventTTL_IsThirtyMinutes — sanity-check the constant
// matches the design doc. Reaper logic in 138.E.3 will key off this.
func TestPairAuditEventTTL_IsThirtyMinutes(t *testing.T) {
	if PairAuditEventTTL != 30*time.Minute {
		t.Errorf("PairAuditEventTTL=%v, design doc says 30m", PairAuditEventTTL)
	}
}

// TestHookCertifyEvents_ResolvesIntersecting — when a certify batch
// includes a file that an unresolved event references, the event
// gets resolved with OutcomeSuccess and the trust score for the
// scope's bucket gets α += 1. [138.E.2]
func TestHookCertifyEvents_ResolvesIntersecting(t *testing.T) {
	setupTestPlanner(t)

	// Emit two events for the same trust bucket — one matches the
	// upcoming certify, the other doesn't.
	scopeKey := "refactor:.go:pkg/state"
	id1, _ := EmitPairAuditEvent(scopeKey, "F1", "claim 1", 7, []string{"pkg/state/daemon_trust.go"})
	id2, _ := EmitPairAuditEvent(scopeKey, "F2", "claim 2", 6, []string{"pkg/state/other_file.go"})

	resolved, err := HookCertifyEvents([]string{"pkg/state/daemon_trust.go"})
	if err != nil {
		t.Fatalf("HookCertifyEvents: %v", err)
	}
	if resolved != 1 {
		t.Errorf("resolved=%d, want 1 (only id1 matches)", resolved)
	}

	// id1 should now be resolved; id2 still unresolved.
	events, _, _ := ListUnresolvedPairEvents()
	for _, e := range events {
		if e.EventID == id1 {
			t.Errorf("id1 should be resolved, still in unresolved list: %+v", e)
		}
	}
	foundID2 := false
	for _, e := range events {
		if e.EventID == id2 {
			foundID2 = true
		}
	}
	if !foundID2 {
		t.Error("id2 should still be unresolved (no intersect)")
	}

	// Trust score for refactor:.go:pkg/state should have α=2 (1 prior + 1 success).
	s, _ := TrustGet("refactor", ".go:pkg/state")
	if s.Alpha != 2 {
		t.Errorf("trust α=%v, want 2 (1 prior + 1 success from hook)", s.Alpha)
	}
}

// TestHookCertifyEvents_NoIntersect — events whose Files don't match
// the certify batch stay unresolved; trust untouched.
func TestHookCertifyEvents_NoIntersect(t *testing.T) {
	setupTestPlanner(t)
	_, _ = EmitPairAuditEvent("refactor:.go:pkg/state", "F1", "claim", 7, []string{"pkg/state/x.go"})

	resolved, err := HookCertifyEvents([]string{"cmd/neo-mcp/main.go"})
	if err != nil {
		t.Fatalf("HookCertifyEvents: %v", err)
	}
	if resolved != 0 {
		t.Errorf("resolved=%d, want 0 (no intersect)", resolved)
	}
	s, _ := TrustGet("refactor", ".go:pkg/state")
	if s.Alpha != 1 {
		t.Errorf("trust α=%v, want 1 (untouched prior)", s.Alpha)
	}
}

// TestHookCertifyEvents_EmptyInputsAreNoop — empty certifiedFiles
// or empty bucket → 0 resolved, no error. Defends against a certify
// call with all-rejected files (extractApprovedFiles returned []).
func TestHookCertifyEvents_EmptyInputsAreNoop(t *testing.T) {
	setupTestPlanner(t)

	// Empty files.
	if r, err := HookCertifyEvents(nil); err != nil || r != 0 {
		t.Errorf("nil files: r=%d err=%v, want 0/nil", r, err)
	}
	if r, err := HookCertifyEvents([]string{}); err != nil || r != 0 {
		t.Errorf("empty files: r=%d err=%v, want 0/nil", r, err)
	}

	// Files non-empty but no events in bucket.
	if r, err := HookCertifyEvents([]string{"pkg/state/x.go"}); err != nil || r != 0 {
		t.Errorf("no events: r=%d err=%v, want 0/nil", r, err)
	}
}

// TestHookCertifyEvents_IdempotentOnReResolve — re-running the hook
// on the same certify set with no new events doesn't double-bill
// trust. Resolved events are filtered out by ListUnresolvedPairEvents.
func TestHookCertifyEvents_IdempotentOnReResolve(t *testing.T) {
	setupTestPlanner(t)
	_, _ = EmitPairAuditEvent("refactor:.go:pkg/state", "F1", "...", 5, []string{"pkg/state/x.go"})

	if _, err := HookCertifyEvents([]string{"pkg/state/x.go"}); err != nil {
		t.Fatalf("first hook: %v", err)
	}
	first, _ := TrustGet("refactor", ".go:pkg/state")

	// Second invocation — event is already resolved, so it shouldn't
	// appear in ListUnresolvedPairEvents and trust shouldn't move.
	resolved, err := HookCertifyEvents([]string{"pkg/state/x.go"})
	if err != nil {
		t.Fatalf("second hook: %v", err)
	}
	if resolved != 0 {
		t.Errorf("second hook: resolved=%d, want 0 (event already resolved)", resolved)
	}
	second, _ := TrustGet("refactor", ".go:pkg/state")
	if second.Alpha != first.Alpha {
		t.Errorf("trust α moved on second hook: was %v, now %v", first.Alpha, second.Alpha)
	}
}

// TestSplitTrustKey_Variants — exercises the helper across normal,
// edge, and malformed inputs.
func TestSplitTrustKey_Variants(t *testing.T) {
	cases := []struct {
		name, in, wantPat, wantScope string
	}{
		{"standard", "refactor:.go:pkg/state", "refactor", ".go:pkg/state"},
		{"unknown bucket", "unknown:unknown:unknown", "unknown", "unknown:unknown"},
		{"missing colon", "noseparator", "", ""},
		{"trailing colon", "pattern:", "", ""},
		{"leading colon", ":scope", "", ""},
		{"empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pat, scope := splitTrustKey(tc.in)
			if pat != tc.wantPat || scope != tc.wantScope {
				t.Errorf("got (%q, %q), want (%q, %q)", pat, scope, tc.wantPat, tc.wantScope)
			}
		})
	}
}

// TestReapStalePairAuditEvents_AgesOutOldEvents — events emitted
// before now-TTL get marked resolved with OutcomeSuccess. Recent
// events stay unresolved. [138.E.3]
func TestReapStalePairAuditEvents_AgesOutOldEvents(t *testing.T) {
	setupTestPlanner(t)

	// Emit two events. Then back-date one of them so it's past TTL.
	idOld, _ := EmitPairAuditEvent("scope1", "F1", "ancient", 5, []string{"a.go"})
	idFresh, _ := EmitPairAuditEvent("scope2", "F2", "fresh", 5, []string{"b.go"})

	// Manually rewrite idOld's EmittedAt to one hour ago. Direct
	// BoltDB mutation since there's no public setter — testing the
	// reaper requires controlling time.
	pastTime := time.Now().Add(-1 * time.Hour)
	if err := plannerDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(pairAuditEventsBucket))
		raw := b.Get([]byte(idOld))
		var e PairAuditEvent
		_ = json.Unmarshal(raw, &e)
		e.EmittedAt = pastTime
		out, _ := json.Marshal(e)
		return b.Put([]byte(idOld), out)
	}); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	reaped, err := ReapStalePairAuditEvents()
	if err != nil {
		t.Fatalf("ReapStalePairAuditEvents: %v", err)
	}
	if reaped != 1 {
		t.Errorf("reaped=%d, want 1 (only idOld is past TTL)", reaped)
	}

	// idFresh still unresolved; idOld should now be resolved.
	events, _, _ := ListUnresolvedPairEvents()
	for _, e := range events {
		if e.EventID == idOld {
			t.Errorf("idOld should be reaped (resolved), still in unresolved list")
		}
		if e.EventID == idFresh && e.Resolved {
			t.Errorf("idFresh should still be unresolved")
		}
	}
}

// TestReapStalePairAuditEvents_DoesNotCallTrustRecord — the reaper
// is state cleanup only. No-edit events MUST NOT credit trust scores
// (would inflate trust for findings the operator never addressed).
// Verify trust remains at prior after reap.
func TestReapStalePairAuditEvents_DoesNotCallTrustRecord(t *testing.T) {
	setupTestPlanner(t)
	idOld, _ := EmitPairAuditEvent("refactor:.go:pkg/state", "F1", "...", 5, []string{"x.go"})

	// Backdate.
	if err := plannerDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(pairAuditEventsBucket))
		raw := b.Get([]byte(idOld))
		var e PairAuditEvent
		_ = json.Unmarshal(raw, &e)
		e.EmittedAt = time.Now().Add(-1 * time.Hour)
		out, _ := json.Marshal(e)
		return b.Put([]byte(idOld), out)
	}); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if _, err := ReapStalePairAuditEvents(); err != nil {
		t.Fatalf("reap: %v", err)
	}

	// Trust score for refactor:.go:pkg/state must be untouched at prior (1, 1).
	s, _ := TrustGet("refactor", ".go:pkg/state")
	if s.Alpha != 1 || s.Beta != 1 {
		t.Errorf("trust touched by reaper: α=%v β=%v, want 1/1 (prior)", s.Alpha, s.Beta)
	}
}

// TestReapStalePairAuditEvents_EmptyBucket — fresh DB returns 0
// without error.
func TestReapStalePairAuditEvents_EmptyBucket(t *testing.T) {
	setupTestPlanner(t)
	reaped, err := ReapStalePairAuditEvents()
	if err != nil {
		t.Fatalf("reap on empty: %v", err)
	}
	if reaped != 0 {
		t.Errorf("reaped=%d, want 0", reaped)
	}
}

// TestPairFeedbackLoop_FullCycle_E2E — the canonical 138.E.5 scenario:
// agent emits 5 events for 3 different files; certify covers 2 of those
// files; reaper ages out the rest. Verify trust scores reflect ONLY
// the certify-driven outcomes (not the reaper, which is no-penalty).
//
// This is the integration test that proves the loop closes correctly
// for pair-mode feedback. [138.E.5]
func TestPairFeedbackLoop_FullCycle_E2E(t *testing.T) {
	setupTestPlanner(t)

	scope := "refactor:.go:pkg/state"
	// 5 events: 2 reference daemon_trust.go, 1 references daemon_audit.go,
	// 2 reference random other files.
	files := [][]string{
		{"pkg/state/daemon_trust.go"},
		{"pkg/state/daemon_trust.go"},
		{"pkg/state/daemon_audit.go"},
		{"pkg/state/random_file.go"},
		{"cmd/neo-mcp/main.go"},
	}
	for i, f := range files {
		if _, err := EmitPairAuditEvent(scope, "F"+string(rune('0'+i)), "claim", 5, f); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}

	// Operator certifies daemon_trust.go AND daemon_audit.go (3 of 5
	// events should match).
	resolved, err := HookCertifyEvents([]string{
		"pkg/state/daemon_trust.go",
		"pkg/state/daemon_audit.go",
	})
	if err != nil {
		t.Fatalf("HookCertifyEvents: %v", err)
	}
	if resolved != 3 {
		t.Errorf("certify resolved=%d, want 3", resolved)
	}

	// Trust score for refactor:.go:pkg/state: prior (1,1) + 3 successes = α=4 β=1.
	s, _ := TrustGet("refactor", ".go:pkg/state")
	if s.Alpha != 4 {
		t.Errorf("post-certify α=%v, want 4 (1 prior + 3 successes)", s.Alpha)
	}
	if s.Beta != 1 {
		t.Errorf("post-certify β=%v, want 1 (untouched prior)", s.Beta)
	}

	// 2 events still unresolved (the ones for random_file.go and main.go).
	unresolved, _, _ := ListUnresolvedPairEvents()
	if len(unresolved) != 2 {
		t.Errorf("unresolved count=%d, want 2", len(unresolved))
	}

	// Backdate both unresolved events to past TTL, run reaper.
	cutoff := time.Now().Add(-1 * time.Hour)
	for _, e := range unresolved {
		if err := plannerDB.Update(func(tx *bbolt.Tx) error {
			b := tx.Bucket([]byte(pairAuditEventsBucket))
			raw := b.Get([]byte(e.EventID))
			var ev PairAuditEvent
			_ = json.Unmarshal(raw, &ev)
			ev.EmittedAt = cutoff
			out, _ := json.Marshal(ev)
			return b.Put([]byte(e.EventID), out)
		}); err != nil {
			t.Fatalf("backdate: %v", err)
		}
	}

	reaped, err := ReapStalePairAuditEvents()
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if reaped != 2 {
		t.Errorf("reaper reaped=%d, want 2", reaped)
	}

	// CRITICAL: trust score must NOT have moved post-reap. Reaper is
	// state cleanup only, not a credit/penalty mechanism.
	postReap, _ := TrustGet("refactor", ".go:pkg/state")
	if postReap.Alpha != 4 || postReap.Beta != 1 {
		t.Errorf("reaper modified trust: α=%v β=%v, want 4/1 (unchanged)", postReap.Alpha, postReap.Beta)
	}

	// All events should now be resolved.
	final, _, _ := ListUnresolvedPairEvents()
	if len(final) != 0 {
		t.Errorf("after certify+reap, unresolved=%d, want 0", len(final))
	}
}

// TestEmitPairAuditEvent_UniqueIDsUnderRapidEmit — emit 100 events in
// a tight loop. EventID combines unix nanos + atomic counter so even
// if two emits land in the same nanosecond, the suffix differs.
// [DeepSeek VULN-001 fix]
func TestEmitPairAuditEvent_UniqueIDsUnderRapidEmit(t *testing.T) {
	setupTestPlanner(t)
	const emits = 100
	seen := make(map[string]bool, emits)
	for i := range emits {
		id, err := EmitPairAuditEvent("scope", "F", "claim", 5, nil)
		if err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
		if seen[id] {
			t.Fatalf("EventID collision on iteration %d: %q", i, id)
		}
		seen[id] = true
	}
	if len(seen) != emits {
		t.Errorf("expected %d unique IDs, got %d", emits, len(seen))
	}
}
