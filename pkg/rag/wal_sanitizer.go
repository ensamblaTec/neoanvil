package rag

import (
	"encoding/json"
	"fmt"
	"log"

	"go.etcd.io/bbolt"
)

// validatorFn returns true if the value is considered valid for the bucket.
// nil → empty values are also allowed (rare).
type validatorFn func(v []byte) bool

// jsonValidator accepts any well-formed JSON.
func jsonValidator(v []byte) bool { return json.Valid(v) }

// nodeBinaryValidator accepts exactly 16-byte Node structs (see wal.go:Insert).
func nodeBinaryValidator(v []byte) bool { return len(v) == 16 }

// uint32ArrayValidator accepts any slice whose length is a multiple of 4.
// Used for edges ([]uint32), vectors ([]float32), and weights ([]float32).
func uint32ArrayValidator(v []byte) bool { return len(v)%4 == 0 }

// bucketValidators maps each BoltDB bucket to the correct validator for its payload.
// [SRE-WAL-SANITIZER-V6.3] Binary buckets are NOT JSON — validating them with
// json.Valid() would purge every single entry on boot, erasing the HNSW graph.
var bucketValidators = map[string]validatorFn{
	"hnsw_nodes":      nodeBinaryValidator,    // 16-byte struct
	"hnsw_edges":      uint32ArrayValidator,   // []uint32
	"hnsw_vectors":    uint32ArrayValidator,   // []float32 bits
	"hnsw_weights":    uint32ArrayValidator,   // []float32 bits (MLP W1/W2)
	"hnsw_docs":       jsonValidator,          // JSON DocMeta
	"hnsw_scars":      jsonValidator,          // JSON []string
	"hnsw_directives": jsonValidator,          // JSON Directive
}

// nonHNSWBuckets are the metadata buckets sanitized with the simple
// per-bucket validator (no cascade awareness needed). SanitizeWAL processes
// these after the HNSW-specific cascade pass. SanitizeWALMetadataOnly uses
// metadataOnlyBuckets (subset without weights). [144.F]
var nonHNSWBuckets = [][]byte{
	bucketDocs, bucketScars,
	bucketWeights, bucketDirectives, bucketHnswMeta,
}

// metadataOnlyBuckets is the lightweight subset scanned on the
// HNSW fast-boot path. Excludes nodes/edges/vectors which dominate the
// page-fault cost on a multi-GB WAL — the snapshot's bit-exact graph
// already represents valid state for those buckets, so re-validation
// at boot adds 10-15s of disk IO with no new safety. [ÉPICA 149.J]
//
// The big buckets are still validated by the deferred background pass
// scheduled ~30s post-boot via runBackgroundSanitize() so corruption
// doesn't accumulate silently across sessions.
var metadataOnlyBuckets = [][]byte{
	bucketDocs, bucketScars,
	bucketDirectives, bucketHnswMeta,
}

// SanitizeWAL scans every BoltDB bucket and purges entries that fail their
// bucket-specific validator. Safe to call at startup before loading the graph —
// idempotent and non-destructive for valid entries. [SRE-31.1.3]
//
// HNSW buckets (nodes/edges/vectors) are sanitized together with cascade
// awareness: deleting a corrupt edge or vector entry also removes the
// corresponding node entry (same 4-byte key) from hnsw_nodes. Without this
// cascade, stale EdgesOffset/EdgesLength in a surviving node entry would
// cause LoadGraph to read garbage or panic on out-of-range accesses. [144.F]
func (wal *WAL) SanitizeWAL() (int, error) {
	hnswPurged, err := wal.sanitizeHNSWWithCascade()
	if err != nil {
		return hnswPurged, err
	}
	metaPurged, err := wal.sanitizeBuckets(nonHNSWBuckets)
	return hnswPurged + metaPurged, err
}

// SanitizeWALMetadataOnly runs the validator only over the small
// metadata buckets (docs/deps/scars/directives/meta), skipping the
// gigabyte-scale nodes/edges/vectors. Designed for the HNSW fast-boot
// path where the snapshot has already validated graph state and we
// want to avoid the 10-15s page-fault storm of iterating big buckets.
// Caller is responsible for scheduling a deferred full SanitizeWAL
// (runBackgroundSanitize). [ÉPICA 149.J]
func (wal *WAL) SanitizeWALMetadataOnly() (int, error) {
	return wal.sanitizeBuckets(metadataOnlyBuckets)
}

