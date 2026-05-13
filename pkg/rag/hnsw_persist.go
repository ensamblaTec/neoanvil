// pkg/rag/hnsw_persist.go — fast-boot snapshot for the HNSW Graph.
// ÉPICA 149 / PILAR XXIX. Mirrors PILAR XXXII pattern (CPG fast-boot to
// .neo/db/cpg.bin) for the in-memory HNSW state.
//
// Problem: WAL.LoadGraph rebuilds Nodes/Edges/Vectors from a 3.3 GB
// bbolt WAL on every cold start, taking ~6 minutes. After this snapshot
// is in place, a fresh boot reads back the binary blob in <5 seconds
// (target on a 3 GB snapshot, raw read into RAM, no per-vector parse).
//
// Format (all little-endian):
//
//	Header (100 bytes)
//	  0   4    Magic "HNSW"
//	  4   2    SchemaVersion uint16  (current: HNSWSnapshotSchemaVersion)
//	  6   2    CanonicalDim  uint16  (== Graph.VecDim)
//	  8   4    NodeCount     uint32  (capped at maxNodeCount for DoS protection)
//	  12  8    EdgeCount     uint64  (capped at maxEdgeCount)
//	  20  8    VectorCount   uint64  (must equal NodeCount * CanonicalDim)
//	  28  8    BuildAtUnix   int64   (informational only)
//	  36  8    WALFileSize   int64   (stale guard: see IsHNSWSnapshotStale)
//	  44  8    WALTxID       uint64  (stale guard: bbolt monotonic write counter)
//	  52  16   Reserved (zeros for future extension)
//	  68  32   ChecksumBlake2b
//
//	Body (lengths in header)
//	  Nodes:   16 * NodeCount   (per-node: DocID u64, EdgesOffset u32, EdgesLength u16, Layer u8, Reserved u8)
//	  Edges:   4  * EdgeCount   (uint32 LE each)
//	  Vectors: 4  * VectorCount (float32 LE each, raw bit pattern)
//
// Checksum scope: blake2b(header_bytes_minus_checksum_field || body_bytes).
// Catches accidental corruption (bit flip, partial write, disk error).
// NOT a security primitive — an attacker with write access to .neo/db/
// already controls the system; recomputing blake2b after a malicious
// edit is trivial. Rename "header authentication" — it's an integrity
// check.
//
// Stale guard rationale: file mtime is rejected because Vacuum_Memory
// (BoltDB freelist rewrite) bumps mtime without semantic change. Instead
// we capture (file size, write tx id). bbolt's Tx.ID() is monotonic on
// commits; file size grows on writes that allocate fresh pages. A write
// that reuses freelist pages still bumps txid. So the (size, txid) pair
// is a tight lower bound for "the WAL has changed since snapshot save".
//
// Concurrency model:
//   - Save acquires Graph.snapshotMu.RLock(); blocks Insert/InsertBatch
//     (which take Lock()) for the duration of body serialization.
//   - Save reads the WAL's tx id via wal.db.View — does NOT open a
//     second bolt handle (that would deadlock against the live RW open).
//   - Atomic rename: write to outPath.tmp → fsync → rename → fsync parent
//     dir. SIGKILL between fsync and rename leaves outPath unchanged.

package rag

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	"golang.org/x/crypto/blake2b"
)

// HNSWSnapshotSchemaVersion bumps when the on-disk format becomes
// incompatible. Older snapshots are rejected with a cold rebuild.
//
// v1 → v2 (2026-05-02): replaced WALTxID stale guard with per-bucket
// KeyN counts (NodeKeyN/EdgeKeyN/VectorKeyN). Runtime evidence showed
// txid bumps from sanitizer no-op writes invalidated every snapshot
// despite no semantic change to the data. KeyN only changes on real
// Insert/InsertBatch/sanitizer purges.
const HNSWSnapshotSchemaVersion uint16 = 2

