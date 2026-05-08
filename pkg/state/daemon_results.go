// Package state — daemon result persistence. [PILAR XXVII / 138.C.4]
//
// DaemonResult is the operator-facing record of one execute_next →
// approve|reject cycle. It captures the task identity, what backend
// ran it, the audit verdict, the trust state at decision time, and
// the eventual operator action.
//
// The bucket is keyed by task_id and survives across daemon restarts
// so the operator can replay any approval decision and so the
// background reaper (138.E.3) can scan for stale entries.
package state

import (
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"
)

const daemonResultsBucket = "daemon_results"

// DaemonResultStatus enumerates the lifecycle states of a daemon result
// entry. Created with `pending_review` by execute_next, transitioned
// to `approved` by approve or `rejected` by reject.
type DaemonResultStatus string

const (
	ResultPendingReview DaemonResultStatus = "pending_review"
	ResultApproved      DaemonResultStatus = "approved"
	ResultRejected      DaemonResultStatus = "rejected"
)

// DaemonResult is the persisted record of one daemon execution cycle.
// Schema-versioned via the embedded `schema_version` field so future
// migrations can detect old shapes. [138.C.4]
type DaemonResult struct {
	SchemaVersion int                `json:"schema_version"`
	TaskID        string             `json:"task_id"`
	TaskDescription string           `json:"task_description,omitempty"`
	TargetFile    string             `json:"target_file,omitempty"`
	Backend       string             `json:"backend"`
	DeepSeekTool  string             `json:"deepseek_tool,omitempty"`
	Pattern       string             `json:"pattern"`
	Scope         string             `json:"scope"`

	// Trust state captured at execute_next time (BEFORE the outcome was
	// recorded). Useful for audit trail — reconstructs the daemon's
	// information state when it suggested the action.
	TrustAlphaBefore   float64 `json:"trust_alpha_before"`
	TrustBetaBefore    float64 `json:"trust_beta_before"`
	TrustTierBefore    string  `json:"trust_tier_before"`

	// Audit verdict from the post-execution pipeline. Empty when
	// dispatch + audit haven't been wired yet (skeleton phase).
	AuditSeverity string `json:"audit_severity,omitempty"`
	AuditPassed   bool   `json:"audit_passed"`

	// DeepSeek metrics. Zero when backend=claude or skeleton phase.
	TokensUsed     int    `json:"tokens_used"`
	OutputSummary  string `json:"output_summary,omitempty"`

	// Daemon's recommendation at execute_next time.
	SuggestedAction string `json:"suggested_action"`

	// Lifecycle.
	Status       DaemonResultStatus `json:"status"`
	OperatorNote string             `json:"operator_note,omitempty"`
	CreatedAt    time.Time          `json:"created_at"`
	// CompletedAt is a pointer so zero-value entries (still pending_review)
	// serialize as null instead of "0001-01-01T00:00:00Z" — consumers can
	// distinguish "not yet approved" from "approved at year 0001" cleanly.
	// [DeepSeek VULN-006]
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	// TrustApplied is set to true once the trust system has been updated
	// for this entry. Prevents double-billing when a partial-failure
	// retry path re-runs TrustRecord on the same daemon_result.
	// [DeepSeek DOUBLE-PENALTY-RETRY fix: idempotent trust update]
	TrustApplied bool `json:"trust_applied,omitempty"`
}

const daemonResultSchemaV1 = 1

// PersistDaemonResult writes (or overwrites) the entry for r.TaskID.
// Used by execute_next to seed the bucket and by approve/reject to
// transition status. [138.C.4]
func PersistDaemonResult(r DaemonResult) error {
	if plannerDB == nil {
		return fmt.Errorf("daemon_results: plannerDB offline")
	}
	if r.TaskID == "" {
		return fmt.Errorf("daemon_results: TaskID required")
	}
	if r.SchemaVersion == 0 {
		r.SchemaVersion = daemonResultSchemaV1
	}
	return plannerDB.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(daemonResultsBucket))
		if err != nil {
			return err
		}
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		return b.Put([]byte(r.TaskID), raw)
	})
}

// GetDaemonResult reads the entry for taskID. Returns nil + nil when
// no entry exists (caller must check both). Returns nil + error on
// I/O or unmarshal failures.
func GetDaemonResult(taskID string) (*DaemonResult, error) {
	if plannerDB == nil {
		return nil, fmt.Errorf("daemon_results: plannerDB offline")
	}
	var r *DaemonResult
	err := plannerDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(daemonResultsBucket))
		if b == nil {
			return nil
		}
		raw := b.Get([]byte(taskID))
		if raw == nil {
			return nil
		}
		var got DaemonResult
		if err := json.Unmarshal(raw, &got); err != nil {
			return err
		}
		r = &got
		return nil
	})
	return r, err
}

// UpdateDaemonResult applies fn to the entry for taskID atomically.
// Returns an error if no entry exists (caller must seed via
// PersistDaemonResult first). [138.C.4]
func UpdateDaemonResult(taskID string, fn func(*DaemonResult)) error {
	if plannerDB == nil {
		return fmt.Errorf("daemon_results: plannerDB offline")
	}
	return plannerDB.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(daemonResultsBucket))
		if err != nil {
			return err
		}
		raw := b.Get([]byte(taskID))
		if raw == nil {
			return fmt.Errorf("daemon_results: no entry for task_id=%s", taskID)
		}
		var r DaemonResult
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		fn(&r)
		out, err := json.Marshal(r)
		if err != nil {
			return err
		}
		return b.Put([]byte(taskID), out)
	})
}
