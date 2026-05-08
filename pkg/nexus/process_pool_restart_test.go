package nexus

import (
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/workspace"
)

// TestRestartTracker_SurvivesProcessReplacement — regression for audit
// finding S9-5 (PILAR XXVIII 143.C, 2026-05-02). Pre-fix, restart-rate
// state lived on *WorkspaceProcess; pp.Start() replaced the map entry on
// every restart, dropping the previous restartTS slice + restarts counter.
// An adversary that crashed the child loop could trigger unlimited
// restarts/hour because each watchdog tick saw zero recent restarts on a
// fresh struct.
//
// Post-fix, restart state lives in pp.restartState[wsID] and outlives the
// process pointer. This test asserts the persistence invariant directly:
// the tracker pointer obtained before a process replacement equals the
// tracker pointer obtained after, AND its accumulated state (restartTS
// length, restarts counter, failures count) is preserved verbatim.
func TestRestartTracker_SurvivesProcessReplacement(t *testing.T) {
	pool := testPool(t)
	const wsID = "test-ws-restart-loop"

	// Simulate the process-pool registering a workspace.
	pool.mu.Lock()
	tracker := pool.getOrCreateTracker(wsID)
	pool.mu.Unlock()

	// Seed the tracker as if 4 restarts already happened in the last hour.
	pool.mu.Lock()
	now := time.Now()
	tracker.restartTS = append(tracker.restartTS,
		now.Add(-30*time.Minute),
		now.Add(-20*time.Minute),
		now.Add(-10*time.Minute),
		now.Add(-2*time.Minute),
	)
	tracker.restarts = 4
	tracker.failures = 2
	pool.mu.Unlock()

	// Simulate pp.Start() replacing the *WorkspaceProcess pointer in the
	// map. Pre-fix code put `failures` and `restartTS` on *WorkspaceProcess,
	// so this replacement dropped them. Post-fix the tracker is in a
	// separate map keyed by wsID, so this replacement is irrelevant.
	pool.mu.Lock()
	pool.processes[wsID] = &WorkspaceProcess{
		Entry:     workspace.WorkspaceEntry{ID: wsID},
		Status:    StatusRunning,
		StartedAt: time.Now(),
		// Restarts (exported field) is initially 0 on a fresh struct; the
		// next call to maybeRestart will mirror tracker.restarts onto it.
	}
	tracker2 := pool.getOrCreateTracker(wsID)
	pool.mu.Unlock()

	// CRITICAL invariant: tracker pointer is the same across the
	// "replacement" because it's stored in pool.restartState, not on the
	// process struct.
	if tracker != tracker2 {
		t.Fatalf("tracker pointer changed after process replacement: pre=%p post=%p (audit S9-5 regression)", tracker, tracker2)
	}

	// State must be preserved verbatim.
	if got := len(tracker2.restartTS); got != 4 {
		t.Errorf("restartTS dropped on replacement: got len=%d, want 4 (audit S9-5 regression)", got)
	}
	if got := tracker2.restarts; got != 4 {
		t.Errorf("restarts counter reset on replacement: got=%d, want 4 (audit S9-5 regression)", got)
	}
	if got := tracker2.failures; got != 2 {
		t.Errorf("failures counter reset on replacement: got=%d, want 2 (audit S9-5 regression)", got)
	}
}

// TestRestartTracker_RateLimitEnforcedAcrossReplacements — verifies that
// the maybeRestart code path actually CONSULTS the persistent tracker and
// quarantines a workspace that has already exhausted its hourly quota,
// even when the *WorkspaceProcess record was replaced between attempts.
//
// We exercise the read-path of maybeRestart directly (the full path also
// kills + respawns the child, which requires a real binary). The test
// pre-loads the tracker with N timestamps where N == MaxRestartsPerHour,
// then asserts that the rate-limit branch trips on the next restart attempt.
func TestRestartTracker_RateLimitEnforcedAcrossReplacements(t *testing.T) {
	pool := testPool(t)
	pool.cfg.Nexus.Watchdog.AutoRestart = true
	pool.cfg.Nexus.Watchdog.MaxRestartsPerHour = 3

	const wsID = "test-ws-rate-limit"
	proc := &WorkspaceProcess{
		Entry:  workspace.WorkspaceEntry{ID: wsID},
		Status: StatusUnhealthy,
	}
	pool.mu.Lock()
	pool.processes[wsID] = proc
	tracker := pool.getOrCreateTracker(wsID)
	now := time.Now()
	// Already at the limit (3 restarts in the last hour).
	tracker.restartTS = []time.Time{
		now.Add(-45 * time.Minute),
		now.Add(-30 * time.Minute),
		now.Add(-15 * time.Minute),
	}
	tracker.restarts = 3
	pool.mu.Unlock()

	// Simulate Start() replacing the process pointer (the bug pre-fix:
	// this would have wiped restartTS).
	pool.mu.Lock()
	newProc := &WorkspaceProcess{
		Entry:  workspace.WorkspaceEntry{ID: wsID},
		Status: StatusUnhealthy,
	}
	pool.processes[wsID] = newProc
	pool.mu.Unlock()

	// Now call maybeRestart on the NEW process. The rate-limit branch must
	// trip because the tracker survived. Pre-fix the new process had
	// len(restartTS)==0 and the limit was bypassed → child re-spawned and
	// tracker.restarts grew unbounded.
	pool.maybeRestart(newProc)

	// The expected outcome: maybeRestart sets newProc.Status = StatusQuarantined
	// and does NOT advance the tracker counters past the existing 3.
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	if newProc.Status != StatusQuarantined {
		t.Errorf("rate-limit bypass: newProc.Status = %q, want %q (audit S9-5 regression)", newProc.Status, StatusQuarantined)
	}
	if tracker.restarts > 3 {
		t.Errorf("rate-limit bypass: tracker.restarts = %d, want ≤3 (audit S9-5 regression)", tracker.restarts)
	}
	if len(tracker.restartTS) > 3 {
		t.Errorf("rate-limit bypass: len(tracker.restartTS) = %d, want ≤3 (audit S9-5 regression)", len(tracker.restartTS))
	}
}
