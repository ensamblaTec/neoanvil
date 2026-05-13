package rag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDirectivesFile creates .claude/rules/neo-synced-directives.md with the
// numbered directives provided. Each entry is rendered as "N. <text>".
func writeDirectivesFile(t *testing.T, workspace string, lines []string) {
	t.Helper()
	dir := filepath.Join(workspace, ".claude", "rules")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString("# NeoAnvil Synced Directives (auto-generated)\n\n")
	for i, line := range lines {
		sb.WriteString(fmtNumberedLine(i+1, line))
		sb.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "neo-synced-directives.md"), []byte(sb.String()), 0600); err != nil {
		t.Fatal(err)
	}
}

func fmtNumberedLine(n int, text string) string {
	// Match the actual marshal format in SyncDirectivesToDisk: "N. <text>".
	// Sprintf is fine in tests — no hot-path constraint here.
	return formatI(n) + ". " + text
}

func formatI(n int) string {
	// Inline strconv-equivalent for clarity in tests.
	if n < 10 {
		return string(rune('0' + n))
	}
	return formatI(n/10) + string(rune('0'+n%10))
}

// openTestWAL opens a fresh WAL inside the workspace's .neo/db/ subdir.
func openTestWAL(t *testing.T, workspace string) *WAL {
	t.Helper()
	dbDir := filepath.Join(workspace, ".neo", "db")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}
	wal, err := OpenWAL(filepath.Join(dbDir, "hnsw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wal.Close() })
	return wal
}

// countActiveDirectives returns the number of non-OBSOLETO entries in BoltDB.
func countActiveDirectives(t *testing.T, wal *WAL) int {
	t.Helper()
	all, err := wal.GetDirectives()
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, r := range all {
		if !strings.HasPrefix(r, "~~OBSOLETO~~") {
			active++
		}
	}
	return active
}

// TestLoadDirectivesFromDisk_DestructiveSync_DeprecatesMissing simulates the
// chronic DUAL-LAYER-SYNC inflation: 10 [TEST]-tagged BoltDB entries, disk
// file has only 5 of those. After LoadDirectivesFromDisk, the 5 [TEST]
// entries not on disk must be marked ~~OBSOLETO~~.
//
// Uses delta-based assertions because OpenWAL may seed non-[TEST] entries
// (workspace doctrine bootstrap) that should not be touched by this test.
func TestLoadDirectivesFromDisk_DestructiveSync_DeprecatesMissing(t *testing.T) {
	ws := t.TempDir()
	wal := openTestWAL(t, ws)

	baseline := countActiveDirectives(t, wal)

	// Seed BoltDB with 10 [TEST]-tagged entries.
	for i := 1; i <= 10; i++ {
		if err := wal.SaveDirective("[TEST] rule number " + formatI(i)); err != nil {
			t.Fatal(err)
		}
	}
	if got := countActiveDirectives(t, wal); got != baseline+10 {
		t.Fatalf("setup: expected baseline+10=%d active, got %d", baseline+10, got)
	}

	// Disk has only entries 1, 3, 5, 7, 9 (5 active).
	writeDirectivesFile(t, ws, []string{
		"[TEST] rule number 1",
		"[TEST] rule number 3",
		"[TEST] rule number 5",
		"[TEST] rule number 7",
		"[TEST] rule number 9",
	})

	if err := wal.LoadDirectivesFromDisk(ws); err != nil {
		t.Fatal(err)
	}

	// Active count assertion: only the 5 disk entries should remain. The
	// 10 [TEST] seeded minus 5 not-in-disk = 5 [TEST] survive. The original
	// baseline entries should ALL be deprecated too (they're not on disk).
	active := countActiveDirectives(t, wal)
	if active != 5 {
		t.Errorf("post-sync: expected 5 active (5 [TEST] on disk; baseline entries also missing from disk so deprecated), got %d", active)
	}

	// Verify the 5 disk [TEST] entries are active.
	all, _ := wal.GetDirectives()
	activeSet := map[string]bool{}
	for _, r := range all {
		if !strings.HasPrefix(r, "~~OBSOLETO~~") {
			activeSet[r] = true
		}
	}
	for _, kept := range []string{
		"[TEST] rule number 1",
		"[TEST] rule number 3",
		"[TEST] rule number 5",
		"[TEST] rule number 7",
		"[TEST] rule number 9",
	} {
		if !activeSet[kept] {
			t.Errorf("expected %q to remain active, but it's deprecated", kept)
		}
	}
}

// TestLoadDirectivesFromDisk_CorruptionGuard verifies the safety net: when
// disk has <5 entries but BoltDB has >50, the destructive sweep is SKIPPED
// (suspecting truncated disk file). Only additive UPSERT runs.
func TestLoadDirectivesFromDisk_CorruptionGuard(t *testing.T) {
	ws := t.TempDir()
	wal := openTestWAL(t, ws)

	baseline := countActiveDirectives(t, wal)

	// Seed BoltDB with enough [TEST] entries to push total above threshold.
	seedCount := syncDestructiveBoltDBThreshold + 10 - baseline
	for i := 1; i <= seedCount; i++ {
		_ = wal.SaveDirective("[TEST] big rule " + formatI(i))
	}
	beforeSync := countActiveDirectives(t, wal)
	if beforeSync <= syncDestructiveBoltDBThreshold {
		t.Fatalf("setup: expected >%d active to trigger guard, got %d", syncDestructiveBoltDBThreshold, beforeSync)
	}

	// Disk has only 3 entries (below MinDisk=5).
	writeDirectivesFile(t, ws, []string{
		"[TEST] big rule 1",
		"[TEST] big rule 2",
		"[TEST] big rule 3",
	})

	if err := wal.LoadDirectivesFromDisk(ws); err != nil {
		t.Fatal(err)
	}

	// Corruption guard MUST have triggered. No destructive sweep ran, so
	// active count stays at beforeSync (no deprecations).
	if got := countActiveDirectives(t, wal); got != beforeSync {
		t.Errorf("corruption guard should prevent mass-deprecation when disk looks truncated; active went %d → %d", beforeSync, got)
	}
}

// TestLoadDirectivesFromDisk_AdditiveStillWorks verifies that entries only
// present on disk (not in BoltDB) still get added. This preserves the
// pre-existing behavior where operator hand-edits disk to add new entries.
func TestLoadDirectivesFromDisk_AdditiveStillWorks(t *testing.T) {
	ws := t.TempDir()
	wal := openTestWAL(t, ws)

	// BoltDB starts empty. Disk has 6 entries.
	writeDirectivesFile(t, ws, []string{
		"[TEST] disk-only A",
		"[TEST] disk-only B",
		"[TEST] disk-only C",
		"[TEST] disk-only D",
		"[TEST] disk-only E",
		"[TEST] disk-only F",
	})

	if err := wal.LoadDirectivesFromDisk(ws); err != nil {
		t.Fatal(err)
	}
	if got := countActiveDirectives(t, wal); got != 6 {
		t.Errorf("expected 6 active after additive load, got %d", got)
	}
}

// TestLoadDirectivesFromDisk_Idempotent verifies that calling twice produces
// the same active count and does NOT keep deprecating already-deprecated
// entries.
func TestLoadDirectivesFromDisk_Idempotent(t *testing.T) {
	ws := t.TempDir()
	wal := openTestWAL(t, ws)

	for i := 1; i <= 10; i++ {
		_ = wal.SaveDirective("[TEST] rule " + formatI(i))
	}
	writeDirectivesFile(t, ws, []string{
		"[TEST] rule 1",
		"[TEST] rule 2",
		"[TEST] rule 3",
		"[TEST] rule 4",
		"[TEST] rule 5",
	})

	if err := wal.LoadDirectivesFromDisk(ws); err != nil {
		t.Fatal(err)
	}
	firstActive := countActiveDirectives(t, wal)

	if err := wal.LoadDirectivesFromDisk(ws); err != nil {
		t.Fatal(err)
	}
	secondActive := countActiveDirectives(t, wal)

	if firstActive != secondActive {
		t.Errorf("second LoadDirectivesFromDisk changed active count: %d → %d (should be idempotent)", firstActive, secondActive)
	}
}