// HNSWSnapshotMagic is the file's first 4 bytes. Any other magic →
// reject as a non-snapshot file.
const HNSWSnapshotMagic = "HNSW"

const (
	// hnswSnapshotHeaderSize is the on-disk size of the header in bytes.
	hnswSnapshotHeaderSize = 100
	// hnswSnapshotChecksumOffset where blake2b begins inside the header.
	hnswSnapshotChecksumOffset = 68
	// hnswSnapshotChecksumLen is the blake2b output length (256-bit).
	hnswSnapshotChecksumLen = 32

	// nodeOnDiskSize is the explicit per-node encoding (NOT struct sizeof).
	nodeOnDiskSize = 16
)

// Bounds on header fields to defuse OOM-by-malicious-snapshot. Snapshot
// counts above these limits → reject. Numbers chosen to fit modern
// production deployments with comfortable headroom.
//
// [F3 from DS audit 2026-05-02]: an attacker with write access to .neo/db/
// can drop a snapshot with NodeCount=0xFFFFFFFF → make([]Node, 4.3B) → OOM
// kill on boot. Bounds make this DoS impossible at parse time.
const (
	maxSnapshotNodeCount   uint32 = 50_000_000  // 50M nodes (~800 MB just for nodes slab)
	maxSnapshotEdgeCount   uint64 = 500_000_000 // 500M edges (~2 GB)
	maxSnapshotVectorCount uint64 = 1 << 35     // 32G floats (128 GB) — guard against overflow
	maxSnapshotDim         uint16 = 8192        // fits any production embedding
)

// ErrHNSWSnapshotMissing is returned by LoadHNSWSnapshot when the file
// doesn't exist. Caller should fall back to cold WAL.LoadGraph.
var ErrHNSWSnapshotMissing = errors.New("hnsw snapshot missing")

// ErrHNSWSnapshotCorrupt is returned when the file exists but fails
// validation (magic, schema, checksum, bounds). Caller should fall back.
var ErrHNSWSnapshotCorrupt = errors.New("hnsw snapshot corrupt")

// ErrHNSWSnapshotSchemaTooNew is returned when SchemaVersion exceeds the
// running binary's known version (downgrade attempt). Caller falls back
// to cold load WITHOUT overwriting the snapshot — never destroy data we
// don't understand.
var ErrHNSWSnapshotSchemaTooNew = errors.New("hnsw snapshot schema too new for this binary")

// HNSWSnapshotHeader is the parsed in-memory view of the on-disk header.
//
// Stale-guard fields explained:
//   - WALFileSize: cheap O(1) Stat — first gate. If different, definitely
//     stale, no need to open bbolt.
//   - {Node,Edge,Vector}KeyN: per-bucket KeyN counts read via tx.Bucket.Stats().
//     Only change on real Insert/InsertBatch/sanitizer purges; immune to
//     no-op write tx (the failure mode of the v1 schema with WALTxID).
type HNSWSnapshotHeader struct {
	SchemaVersion uint16
	CanonicalDim  uint16
	NodeCount     uint32
	EdgeCount     uint64
	VectorCount   uint64
	BuildAtUnix   int64
	WALFileSize   int64
	NodeKeyN      uint64
	EdgeKeyN      uint64
	VectorKeyN    uint64
}

// HNSWSnapshot is the result of a successful Load. The Graph it holds
// is fully populated and ready to be published as the live graph.
type HNSWSnapshot struct {
	Header HNSWSnapshotHeader
	Graph  *Graph
}

// hnswPersistMu serializes Save vs Save calls (periodic ticker + SIGTERM
// handler must not race writing to the same .tmp file). Per-process
// global because there's only one Graph being persisted at a time.
var hnswPersistMu sync.Mutex

