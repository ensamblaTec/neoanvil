// pkg/rag/shared_graph.go — Shared HNSW tier with advisory POSIX lock. [Épica 287]
//
// SharedGraph is a project-level flat vector store backed by BoltDB.  Unlike the
// per-workspace WAL (which indexes by HNSW nodeID uint32), SharedGraph stores
// DocMeta + raw vectors keyed by docID uint64 in two dedicated buckets
// ("shared_docs", "shared_vecs").  This avoids nodeID collisions across workspaces
// and lets the shared tier be rebuilt as a fresh HNSW graph by any reader.
//
// Cross-process write exclusion is provided by a companion .lock file via
// syscall.Flock (advisory, non-blocking).  Readers never lock.
package rag

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"go.etcd.io/bbolt"
)

var (
	sharedBucketDocs = []byte("shared_docs") // docID(uint64) → JSON DocMeta
	sharedBucketVecs = []byte("shared_vecs") // docID(uint64) → []float32 bytes
)

// ErrLockBusy is returned by TryLock when another process holds the exclusive lock.
var ErrLockBusy = errors.New("shared_graph: lock busy")

// SharedGraph is a project-level flat vector store protected by a POSIX advisory
// lock.  Safe for concurrent use within a single process. [287.A]
type SharedGraph struct {
	mu       sync.Mutex
	dbPath   string
	lockPath string
	lockFD   *os.File
	db       *bbolt.DB
	held     bool
	readOnly bool // [314.B] true when opened as non-coordinator (read-only BoltDB)
}

// OpenSharedGraph opens (or creates) the shared BoltDB at dbPath. [287.A / 302.B]
// Does NOT acquire a write lock — call TryLock before MergeFrom.
// Uses a non-blocking open with a short per-attempt timeout plus an external
// retry loop (5 attempts, 100→200→400→800→1600ms base, ±20% jitter) to handle
// the race when multiple neo-mcp children boot simultaneously.
func OpenSharedGraph(dbPath string) (*SharedGraph, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		return nil, fmt.Errorf("shared_graph: mkdir: %w", err)
	}
	lockPath := dbPath + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304-DIR-WALK: lockPath derived from controlled dbPath
	if err != nil {
		return nil, fmt.Errorf("shared_graph: open lock: %w", err)
	}

	// [302.B] Short per-attempt timeout (detect lock quickly) + external retry.
	const maxAttempts = 5
	baseDelays := [maxAttempts]time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1600 * time.Millisecond,
	}
	var db *bbolt.DB
	for i := range maxAttempts {
		db, err = bbolt.Open(dbPath, 0o600, &bbolt.Options{Timeout: 50 * time.Millisecond}) //nolint:gosec // G304-DIR-WALK: dbPath under process control
		if err == nil {
			break
		}
		// Both EWOULDBLOCK and bbolt timeout ("timeout") indicate lock contention.
		if i < maxAttempts-1 {
			base := baseDelays[i]
			jitter := time.Duration(rand.Int63n(int64(base) / 5)) //nolint:gosec // G404: non-crypto jitter for retry backoff
			delay := base + jitter
			log.Printf("[shared_graph] lock busy (attempt %d/%d) — retrying in %dms", i+1, maxAttempts, delay.Milliseconds())
			time.Sleep(delay)
		}
	}
	if err != nil {
		lf.Close()
		return nil, fmt.Errorf("shared_graph: open db: %w", err)
	}

	// Ensure buckets exist.
	if err := db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(sharedBucketDocs); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(sharedBucketVecs)
		return err
	}); err != nil {
		db.Close()
		lf.Close()
		return nil, fmt.Errorf("shared_graph: bucket init: %w", err)
	}
	return &SharedGraph{
		dbPath:   dbPath,
		lockPath: lockPath,
		lockFD:   lf,
		db:       db,
	}, nil
}

