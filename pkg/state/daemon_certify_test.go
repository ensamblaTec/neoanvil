// pkg/state/daemon_certify_test.go — tests for daemon TTL seal auto-renew. [132.D]
package state

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeLockFile creates a certified_state.lock with the given entries.
func writeLockFile(t *testing.T, dir string, entries []struct{ file string; sealedAt int64 }) string {
	t.Helper()
	lockPath := filepath.Join(dir, "certified_state.lock")
	f, err := os.Create(lockPath)
	if err != nil {
		t.Fatalf("writeLockFile: %v", err)
	}
	defer f.Close()
	for _, e := range entries {
		fmt.Fprintf(f, "%s|%d\n", e.file, e.sealedAt)
	}
	return lockPath
}

// TestSealAutoRenewed verifies that files with seals older than (ttl-buffer) seconds
// are returned as needing renewal. [132.D]
func TestSealAutoRenewed(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Unix()

	// Seal written 250s ago with TTL=300s and buffer=60s → threshold=240s → age>=240 → stale.
	oldSeal := now - 250
	// Seal written 10s ago → age=10 < 240 → fresh.
	freshSeal := now - 10

	lockPath := writeLockFile(t, dir, []struct{ file string; sealedAt int64 }{
		{"/repo/stale.go", oldSeal},
		{"/repo/fresh.go", freshSeal},
	})

	stale, err := GetSealedFilesNeedingRenewal(lockPath, 300, 60)
	if err != nil {
		t.Fatalf("GetSealedFilesNeedingRenewal: %v", err)
	}
	if len(stale) != 1 {
		t.Fatalf("want 1 stale file, got %d: %v", len(stale), stale)
	}
	if stale[0] != "/repo/stale.go" {
		t.Errorf("stale file=%q, want %q", stale[0], "/repo/stale.go")
	}
}

// TestNoRenewalWhenRecent verifies that freshly-sealed files are not returned. [132.D]
func TestNoRenewalWhenRecent(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Unix()

	lockPath := writeLockFile(t, dir, []struct{ file string; sealedAt int64 }{
		{"/repo/handler.go", now - 30},  // only 30s old — well within 240s threshold
		{"/repo/service.go", now - 120}, // 120s old — still within threshold
	})

	stale, err := GetSealedFilesNeedingRenewal(lockPath, 300, 60)
	if err != nil {
		t.Fatalf("GetSealedFilesNeedingRenewal: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("expected 0 stale files for recent seals, got %d: %v", len(stale), stale)
	}
}

// TestDisabledSkips verifies that MaybeSealedFilesNeedingRenewal returns nil
// without reading the lock when autoRecertify=false. [132.D]
func TestDisabledSkips(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Unix()

	// Write a lock with very old seals that would normally trigger renewal.
	lockPath := writeLockFile(t, dir, []struct{ file string; sealedAt int64 }{
		{"/repo/old.go", now - 3600},
	})

	result, err := MaybeSealedFilesNeedingRenewal(lockPath, 300, 60, false)
	if err != nil {
		t.Fatalf("MaybeSealedFilesNeedingRenewal: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected nil/empty result when disabled, got %v", result)
	}

	// Enabled path: same lock should return the stale file.
	result2, err := MaybeSealedFilesNeedingRenewal(lockPath, 300, 60, true)
	if err != nil {
		t.Fatalf("MaybeSealedFilesNeedingRenewal (enabled): %v", err)
	}
	if len(result2) != 1 {
		t.Errorf("expected 1 stale file when enabled, got %d: %v", len(result2), result2)
	}
}