// SaveHNSWSnapshot serializes g to outPath atomically. Acquires
// Graph.snapshotMu.RLock for the duration of the body read so concurrent
// Insert/InsertBatch don't mutate the slices mid-serialization.
//
// Atomicity: writes to outPath.tmp, fsyncs the file and the parent
// directory, then renames. SIGKILL anywhere before the rename leaves
// outPath unchanged (next save retries cleanly).
func SaveHNSWSnapshot(g *Graph, w *WAL, outPath string) error {
	if g == nil {
		return errors.New("SaveHNSWSnapshot: nil graph")
	}
	if w == nil {
		return errors.New("SaveHNSWSnapshot: nil wal")
	}

	hnswPersistMu.Lock()
	defer hnswPersistMu.Unlock()

	// [F2 from DS audit] Read-side lock on the graph for the duration of
	// serialization. Concurrent inserts block until we release.
	g.snapshotMu.RLock()
	defer g.snapshotMu.RUnlock()

	header, err := buildSnapshotHeader(g, w)
	if err != nil {
		return err
	}

	tmpPath := outPath + ".tmp"
	if err := writeSnapshotFile(tmpPath, header, g); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		return fmt.Errorf("SaveHNSWSnapshot: rename: %w", err)
	}
	// fsync parent dir so the rename is durable.
	if dir, derr := os.Open(filepath.Dir(outPath)); derr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// buildSnapshotHeader validates graph invariants and probes the WAL
// for stale-guard fields, returning the populated header. Extracted
// from SaveHNSWSnapshot to keep CC≤15. [ÉPICA 149]
func buildSnapshotHeader(g *Graph, w *WAL) (HNSWSnapshotHeader, error) {
	nodeCount := uint32(len(g.Nodes))
	edgeCount := uint64(len(g.Edges))
	vectorCount := uint64(len(g.Vectors))
	if g.VecDim < 0 || g.VecDim > int(maxSnapshotDim) {
		return HNSWSnapshotHeader{}, fmt.Errorf("SaveHNSWSnapshot: VecDim=%d out of range [0..%d]", g.VecDim, maxSnapshotDim)
	}
	if uint64(nodeCount)*uint64(g.VecDim) != vectorCount {
		return HNSWSnapshotHeader{}, fmt.Errorf("SaveHNSWSnapshot: vector slab length mismatch nodes=%d dim=%d vectors=%d",
			nodeCount, g.VecDim, vectorCount)
	}
	// [F1 from DS audit + v2 stale-guard fix] Probe per-bucket KeyN counts
	// from the already-open WAL. Bucket.Stats() walks the bucket tree
	// which is O(pages) — fast enough (microseconds for typical graphs).
	// Does NOT open a second bolt handle (deadlock).
	var nodeKeyN, edgeKeyN, vectorKeyN uint64
	if err := w.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(bucketNodes); b != nil {
			nodeKeyN = uint64(b.Stats().KeyN)
		}
		if b := tx.Bucket(bucketEdges); b != nil {
			edgeKeyN = uint64(b.Stats().KeyN)
		}
		if b := tx.Bucket(bucketVectors); b != nil {
			vectorKeyN = uint64(b.Stats().KeyN)
		}
		return nil
	}); err != nil {
		return HNSWSnapshotHeader{}, fmt.Errorf("SaveHNSWSnapshot: probe wal bucket counts: %w", err)
	}
	// [BUG-FIX 2026-05-13] During SIGTERM shutdown, w.db.Path() may return
	// empty due to race with deferred wal.Close() (boot_helpers.go:111 closes
	// the WAL via defer cleanupRAG, while the SIGTERM goroutine at
	// cmd/neo-mcp/main.go:528 simultaneously calls SaveHNSWSnapshot). When
	// bbolt is closed, db.Path() returns "" → os.Stat("") fails → snapshot
	// save fails → next boot does cold rebuild (sees node-count mismatch).
	// Tolerate this: WALFileSize=0 means "unknown" — IsHNSWSnapshotStale
	// skips the file-size gate when this sentinel is present, falling back
	// to NodeKeyN/EdgeKeyN/VectorKeyN which are the semantic stale-detectors.
	var walSize int64
	if walFileInfo, err := os.Stat(w.db.Path()); err == nil {
		walSize = walFileInfo.Size()
	}
	return HNSWSnapshotHeader{
		SchemaVersion: HNSWSnapshotSchemaVersion,
		CanonicalDim:  uint16(g.VecDim),
		NodeCount:     nodeCount,
		EdgeCount:     edgeCount,
		VectorCount:   vectorCount,
		BuildAtUnix:   nowUnix(),
		WALFileSize:   walSize, // 0 = unknown (shutdown race); see comment above
		NodeKeyN:      nodeKeyN,
		EdgeKeyN:      edgeKeyN,
		VectorKeyN:    vectorKeyN,
	}, nil
}