// OpenSharedGraphReadOnly opens an existing shared BoltDB in read-only mode. [314.B]
// Used by non-coordinator workspaces: BoltDB allows multiple concurrent readers
// without an exclusive write lock. The lock file is not created or required.
// Returns an error if the DB does not yet exist (coordinator must boot first).
func OpenSharedGraphReadOnly(dbPath string) (*SharedGraph, error) {
	db, err := bbolt.Open(dbPath, 0o600, &bbolt.Options{ //nolint:gosec // G304-DIR-WALK: dbPath under process control
		Timeout:  2 * time.Second,
		ReadOnly: true,
	})
	if err != nil {
		return nil, fmt.Errorf("shared_graph: open read-only: %w", err)
	}
	return &SharedGraph{
		dbPath:   dbPath,
		db:       db,
		readOnly: true,
	}, nil
}

// IsReadOnly reports whether this SharedGraph was opened in read-only mode. [314.B]
func (sg *SharedGraph) IsReadOnly() bool { return sg.readOnly }

// TryLock attempts a non-blocking POSIX exclusive flock. [287.B]
// Returns ErrLockBusy if another process holds LOCK_EX.
func (sg *SharedGraph) TryLock() error {
	sg.mu.Lock()
	defer sg.mu.Unlock()
	if sg.held {
		return nil
	}
	err := syscall.Flock(int(sg.lockFD.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrLockBusy
		}
		return fmt.Errorf("shared_graph: flock: %w", err)
	}
	sg.held = true
	return nil
}

// Unlock releases the exclusive flock. [287.B]
func (sg *SharedGraph) Unlock() {
	sg.mu.Lock()
	defer sg.mu.Unlock()
	if !sg.held {
		return
	}
	_ = syscall.Flock(int(sg.lockFD.Fd()), syscall.LOCK_UN)
	sg.held = false
}

// MergeFrom copies docs absent from the shared graph from srcWAL. [287.C]
// TryLock must be held. Deduplicates by docID (uint64).
// Returns the number of new docs added.
func (sg *SharedGraph) MergeFrom(srcWAL *WAL) (added int, err error) {
	sg.mu.Lock()
	held := sg.held
	sg.mu.Unlock()
	if !held {
		return 0, fmt.Errorf("shared_graph: MergeFrom called without holding the lock")
	}

	// Build presence set from shared graph.
	present := make(map[uint64]struct{})
	if rerr := sg.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(sharedBucketDocs)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, _ []byte) error {
			if len(k) == 8 {
				present[binary.LittleEndian.Uint64(k)] = struct{}{}
			}
			return nil
		})
	}); rerr != nil {
		return 0, fmt.Errorf("shared_graph: read present: %w", rerr)
	}

	// Build docID→nodeID map from source WAL so we can retrieve raw vectors.
	docToNode, err := srcWAL.loadDocIDToNodeID()
	if err != nil {
		return 0, fmt.Errorf("shared_graph: load docToNode: %w", err)
	}

	// Iterate source docs and copy missing entries.
	var srcDocs []struct {
		id   uint64
		meta DocMeta
		vec  []byte
	}
	if rerr := srcWAL.db.View(func(tx *bbolt.Tx) error {
		db := tx.Bucket(bucketDocs)
		vb := tx.Bucket(bucketVectors)
		if db == nil {
			return nil
		}
		return db.ForEach(func(k, v []byte) error {
			if len(k) != 8 {
				return nil
			}
			docID := binary.LittleEndian.Uint64(k)
			if _, ok := present[docID]; ok {
				return nil
			}
			var meta DocMeta
			if json.Unmarshal(v, &meta) != nil {
				return nil
			}
			nodeID, ok := docToNode[docID]
			if !ok {
				return nil
			}
			nk := make([]byte, 4)
			binary.LittleEndian.PutUint32(nk, nodeID)
			rawVec := vb.Get(nk)
			if len(rawVec) == 0 {
				return nil
			}
			cp := make([]byte, len(rawVec))
			copy(cp, rawVec)
			srcDocs = append(srcDocs, struct {
				id   uint64
				meta DocMeta
				vec  []byte
			}{docID, meta, cp})
			return nil
		})
	}); rerr != nil {
		return 0, fmt.Errorf("shared_graph: read src: %w", rerr)
	}

	if len(srcDocs) == 0 {
		return 0, nil
	}

	err = sg.db.Batch(func(tx *bbolt.Tx) error {
		db := tx.Bucket(sharedBucketDocs)
		vb := tx.Bucket(sharedBucketVecs)
		key := make([]byte, 8)
		for _, entry := range srcDocs {
			binary.LittleEndian.PutUint64(key, entry.id)
			val, merr := json.Marshal(entry.meta)
			if merr != nil {
				log.Printf("[287.C] skip docID %d: marshal: %v", entry.id, merr)
				continue
			}
			if perr := db.Put(key, val); perr != nil {
				return perr
			}
			if perr := vb.Put(key, entry.vec); perr != nil {
				return perr
			}
			added++
		}
		return nil
	})
	return added, err
}

