package rag

import (
	"context"
	"path/filepath"
	"testing"
)

// TestCompactWAL_PreservesDataAndDoesNotGrow inserts then repeatedly overwrites
// nodes to generate BoltDB free pages, compacts, and verifies the data survives
// and the file never grows. [D5]
func TestCompactWAL_PreservesDataAndDoesNotGrow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compact_test.db")

	wal, err := OpenWAL(path)
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	// Insert then overwrite (same nodeIDs) to free pages — the bloat compaction reclaims.
	const n = 300
	for round := 0; round < 4; round++ {
		for i := 0; i < n; i++ {
			vec := []float32{float32(i), float32(round), 3, 4}
			if err := wal.Insert(uint32(i), Node{DocID: uint64(i + 1)}, []uint32{}, vec); err != nil {
				t.Fatalf("insert round %d node %d: %v", round, i, err)
			}
		}
	}
	if err := wal.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	oldSize, newSize, err := CompactWAL(path)
	if err != nil {
		t.Fatalf("CompactWAL: %v", err)
	}
	if newSize <= 0 {
		t.Fatalf("compacted file size must be > 0, got %d", newSize)
	}
	if newSize > oldSize {
		t.Errorf("compaction must never grow the file: old=%d new=%d", oldSize, newSize)
	}

	// Data must survive: reopen and load the graph.
	wal2, err := OpenWAL(path)
	if err != nil {
		t.Fatalf("OpenWAL after compact: %v", err)
	}
	defer wal2.Close()
	g, err := wal2.LoadGraph(context.Background())
	if err != nil {
		t.Fatalf("LoadGraph after compact: %v", err)
	}
	if len(g.Nodes) != n {
		t.Errorf("expected %d nodes after compaction, got %d", n, len(g.Nodes))
	}
}

// TestCompactWALIfOversized_Threshold checks the threshold gate: below the
// threshold and when disabled (thresholdMB <= 0) it must be a clean no-op.
func TestCompactWALIfOversized_Threshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "thresh_test.db")
	wal, err := OpenWAL(path)
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	if err := wal.Insert(0, Node{DocID: 1}, []uint32{}, []float32{1, 2, 3, 4}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_ = wal.Close()

	// A tiny fresh WAL is well under any sane MB threshold → no-op.
	compacted, _, _, err := CompactWALIfOversized(path, 256)
	if err != nil {
		t.Fatalf("CompactWALIfOversized below threshold: %v", err)
	}
	if compacted {
		t.Error("small WAL under threshold should not be compacted")
	}

	// thresholdMB <= 0 disables the check entirely.
	for _, disabled := range []int{0, -1} {
		c, _, _, derr := CompactWALIfOversized(path, disabled)
		if derr != nil {
			t.Errorf("disabled (%d) should be a clean no-op: %v", disabled, derr)
		}
		if c {
			t.Errorf("thresholdMB=%d must disable compaction", disabled)
		}
	}
}

// TestCompactWAL_MissingFile — a nonexistent path is an error, not a panic.
func TestCompactWAL_MissingFile(t *testing.T) {
	if _, _, err := CompactWAL(filepath.Join(t.TempDir(), "does-not-exist.db")); err == nil {
		t.Fatal("CompactWAL on a missing file must return an error")
	}
}

// TestCompactWAL_LockedFile — compacting a WAL that another handle holds open
// must fail (the exclusive bbolt lock), never corrupt the file or block forever.
func TestCompactWAL_LockedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "locked_test.db")
	wal, err := OpenWAL(path)
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	defer wal.Close()
	if err := wal.Insert(0, Node{DocID: 1}, []uint32{}, []float32{1, 2, 3, 4}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// The wal handle still holds the exclusive lock — compaction must fail fast.
	if _, _, err := CompactWAL(path); err == nil {
		t.Error("CompactWAL on an open WAL must fail (lock conflict), got nil")
	}
}