// writeSnapshotFile creates the .tmp file, writes header+body, fsyncs.
// Caller is responsible for the rename. Extracted from SaveHNSWSnapshot
// to keep CC≤15 and to make the I/O sequence testable in isolation.
// [ÉPICA 149]
func writeSnapshotFile(tmpPath string, header HNSWSnapshotHeader, g *Graph) error {
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644) //nolint:gosec // G304-WORKSPACE-CANON: tmpPath derived from outPath which is cfg-joined with workspace
	if err != nil {
		return fmt.Errorf("SaveHNSWSnapshot: open tmp: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()

	hasher, _ := blake2b.New256(nil)
	mw := io.MultiWriter(f, hasher)

	headerBytes := encodeHNSWSnapshotHeader(header)
	if _, err := mw.Write(headerBytes[:hnswSnapshotChecksumOffset]); err != nil {
		return fmt.Errorf("SaveHNSWSnapshot: write header prefix: %w", err)
	}
	// Reserve checksum bytes — written separately after body hashing.
	// These zeros do NOT enter the hash (mw only got the prefix above).
	if _, err := f.Write(make([]byte, hnswSnapshotChecksumLen)); err != nil {
		return fmt.Errorf("SaveHNSWSnapshot: reserve checksum: %w", err)
	}
	if err := writeNodesBody(mw, g.Nodes); err != nil {
		return err
	}
	if err := writeEdgesBody(mw, g.Edges); err != nil {
		return err
	}
	if err := writeVectorsBody(mw, g.Vectors); err != nil {
		return err
	}
	// Patch checksum at the reserved offset.
	if _, err := f.WriteAt(hasher.Sum(nil), int64(hnswSnapshotChecksumOffset)); err != nil {
		return fmt.Errorf("SaveHNSWSnapshot: write checksum: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("SaveHNSWSnapshot: fsync tmp: %w", err)
	}
	closed = true
	if err := f.Close(); err != nil {
		return fmt.Errorf("SaveHNSWSnapshot: close tmp: %w", err)
	}
	return nil
}

// nowUnix is a var so tests can shim time.
var nowUnix = func() int64 {
	return time.Now().Unix()
}

// LoadHNSWSnapshot reads + validates a snapshot from path. On success,
// returns *HNSWSnapshot with a fully-populated Graph. On any validation
// failure, returns ErrHNSWSnapshotMissing | ErrHNSWSnapshotCorrupt |
// ErrHNSWSnapshotSchemaTooNew so the caller can fall back to cold load.
func LoadHNSWSnapshot(path string) (*HNSWSnapshot, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304-WORKSPACE-CANON: path comes from cfg, joined with workspace
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrHNSWSnapshotMissing
		}
		return nil, fmt.Errorf("LoadHNSWSnapshot: read %s: %w", path, err)
	}
	if len(data) < hnswSnapshotHeaderSize {
		return nil, fmt.Errorf("%w: file too small (%d bytes)", ErrHNSWSnapshotCorrupt, len(data))
	}
	if string(data[0:4]) != HNSWSnapshotMagic {
		return nil, fmt.Errorf("%w: bad magic %q", ErrHNSWSnapshotCorrupt, string(data[0:4]))
	}
	h := decodeHNSWSnapshotHeader(data[:hnswSnapshotHeaderSize])
	if h.SchemaVersion > HNSWSnapshotSchemaVersion {
		return nil, fmt.Errorf("%w: stored=%d known=%d", ErrHNSWSnapshotSchemaTooNew, h.SchemaVersion, HNSWSnapshotSchemaVersion)
	}
	if h.SchemaVersion != HNSWSnapshotSchemaVersion {
		return nil, fmt.Errorf("%w: schema=%d expected=%d (cold rebuild)", ErrHNSWSnapshotCorrupt, h.SchemaVersion, HNSWSnapshotSchemaVersion)
	}
	if err := validateSnapshotBounds(h); err != nil {
		return nil, err
	}
	expectedSize := int64(hnswSnapshotHeaderSize) +
		int64(uint64(h.NodeCount)*nodeOnDiskSize) +
		int64(h.EdgeCount*4) +
		int64(h.VectorCount*4)
	if int64(len(data)) != expectedSize {
		return nil, fmt.Errorf("%w: file size %d != expected %d (header counts mismatch body)",
			ErrHNSWSnapshotCorrupt, len(data), expectedSize)
	}

	// Verify checksum: blake2b(header[:checksumOffset] || body).
	stored := make([]byte, hnswSnapshotChecksumLen)
	copy(stored, data[hnswSnapshotChecksumOffset:hnswSnapshotChecksumOffset+hnswSnapshotChecksumLen])
	hasher, _ := blake2b.New256(nil)
	hasher.Write(data[:hnswSnapshotChecksumOffset])
	hasher.Write(data[hnswSnapshotHeaderSize:])
	computed := hasher.Sum(nil)
	if !equalBytes(stored, computed) {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrHNSWSnapshotCorrupt)
	}

	g, err := decodeHNSWBody(h, data[hnswSnapshotHeaderSize:])
	if err != nil {
		return nil, err
	}
	return &HNSWSnapshot{Header: h, Graph: g}, nil
}