// sanitizeBuckets is the shared implementation used by SanitizeWAL and
// SanitizeWALMetadataOnly. Iterates the supplied bucket list, applying
// per-bucket validators when present.
func (wal *WAL) sanitizeBuckets(buckets [][]byte) (int, error) {
	if wal == nil || wal.db == nil {
		return 0, nil
	}
	totalPurged := 0
	for _, bucket := range buckets {
		validator, ok := bucketValidators[string(bucket)]
		if !ok {
			log.Printf("[SRE-WAL-SANITIZER] No validator for bucket %s — skipping.", bucket)
			continue
		}
		purged, err := wal.sanitizeBucket(bucket, validator)
		if err != nil {
			log.Printf("[SRE-WAL-SANITIZER] Error scanning bucket %s: %v", bucket, err)
			continue
		}
		totalPurged += purged
	}
	if totalPurged > 0 {
		log.Printf("[SRE-WAL-SANITIZER] Purged %d corrupted entries from WAL.", totalPurged)
	}
	return totalPurged, nil
}

// sanitizeBucket scans a single bucket, collecting and deleting keys whose
// values fail the supplied validator (plus any zero-length values).
func (wal *WAL) sanitizeBucket(bucketName []byte, isValid validatorFn) (int, error) {
	var corruptedKeys [][]byte

	// Read pass: collect corrupted keys (read-only tx).
	viewErr := wal.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			if len(v) == 0 || !isValid(v) {
				keyCopy := make([]byte, len(k))
				copy(keyCopy, k)
				corruptedKeys = append(corruptedKeys, keyCopy)
				log.Printf("[SRE-WAL-SANITIZER] Corrupted entry in bucket %s key %x — purging.", bucketName, k)
			}
			return nil
		})
	})
	if viewErr != nil {
		return 0, fmt.Errorf("view scan of bucket %s: %w", bucketName, viewErr)
	}

	if len(corruptedKeys) == 0 {
		return 0, nil
	}

	// Write pass: delete corrupted keys.
	writeErr := wal.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		if b == nil {
			return nil
		}
		for _, k := range corruptedKeys {
			if err := b.Delete(k); err != nil {
				return fmt.Errorf("delete key %x: %w", k, err)
			}
		}
		return nil
	})
	if writeErr != nil {
		return 0, fmt.Errorf("purge of bucket %s: %w", bucketName, writeErr)
	}

	return len(corruptedKeys), nil
}

// sanitizeHNSWWithCascade scans the three HNSW buckets (nodes/edges/vectors)
// together. Any key found corrupt in edges or vectors is also removed from the
// other two buckets in a single atomic transaction — prevents LoadGraph from
// following a surviving node's stale EdgesOffset/EdgesLength into a hole left
// by the deleted edge entry (audit finding 144.F).
//
// Returns the number of distinct corrupt node-IDs purged. A single corrupt
// edge entry removes up to three BoltDB keys (node + edge + vector).
func (wal *WAL) sanitizeHNSWWithCascade() (int, error) {
	if wal == nil || wal.db == nil {
		return 0, nil
	}
	// corruptIDs accumulates every 4-byte key that must be purged across all
	// three HNSW buckets. Using string(k) as map key makes deduplication safe
	// without storing []byte slices that share underlying BoltDB pages.
	corruptIDs := make(map[string]struct{})

	viewErr := wal.db.View(func(tx *bbolt.Tx) error {
		scanBucket := func(b *bbolt.Bucket, isValid validatorFn, bName string) {
			if b == nil {
				return
			}
			b.ForEach(func(k, v []byte) error { //nolint:errcheck // ForEach never returns err from our cb
				if len(v) == 0 || !isValid(v) {
					corruptIDs[string(k)] = struct{}{}
					log.Printf("[SRE-WAL-SANITIZER] Corrupted entry in bucket %s key %x — scheduling cascade delete.", bName, k)
				}
				return nil
			})
		}
		scanBucket(tx.Bucket(bucketEdges), uint32ArrayValidator, "hnsw_edges")
		scanBucket(tx.Bucket(bucketVectors), uint32ArrayValidator, "hnsw_vectors")
		scanBucket(tx.Bucket(bucketNodes), nodeBinaryValidator, "hnsw_nodes")
		return nil
	})
	if viewErr != nil {
		return 0, fmt.Errorf("sanitizeHNSWWithCascade view: %w", viewErr)
	}
	if len(corruptIDs) == 0 {
		return 0, nil
	}

	writeErr := wal.db.Update(func(tx *bbolt.Tx) error {
		for keyStr := range corruptIDs {
			key := []byte(keyStr)
			for _, bkt := range [][]byte{bucketNodes, bucketEdges, bucketVectors} {
				if b := tx.Bucket(bkt); b != nil {
					if err := b.Delete(key); err != nil {
						return fmt.Errorf("cascade delete key %x from %s: %w", key, bkt, err)
					}
				}
			}
		}
		return nil
	})
	if writeErr != nil {
		return 0, fmt.Errorf("sanitizeHNSWWithCascade write: %w", writeErr)
	}
	log.Printf("[SRE-WAL-SANITIZER] HNSW cascade purged %d corrupt node-groups.", len(corruptIDs))
	return len(corruptIDs), nil
}
