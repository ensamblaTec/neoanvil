package rag

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// TestWAL_DimMismatch_RejectedOnInsert — regression for audit finding S9-6
// (PILAR XXVIII 143.D, 2026-05-02). Pre-fix, Insert wrote raw float32 bits
// without validating len(vec) against any canonical, so two inserts with
// different dimensions both succeeded silently and corrupted LoadGraph.
// Post-fix, the SECOND insert must fail with ErrVectorDimMismatch and the
// ciphertext for the first remain intact.
func TestWAL_DimMismatch_RejectedOnInsert(t *testing.T) {
	dir := t.TempDir()
	wal, err := OpenWAL(filepath.Join(dir, "dim_test.db"))
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	defer wal.Close()

	// First insert sets the canonical dimension to 4.
	err = wal.Insert(0, Node{DocID: 1, EdgesLength: 0}, []uint32{}, []float32{1, 2, 3, 4})
	if err != nil {
		t.Fatalf("first insert (dim=4) should succeed: %v", err)
	}

	// Second insert with dim=3 must fail — pre-fix it would silently corrupt.
	err = wal.Insert(1, Node{DocID: 2, EdgesLength: 0}, []uint32{}, []float32{5, 6, 7})
	if err == nil {
		t.Fatal("second insert with dim=3 should have failed; pre-fix bug allows mixed-dim WAL")
	}
	if !errors.Is(err, ErrVectorDimMismatch) {
		t.Errorf("expected ErrVectorDimMismatch, got %v", err)
	}

	// Same-dim insert must still succeed.
	err = wal.Insert(2, Node{DocID: 3, EdgesLength: 0}, []uint32{}, []float32{8, 9, 10, 11})
	if err != nil {
		t.Errorf("third insert (dim=4 again) should succeed: %v", err)
	}
}

