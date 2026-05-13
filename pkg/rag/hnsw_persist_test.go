package rag

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// makeTestGraph builds a deterministic small Graph + matching WAL for
// round-trip testing. dim=4 keeps the file tiny so tests are fast.
func makeTestGraph(t *testing.T, dim int) (*Graph, *WAL, string) {
	t.Helper()
	tmp := t.TempDir()
	walPath := filepath.Join(tmp, "test.db")
	w, err := OpenWAL(walPath)
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	g := &Graph{
		Nodes: []Node{
			{DocID: 100, EdgesOffset: 0, EdgesLength: 2, Layer: 0},
			{DocID: 200, EdgesOffset: 2, EdgesLength: 1, Layer: 1},
			{DocID: 300, EdgesOffset: 3, EdgesLength: 0, Layer: 0},
		},
		Edges:   []uint32{1, 2, 0},
		Vectors: make([]float32, 3*dim),
		VecDim:  dim,
	}
	// Populate vectors with deterministic non-trivial bit patterns so
	// equalGraph catches any encoding errors.
	for i := range g.Vectors {
		g.Vectors[i] = float32(i)*0.5 - 1.0
	}
	return g, w, walPath
}

// equalGraph asserts bit-exact equality of two graphs' serializable
// fields. Companion arrays (Int8/Binary) are derived; not checked.
func equalGraph(t *testing.T, want, got *Graph) {
	t.Helper()
	if got.VecDim != want.VecDim {
		t.Errorf("VecDim got %d want %d", got.VecDim, want.VecDim)
	}
	if len(got.Nodes) != len(want.Nodes) {
		t.Fatalf("len(Nodes) got %d want %d", len(got.Nodes), len(want.Nodes))
	}
	for i := range want.Nodes {
		if got.Nodes[i] != want.Nodes[i] {
			t.Errorf("Nodes[%d] got %+v want %+v", i, got.Nodes[i], want.Nodes[i])
		}
	}
	if len(got.Edges) != len(want.Edges) {
		t.Fatalf("len(Edges) got %d want %d", len(got.Edges), len(want.Edges))
	}
	for i := range want.Edges {
		if got.Edges[i] != want.Edges[i] {
			t.Errorf("Edges[%d] got %d want %d", i, got.Edges[i], want.Edges[i])
		}
	}
	if len(got.Vectors) != len(want.Vectors) {
		t.Fatalf("len(Vectors) got %d want %d", len(got.Vectors), len(want.Vectors))
	}
	for i := range want.Vectors {
		if got.Vectors[i] != want.Vectors[i] {
			t.Errorf("Vectors[%d] got %f want %f", i, got.Vectors[i], want.Vectors[i])
		}
	}
}

// TestSaveLoad_RoundTripBitExact covers the happy path: write + read
// produces an identical graph. Exercises [149.H case 1].
func TestSaveLoad_RoundTripBitExact(t *testing.T) {
	for _, dim := range []int{4, 128, 768} {
		t.Run("dim_"+itoa(dim), func(t *testing.T) {
			g, w, _ := makeTestGraph(t, dim)
			snapshotPath := filepath.Join(t.TempDir(), "snap.bin")
			if err := SaveHNSWSnapshot(g, w, snapshotPath); err != nil {
				t.Fatalf("Save: %v", err)
			}
			snap, err := LoadHNSWSnapshot(snapshotPath)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			equalGraph(t, g, snap.Graph)
		})
	}
}

// TestLoad_Missing covers ErrHNSWSnapshotMissing for first-boot.
func TestLoad_Missing(t *testing.T) {
	_, err := LoadHNSWSnapshot(filepath.Join(t.TempDir(), "does_not_exist.bin"))
	if !errors.Is(err, ErrHNSWSnapshotMissing) {
		t.Errorf("got %v want ErrHNSWSnapshotMissing", err)
	}
}

// TestLoad_BadMagic rejects non-snapshot files. [149.H case 7]
func TestLoad_BadMagic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.bin")
	bogus := make([]byte, hnswSnapshotHeaderSize)
	copy(bogus[0:4], "NOPE")
	if err := os.WriteFile(path, bogus, 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadHNSWSnapshot(path)
	if !errors.Is(err, ErrHNSWSnapshotCorrupt) {
		t.Errorf("got %v want ErrHNSWSnapshotCorrupt", err)
	}
}