// IsHNSWSnapshotStale compares the snapshot header's stale-guard fields
// against the live WAL. Returns (stale, reason) — reason is a short
// human-readable string for boot logging.
//
// [F1 from DS audit] Takes the already-open *WAL — does NOT bolt.Open
// a second handle.
//
// [v2 stale-guard rationale, 2026-05-02] Uses per-bucket KeyN counts
// instead of bbolt's Tx.ID. Empirical observation: every WAL.Update —
// including no-op writes from sanitizeBucket when no corrupt entries
// were found — bumps Tx.ID. That meant snapshot was reported stale on
// EVERY boot regardless of semantic data change. KeyN only changes on
// real Insert/InsertBatch (counts go up) or sanitizer purges (counts
// go down). File-size check stays as the cheap first gate.
func IsHNSWSnapshotStale(w *WAL, h *HNSWSnapshotHeader) (bool, string) {
	if w == nil || h == nil {
		return true, "nil wal or header"
	}
	walPath := w.db.Path()
	info, err := os.Stat(walPath)
	if err != nil {
		return true, fmt.Sprintf("stat wal failed: %v", err)
	}
	// [BUG-FIX 2026-05-13] WALFileSize=0 is the "unknown" sentinel emitted by
	// buildSnapshotHeader when shutdown raced db.Close() (see comment there).
	// Skip the size gate when sentinel present — the NodeKeyN/EdgeKeyN/
	// VectorKeyN checks below detect semantic stalessness without needing the
	// raw file size.
	if h.WALFileSize != 0 && info.Size() != h.WALFileSize {
		return true, fmt.Sprintf("wal size %d != snapshot %d", info.Size(), h.WALFileSize)
	}
	var nodeKeyN, edgeKeyN, vectorKeyN uint64
	if err := w.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(bucketNodes); b != nil {
			nodeKeyN = uint64(b.Stats().KeyN)
		}
		if b := tx.Bucket(bucketEdges); b != nil {
			edgeKeyN = uint64(b.Stats().KeyN)
		}
		if b := tx.Bucket(bucketVectors); b != nil {
			vectorKeyN = uint64(b.Stats().KeyN)
		}
		return nil
	}); err != nil {
		return true, fmt.Sprintf("read wal bucket counts: %v", err)
	}
	if nodeKeyN != h.NodeKeyN {
		return true, fmt.Sprintf("nodes %d != snapshot %d", nodeKeyN, h.NodeKeyN)
	}
	if edgeKeyN != h.EdgeKeyN {
		return true, fmt.Sprintf("edges %d != snapshot %d", edgeKeyN, h.EdgeKeyN)
	}
	if vectorKeyN != h.VectorKeyN {
		return true, fmt.Sprintf("vectors %d != snapshot %d", vectorKeyN, h.VectorKeyN)
	}
	return false, "fresh"
}

