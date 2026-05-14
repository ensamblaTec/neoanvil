// cmd/../pkg/rag/wal_compact.go — offline BoltDB compaction for the HNSW WAL.
//
// [D5 / technical_debt 2026-05-14] BoltDB files only ever grow: deleted/freed
// pages are reused in-place but the file's on-disk high-water mark is never
// lowered. A long-lived workspace's hnsw.db accumulates churn (directive sync,
// session-state, SaveDocMeta/Scar/Weights, re-ingestion) until boot exceeds the
// Nexus startup timeout — strategos hit 5.3 GB. There is no in-place compaction
// in BoltDB; the only fix is to copy live pages into a fresh file.
//
// CompactWAL does that copy offline (no live *bbolt.DB handle is mutated): it
// opens the source read-only, writes a fresh temp DB, and atomically renames it
// over the original. A crash or disk-full at any point before the rename leaves
// the original file untouched.

package rag

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/bbolt"
)

// compactTxMaxBytes bounds a single bbolt.Compact copy transaction. A multi-GB
// source compacted with an unbounded (0) txn would force one giant in-memory
// copy; 64 MiB commits incrementally and keeps the heap flat.
const compactTxMaxBytes int64 = 64 << 20

// compactLockTimeout is how long we wait for the exclusive bbolt file lock
// before giving up — a non-zero wait that still fails fast when the workspace's
// neo-mcp is holding the file open.
const compactLockTimeout = 2 * time.Second

// CompactWAL performs an offline BoltDB compaction of the WAL file at path and
// returns the file size before and after. The workspace's neo-mcp must NOT hold
// the file open: the exclusive bbolt lock makes a running workspace an error
// here (caught via compactLockTimeout).
//
// Crash-safety: the source is only ever read; the compacted copy is built in a
// sibling temp file and swapped in with a single os.Rename (atomic on POSIX
// within one filesystem). Any failure before the rename — including SIGKILL,
// disk-full, or a lock timeout — leaves the original file fully intact and
// removes the partial temp file.
func CompactWAL(path string) (oldSize, newSize int64, err error) {
	info, statErr := os.Stat(path)
	if statErr != nil {
		return 0, 0, fmt.Errorf("compact wal: stat %s: %w", path, statErr)
	}
	oldSize = info.Size()

	src, openErr := bbolt.Open(path, 0600, &bbolt.Options{Timeout: compactLockTimeout, ReadOnly: true})
	if openErr != nil {
		return oldSize, 0, fmt.Errorf("compact wal: open source (is the workspace running?): %w", openErr)
	}

	tmpFile, tmpErr := os.CreateTemp(filepath.Dir(path), "hnsw-compact-*.tmp")
	if tmpErr != nil {
		_ = src.Close()
		return oldSize, 0, fmt.Errorf("compact wal: create temp: %w", tmpErr)
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()

	dst, dstErr := bbolt.Open(tmpPath, 0600, &bbolt.Options{Timeout: compactLockTimeout})
	if dstErr != nil {
		_ = src.Close()
		_ = os.Remove(tmpPath)
		return oldSize, 0, fmt.Errorf("compact wal: open temp dest: %w", dstErr)
	}

	if compactErr := bbolt.Compact(dst, src, compactTxMaxBytes); compactErr != nil {
		_ = dst.Close()
		_ = src.Close()
		_ = os.Remove(tmpPath)
		return oldSize, 0, fmt.Errorf("compact wal: copy pages: %w", compactErr)
	}
	if closeErr := dst.Close(); closeErr != nil {
		_ = src.Close()
		_ = os.Remove(tmpPath)
		return oldSize, 0, fmt.Errorf("compact wal: close temp dest: %w", closeErr)
	}
	// Release the source lock before the rename so the path is free to replace.
	if closeErr := src.Close(); closeErr != nil {
		_ = os.Remove(tmpPath)
		return oldSize, 0, fmt.Errorf("compact wal: close source: %w", closeErr)
	}
	if renameErr := os.Rename(tmpPath, path); renameErr != nil {
		_ = os.Remove(tmpPath)
		return oldSize, 0, fmt.Errorf("compact wal: atomic rename: %w", renameErr)
	}

	newInfo, statErr2 := os.Stat(path)
	if statErr2 != nil {
		return oldSize, 0, fmt.Errorf("compact wal: stat after rename: %w", statErr2)
	}
	return oldSize, newInfo.Size(), nil
}

// CompactWALIfOversized compacts the WAL only when its on-disk size exceeds
// thresholdMB. A thresholdMB <= 0 disables the check entirely (returns
// compacted=false, nil err) — that is the operator opt-out. A missing file
// (fresh workspace) is also a no-op, not an error.
//
// Returns compacted=true only when a compaction actually ran and succeeded.
func CompactWALIfOversized(path string, thresholdMB int) (compacted bool, oldSize, newSize int64, err error) {
	if thresholdMB <= 0 {
		return false, 0, 0, nil
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return false, 0, 0, nil // fresh workspace — nothing to compact
		}
		return false, 0, 0, fmt.Errorf("compact wal: stat %s: %w", path, statErr)
	}
	threshold := int64(thresholdMB) << 20
	if info.Size() < threshold {
		return false, info.Size(), info.Size(), nil
	}
	oldSize, newSize, err = CompactWAL(path)
	return err == nil, oldSize, newSize, err
}