// TestLoad_SchemaTooNew rejects newer-than-known schema (downgrade
// attempt). [149.H case 7] Variant — never overwrite, never panic.
func TestLoad_SchemaTooNew(t *testing.T) {
	g, w, _ := makeTestGraph(t, 4)
	path := filepath.Join(t.TempDir(), "snap.bin")
	if err := SaveHNSWSnapshot(g, w, path); err != nil {
		t.Fatal(err)
	}
	// Bump the on-disk SchemaVersion past current.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint16(data[4:6], HNSWSnapshotSchemaVersion+1)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadHNSWSnapshot(path)
	if !errors.Is(err, ErrHNSWSnapshotSchemaTooNew) {
		t.Errorf("got %v want ErrHNSWSnapshotSchemaTooNew", err)
	}
	// File must still exist (no overwrite of unknown schema).
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("file destroyed on schema-too-new: %v", statErr)
	}
}

// TestLoad_ChecksumMismatch covers [149.H case 5] — bit flip detection.
func TestLoad_ChecksumMismatch(t *testing.T) {
	g, w, _ := makeTestGraph(t, 4)
	path := filepath.Join(t.TempDir(), "snap.bin")
	if err := SaveHNSWSnapshot(g, w, path); err != nil {
		t.Fatal(err)
	}
	// Flip a bit in the body (after header) — checksum will mismatch.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[hnswSnapshotHeaderSize] ^= 0x01
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadHNSWSnapshot(path)
	if !errors.Is(err, ErrHNSWSnapshotCorrupt) {
		t.Errorf("got %v want ErrHNSWSnapshotCorrupt (bit flip)", err)
	}
}

