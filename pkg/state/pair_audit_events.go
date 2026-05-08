// Package state — Pair-mode audit event persistence. [PILAR XXVII / 138.E.1]
//
// PairAuditEvent captures the operator-visible side of a DeepSeek
// red_team_audit invocation. After Claude (the operator's agent) gets
// findings back from `mcp__neoanvil__deepseek_call(action: red_team_audit)`,
// it emits a PairAuditEvent describing (scope, files, finding count,
// severity stats). The event sits in BoltDB until a subsequent
// neo_sre_certify_mutation invocation either:
//
//	(a) certifies a file in the event's `Files` list within the TTL
//	    → infer OutcomeSuccess (the operator agreed with the finding
//	    enough to fix it before certify), OR
//	(b) goes 30 min without intersection
//	    → reaper marks OutcomeSuccess (assume the operator silently
//	    accepted the audit as out-of-scope; no penalty).
//
// The (mismatch, accept-no-edit) case — where the operator dismisses
// findings as wrong without editing — is captured by 138.E.2's
// heuristic via a "no-edit window" inference; OutcomeQuality is
// emitted there. This file just provides the storage layer.
//
// Bucket schema is JSON, keyed by EventID (unix nanos + workspace id).
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"go.etcd.io/bbolt"
)

// pairAuditEventCounter monotonic suffix that disambiguates EventIDs
// when multiple emits land in the same nanosecond (low-resolution
// clocks on Windows/some Linux configs would otherwise collide and
// silently overwrite via b.Put). [DeepSeek VULN-001]
var pairAuditEventCounter atomic.Uint64

// pairAuditEventsBucket holds PairAuditEvent records keyed by EventID.
// Lives in planner.db so it shares the same BoltDB file as daemon_trust
// — a single transaction can both mark events resolved and update
// TrustScore atomically (138.E.2 wires this).
const pairAuditEventsBucket = "pair_audit_events"

// PairAuditEventTTL is how long an event waits for a matching certify
// before the reaper marks it OutcomeSuccess (conservative no-penalty).
// 30 minutes matches the typical operator review-and-edit cycle.
const PairAuditEventTTL = 30 * time.Minute

// PairAuditEvent is one operator-visible DeepSeek red_team_audit run.
// Per-finding granularity is intentional — the operator may dismiss
// finding 3 of 7 but accept the rest, and Hash-different scopes don't
// mix. The agent emits one event per finding it received.
type PairAuditEvent struct {
	EventID     string    `json:"event_id"`
	Scope       string    `json:"scope"`           // "pattern:file_ext:dir_root" — same shape as TrustScore.Key()
	FindingID   string    `json:"finding_id"`      // model-supplied ID (e.g. "TRUST-LOGIC-001")
	ClaimText   string    `json:"claim_text"`      // short version of the finding for audit trail
	Severity    int       `json:"severity"`        // 1-10 — model's self-rated severity
	Files       []string  `json:"files,omitempty"` // files the finding references; certify-intersection check uses this
	EmittedAt   time.Time `json:"emitted_at"`
	Resolved    bool      `json:"resolved"`
	Outcome     string    `json:"outcome,omitempty"` // FailureCategory string when Resolved=true
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
}

// EmitPairAuditEvent persists a new event with EventID auto-generated
// from emitted-at unix nanos. Returns the EventID for callers that
// want to correlate later. [138.E.1]
func EmitPairAuditEvent(scope, findingID, claimText string, severity int, files []string) (string, error) {
	if plannerDB == nil {
		return "", fmt.Errorf("pair_audit_events: plannerDB offline")
	}
	if scope == "" {
		return "", errors.New("pair_audit_events: scope required")
	}
	if severity < 1 || severity > 10 {
		return "", fmt.Errorf("pair_audit_events: severity=%d out of [1,10]", severity)
	}
	now := time.Now()
	// EventID combines unix nanos + monotonic counter to survive
	// low-resolution clocks. Format: "evt-<nanos>-<6hex>".
	// [DeepSeek VULN-001]
	event := PairAuditEvent{
		EventID:   fmt.Sprintf("evt-%d-%06x", now.UnixNano(), pairAuditEventCounter.Add(1)),
		Scope:     scope,
		FindingID: findingID,
		ClaimText: truncatePairAuditClaim(claimText, 240),
		Severity:  severity,
		Files:     files,
		EmittedAt: now,
		Resolved:  false,
	}
	err := plannerDB.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(pairAuditEventsBucket))
		if err != nil {
			return err
		}
		raw, err := json.Marshal(event)
		if err != nil {
			return err
		}
		return b.Put([]byte(event.EventID), raw)
	})
	if err != nil {
		return "", err
	}
	return event.EventID, nil
}