// encodeHNSWSnapshotHeader writes the 100-byte header. ChecksumBlake2b
// region is left as zero — caller fills it after computing.
//
// On-disk layout (v2):
//
//	0..4    magic "HNSW"
//	4..6    schema version
//	6..8    canonical dim
//	8..12   node count
//	12..20  edge count
//	20..28  vector count
//	28..36  build_at_unix
//	36..44  wal_file_size
//	44..52  node_key_n     ← v2: was WALTxID in v1
//	52..60  edge_key_n     ← v2: was reserved in v1
//	60..68  vector_key_n   ← v2: was reserved in v1
//	68..100 checksum_blake2b
func encodeHNSWSnapshotHeader(h HNSWSnapshotHeader) [hnswSnapshotHeaderSize]byte {
	var buf [hnswSnapshotHeaderSize]byte
	copy(buf[0:4], HNSWSnapshotMagic)
	binary.LittleEndian.PutUint16(buf[4:6], h.SchemaVersion)
	binary.LittleEndian.PutUint16(buf[6:8], h.CanonicalDim)
	binary.LittleEndian.PutUint32(buf[8:12], h.NodeCount)
	binary.LittleEndian.PutUint64(buf[12:20], h.EdgeCount)
	binary.LittleEndian.PutUint64(buf[20:28], h.VectorCount)
	binary.LittleEndian.PutUint64(buf[28:36], uint64(h.BuildAtUnix))
	binary.LittleEndian.PutUint64(buf[36:44], uint64(h.WALFileSize))
	binary.LittleEndian.PutUint64(buf[44:52], h.NodeKeyN)
	binary.LittleEndian.PutUint64(buf[52:60], h.EdgeKeyN)
	binary.LittleEndian.PutUint64(buf[60:68], h.VectorKeyN)
	// 68..100 checksum, written separately
	return buf
}

// decodeHNSWSnapshotHeader is the symmetric reader.
func decodeHNSWSnapshotHeader(buf []byte) HNSWSnapshotHeader {
	return HNSWSnapshotHeader{
		SchemaVersion: binary.LittleEndian.Uint16(buf[4:6]),
		CanonicalDim:  binary.LittleEndian.Uint16(buf[6:8]),
		NodeCount:     binary.LittleEndian.Uint32(buf[8:12]),
		EdgeCount:     binary.LittleEndian.Uint64(buf[12:20]),
		VectorCount:   binary.LittleEndian.Uint64(buf[20:28]),
		BuildAtUnix:   int64(binary.LittleEndian.Uint64(buf[28:36])),
		WALFileSize:   int64(binary.LittleEndian.Uint64(buf[36:44])),
		NodeKeyN:      binary.LittleEndian.Uint64(buf[44:52]),
		EdgeKeyN:      binary.LittleEndian.Uint64(buf[52:60]),
		VectorKeyN:    binary.LittleEndian.Uint64(buf[60:68]),
	}
}

