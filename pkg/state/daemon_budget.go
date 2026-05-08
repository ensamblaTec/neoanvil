// Package state — daemon per-task and per-session token budget accounting. [132.B]
package state

import (
	"encoding/json"
	"fmt"

	"go.etcd.io/bbolt"
)

const daemonBudgetBucket = "daemon_budget" // [132.B]

// DaemonBudget tracks token consumption for a daemon session.
type DaemonBudget struct {
	SessionID           string `json:"session_id"`
	TokensUsed          int    `json:"tokens_used"`
	TasksCompleted      int    `json:"tasks_completed"`
	BudgetExceededCount int    `json:"budget_exceeded_count"` // number of tasks that exceeded per-task limit
}

// DaemonBudgetGet returns the current budget record for sessionID, or a zero-value struct if none exists.
func DaemonBudgetGet(sessionID string) (*DaemonBudget, error) {
	if plannerDB == nil {
		return &DaemonBudget{SessionID: sessionID}, nil
	}
	var b DaemonBudget
	err := plannerDB.View(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket([]byte(daemonBudgetBucket))
		if bkt == nil {
			return nil
		}
		v := bkt.Get([]byte(sessionID))
		if v == nil {
			return nil
		}
		return json.Unmarshal(v, &b)
	})
	if err != nil {
		return nil, err
	}
	if b.SessionID == "" {
		b.SessionID = sessionID
	}
	return &b, nil
}

// DaemonBudgetRecordTask adds tokens to the session total and increments TasksCompleted.
func DaemonBudgetRecordTask(sessionID string, tokens int) error {
	return updateBudget(sessionID, func(b *DaemonBudget) {
		b.TokensUsed += tokens
		b.TasksCompleted++
	})
}

// DaemonBudgetMarkExceeded records that a task exceeded the per-task limit.
func DaemonBudgetMarkExceeded(sessionID string) error {
	return updateBudget(sessionID, func(b *DaemonBudget) {
		b.BudgetExceededCount++
	})
}

// DaemonBudgetReset zeroes the budget counters for sessionID (new session start).
func DaemonBudgetReset(sessionID string) error {
	if plannerDB == nil {
		return nil
	}
	return plannerDB.Update(func(tx *bbolt.Tx) error {
		bkt, err := tx.CreateBucketIfNotExists([]byte(daemonBudgetBucket))
		if err != nil {
			return err
		}
		return bkt.Delete([]byte(sessionID))
	})
}

// DaemonBudgetTaskExceeds reports whether taskTokens exceeds the per-task limit.
func DaemonBudgetTaskExceeds(taskTokens, perTaskLimit int) bool {
	return perTaskLimit > 0 && taskTokens > perTaskLimit
}

// DaemonBudgetUsagePct returns the session's token usage as a percentage of sessionLimit.
// Returns 0 when plannerDB is offline or sessionLimit is 0.
func DaemonBudgetUsagePct(sessionID string, sessionLimit int) (float64, error) {
	if sessionLimit <= 0 {
		return 0, nil
	}
	b, err := DaemonBudgetGet(sessionID)
	if err != nil {
		return 0, err
	}
	return float64(b.TokensUsed) / float64(sessionLimit) * 100, nil
}

// DaemonBudgetBriefingLine formats the compact BRIEFING line for daemon mode.
// Format: "daemon: Q/N tasks · budget:XX%"
func DaemonBudgetBriefingLine(b *DaemonBudget, pendingTasks, totalTasks, sessionLimit int) string {
	if b == nil {
		return "daemon: 0/0 tasks · budget:0%"
	}
	pct := 0
	if sessionLimit > 0 {
		pct = b.TokensUsed * 100 / sessionLimit
	}
	return fmt.Sprintf("daemon: %d/%d tasks · budget:%d%%", pendingTasks, totalTasks, pct)
}

// updateBudget is a helper that loads, mutates, and saves a DaemonBudget record atomically.
func updateBudget(sessionID string, fn func(*DaemonBudget)) error {
	if plannerDB == nil {
		return nil
	}
	return plannerDB.Update(func(tx *bbolt.Tx) error {
		bkt, err := tx.CreateBucketIfNotExists([]byte(daemonBudgetBucket))
		if err != nil {
			return err
		}
		var b DaemonBudget
		if v := bkt.Get([]byte(sessionID)); v != nil {
			if jerr := json.Unmarshal(v, &b); jerr != nil {
				return jerr
			}
		}
		b.SessionID = sessionID
		fn(&b)
		val, merr := json.Marshal(b)
		if merr != nil {
			return merr
		}
		return bkt.Put([]byte(sessionID), val)
	})
}