// TestWAL_DimMismatch_BatchRejectedAtomic — InsertBatch with mixed dims must
// be rejected as a UNIT (no partial write). Pre-fix would write some entries
// before hitting the bad one. Post-fix the homogeneity check runs before any
// bbolt mutation.
func TestWAL_DimMismatch_BatchRejectedAtomic(t *testing.T) {
	dir := t.TempDir()
	wal, err := OpenWAL(filepath.Join(dir, "batch_test.db"))
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	defer wal.Close()

	// Establish canonical dim=4 via single Insert first.
	if err := wal.Insert(0, Node{DocID: 1}, []uint32{}, []float32{1, 2, 3, 4}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Batch with mixed dims — second vec is wrong.
	err = wal.InsertBatch(
		[]uint32{1, 2},
		[]Node{{DocID: 2}, {DocID: 3}},
		[][]uint32{{}, {}},
		[][]float32{{5, 6, 7, 8}, {9, 10, 11}},
	)
	if err == nil {
		t.Fatal("batch with mixed dims should have failed atomically")
	}
	if !errors.Is(err, ErrVectorDimMismatch) {
		t.Errorf("expected ErrVectorDimMismatch, got %v", err)
	}

	// Verify nothing from the bad batch landed: nodes 1 and 2 must be absent.
	err = wal.db.View(func(tx *bbolt.Tx) error {
		nb := tx.Bucket(bucketNodes)
		for _, id := range []uint32{1, 2} {
			key := make([]byte, 4)
			binary.LittleEndian.PutUint32(key, id)
			if v := nb.Get(key); v != nil {
				t.Errorf("partial-batch leak: node %d persisted bytes despite atomic reject", id)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestWAL_CanonicalDimPersistedAcrossReopens — first Insert sets the
// canonical; reopen the WAL and confirm a wrong-dim insert STILL fails.
// Verifies the canonical value lives in bucketHnswMeta and survives bbolt
// close+reopen, not just process-local memoization.
func TestWAL_CanonicalDimPersistedAcrossReopens(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persist_test.db")

	wal1, err := OpenWAL(dbPath)
	if err != nil {
		t.Fatalf("OpenWAL #1: %v", err)
	}
	if err := wal1.Insert(0, Node{DocID: 1}, []uint32{}, []float32{1, 2, 3, 4, 5}); err != nil {
		t.Fatalf("seed dim=5: %v", err)
	}
	if err := wal1.Close(); err != nil {
		t.Fatalf("close #1: %v", err)
	}

	wal2, err := OpenWAL(dbPath)
	if err != nil {
		t.Fatalf("OpenWAL #2: %v", err)
	}
	defer wal2.Close()

	err = wal2.Insert(1, Node{DocID: 2}, []uint32{}, []float32{6, 7, 8, 9})
	if err == nil {
		t.Fatal("post-reopen insert with dim=4 should fail (canonical=5)")
	}
	if !errors.Is(err, ErrVectorDimMismatch) {
		t.Errorf("expected ErrVectorDimMismatch post-reopen, got %v", err)
	}
}

func TestWAL_Roundtrip_FullPersistence(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test_hnsw.db")

	wal, err := OpenWAL(dbPath)
	if err != nil {
		t.Fatalf("OpenWAL failed: %v", err)
	}

	const dim = 3

	err = wal.Insert(0, Node{
		DocID: 100, EdgesOffset: 0, EdgesLength: 2, Layer: 0,
	}, []uint32{1, 2}, []float32{1.0, 0.0, 0.0})
	if err != nil {
		t.Fatalf("Insert node 0 failed: %v", err)
	}

	err = wal.Insert(1, Node{
		DocID: 101, EdgesOffset: 2, EdgesLength: 2, Layer: 0,
	}, []uint32{0, 2}, []float32{0.0, 1.0, 0.0})
	if err != nil {
		t.Fatalf("Insert node 1 failed: %v", err)
	}

	err = wal.Insert(2, Node{
		DocID: 102, EdgesOffset: 4, EdgesLength: 2, Layer: 0,
	}, []uint32{0, 1}, []float32{0.0, 0.0, 1.0})
	if err != nil {
		t.Fatalf("Insert node 2 failed: %v", err)
	}

	if err := wal.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	wal2, err := OpenWAL(dbPath)
	if err != nil {
		t.Fatalf("Reopen failed: %v", err)
	}
	defer wal2.Close()

	graph, err := wal2.LoadGraph(context.Background())
	if err != nil {
		t.Fatalf("LoadGraph failed: %v", err)
	}

	if len(graph.Nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(graph.Nodes))
	}
	verifyRoundtripGraph(t, graph, dim)
}

func verifyRoundtripGraph(t *testing.T, graph *Graph, dim int) {
	t.Helper()
	if graph.Nodes[0].DocID != 100 || graph.Nodes[1].DocID != 101 || graph.Nodes[2].DocID != 102 {
		t.Errorf("DocIDs mismatch: got %v, %v, %v",
			graph.Nodes[0].DocID, graph.Nodes[1].DocID, graph.Nodes[2].DocID)
	}
	if len(graph.Edges) != 6 {
		t.Fatalf("expected 6 edges, got %d", len(graph.Edges))
	}
	expectedEdges := []uint32{1, 2, 0, 2, 0, 1}
	for i, e := range expectedEdges {
		if graph.Edges[i] != e {
			t.Errorf("edge[%d] = %d, want %d", i, graph.Edges[i], e)
		}
	}
	if graph.VecDim != dim {
		t.Fatalf("expected VecDim=%d, got %d", dim, graph.VecDim)
	}
	if len(graph.Vectors) != 9 {
		t.Fatalf("expected 9 vector components, got %d", len(graph.Vectors))
	}
	expectedVecs := []float32{1, 0, 0, 0, 1, 0, 0, 0, 1}
	for i, v := range expectedVecs {
		if math.Abs(float64(graph.Vectors[i]-v)) > 1e-6 {
			t.Errorf("vector[%d] = %f, want %f", i, graph.Vectors[i], v)
		}
	}
}

func TestWAL_EmptyDB_LoadGraph(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "empty.db")

	wal, err := OpenWAL(dbPath)
	if err != nil {
		t.Fatalf("OpenWAL failed: %v", err)
	}
	defer wal.Close()

	graph, err := wal.LoadGraph(context.Background())
	if err != nil {
		t.Fatalf("LoadGraph on empty DB failed: %v", err)
	}

	if len(graph.Nodes) != 0 {
		t.Errorf("expected 0 nodes on empty DB, got %d", len(graph.Nodes))
	}
	if len(graph.Edges) != 0 {
		t.Errorf("expected 0 edges on empty DB, got %d", len(graph.Edges))
	}
	if len(graph.Vectors) != 0 {
		t.Errorf("expected 0 vectors on empty DB, got %d", len(graph.Vectors))
	}
}

func TestWAL_LegacyAppendNode(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "legacy.db")

	wal, err := OpenWAL(dbPath)
	if err != nil {
		t.Fatalf("OpenWAL failed: %v", err)
	}

	err = wal.AppendNode(0, Node{DocID: 200, EdgesOffset: 0, EdgesLength: 0, Layer: 1})
	if err != nil {
		t.Fatalf("AppendNode failed: %v", err)
	}

	wal.Close()

	wal2, err := OpenWAL(dbPath)
	if err != nil {
		t.Fatalf("Reopen failed: %v", err)
	}
	defer wal2.Close()

	graph, err := wal2.LoadGraph(context.Background())
	if err != nil {
		t.Fatalf("LoadGraph failed: %v", err)
	}

	if len(graph.Nodes) != 1 || graph.Nodes[0].DocID != 200 || graph.Nodes[0].Layer != 1 {
		t.Errorf("legacy node mismatch: %+v", graph.Nodes)
	}
}

// TestPurgeOldSessions verifies session_state TTL purging removes entries with
// :ts older than maxAge while preserving recent ones. [SRE-108.C]
func TestPurgeOldSessions(t *testing.T) {
	dir := t.TempDir()
	wal, err := OpenWAL(filepath.Join(dir, "wal.db"))
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	defer wal.Close()

	// Recent session — written via the public path, gets a fresh :ts.
	if err := wal.AppendSessionCertified("recent-session", "/tmp/recent.go"); err != nil {
		t.Fatalf("AppendSessionCertified: %v", err)
	}
	// Old session — manually backdate the :ts key past the cutoff.
	if err := wal.AppendSessionCertified("old-session", "/tmp/old.go"); err != nil {
		t.Fatalf("AppendSessionCertified old: %v", err)
	}
	pastTS := time.Now().Add(-48 * time.Hour).Unix()
	err = wal.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSessionState)
		raw, _ := json.Marshal(pastTS)
		return b.Put([]byte("old-session:ts"), raw)
	})
	if err != nil {
		t.Fatalf("backdate ts: %v", err)
	}

	if err := wal.PurgeOldSessions(24 * time.Hour); err != nil {
		t.Fatalf("PurgeOldSessions: %v", err)
	}

	// Recent must survive.
	if got, _ := wal.GetSessionMutations("recent-session"); len(got) != 1 {
		t.Errorf("expected recent session preserved, got %v", got)
	}
	// Old must be gone — including its :ts meta-key.
	err = wal.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSessionState)
		if v := b.Get([]byte("old-session")); v != nil {
			t.Errorf("old session not purged: %s", v)
		}
		if v := b.Get([]byte("old-session:ts")); v != nil {
			t.Errorf("old session :ts not purged: %s", v)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestSanitizeWAL_CascadeDeleteOnCorruptEdge — regression for audit 144.F.
// Pre-fix, SanitizeWAL deleted a corrupt edge entry but left the parent node
// entry intact with its stale EdgesOffset/EdgesLength. LoadGraph would then
// use those stale offsets, causing out-of-range reads or garbage edge lists.
// Post-fix, SanitizeWAL must also remove the corresponding node (and vector)
// entry so that LoadGraph never sees the orphaned node.
func TestSanitizeWAL_CascadeDeleteOnCorruptEdge(t *testing.T) {
	dir := t.TempDir()
	wal, err := OpenWAL(filepath.Join(dir, "cascade_test.db"))
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	defer wal.Close()

	// Insert one valid node (ID=1) so the graph is non-trivially populated.
	validNode := Node{DocID: 42, EdgesOffset: 0, EdgesLength: 1, Layer: 0}
	validEdges := []uint32{0}
	validVec := []float32{0.1, 0.2, 0.3}
	if err := wal.Insert(1, validNode, validEdges, validVec); err != nil {
		t.Fatalf("Insert valid node: %v", err)
	}

	// Insert a second node (ID=2) normally, then overwrite its edge entry with
	// garbage bytes that pass the uint32 length check (multiple of 4) but
	// are intentionally non-zero-length-zero-value to serve as the corrupt marker.
	// We simulate corruption by writing a 3-byte blob (not a multiple of 4).
	corruptNode := Node{DocID: 99, EdgesOffset: 99, EdgesLength: 99, Layer: 0}
	corruptEdges := []uint32{1, 2}
	corruptVec := []float32{0.4, 0.5, 0.6}
	if err := wal.Insert(2, corruptNode, corruptEdges, corruptVec); err != nil {
		t.Fatalf("Insert corrupt-target node: %v", err)
	}

	// Overwrite node 2's edge entry with 3 bytes (not a multiple of 4 → fails
	// uint32ArrayValidator) to simulate on-disk corruption.
	corruptKey := make([]byte, 4)
	binary.LittleEndian.PutUint32(corruptKey, 2)
	corruptErr := wal.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketEdges).Put(corruptKey, []byte{0xDE, 0xAD, 0xBE})
	})
	if corruptErr != nil {
		t.Fatalf("inject corruption: %v", corruptErr)
	}

	// Run SanitizeWAL — should remove node 2 from all three HNSW buckets.
	purged, err := wal.SanitizeWAL()
	if err != nil {
		t.Fatalf("SanitizeWAL: %v", err)
	}
	if purged == 0 {
		t.Fatal("SanitizeWAL purged 0 entries; expected ≥1 for the corrupt edge")
	}

	// Verify node 2 was cascade-deleted from all three buckets.
	wal.db.View(func(tx *bbolt.Tx) error {
		for _, bkt := range [][]byte{bucketNodes, bucketEdges, bucketVectors} {
			if b := tx.Bucket(bkt); b != nil {
				if v := b.Get(corruptKey); v != nil {
					t.Errorf("cascade delete missed key 2 in bucket %s", bkt)
				}
			}
		}
		return nil
	})

	// LoadGraph must succeed and return only node 1 (node 2 was purged).
	g, err := wal.LoadGraph(context.Background())
	if err != nil {
		t.Fatalf("LoadGraph after sanitize: %v", err)
	}
	if len(g.Nodes) != 1 {
		t.Errorf("expected 1 node after cascade purge, got %d", len(g.Nodes))
	}
}