// ListUnresolvedPairEvents returns events with Resolved=false plus a
// count of corrupt JSON entries that had to be skipped. Used by the
// certify-time hook (138.E.2) to find candidates whose Files
// intersect the just-mutated set, and by the reaper (138.E.3) to find
// events past TTL.
//
// Surfacing skipped count (matching ListTrustScores) so the operator
// sees data integrity issues rather than silently losing audit
// trail. [DeepSeek VULN-002]
func ListUnresolvedPairEvents() (events []PairAuditEvent, skipped int, err error) {
	if plannerDB == nil {
		return nil, 0, nil
	}
	err = plannerDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(pairAuditEventsBucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var e PairAuditEvent
			if jerr := json.Unmarshal(v, &e); jerr != nil {
				log.Printf("[PAIR-AUDIT-EVENTS] skipping corrupt entry key=%q: %v", string(k), jerr)
				skipped++
				return nil
			}
			if !e.Resolved {
				events = append(events, e)
			}
			return nil
		})
	})
	return events, skipped, err
}

// MarkPairEventResolved transitions an event to resolved with the
// inferred outcome. Idempotent — re-marking a resolved event is a
// no-op. [138.E.2 helper]
func MarkPairEventResolved(eventID string, outcome FailureCategory) error {
	if plannerDB == nil {
		return fmt.Errorf("pair_audit_events: plannerDB offline")
	}
	return plannerDB.Update(func(tx *bbolt.Tx) error {
		// Use Bucket() (not CreateBucketIfNotExists) so resolving a
		// non-existent event ID doesn't side-effect-create an empty
		// bucket. [DeepSeek VULN-003]
		b := tx.Bucket([]byte(pairAuditEventsBucket))
		if b == nil {
			return fmt.Errorf("pair_audit_events: bucket missing — no events yet, id=%s", eventID)
		}
		raw := b.Get([]byte(eventID))
		if raw == nil {
			return fmt.Errorf("pair_audit_events: no event with id=%s", eventID)
		}
		var e PairAuditEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			return err
		}
		if e.Resolved {
			return nil // idempotent
		}
		now := time.Now()
		e.Resolved = true
		e.Outcome = string(outcome)
		e.ResolvedAt = &now
		out, err := json.Marshal(e)
		if err != nil {
			return err
		}
		return b.Put([]byte(eventID), out)
	})
}

// FilesIntersect returns true when any file in mutated appears in the
// event's Files list. Used by the certify-time hook (138.E.2) to
// decide whether a just-certified mutation matches an open audit.
//
// Match is exact path equality. Substring/glob matching is intentionally
// out of scope — the operator's edit either touched the file the
// finding referenced or it didn't.
func (e PairAuditEvent) FilesIntersect(mutated []string) bool {
	if len(e.Files) == 0 || len(mutated) == 0 {
		return false
	}
	mutSet := make(map[string]struct{}, len(mutated))
	for _, m := range mutated {
		mutSet[m] = struct{}{}
	}
	for _, f := range e.Files {
		if _, ok := mutSet[f]; ok {
			return true
		}
	}
	return false
}

