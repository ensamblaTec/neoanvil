package rag

import (
	"encoding/json"
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
// chronic DUAL-LAYER-SYNC inflation: 5 [TEST]-tagged BoltDB entries, disk
// file has 3 of those. After LoadDirectivesFromDisk, the 2 [TEST] entries
// not on disk must be marked ~~OBSOLETO~~.
//
// Sizing rationale: keep BoltDB total below syncRelativeLossSampleMin (10)
// so the relative-loss guard does NOT trigger. The 2026-05-13 7-directive
// drift incident demonstrated that 50%+ losses are suspect and the guard
// MUST stop them — this test deliberately stays in the "small set" regime
// where mass deprecation is legitimate operator intent.
//
// Uses delta-based assertions because OpenWAL may seed non-[TEST] entries
// (workspace doctrine bootstrap) that should not be touched by this test.
func TestLoadDirectivesFromDisk_DestructiveSync_DeprecatesMissing(t *testing.T) {
	ws := t.TempDir()
	wal := openTestWAL(t, ws)

	baseline := countActiveDirectives(t, wal)

	// Seed BoltDB with 5 [TEST]-tagged entries.
	for i := 1; i <= 5; i++ {
		if err := wal.SaveDirective("[TEST] rule number " + formatI(i)); err != nil {
			t.Fatal(err)
		}
	}
	if got := countActiveDirectives(t, wal); got != baseline+5 {
		t.Fatalf("setup: expected baseline+5=%d active, got %d", baseline+5, got)
	}
	if baseline+5 >= syncRelativeLossSampleMin {
		t.Skipf("baseline (%d) + 5 ≥ rel-loss sample-min (%d) — rel-loss guard would trigger; this test only exercises the small-set regime",
			baseline, syncRelativeLossSampleMin)
	}

	// Disk has only entries 1, 3, 5 (3 active [TEST]).
	writeDirectivesFile(t, ws, []string{
		"[TEST] rule number 1",
		"[TEST] rule number 3",
		"[TEST] rule number 5",
	})

	if err := wal.LoadDirectivesFromDisk(ws); err != nil {
		t.Fatal(err)
	}

	// Active count assertion: only the 3 disk entries should remain. The
	// 5 [TEST] seeded minus 2 not-in-disk = 3 [TEST] survive. The original
	// baseline entries are also missing from disk so deprecated. With
	// BoltDB total < sample-min the rel-loss guard skips → sweep runs.
	active := countActiveDirectives(t, wal)
	if active != 3 {
		t.Errorf("post-sync: expected 3 active (3 [TEST] on disk; baseline entries also missing from disk so deprecated), got %d", active)
	}

	// Verify the 3 disk [TEST] entries are active.
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

// TestLoadDirectivesFromDisk_RelativeLossGuard verifies the second guard: when
// BoltDB has ≥10 active entries AND disk lost >20% of them, the destructive
// sweep is SKIPPED. Closes the gap from the 2026-05-13 7-directive drift
// incident where disk=50 vs BoltDB=57 (12% loss) slipped through the abs guard.
func TestLoadDirectivesFromDisk_RelativeLossGuard(t *testing.T) {
	ws := t.TempDir()
	wal := openTestWAL(t, ws)

	baseline := countActiveDirectives(t, wal)

	// Seed BoltDB with 30 entries — above the sample-min 10, below abs threshold 50.
	seedCount := 30 - baseline
	if seedCount < syncRelativeLossSampleMin {
		seedCount = syncRelativeLossSampleMin
	}
	for i := 1; i <= seedCount; i++ {
		if err := wal.SaveDirective("[TEST] rel-loss rule " + formatI(i)); err != nil {
			t.Fatal(err)
		}
	}
	before := countActiveDirectives(t, wal)
	if before < syncRelativeLossSampleMin {
		t.Fatalf("setup: expected ≥%d active to trigger relative-loss guard, got %d", syncRelativeLossSampleMin, before)
	}

	// Disk has only ~67% of the seeded entries (33% loss > 20% threshold).
	keep := seedCount * 2 / 3
	var lines []string
	for i := 1; i <= keep; i++ {
		lines = append(lines, "[TEST] rel-loss rule "+formatI(i))
	}
	writeDirectivesFile(t, ws, lines)

	if err := wal.LoadDirectivesFromDisk(ws); err != nil {
		t.Fatal(err)
	}

	// Guard MUST trigger — no destructive sweep, active count unchanged.
	if got := countActiveDirectives(t, wal); got != before {
		t.Errorf("relative-loss guard should prevent mass-deprecation when disk lost >20%%; active went %d → %d", before, got)
	}
}

// TestLoadDirectivesFromDisk_RelativeLossWithinThreshold verifies the inverse:
// small drift (≤20%) is treated as legitimate intent — destructive sweep runs.
func TestLoadDirectivesFromDisk_RelativeLossWithinThreshold(t *testing.T) {
	ws := t.TempDir()
	wal := openTestWAL(t, ws)

	baseline := countActiveDirectives(t, wal)

	// Seed BoltDB with 20 [TEST] entries.
	for i := 1; i <= 20; i++ {
		if err := wal.SaveDirective("[TEST] small-drift rule " + formatI(i)); err != nil {
			t.Fatal(err)
		}
	}
	before := countActiveDirectives(t, wal)

	// Disk drops 1 of the 20 [TEST] entries (5% loss < 20% threshold). Baseline
	// entries are also missing so the loss reported to the guard is larger;
	// we just verify the destructive sweep actually ran for ours.
	var lines []string
	for i := 2; i <= 20; i++ {
		lines = append(lines, "[TEST] small-drift rule "+formatI(i))
	}
	writeDirectivesFile(t, ws, lines)

	if err := wal.LoadDirectivesFromDisk(ws); err != nil {
		t.Fatal(err)
	}

	// We expect rule #1 deprecated. Net active = before - 1 - baseline (baselines also drop).
	// Concrete check: rule #1 should now be ~~OBSOLETO~~ in BoltDB.
	all, _ := wal.GetDirectives()
	found := false
	for _, r := range all {
		if strings.HasPrefix(r, "~~OBSOLETO~~") && strings.Contains(r, "[TEST] small-drift rule 1") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected destructive sweep to deprecate rule #1 when relative loss < threshold; baseline=%d, before=%d, active_now=%d",
			baseline, before, countActiveDirectives(t, wal))
	}
}

// TestSnapshotDirectives_WritesValidJSON verifies the pre-destructive backup
// helper produces a parseable JSON snapshot with correct counts.
func TestSnapshotDirectives_WritesValidJSON(t *testing.T) {
	ws := t.TempDir()
	wal := openTestWAL(t, ws)
	baseline := countActiveDirectives(t, wal)

	if err := wal.SaveDirective("[TEST] snapshot rule 1"); err != nil {
		t.Fatal(err)
	}
	if err := wal.SaveDirective("[TEST] snapshot rule 2"); err != nil {
		t.Fatal(err)
	}
	// Soft-delete one entry so the snapshot has both active + deprecated.
	if err := wal.DeprecateDirective(baseline+1, 0); err != nil {
		t.Fatal(err)
	}

	snapshotPath := filepath.Join(ws, ".neo", "db", "directives_snapshot.json")
	if err := wal.SnapshotDirectives(snapshotPath); err != nil {
		t.Fatalf("SnapshotDirectives failed: %v", err)
	}

	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("snapshot file unreadable: %v", err)
	}

	var payload struct {
		SnapshotAtUnix  int64    `json:"snapshot_at_unix"`
		ActiveCount     int      `json:"active_count"`
		DeprecatedCount int      `json:"deprecated_count"`
		Directives      []string `json:"directives"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("snapshot JSON invalid: %v", err)
	}

	if payload.SnapshotAtUnix == 0 {
		t.Error("snapshot_at_unix should be non-zero")
	}
	if payload.DeprecatedCount < 1 {
		t.Errorf("expected ≥1 deprecated entry in snapshot, got %d", payload.DeprecatedCount)
	}
	if payload.ActiveCount < 1 {
		t.Errorf("expected ≥1 active entry in snapshot, got %d", payload.ActiveCount)
	}
	totalEntries := payload.ActiveCount + payload.DeprecatedCount
	if len(payload.Directives) != totalEntries {
		t.Errorf("directives slice length %d != active+deprecated %d", len(payload.Directives), totalEntries)
	}
}

// TestSnapshotDirectives_MissingDirReturnsNil verifies that SnapshotDirectives
// creates parent directories rather than failing on missing paths.
func TestSnapshotDirectives_CreatesParentDir(t *testing.T) {
	ws := t.TempDir()
	wal := openTestWAL(t, ws)

	if err := wal.SaveDirective("[TEST] dir-create rule"); err != nil {
		t.Fatal(err)
	}

	// Path with two missing parent levels.
	snapshotPath := filepath.Join(ws, "nonexistent", "deeper", "snap.json")
	if err := wal.SnapshotDirectives(snapshotPath); err != nil {
		t.Fatalf("SnapshotDirectives should create parents: %v", err)
	}
	if _, err := os.Stat(snapshotPath); err != nil {
		t.Fatalf("snapshot file not written: %v", err)
	}
}

// TestRestoreDirectivesFromSnapshot_FillsGaps verifies the restore loop:
// take a snapshot, soft-delete entries, compact them out, then restore from
// the snapshot — missing entries should be re-added (active count restored).
func TestRestoreDirectivesFromSnapshot_FillsGaps(t *testing.T) {
	ws := t.TempDir()
	wal := openTestWAL(t, ws)

	// Seed 4 entries.
	for i := 1; i <= 4; i++ {
		if err := wal.SaveDirective("[TEST] restore rule " + formatI(i)); err != nil {
			t.Fatal(err)
		}
	}
	before := countActiveDirectives(t, wal)

	// Take snapshot (state = before).
	snapshotPath := filepath.Join(ws, ".neo", "db", "directives_snapshot.json")
	if err := wal.SnapshotDirectives(snapshotPath); err != nil {
		t.Fatal(err)
	}

	// Simulate accidental loss: deprecate 2 + compact (hard-purge).
	all, _ := wal.GetDirectives()
	deprecated := 0
	for i, r := range all {
		if strings.HasPrefix(r, "[TEST] restore rule") && deprecated < 2 {
			if err := wal.DeprecateDirective(i+1, 0); err == nil {
				deprecated++
			}
		}
	}
	if _, _, err := wal.CompactDirectives(); err != nil {
		t.Fatal(err)
	}
	afterLoss := countActiveDirectives(t, wal)
	if afterLoss >= before {
		t.Fatalf("setup: expected loss after deprecate+compact, before=%d after=%d", before, afterLoss)
	}

	// Restore from snapshot.
	added, err := wal.RestoreDirectivesFromSnapshot(snapshotPath)
	if err != nil {
		t.Fatalf("RestoreDirectivesFromSnapshot failed: %v", err)
	}
	if added < 1 {
		t.Errorf("expected ≥1 entry restored, got %d", added)
	}

	final := countActiveDirectives(t, wal)
	if final < before {
		t.Errorf("post-restore: expected active count ≥ pre-loss %d, got %d (added=%d)", before, final, added)
	}
}

// TestRestoreDirectivesFromSnapshot_Idempotent verifies a second restore call
// adds zero entries (all already present).
func TestRestoreDirectivesFromSnapshot_Idempotent(t *testing.T) {
	ws := t.TempDir()
	wal := openTestWAL(t, ws)

	if err := wal.SaveDirective("[TEST] idem rule 1"); err != nil {
		t.Fatal(err)
	}
	snapshotPath := filepath.Join(ws, ".neo", "db", "directives_snapshot.json")
	if err := wal.SnapshotDirectives(snapshotPath); err != nil {
		t.Fatal(err)
	}

	// First restore — should add 0 since everything in snapshot is already present.
	added1, err := wal.RestoreDirectivesFromSnapshot(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if added1 != 0 {
		t.Errorf("first restore: expected 0 added (no gap), got %d", added1)
	}

	// Second restore — same result.
	added2, err := wal.RestoreDirectivesFromSnapshot(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if added2 != 0 {
		t.Errorf("second restore: expected 0 added (idempotent), got %d", added2)
	}
}

// TestRestoreDirectivesFromSnapshot_SkipsObsoleted verifies that ~~OBSOLETO~~
// entries in the snapshot are NOT re-activated by restore.
func TestRestoreDirectivesFromSnapshot_SkipsObsoleted(t *testing.T) {
	ws := t.TempDir()
	wal := openTestWAL(t, ws)

	if err := wal.SaveDirective("[TEST] obsoleto rule"); err != nil {
		t.Fatal(err)
	}
	// Pre-seed: deprecate the entry so the snapshot captures the obsoleto marker.
	all, _ := wal.GetDirectives()
	for i, r := range all {
		if strings.Contains(r, "[TEST] obsoleto rule") {
			if err := wal.DeprecateDirective(i+1, 0); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	snapshotPath := filepath.Join(ws, ".neo", "db", "directives_snapshot.json")
	if err := wal.SnapshotDirectives(snapshotPath); err != nil {
		t.Fatal(err)
	}

	// Hard-purge the deprecated entry.
	if _, _, err := wal.CompactDirectives(); err != nil {
		t.Fatal(err)
	}

	// Restore — should NOT re-introduce the obsoleted entry.
	added, err := wal.RestoreDirectivesFromSnapshot(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	postAll, _ := wal.GetDirectives()
	for _, r := range postAll {
		if strings.Contains(r, "[TEST] obsoleto rule") {
			t.Errorf("restore should NOT re-introduce obsoleto entries; found %q after restore (added=%d)", r, added)
		}
	}
}

// TestRelativeLossPct verifies the math helper handles edge cases cleanly.
func TestRelativeLossPct(t *testing.T) {
	cases := []struct {
		name                       string
		activeOnDisk, activeInBolt int
		want                       int
	}{
		{"zero bolt", 0, 0, 0},
		{"zero bolt with disk", 5, 0, 0},
		{"disk equals bolt", 50, 50, 0},
		{"disk exceeds bolt", 60, 50, 0},
		{"50% loss", 25, 50, 50},
		{"20% loss exact", 40, 50, 20},
		{"21% loss", 39, 50, 22}, // (50-39)*100/50 = 22
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := relativeLossPct(tc.activeOnDisk, tc.activeInBolt); got != tc.want {
				t.Errorf("relativeLossPct(%d,%d) = %d, want %d", tc.activeOnDisk, tc.activeInBolt, got, tc.want)
			}
		})
	}
}