// validateSnapshotBounds enforces sanity caps to defuse DoS. [F3]
func validateSnapshotBounds(h HNSWSnapshotHeader) error {
	if h.CanonicalDim > maxSnapshotDim {
		return fmt.Errorf("%w: dim %d > max %d", ErrHNSWSnapshotCorrupt, h.CanonicalDim, maxSnapshotDim)
	}
	if h.NodeCount > maxSnapshotNodeCount {
		return fmt.Errorf("%w: nodes %d > max %d", ErrHNSWSnapshotCorrupt, h.NodeCount, maxSnapshotNodeCount)
	}
	if h.EdgeCount > maxSnapshotEdgeCount {
		return fmt.Errorf("%w: edges %d > max %d", ErrHNSWSnapshotCorrupt, h.EdgeCount, maxSnapshotEdgeCount)
	}
	if h.VectorCount > maxSnapshotVectorCount {
		return fmt.Errorf("%w: vectors %d > max %d", ErrHNSWSnapshotCorrupt, h.VectorCount, maxSnapshotVectorCount)
	}
	expectedVectors := uint64(h.NodeCount) * uint64(h.CanonicalDim)
	if h.VectorCount != expectedVectors {
		return fmt.Errorf("%w: vector count %d != nodes×dim %d", ErrHNSWSnapshotCorrupt, h.VectorCount, expectedVectors)
	}
	return nil
}

// writeNodesBody serializes nodes to w in 16-byte explicit LE format.
// [ÉPICA 149 perf] Pre-encodes all nodes into a single buffer then issues
// one Write — turns 1M Write(4-byte) calls into one Write(16MB) call.
// Original loop ran at ~5 MB/s; bulk write hits disk bandwidth.
func writeNodesBody(w io.Writer, nodes []Node) error {
	if len(nodes) == 0 {
		return nil
	}
	buf := make([]byte, len(nodes)*nodeOnDiskSize)
	off := 0
	for i := range nodes {
		binary.LittleEndian.PutUint64(buf[off:off+8], nodes[i].DocID)
		binary.LittleEndian.PutUint32(buf[off+8:off+12], nodes[i].EdgesOffset)
		binary.LittleEndian.PutUint16(buf[off+12:off+14], nodes[i].EdgesLength)
		buf[off+14] = nodes[i].Layer
		buf[off+15] = 0 // reserved
		off += nodeOnDiskSize
	}
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("write nodes (%d): %w", len(nodes), err)
	}
	return nil
}

// writeEdgesBody serializes edges (uint32 LE each). Bulk write — see
// writeNodesBody comment for the perf rationale.
func writeEdgesBody(w io.Writer, edges []uint32) error {
	if len(edges) == 0 {
		return nil
	}
	buf := make([]byte, len(edges)*4)
	for i, e := range edges {
		binary.LittleEndian.PutUint32(buf[i*4:i*4+4], e)
	}
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("write edges (%d): %w", len(edges), err)
	}
	return nil
}

// writeVectorsBody serializes vectors (float32 LE each via Float32bits).
// Bulk write — for a typical 1M nodes × 768 dim graph this is ~3 GB, so
// allocating a buffer of that size up front would double-up RAM. Stream
// in chunks of 1M floats (4 MB) instead. Still 1000× fewer Write() calls
// than the per-element loop, hits disk bandwidth.
func writeVectorsBody(w io.Writer, vectors []float32) error {
	if len(vectors) == 0 {
		return nil
	}
	const chunkFloats = 1 << 20 // 1M floats = 4 MB per Write call
	buf := make([]byte, chunkFloats*4)
	for start := 0; start < len(vectors); start += chunkFloats {
		end := min(start+chunkFloats, len(vectors))
		used := buf[:(end-start)*4]
		for i, v := range vectors[start:end] {
			binary.LittleEndian.PutUint32(used[i*4:i*4+4], math.Float32bits(v))
		}
		if _, err := w.Write(used); err != nil {
			return fmt.Errorf("write vectors[%d:%d]: %w", start, end, err)
		}
	}
	return nil
}