// LoadVectors returns all (docID, DocMeta, []float32) triples from the shared graph. [287.D]
// Used to build a fresh in-memory HNSW from the shared tier.
func (sg *SharedGraph) LoadVectors() ([]uint64, []DocMeta, [][]float32, error) {
	var ids []uint64
	var metas []DocMeta
	var vecs [][]float32
	err := sg.db.View(func(tx *bbolt.Tx) error {
		db := tx.Bucket(sharedBucketDocs)
		vb := tx.Bucket(sharedBucketVecs)
		if db == nil {
			return nil
		}
		return db.ForEach(func(k, v []byte) error {
			if len(k) != 8 {
				return nil
			}
			docID := binary.LittleEndian.Uint64(k)
			var meta DocMeta
			if json.Unmarshal(v, &meta) != nil {
				return nil
			}
			rawVec := vb.Get(k)
			if len(rawVec) == 0 || len(rawVec)%4 != 0 {
				return nil
			}
			floats := make([]float32, len(rawVec)/4)
			for i := range floats {
				floats[i] = math.Float32frombits(binary.LittleEndian.Uint32(rawVec[i*4 : (i+1)*4]))
			}
			ids = append(ids, docID)
			metas = append(metas, meta)
			vecs = append(vecs, floats)
			return nil
		})
	})
	return ids, metas, vecs, err
}

// Search performs a brute-force cosine-similarity search over the shared graph. [287.E]
// O(N) — acceptable for a shared tier that stays small relative to per-workspace graphs.
// Returns the top-k DocMeta results sorted by descending similarity.
func (sg *SharedGraph) Search(query []float32, k int) ([]DocMeta, error) {
	_, metas, vecs, err := sg.LoadVectors()
	if err != nil || len(vecs) == 0 {
		return nil, err
	}
	type scored struct {
		meta  DocMeta
		score float32
	}
	results := make([]scored, 0, len(vecs))
	for i, v := range vecs {
		results = append(results, scored{meta: metas[i], score: cosineSim(query, v)})
	}
	// Partial sort: bubble up top-k.
	if k > len(results) {
		k = len(results)
	}
	for i := 0; i < k; i++ {
		maxIdx := i
		for j := i + 1; j < len(results); j++ {
			if results[j].score > results[maxIdx].score {
				maxIdx = j
			}
		}
		results[i], results[maxIdx] = results[maxIdx], results[i]
	}
	out := make([]DocMeta, k)
	for i := range out {
		out[i] = results[i].meta
	}
	return out, nil
}