// ReapStalePairAuditEvents marks every unresolved event whose
// EmittedAt + PairAuditEventTTL < now as resolved with OutcomeSuccess.
// Conservative default: an event the operator left untouched for 30
// minutes is assumed accepted-as-out-of-scope (e.g., DeepSeek flagged
// a finding the operator quietly judged irrelevant). No trust penalty.
//
// The reaper does NOT call TrustRecord because:
//
//	(a) OutcomeSuccess on a no-edit event would inflate trust for a
//	    finding the operator never actually addressed, and
//	(b) OutcomeQuality would penalize DeepSeek for findings the
//	    operator just chose not to engage with — also wrong.
//
// So the reaper is a state cleanup only: it ages the event out of
// the unresolved list so subsequent certify scans don't keep
// considering it. Trust calibration happens exclusively at certify
// time via HookCertifyEvents. [138.E.3]
//
// Returns the count of events reaped. Best-effort — individual
// MarkPairEventResolved failures log + continue.
func ReapStalePairAuditEvents() (int, error) {
	if plannerDB == nil {
		return 0, nil
	}
	events, _, err := ListUnresolvedPairEvents()
	if err != nil {
		return 0, fmt.Errorf("reap_pair_audit_events: list: %w", err)
	}
	cutoff := time.Now().Add(-PairAuditEventTTL)
	reaped := 0
	for _, e := range events {
		if e.EmittedAt.After(cutoff) {
			continue // still within TTL
		}
		if merr := MarkPairEventResolved(e.EventID, OutcomeSuccess); merr != nil {
			log.Printf("[PAIR-FEEDBACK] reaper mark resolved %s failed: %v", e.EventID, merr)
			continue
		}
		reaped++
	}
	if reaped > 0 {
		log.Printf("[PAIR-FEEDBACK] reaper marked %d stale event(s) as out-of-scope (TTL %v)", reaped, PairAuditEventTTL)
	}
	return reaped, nil
}

// HookCertifyEvents is the certify-time integration point [138.E.2].
// Called after a successful neo_sre_certify_mutation batch with the
// list of approved file paths. For every unresolved PairAuditEvent
// whose Files intersect the certified set:
//
//  1. Mark the event resolved with OutcomeSuccess (operator addressed
//     the finding by editing AND certifying the matching file).
//  2. Call TrustRecord with the same OutcomeSuccess so the trust
//     score for that (pattern, scope) bucket gets the credit.
//
// Returns the count of events resolved. Best-effort on individual
// failures: a log line + continue rather than aborting the whole
// batch — the operator just finished a certify and shouldn't have
// the trust hook block their flow on a transient bbolt blip.
//
// Empty certifiedFiles or nil plannerDB is a no-op (no error).
func HookCertifyEvents(certifiedFiles []string) (int, error) {
	if plannerDB == nil || len(certifiedFiles) == 0 {
		return 0, nil
	}
	events, _, err := ListUnresolvedPairEvents()
	if err != nil {
		return 0, fmt.Errorf("hook_certify_events: list: %w", err)
	}
	resolved := 0
	for _, e := range events {
		if !e.FilesIntersect(certifiedFiles) {
			continue
		}
		// Mark resolved BEFORE TrustRecord so a partial failure leaves
		// the system in "we counted this finding but couldn't update
		// trust" state — re-running the hook on a future certify
		// against the same files won't double-bill (event is already
		// resolved). The trust update only happens once.
		if merr := MarkPairEventResolved(e.EventID, OutcomeSuccess); merr != nil {
			log.Printf("[PAIR-FEEDBACK] mark resolved %s failed: %v", e.EventID, merr)
			continue
		}
		pattern, scope := splitTrustKey(e.Scope)
		if pattern == "" || scope == "" {
			log.Printf("[PAIR-FEEDBACK] skip trust update — invalid scope %q on event %s", e.Scope, e.EventID)
			continue
		}
		if rerr := TrustRecord(pattern, scope, OutcomeSuccess); rerr != nil {
			log.Printf("[PAIR-FEEDBACK] trust record %s/%s failed: %v", pattern, scope, rerr)
			continue
		}
		resolved++
	}
	return resolved, nil
}

// splitTrustKey splits a "pattern:scope" string back into separate
// pattern and scope tokens. The split happens at the first colon so
// scopes that themselves contain colons (".go:pkg/state") survive
// intact. Returns ("", "") if the input is malformed (missing colon).
func splitTrustKey(key string) (pattern, scope string) {
	idx := strings.Index(key, ":")
	if idx <= 0 || idx == len(key)-1 {
		return "", ""
	}
	return key[:idx], key[idx+1:]
}

// truncatePairAuditClaim trims long claim text to a max length while
// preserving readable prefix. Avoids bloating the bucket when DeepSeek
// returns multi-paragraph claims; the full text lives in the original
// red_team_audit response that the operator already has.
func truncatePairAuditClaim(claim string, maxLen int) string {
	if len(claim) <= maxLen {
		return claim
	}
	// TrimRightFunc with unicode.IsSpace handles tab/newline/NBSP/em-space
	// at the cut point so the audit log doesn't show "\t…" etc.
	// [DeepSeek VULN-006]
	return strings.TrimRightFunc(claim[:maxLen], unicode.IsSpace) + "…"
}