// decodeHNSWBody reads nodes/edges/vectors from the body slice and
// returns a fully-populated Graph.
//
// [ÉPICA 149.O / DS audit M8] Vectors decode is parallelized across N
// goroutines (default GOMAXPROCS, capped at 8). Each goroutine writes
// to a disjoint slab of the pre-allocated vectors slice — no shared
// state, no lock. For a 1M-node × 768-dim graph (~3 GB body) this
// drops decode time from ~2s to ~0.4s on an 8-core machine. Nodes
// (16 MB) and edges (4 MB) are left sequential — their decode time
// is dwarfed by goroutine spawn overhead at those sizes.
func decodeHNSWBody(h HNSWSnapshotHeader, body []byte) (*Graph, error) {
	nodeBytes := int(uint64(h.NodeCount) * nodeOnDiskSize)
	edgeBytes := int(h.EdgeCount * 4)
	vectorBytes := int(h.VectorCount * 4)
	if nodeBytes+edgeBytes+vectorBytes != len(body) {
		return nil, fmt.Errorf("%w: body size mismatch (n=%d e=%d v=%d total=%d)",
			ErrHNSWSnapshotCorrupt, nodeBytes, edgeBytes, vectorBytes, len(body))
	}

	nodes := make([]Node, h.NodeCount)
	off := 0
	for i := range nodes {
		nodes[i].DocID = binary.LittleEndian.Uint64(body[off : off+8])
		nodes[i].EdgesOffset = binary.LittleEndian.Uint32(body[off+8 : off+12])
		nodes[i].EdgesLength = binary.LittleEndian.Uint16(body[off+12 : off+14])
		nodes[i].Layer = body[off+14]
		off += nodeOnDiskSize
	}

	edges := make([]uint32, h.EdgeCount)
	for i := range edges {
		edges[i] = binary.LittleEndian.Uint32(body[off : off+4])
		off += 4
	}

	vectors := make([]float32, h.VectorCount)
	decodeVectorsParallel(body[off:off+vectorBytes], vectors)

	g := &Graph{
		Nodes:   nodes,
		Edges:   edges,
		Vectors: vectors,
		VecDim:  int(h.CanonicalDim),
	}
	return g, nil
}

// decodeVectorsParallel splits the float32 decode work across goroutines.
// Each goroutine handles a contiguous range of the destination slice; no
// goroutine reads or writes another's slab so we don't need locks. Below
// the parallelizeThreshold the cost of spawning goroutines exceeds the
// per-element work; in that case decode sequentially.
//
// [ÉPICA 149.O / DS audit M8] Saves ~1.5s on a 1M-node × 768-dim graph
// (~770M float32 elements) on a typical 8-core box. Independent of any
// other 149 path; safe to ship without 149.J.
func decodeVectorsParallel(src []byte, dst []float32) {
	const parallelizeThreshold = 1 << 20 // 1M floats — below this, serial wins
	n := len(dst)
	if n < parallelizeThreshold {
		decodeVectorsRange(src, dst, 0, n)
		return
	}
	workers := min(runtime.GOMAXPROCS(0), 8)
	if workers < 2 {
		decodeVectorsRange(src, dst, 0, n)
		return
	}
	chunk := (n + workers - 1) / workers
	var wg sync.WaitGroup
	for w := range workers {
		start := w * chunk
		if start >= n {
			break
		}
		end := min(start+chunk, n)
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			decodeVectorsRange(src, dst, start, end)
		}(start, end)
	}
	wg.Wait()
}

// decodeVectorsRange decodes dst[start:end] from the corresponding byte
// range in src. Each float32 is 4 bytes LE.
func decodeVectorsRange(src []byte, dst []float32, start, end int) {
	for i := start; i < end; i++ {
		off := i * 4
		dst[i] = math.Float32frombits(binary.LittleEndian.Uint32(src[off : off+4]))
	}
}

// equalBytes is a constant-time comparison wrapper. blake2b output is
// not security-critical here (integrity, not auth), so this is just
// defensive — saves us from importing crypto/subtle for a single call.
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