// TestLoad_HeaderTamperDetected covers the same checksum scope check
// for header bytes [0..68]. Flipping a header byte must fail load.
// [149.H case 6]
func TestLoad_HeaderTamperDetected(t *testing.T) {
	g, w, _ := makeTestGraph(t, 4)
	path := filepath.Join(t.TempDir(), "snap.bin")
	if err := SaveHNSWSnapshot(g, w, path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte in BuildAtUnix (offset 28) — included in checksum scope.
	data[28] ^= 0x80
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadHNSWSnapshot(path)
	if !errors.Is(err, ErrHNSWSnapshotCorrupt) {
		t.Errorf("got %v want ErrHNSWSnapshotCorrupt (header tamper)", err)
	}
}

// TestLoad_TruncatedFile covers [149.H case 5] — partial write recovery.
func TestLoad_TruncatedFile(t *testing.T) {
	g, w, _ := makeTestGraph(t, 4)
	path := filepath.Join(t.TempDir(), "snap.bin")
	if err := SaveHNSWSnapshot(g, w, path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Truncate to 50% — body short, size check fires before checksum.
	if err := os.WriteFile(path, data[:len(data)/2], 0644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadHNSWSnapshot(path)
	if !errors.Is(err, ErrHNSWSnapshotCorrupt) {
		t.Errorf("got %v want ErrHNSWSnapshotCorrupt (truncated)", err)
	}
}

// TestLoad_OOMBoundsRejected covers [149.H — F3 from DS audit] —
// crafted snapshot with NodeCount=4G must NOT trigger make([]Node,4G).
func TestLoad_OOMBoundsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.bin")
	hdr := encodeHNSWSnapshotHeader(HNSWSnapshotHeader{
		SchemaVersion: HNSWSnapshotSchemaVersion,
		CanonicalDim:  4,
		NodeCount:     maxSnapshotNodeCount + 1, // exceed cap
		EdgeCount:     0,
		VectorCount:   0,
		BuildAtUnix:   time.Now().Unix(),
	})
	if err := os.WriteFile(path, hdr[:], 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadHNSWSnapshot(path)
	if !errors.Is(err, ErrHNSWSnapshotCorrupt) {
		t.Errorf("got %v want ErrHNSWSnapshotCorrupt (oom bounds)", err)
	}
}

// TestStaleGuard_NoOpWriteIsNotStale verifies the v2 redesign:
// no-op write transactions (e.g. sanitizer iterating buckets without
// purging anything) MUST NOT mark the snapshot stale. Prior schema (v1
// with WALTxID) failed here because every Update bumped Tx.ID.
// [149 v2 stale-guard fix, 2026-05-02]
func TestStaleGuard_NoOpWriteIsNotStale(t *testing.T) {
	g, w, _ := makeTestGraph(t, 4)
	path := filepath.Join(t.TempDir(), "snap.bin")
	if err := SaveHNSWSnapshot(g, w, path); err != nil {
		t.Fatal(err)
	}
	snap, err := LoadHNSWSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	// Empty Update — bumps Tx.ID but doesn't touch bucket key counts.
	if err := w.db.Update(func(_ *bolt.Tx) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// After no-op write: snapshot must still be FRESH.
	if stale, reason := IsHNSWSnapshotStale(w, &snap.Header); stale {
		t.Errorf("no-op write should NOT mark snapshot stale; got stale: %s", reason)
	}
}

// TestStaleGuard_KeyCountChangeMarksStale verifies the new positive
// trigger: when a real bucket Put adds a key, the snapshot becomes
// stale. This is the inverse of TestStaleGuard_NoOpWriteIsNotStale.
// [149 v2 stale-guard fix, 2026-05-02]
func TestStaleGuard_KeyCountChangeMarksStale(t *testing.T) {
	g, w, _ := makeTestGraph(t, 4)
	path := filepath.Join(t.TempDir(), "snap.bin")
	if err := SaveHNSWSnapshot(g, w, path); err != nil {
		t.Fatal(err)
	}
	snap, err := LoadHNSWSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	// Add a key to the nodes bucket — direct bbolt Put bypasses the WAL
	// API. Sufficient for the test: Stats().KeyN will go up.
	if err := w.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketNodes)
		if b == nil {
			return nil
		}
		return b.Put([]byte("test_key_extra"), make([]byte, 16))
	}); err != nil {
		t.Fatal(err)
	}
	if stale, reason := IsHNSWSnapshotStale(w, &snap.Header); !stale {
		t.Errorf("expected stale after node bucket Put; got fresh: %s", reason)
	}
}

// TestStaleGuard_NilInputs verifies defensive returns.
func TestStaleGuard_NilInputs(t *testing.T) {
	if stale, _ := IsHNSWSnapshotStale(nil, nil); !stale {
		t.Error("nil wal+header should report stale")
	}
}

// TestStaleGuard_WALSizeZeroSkipsFileSizeGate verifies the WALFileSize=0
// sentinel emitted by buildSnapshotHeader when shutdown raced wal.Close().
// On next boot, the file-size gate must be skipped (size 0 ≠ actual size
// always, would force unnecessary cold rebuild). Stale must come solely
// from NodeKeyN/EdgeKeyN/VectorKeyN mismatch. [bug-fix 2026-05-13]
func TestStaleGuard_WALSizeZeroSkipsFileSizeGate(t *testing.T) {
	g, w, _ := makeTestGraph(t, 4)
	path := filepath.Join(t.TempDir(), "snap.bin")
	if err := SaveHNSWSnapshot(g, w, path); err != nil {
		t.Fatal(err)
	}
	snap, err := LoadHNSWSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the shutdown race: zero out WALFileSize as buildSnapshotHeader
	// would do when stat fails. WAL file on disk has its actual size; the
	// sentinel must skip the comparison.
	snap.Header.WALFileSize = 0
	if stale, reason := IsHNSWSnapshotStale(w, &snap.Header); stale {
		t.Errorf("WALFileSize=0 sentinel must not mark snapshot stale on its own; got stale: %s", reason)
	}
	// Sanity: with real key change, sentinel-stamped snapshot still stales.
	if err := w.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketNodes)
		if b == nil {
			return nil
		}
		return b.Put([]byte("extra_after_zero_sentinel"), make([]byte, 16))
	}); err != nil {
		t.Fatal(err)
	}
	if stale, _ := IsHNSWSnapshotStale(w, &snap.Header); !stale {
		t.Error("expected stale after node bucket Put even with WALFileSize=0 sentinel")
	}
}

// TestSaveSnapshot_HealthyWALSetsSize verifies the happy path: a healthy
// open WAL produces a non-zero WALFileSize. The shutdown-race case (WAL
// closed mid-flight → Path() empty → stat fails → sentinel 0) cannot be
// reliably reproduced in-test (closing the WAL also breaks bucket probe),
// but the stale-guard test above proves the sentinel works end-to-end on
// the read side. [bug-fix 2026-05-13]
func TestSaveSnapshot_HealthyWALSetsSize(t *testing.T) {
	g, w, _ := makeTestGraph(t, 4)
	path := filepath.Join(t.TempDir(), "snap.bin")
	if err := SaveHNSWSnapshot(g, w, path); err != nil {
		t.Fatal(err)
	}
	healthy, err := LoadHNSWSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if healthy.Header.WALFileSize == 0 {
		t.Error("healthy WAL save must produce non-zero WALFileSize (sentinel 0 only on stat failure)")
	}
}

// itoa is a tiny helper to avoid importing strconv just for subtests.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	negative := n < 0
	if negative {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