// cosineSim computes cosine similarity between two float32 vectors.
// 4-way unrolled loop keeps four independent float32 accumulator chains,
// letting the compiler emit packed VFMADD231PS instructions (GOAMD64=v3).
// The float64 upcast was removed — float64 breaks auto-vectorization.
func cosineSim(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	n := len(a)
	var d0, d1, d2, d3 float32
	var na0, na1, na2, na3 float32
	var nb0, nb1, nb2, nb3 float32
	end4 := n - (n % 4)
	for i := 0; i < end4; i += 4 {
		x0, y0 := a[i], b[i]
		x1, y1 := a[i+1], b[i+1]
		x2, y2 := a[i+2], b[i+2]
		x3, y3 := a[i+3], b[i+3]
		d0 += x0 * y0
		d1 += x1 * y1
		d2 += x2 * y2
		d3 += x3 * y3
		na0 += x0 * x0
		na1 += x1 * x1
		na2 += x2 * x2
		na3 += x3 * x3
		nb0 += y0 * y0
		nb1 += y1 * y1
		nb2 += y2 * y2
		nb3 += y3 * y3
	}
	var dotT, naT, nbT float32
	for i := end4; i < n; i++ {
		x, y := a[i], b[i]
		dotT += x * y
		naT += x * x
		nbT += y * y
	}
	dot := d0 + d1 + d2 + d3 + dotT
	na := na0 + na1 + na2 + na3 + naT
	nb := nb0 + nb1 + nb2 + nb3 + nbT
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(float64(dot) / (math.Sqrt(float64(na)) * math.Sqrt(float64(nb))))
}

// Count returns the number of documents in the shared graph. [287.F]
func (sg *SharedGraph) Count() (int, error) {
	var n int
	err := sg.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(sharedBucketDocs)
		if b == nil {
			return nil
		}
		n = b.Stats().KeyN
		return nil
	})
	return n, err
}

// Prune removes entries whose WorkspaceID is no longer in knownIDs. [287.G]
// Should be called after a SIGHUP that removes a workspace from the project.
func (sg *SharedGraph) Prune(knownIDs map[string]struct{}) (removed int, err error) {
	if lockErr := sg.TryLock(); lockErr != nil {
		return 0, lockErr
	}
	defer sg.Unlock()

	var toDelete [][]byte
	if viewErr := sg.db.View(func(tx *bbolt.Tx) error {
		db := tx.Bucket(sharedBucketDocs)
		if db == nil {
			return nil
		}
		return db.ForEach(func(k, v []byte) error {
			var meta DocMeta
			if json.Unmarshal(v, &meta) != nil {
				return nil
			}
			if meta.WorkspaceID != "" {
				if _, ok := knownIDs[meta.WorkspaceID]; !ok {
					cp := make([]byte, len(k))
					copy(cp, k)
					toDelete = append(toDelete, cp)
				}
			}
			return nil
		})
	}); viewErr != nil {
		return 0, viewErr
	}
	if len(toDelete) == 0 {
		return 0, nil
	}
	err = sg.db.Batch(func(tx *bbolt.Tx) error {
		db := tx.Bucket(sharedBucketDocs)
		vb := tx.Bucket(sharedBucketVecs)
		for _, k := range toDelete {
			if derr := db.Delete(k); derr != nil {
				return derr
			}
			_ = vb.Delete(k)
		}
		return nil
	})
	if err == nil {
		removed = len(toDelete)
	}
	return removed, err
}

// Close releases the lock (if held) and closes the DB and lock file. [287.A]
func (sg *SharedGraph) Close() error {
	sg.Unlock()
	var errs []error
	if sg.db != nil {
		if err := sg.db.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if sg.lockFD != nil {
		if err := sg.lockFD.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("shared_graph close: %v", errs)
	}
	return nil
}

// loadDocIDToNodeID builds a docID→nodeID reverse map from bucketNodes. [287.C internal]
func (wal *WAL) loadDocIDToNodeID() (map[uint64]uint32, error) {
	m := make(map[uint64]uint32)
	err := wal.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketNodes)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			if len(k) != 4 || len(v) < 8 {
				return nil
			}
			nodeID := binary.LittleEndian.Uint32(k)
			docID := binary.LittleEndian.Uint64(v[0:8])
			m[docID] = nodeID
			return nil
		})
	})
	return m, err
}
