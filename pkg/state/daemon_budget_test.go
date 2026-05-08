// pkg/state/daemon_budget_test.go — tests for daemon per-task/session budget accounting. [132.B]
package state

import (
	"strings"
	"testing"
)

// TestDaemonBudget_PerTaskExceededMovesNext verifies that when a task exceeds the
// per-task token limit, DaemonBudgetTaskExceeds returns true and BudgetExceededCount
// is incremented via DaemonBudgetMarkExceeded. [132.B]
func TestDaemonBudget_PerTaskExceededMovesNext(t *testing.T) {
	setupTestPlanner(t)
	const sessionID = "sess-exceed"
	const perTaskLimit = 20000

	// Record a task that uses more tokens than the per-task limit.
	taskTokens := 25000
	if err := DaemonBudgetRecordTask(sessionID, taskTokens); err != nil {
		t.Fatalf("DaemonBudgetRecordTask: %v", err)
	}

	if !DaemonBudgetTaskExceeds(taskTokens, perTaskLimit) {
		t.Error("DaemonBudgetTaskExceeds should return true when taskTokens > perTaskLimit")
	}

	// Mark the exceeded task.
	if err := DaemonBudgetMarkExceeded(sessionID); err != nil {
		t.Fatalf("DaemonBudgetMarkExceeded: %v", err)
	}

	b, err := DaemonBudgetGet(sessionID)
	if err != nil {
		t.Fatalf("DaemonBudgetGet: %v", err)
	}
	if b.BudgetExceededCount != 1 {
		t.Errorf("BudgetExceededCount=%d, want 1", b.BudgetExceededCount)
	}
	if b.TokensUsed != taskTokens {
		t.Errorf("TokensUsed=%d, want %d", b.TokensUsed, taskTokens)
	}

	// A task within limit should not trigger exceeded.
	if DaemonBudgetTaskExceeds(5000, perTaskLimit) {
		t.Error("DaemonBudgetTaskExceeds should return false when taskTokens <= perTaskLimit")
	}
}

// TestDaemonBudget_Session90PctEmitsWarning verifies that DaemonBudgetUsagePct
// returns >= 90.0 when session usage reaches 90% of the configured limit. [132.B]
func TestDaemonBudget_Session90PctEmitsWarning(t *testing.T) {
	setupTestPlanner(t)
	const sessionID = "sess-warn"
	const sessionLimit = 200000

	// Record tokens at exactly 90% of session limit.
	tokensAt90Pct := int(float64(sessionLimit) * 0.90)
	if err := DaemonBudgetRecordTask(sessionID, tokensAt90Pct); err != nil {
		t.Fatalf("DaemonBudgetRecordTask: %v", err)
	}

	pct, err := DaemonBudgetUsagePct(sessionID, sessionLimit)
	if err != nil {
		t.Fatalf("DaemonBudgetUsagePct: %v", err)
	}
	if pct < 90.0 {
		t.Errorf("expected usage >= 90%%, got %.2f%%", pct)
	}

	// Below 90% should not trigger.
	const sessionID2 = "sess-warn-low"
	tokensBelow90 := int(float64(sessionLimit) * 0.50)
	if err := DaemonBudgetRecordTask(sessionID2, tokensBelow90); err != nil {
		t.Fatalf("DaemonBudgetRecordTask: %v", err)
	}
	pct2, err := DaemonBudgetUsagePct(sessionID2, sessionLimit)
	if err != nil {
		t.Fatalf("DaemonBudgetUsagePct: %v", err)
	}
	if pct2 >= 90.0 {
		t.Errorf("expected usage < 90%% at 50%% load, got %.2f%%", pct2)
	}
}

// TestDaemonBudget_BriefingSegment verifies that DaemonBudgetBriefingLine produces
// the expected compact format for BRIEFING output. [132.B]
func TestDaemonBudget_BriefingSegment(t *testing.T) {
	b := &DaemonBudget{
		SessionID:  "sess-brief",
		TokensUsed: 90000,
	}
	const sessionLimit = 200000

	line := DaemonBudgetBriefingLine(b, 3, 10, sessionLimit)

	if !strings.Contains(line, "daemon:") {
		t.Errorf("briefing line missing 'daemon:': %s", line)
	}
	if !strings.Contains(line, "3/10 tasks") {
		t.Errorf("briefing line missing '3/10 tasks': %s", line)
	}
	if !strings.Contains(line, "budget:45%") {
		t.Errorf("briefing line missing 'budget:45%%': %s", line)
	}

	// Nil budget should not panic.
	nilLine := DaemonBudgetBriefingLine(nil, 0, 0, sessionLimit)
	if !strings.Contains(nilLine, "budget:0%") {
		t.Errorf("nil budget line should show budget:0%%, got: %s", nilLine)
	}
}

// TestDaemonBudget_ResetsNewSession verifies that DaemonBudgetReset zeroes the
// budget so a new session starts with clean counters. [132.B]
func TestDaemonBudget_ResetsNewSession(t *testing.T) {
	setupTestPlanner(t)
	const sessionID = "sess-reset"

	if err := DaemonBudgetRecordTask(sessionID, 50000); err != nil {
		t.Fatalf("DaemonBudgetRecordTask: %v", err)
	}
	if err := DaemonBudgetMarkExceeded(sessionID); err != nil {
		t.Fatalf("DaemonBudgetMarkExceeded: %v", err)
	}

	// Verify state before reset.
	before, _ := DaemonBudgetGet(sessionID)
	if before.TokensUsed == 0 {
		t.Fatal("expected non-zero TokensUsed before reset")
	}

	// Reset.
	if err := DaemonBudgetReset(sessionID); err != nil {
		t.Fatalf("DaemonBudgetReset: %v", err)
	}

	after, err := DaemonBudgetGet(sessionID)
	if err != nil {
		t.Fatalf("DaemonBudgetGet after reset: %v", err)
	}
	if after.TokensUsed != 0 {
		t.Errorf("TokensUsed=%d after reset, want 0", after.TokensUsed)
	}
	if after.TasksCompleted != 0 {
		t.Errorf("TasksCompleted=%d after reset, want 0", after.TasksCompleted)
	}
	if after.BudgetExceededCount != 0 {
		t.Errorf("BudgetExceededCount=%d after reset, want 0", after.BudgetExceededCount)
	}
}
