package rag

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.etcd.io/bbolt"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

var (
	bucketNodes      = []byte("hnsw_nodes")
	bucketEdges      = []byte("hnsw_edges")
	bucketVectors    = []byte("hnsw_vectors")
	bucketDocs       = []byte("hnsw_docs")
	bucketScars      = []byte("hnsw_scars")
	bucketWeights    = []byte("hnsw_weights")
	bucketDirectives = []byte("hnsw_directives")
	// bucketHnswMeta stores HNSW graph-wide invariants (canonical vector
	// dimension etc.). Audit finding S9-6 (PILAR XXVIII 143.D, 2026-05-02):
	// pre-fix Insert/InsertBatch wrote raw float32 bits without validating
	// len(vec) against any canonical, and LoadGraph derived vecDim from the
	// FIRST entry. Mixed-dimension WAL entries either panicked LoadGraph
	// (out-of-range write into the flat slice) or silently corrupted the
	// graph (under-write → zero padding). The canonical dim is now written
	// once on first Insert and validated on every subsequent Insert.
	bucketHnswMeta = []byte("hnsw_meta")
)

// keyCanonicalVecDim is the key under which the canonical vector dimension
// (uint32 little-endian) is persisted in bucketHnswMeta. Set on first Insert
// (or on first LoadGraph of a legacy DB via best-effort derivation).
var keyCanonicalVecDim = []byte("canonical_vec_dim")

// ErrVectorDimMismatch is returned by Insert/InsertBatch when a vector's
// length differs from the canonical dimension already stored in bucketHnswMeta.
// Callers should treat this as a non-recoverable input error — re-attempting
// the same Insert produces the same error. The proper response is to fix the
// embedder configuration that produced the wrong-dimension vector.
var ErrVectorDimMismatch = errors.New("rag/wal: vector dimension mismatch")

type DocMeta struct {
	Path          string `json:"path"`
	Content       string `json:"content"`
	InboundDegree int    `json:"inbound_degree"`
	DeletedAt     int64  `json:"deleted_at"`
	WorkspaceID   string `json:"workspace_id,omitempty"` // [SRE-34.1.3] empty = legacy (matches all workspaces)
}

type WAL struct {
	db *bbolt.DB
	// directivesMu serializes the read-snapshot-then-write-file unit in
	// SyncDirectivesToDisk. Without it, two concurrent neo_learn_directive
	// calls can interleave so a goroutine reads the BoltDB snapshot BEFORE a
	// peer commits its SaveDirective, yet its file write lands AFTER the
	// peer's — clobbering the disk file with a stale snapshot that drops the
	// peer's directive. [D4 / technical_debt 2026-05-13 DUAL-LAYER-SYNC drift]
	directivesMu sync.Mutex
}

func OpenWAL(dbPath string) (*WAL, error) {
	db, err := bbolt.Open(dbPath, 0600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("failed to acquire lock from bbolt in: %s: %w", dbPath, err)
	}

	err = db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(bucketNodes); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketEdges); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketVectors); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketDocs); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketScars); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketWeights); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketDirectives); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketHnswMeta); err != nil {
			return err
		}

		bDirectives := tx.Bucket(bucketDirectives)
		if bDirectives != nil && bDirectives.Get([]byte("global_rules")) == nil {
			// [SRE-113.B] Bootstrap directives — must reflect *current* tooling.
			// Older versions referenced neo_apply_patch / neo_dependency_graph
			// (long deleted). Keep this list minimal: load-bearing invariants
			// only. Detailed directives live in .claude/rules/ and are imported
			// at boot via dual-layer sync (neo_learn_directive).
			defaultRules := []string{
				"[SCOPE: GLOBAL] CICLO-OUROBOROS: Flujo obligatorio: BRIEFING → BLAST_RADIUS → Edit/Write → neo_sre_certify_mutation. No editar sin investigar; no commit sin certificar.",
				"[SCOPE: GO & KERNEL] ZERO-ALLOCATION: Prohibido make()/new() en Hot-Paths (RAG, MCTS). Usar sync.Pool y memoria plana. Slices reciclados con [:0].",
				"[SCOPE: GO & KERNEL] AISLAMIENTO-MCP: NUNCA usar fmt.Print u os.Stdout en código MCP — destruye la conexión JSON-RPC. Usar exclusivamente log.Printf.",
				"[SCOPE: GLOBAL] ZERO-HARDCODING: Todo enlace a BD, puertos y endpoints viene de neo.yaml o variables de entorno. Resolución por búsqueda recursiva.",
			}
			return sre.ZeroAllocJSONMarshal(defaultRules, func(data []byte) error {
				return bDirectives.Put([]byte("global_rules"), data)
			})
		}

		return nil
	})

	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize buckets in: %s: %w", dbPath, err)
	}

	return &WAL{db: db}, nil
}

func (wal *WAL) SaveDocMeta(docID uint64, path string, content string, inboundDegree int) error {
	return wal.db.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketDocs)
		meta := DocMeta{Path: path, Content: content, InboundDegree: inboundDegree}

		key := make([]byte, 8)
		binary.LittleEndian.PutUint64(key, docID)
		return sre.ZeroAllocJSONMarshal(meta, func(data []byte) error {
			return b.Put(key, data)
		})
	})
}

func (wal *WAL) GetDocMeta(docID uint64) (string, string, int, error) {
	var path, content string
	var inboundDegree int
	err := wal.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketDocs)
		key := make([]byte, 8)
		binary.LittleEndian.PutUint64(key, docID)
		data := b.Get(key)
		if data == nil {
			return fmt.Errorf("docID %d not found", docID)
		}
		var meta DocMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			return err
		}
		path = meta.Path
		content = meta.Content
		inboundDegree = meta.InboundDegree
		return nil
	})
	return path, content, inboundDegree, err
}

// GetDocsByWorkspace returns all DocMeta entries belonging to workspaceID.
// Legacy entries with empty WorkspaceID are included when workspaceID is empty
// (cross-workspace search). [SRE-34.1.3]
func (wal *WAL) GetDocsByWorkspace(workspaceID string) ([]DocMeta, error) {
	var results []DocMeta
	err := wal.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketDocs)
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			if v == nil {
				return nil
			}
			var meta DocMeta
			if err := json.Unmarshal(v, &meta); err != nil {
				return nil // skip corrupted entries
			}
			// Empty workspaceID → return all (cross-workspace). Otherwise filter.
			if workspaceID == "" || meta.WorkspaceID == "" || meta.WorkspaceID == workspaceID {
				results = append(results, meta)
			}
			return nil
		})
	})
	return results, err
}

// PrecomputeAndStoreTopology ejecuta la fase de indexación batch para BoltDB.
func (wal *WAL) PrecomputeAndStoreTopology(ctx context.Context, degreeUpdates map[uint64]int) error {
	if len(degreeUpdates) == 0 {
		return nil
	}
	return wal.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketDocs)
		if bucket == nil {
			return fmt.Errorf("bucketDocs no existe")
		}

		key := make([]byte, 8)
		for docID, degree := range degreeUpdates {
			binary.LittleEndian.PutUint64(key, docID)
			val := bucket.Get(key)
			if val == nil {
				continue // Documento huérfano, ignorar
			}

			var meta DocMeta
			if err := json.Unmarshal(val, &meta); err != nil {
				return fmt.Errorf("corrupción JSON en doc %d: %w", docID, err)
			}

			// Actualizar y re-empaquetar de forma zero-alloc
			meta.InboundDegree = degree
			if err := sre.ZeroAllocJSONMarshal(meta, func(updatedVal []byte) error {
				return bucket.Put(key, updatedVal)
			}); err != nil {
				return fmt.Errorf("error serializando doc %d: %w", docID, err)
			}
		}
		return nil
	})
}

// readCanonicalDim returns the HNSW canonical vector dimension stored in
// bucketHnswMeta, or 0 if no value has been persisted yet (legacy DB or
// fresh install). Read-only — usable inside both View and Update tx.
//
// Audit finding S9-6 (PILAR XXVIII 143.D): callers MUST consult this before
// trusting the dimension of any individual vector entry; the older "derive
// from first entry" pattern silently corrupts the graph when a downstream
// embedder is misconfigured and produces a wrong-length vector.
func readCanonicalDim(tx *bbolt.Tx) int {
	b := tx.Bucket(bucketHnswMeta)
	if b == nil {
		return 0
	}
	v := b.Get(keyCanonicalVecDim)
	if len(v) != 4 {
		return 0
	}
	return int(binary.LittleEndian.Uint32(v))
}

// ensureCanonicalDim persists vecLen as the canonical dimension on first
// Insert into a fresh WAL, or validates that an incoming vecLen matches a
// previously-stored canonical. Returns ErrVectorDimMismatch when the input
// disagrees with the stored value. Update tx required.
//
// vecLen is the length in float32 elements (NOT bytes). Caller passes
// len(vec) directly.
func ensureCanonicalDim(tx *bbolt.Tx, vecLen int) error {
	if vecLen <= 0 {
		return fmt.Errorf("rag/wal: vector length must be > 0, got %d", vecLen)
	}
	b, err := tx.CreateBucketIfNotExists(bucketHnswMeta)
	if err != nil {
		return fmt.Errorf("rag/wal: meta bucket: %w", err)
	}
	if existing := b.Get(keyCanonicalVecDim); len(existing) == 4 {
		stored := int(binary.LittleEndian.Uint32(existing))
		if stored != vecLen {
			return fmt.Errorf("%w: stored canonical=%d, incoming=%d", ErrVectorDimMismatch, stored, vecLen)
		}
		return nil
	}
	// No canonical set yet — persist this vector's length as the canonical.
	dimBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(dimBuf, uint32(vecLen)) //nolint:gosec // vecLen bounded by HNSW config (typically 128/384/768/1536), well below uint32 max
	if err := b.Put(keyCanonicalVecDim, dimBuf); err != nil {
		return fmt.Errorf("rag/wal: persist canonical dim: %w", err)
	}
	return nil
}

func (wal *WAL) Insert(nodeID uint32, node Node, edges []uint32, vec []float32) error {
	return wal.db.Batch(func(tx *bbolt.Tx) error {
		// Audit S9-6 fix: validate vector dim against canonical BEFORE writing
		// any bucket. If this is the first Insert into a fresh WAL, the
		// canonical is persisted from len(vec). Subsequent inserts with
		// mismatched length fail with ErrVectorDimMismatch — they MUST NOT
		// be silently truncated/padded into the existing flat layout.
		if err := ensureCanonicalDim(tx, len(vec)); err != nil {
			return fmt.Errorf("[wal] node %d: %w", nodeID, err)
		}

		key := make([]byte, 4)
		binary.LittleEndian.PutUint32(key, nodeID)

		nodeBuf := make([]byte, 16)
		binary.LittleEndian.PutUint64(nodeBuf[0:8], node.DocID)
		binary.LittleEndian.PutUint32(nodeBuf[8:12], node.EdgesOffset)
		binary.LittleEndian.PutUint16(nodeBuf[12:14], node.EdgesLength)
		nodeBuf[14] = node.Layer
		nodeBuf[15] = 0

		if err := tx.Bucket(bucketNodes).Put(key, nodeBuf); err != nil {
			return fmt.Errorf("[wal] failed to write node %d: %w", nodeID, err)
		}

		edgeBuf := make([]byte, len(edges)*4)
		for i, e := range edges {
			binary.LittleEndian.PutUint32(edgeBuf[i*4:(i+1)*4], e)
		}
		if err := tx.Bucket(bucketEdges).Put(key, edgeBuf); err != nil {
			return fmt.Errorf("[wal] failed to write edges for node %d: %w", nodeID, err)
		}

		vecBuf := make([]byte, len(vec)*4)
		for i, v := range vec {
			binary.LittleEndian.PutUint32(vecBuf[i*4:(i+1)*4], math.Float32bits(v))
		}
		if err := tx.Bucket(bucketVectors).Put(key, vecBuf); err != nil {
			return fmt.Errorf("[wal] failed to write vector for node %d: %w", nodeID, err)
		}

		return nil
	})
}

func (wal *WAL) InsertBatch(nodeIDs []uint32, nodes []Node, edgesList [][]uint32, vectors [][]float32) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	// Audit S9-6 fix: pre-flight vector dimension homogeneity check. ALL
	// vectors in the batch must share the same length, AND that length
	// must match the canonical (or set the canonical on first batch into a
	// fresh WAL). Reject the entire batch on mismatch — partial writes
	// would leave the WAL in an inconsistent state per audit S9-6.
	expectedDim := len(vectors[0])
	for i, vec := range vectors {
		if len(vec) != expectedDim {
			return fmt.Errorf("[wal] InsertBatch: vector %d has dim %d, batch dim %d (%w)", i, len(vec), expectedDim, ErrVectorDimMismatch)
		}
	}
	return wal.db.Batch(func(tx *bbolt.Tx) error {
		if err := ensureCanonicalDim(tx, expectedDim); err != nil {
			return fmt.Errorf("[wal] InsertBatch: %w", err)
		}
		totalEdges := 0
		totalVecs := 0
		for i := range nodeIDs {
			totalEdges += len(edgesList[i])
			totalVecs += len(vectors[i])
		}

		keyArena := make([]byte, len(nodeIDs)*4)
		nodeArena := make([]byte, len(nodeIDs)*16)
		edgeArena := make([]byte, totalEdges*4)
		vecArena := make([]byte, totalVecs*4)

		edgeOffset := 0
		vecOffset := 0

		for idx, nodeID := range nodeIDs {
			keyBuf := keyArena[idx*4 : (idx+1)*4]
			binary.LittleEndian.PutUint32(keyBuf, nodeID)

			node := nodes[idx]
			nodeBuf := nodeArena[idx*16 : (idx+1)*16]
			binary.LittleEndian.PutUint64(nodeBuf[0:8], node.DocID)
			binary.LittleEndian.PutUint32(nodeBuf[8:12], node.EdgesOffset)
			binary.LittleEndian.PutUint16(nodeBuf[12:14], node.EdgesLength)
			nodeBuf[14] = node.Layer
			nodeBuf[15] = 0
			if err := tx.Bucket(bucketNodes).Put(keyBuf, nodeBuf); err != nil {
				return err
			}

			edges := edgesList[idx]
			edgeLen := len(edges) * 4
			edgeBuf := edgeArena[edgeOffset : edgeOffset+edgeLen]
			for i, e := range edges {
				binary.LittleEndian.PutUint32(edgeBuf[i*4:(i+1)*4], e)
			}
			if err := tx.Bucket(bucketEdges).Put(keyBuf, edgeBuf); err != nil {
				return err
			}
			edgeOffset += edgeLen

			vec := vectors[idx]
			vecLen := len(vec) * 4
			vecBuf := vecArena[vecOffset : vecOffset+vecLen]
			for i, v := range vec {
				binary.LittleEndian.PutUint32(vecBuf[i*4:(i+1)*4], math.Float32bits(v))
			}
			if err := tx.Bucket(bucketVectors).Put(keyBuf, vecBuf); err != nil {
				return err
			}
			vecOffset += vecLen
		}
		return nil
	})
}

func (wal *WAL) AppendNode(nodeID uint32, node Node) error {
	return wal.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketNodes)

		buf := make([]byte, 16)
		binary.LittleEndian.PutUint64(buf[0:8], node.DocID)
		binary.LittleEndian.PutUint32(buf[8:12], node.EdgesOffset)
		binary.LittleEndian.PutUint16(buf[12:14], node.EdgesLength)
		buf[14] = node.Layer
		buf[15] = 0

		key := make([]byte, 4)
		binary.LittleEndian.PutUint32(key, nodeID)
		return bucket.Put(key, buf)
	})
}

func (w *WAL) LoadGraph(ctx context.Context) (*Graph, error) {
	var (
		nodes      []Node
		allEdges   []uint32
		allVectors []float32
		vecDim     int
	)

	err := w.db.View(func(tx *bbolt.Tx) error {

		nb := tx.Bucket(bucketNodes)
		nodeCount := nb.Stats().KeyN
		nodes = make([]Node, 0, nodeCount)

		if err := nb.ForEach(func(k, v []byte) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if len(v) != 16 {
				return fmt.Errorf("[wal] node corruption: expected 16 bytes, got %d", len(v))
			}
			nodes = append(nodes, Node{
				DocID:       binary.LittleEndian.Uint64(v[0:8]),
				EdgesOffset: binary.LittleEndian.Uint32(v[8:12]),
				EdgesLength: binary.LittleEndian.Uint16(v[12:14]),
				Layer:       v[14],
			})
			return nil
		}); err != nil {
			return err
		}

		eb := tx.Bucket(bucketEdges)
		edgeBytes := 0
		eb.ForEach(func(k, v []byte) error {
			edgeBytes += len(v)
			return nil
		})
		allEdges = make([]uint32, 0, edgeBytes/4)

		if err := eb.ForEach(func(k, v []byte) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			for i := 0; i+4 <= len(v); i += 4 {
				allEdges = append(allEdges, binary.LittleEndian.Uint32(v[i:i+4]))
			}
			return nil
		}); err != nil {
			return err
		}

		vb := tx.Bucket(bucketVectors)
		// Audit S9-6 fix: prefer canonical dim from meta bucket — set on first
		// Insert by ensureCanonicalDim and authoritative thereafter. Fall back
		// to first-entry derivation only on legacy DBs that predate this
		// metadata. The fallback is best-effort and does NOT validate
		// homogeneity at load time (subsequent Inserts will validate via
		// ensureCanonicalDim on first write into the meta bucket).
		vecDim = readCanonicalDim(tx)
		if vecDim == 0 {
			if _, firstVal := vb.Cursor().First(); len(firstVal) >= 4 {
				vecDim = len(firstVal) / 4
			}
		}
		allVectors = make([]float32, nodeCount*vecDim)
		wIdx := 0
		if err := vb.ForEach(func(k, v []byte) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Defensive: per-entry length check. Even with the metadata
			// canonical in place, a hand-edited or partially-written WAL
			// can present a wrong-length entry; refusing to load is safer
			// than the pre-fix behaviour of silently corrupting the slice.
			if vecDim > 0 && len(v) != vecDim*4 {
				return fmt.Errorf("[wal] LoadGraph: vector for node %x has %d bytes, canonical dim requires %d (run repair)", k, len(v), vecDim*4)
			}
			for i := 0; i+4 <= len(v); i, wIdx = i+4, wIdx+1 {
				if wIdx >= len(allVectors) {
					return fmt.Errorf("[wal] LoadGraph: vector overflow at node %x (canonical dim=%d, nodeCount=%d)", k, vecDim, nodeCount)
				}
				allVectors[wIdx] = math.Float32frombits(binary.LittleEndian.Uint32(v[i:i+4]))
			}
			return nil
		}); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	log.Printf("[SRE-INFO] HNSW graph recovered: %d nodes, %d edges, %d vector components (dim=%d)",
		len(nodes), len(allEdges), len(allVectors), vecDim)

	return &Graph{
		Nodes:   nodes,
		Edges:   allEdges,
		Vectors: allVectors,
		VecDim:  vecDim,
	}, nil
}

func (wal *WAL) SaveScar(filePath string, scar string) error {
	return wal.db.Batch(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketScars)
		if bucket == nil {
			return fmt.Errorf("bucket_scars not found")
		}

		key := []byte(filePath)
		var existing []string
		if current := bucket.Get(key); current != nil {
			if err := json.Unmarshal(current, &existing); err != nil {
				log.Printf("[SRE] Error: %v", err)
			}
		}

		existing = append(existing, scar)

		return sre.ZeroAllocJSONMarshal(existing, func(data []byte) error {
			return bucket.Put(key, data)
		})
	})
}

func (wal *WAL) GetScars(filePath string) ([]string, error) {
	var scars []string
	err := wal.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketScars)
		if bucket == nil {
			return fmt.Errorf("bucket_scars not found")
		}

		if data := bucket.Get([]byte(filePath)); data != nil {
			if err := json.Unmarshal(data, &scars); err != nil {
				return err
			}
		}
		return nil
	})
	return scars, err
}

func (wal *WAL) SaveWeights(w1, w2 []float32) error {
	return wal.db.Batch(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketWeights)
		if bucket == nil {
			return fmt.Errorf("bucket_weights not found")
		}

		w1Bytes := make([]byte, len(w1)*4)
		for i, v := range w1 {
			binary.LittleEndian.PutUint32(w1Bytes[i*4:], math.Float32bits(v))
		}

		w2Bytes := make([]byte, len(w2)*4)
		for i, v := range w2 {
			binary.LittleEndian.PutUint32(w2Bytes[i*4:], math.Float32bits(v))
		}

		if err := bucket.Put([]byte("W1"), w1Bytes); err != nil {
			return err
		}
		return bucket.Put([]byte("W2"), w2Bytes)
	})
}

func (wal *WAL) LoadWeights() ([]float32, []float32, error) {
	var w1, w2 []float32
	err := wal.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketWeights)
		if bucket == nil {
			return fmt.Errorf("bucket_weights not found")
		}

		w1Bytes := bucket.Get([]byte("W1"))
		if w1Bytes == nil {
			return fmt.Errorf("W1 not found")
		}
		for i := 0; i < len(w1Bytes); i += 4 {
			w1 = append(w1, math.Float32frombits(binary.LittleEndian.Uint32(w1Bytes[i:])))
		}

		w2Bytes := bucket.Get([]byte("W2"))
		if w2Bytes == nil {
			return fmt.Errorf("W2 not found")
		}
		for i := 0; i < len(w2Bytes); i += 4 {
			w2 = append(w2, math.Float32frombits(binary.LittleEndian.Uint32(w2Bytes[i:])))
		}

		return nil
	})
	return w1, w2, err
}

func (wal *WAL) SaveDirective(rule string) error {
	return wal.db.Batch(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketDirectives)
		if bucket == nil {
			return fmt.Errorf("bucket_directives not found")
		}

		key := []byte("global_rules")
		var existing []string
		if current := bucket.Get(key); current != nil {
			if err := json.Unmarshal(current, &existing); err != nil {
				log.Printf("[SRE] Error: %v", err)
			}
		}

		existing = append(existing, rule)

		return sre.ZeroAllocJSONMarshal(existing, func(data []byte) error {
			return bucket.Put(key, data)
		})
	})
}

// [SRE-77.2] UpdateDirective replaces the text of a directive by 1-based ID.
func (wal *WAL) UpdateDirective(id int, newText string) error {
	return wal.db.Batch(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketDirectives)
		if bucket == nil {
			return fmt.Errorf("bucket_directives not found")
		}
		key := []byte("global_rules")
		var rules []string
		if current := bucket.Get(key); current != nil {
			if err := json.Unmarshal(current, &rules); err != nil {
				return err
			}
		}
		if id < 1 || id > len(rules) {
			return fmt.Errorf("directive_id %d out of range (1-%d)", id, len(rules))
		}
		rules[id-1] = newText
		return sre.ZeroAllocJSONMarshal(rules, func(data []byte) error {
			return bucket.Put(key, data)
		})
	})
}

// [SRE-77.3] DeprecateDirective soft-deletes a directive by 1-based ID.
// The entry is marked ~~OBSOLETO~~ (soft delete) and optionally links to the superseding directive ID.
func (wal *WAL) DeprecateDirective(id int, deprecatedBy int) error {
	return wal.db.Batch(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketDirectives)
		if bucket == nil {
			return fmt.Errorf("bucket_directives not found")
		}
		key := []byte("global_rules")
		var rules []string
		if current := bucket.Get(key); current != nil {
			if err := json.Unmarshal(current, &rules); err != nil {
				return err
			}
		}
		if id < 1 || id > len(rules) {
			return fmt.Errorf("directive_id %d out of range (1-%d)", id, len(rules))
		}
		text := rules[id-1]
		if strings.HasPrefix(text, "~~OBSOLETO~~") {
			return nil // idempotent
		}
		if deprecatedBy > 0 {
			rules[id-1] = fmt.Sprintf("~~OBSOLETO~~ %s (deprecated_by: %d)", text, deprecatedBy)
		} else {
			rules[id-1] = fmt.Sprintf("~~OBSOLETO~~ %s", text)
		}
		return sre.ZeroAllocJSONMarshal(rules, func(data []byte) error {
			return bucket.Put(key, data)
		})
	})
}

// [SRE-23.1.1] Sync directives to .claude/rules/ for dual-layer persistence.
func (wal *WAL) SyncDirectivesToDisk(workspace string) error {
	// [D4] Serialize the read-then-write unit so a stale GetDirectives snapshot
	// can never overwrite a fresher one. See the directivesMu field doc.
	wal.directivesMu.Lock()
	defer wal.directivesMu.Unlock()

	rules, err := wal.GetDirectives()
	if err != nil || len(rules) == 0 {
		return err
	}
	syncPath := filepath.Join(workspace, ".claude", "rules", "neo-synced-directives.md")
	if mkErr := os.MkdirAll(filepath.Dir(syncPath), 0755); mkErr != nil {
		return fmt.Errorf("SyncDirectivesToDisk: mkdir: %w", mkErr)
	}

	var sb strings.Builder
	sb.WriteString("# NeoAnvil Synced Directives (auto-generated)\n\n")
	sb.WriteString("Do not edit manually. Generated by neo_learn_directive via dual-layer sync.\n\n")
	// [SRE-77.4] Active-only sync: deprecated entries shown as strikethrough for auditability.
	for i, rule := range rules {
		if strings.HasPrefix(rule, "~~OBSOLETO~~") {
			fmt.Fprintf(&sb, "%d. ~~%s~~\n", i+1, strings.TrimPrefix(rule, "~~OBSOLETO~~ "))
		} else {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, rule)
		}
	}
	// [D4] Crash-atomic write: os.WriteFile truncates the target before
	// writing, so a crash mid-write leaves neo-synced-directives.md truncated.
	return atomicWriteFile(syncPath, []byte(sb.String()), 0644)
}

// atomicWriteFile writes data to a sibling temp file, fsyncs it, and renames it
// over path — the rename is atomic on POSIX within one filesystem. A crash,
// disk-full, or SIGKILL before the rename leaves the original file fully intact
// (os.WriteFile, by contrast, truncates the target first). The parent directory
// is fsynced so the rename itself survives a power loss. The temp file is a
// hidden sibling (".<name>.tmp-*") so a *.md glob never picks it up mid-write.
// [D4 — same crash-safety pattern as CompactWAL in wal_compact.go]
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("atomicWriteFile: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, wErr := tmp.Write(data); wErr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomicWriteFile: write: %w", wErr)
	}
	if sErr := tmp.Sync(); sErr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomicWriteFile: fsync: %w", sErr)
	}
	if cErr := tmp.Close(); cErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomicWriteFile: close: %w", cErr)
	}
	if chErr := os.Chmod(tmpPath, perm); chErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomicWriteFile: chmod: %w", chErr)
	}
	if rErr := os.Rename(tmpPath, path); rErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomicWriteFile: rename: %w", rErr)
	}
	if d, dErr := os.Open(dir); dErr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// normalizeDirective strips soft-delete markers for comparison purposes.
// WAL stores "~~OBSOLETO~~ text"; .md renders "~~text~~". Both normalize to "text".
// Extracted as a package-level helper so LoadDirectivesFromDisk and the
// destructive-sync pre-pass can share the same canonicalization.
func normalizeDirective(s string) string {
	s = strings.TrimPrefix(s, "~~OBSOLETO~~ ")
	s = strings.TrimPrefix(s, "~~")
	s = strings.TrimSuffix(s, "~~")
	return strings.TrimSpace(s)
}

// [SRE-23.1.2] Load directives from .claude/rules/ into BoltDB at startup.
// [SRE-79-FIX] Dedup normalizes both WAL format (~~OBSOLETO~~ prefix) and .md format
// (~~...~~ wrapping) so soft-deleted entries are never re-imported as new entries.
// [LARGE-PROJECT/DUAL-LAYER-SYNC FIX 2026-05-13, org-debt c9a9 P1]
// Destructive sync: disk is the authoritative source. BoltDB entries whose
// normalized text is NOT present in the disk file get marked ~~OBSOLETO~~
// (existing soft-delete semantics — recoverable via git revert of disk file
// or manual SaveDirective). This breaks the chronic inflation loop where
// BoltDB accumulated entries forever via boot replay union with disk.
//
// Safety guard: if disk has < syncDestructiveMinDisk active entries while
// BoltDB has > syncDestructiveBoltDBThreshold, skip the destructive sweep
// and log a warning (likely indicates a truncated / corrupted disk file).
//
// The additive UPSERT step (disk-only directives → BoltDB) remains so the
// operator can still hand-add entries to the disk file and have them picked
// up at next boot.
const (
	syncDestructiveMinDisk         = 5
	syncDestructiveBoltDBThreshold = 50
	// Relative-loss guard closes the gap where the absolute guard didn't
	// fire on small drift (e.g. disk=50 vs BoltDB=57 → 12% loss slipped
	// through in the 2026-05-13 7-directive drift incident).
	syncRelativeLossSampleMin    = 10
	syncDestructiveMaxRelLossPct = 20
)

func (wal *WAL) LoadDirectivesFromDisk(workspace string) error {
	syncPath := filepath.Join(workspace, ".claude", "rules", "neo-synced-directives.md")
	data, err := os.ReadFile(syncPath)
	if err != nil {
		return nil // File doesn't exist yet, not an error
	}

	diskSet := parseDirectivesFromMarkdown(data)
	existing, _ := wal.GetDirectives()
	activeOnDisk := len(diskSet)
	activeInBoltDB := countActiveDirectivesIn(existing)

	if shouldSkipDestructiveSweep(activeOnDisk, activeInBoltDB) {
		log.Printf("[DIRECTIVES-SYNC] corruption guard: disk=%d active, BoltDB=%d active (loss=%d%%) — skipping destructive sweep, falling back to additive only",
			activeOnDisk, activeInBoltDB, relativeLossPct(activeOnDisk, activeInBoltDB))
	} else {
		obsoleted := runDestructiveSweep(wal, existing, diskSet)
		if obsoleted > 0 {
			log.Printf("[DIRECTIVES-SYNC] deprecated %d BoltDB entries not present on disk (disk=%d active, was BoltDB=%d active)",
				obsoleted, activeOnDisk, activeInBoltDB)
		}
	}

	runAdditiveUpsertFromDisk(wal, diskSet)
	return nil
}

func countActiveDirectivesIn(rules []string) int {
	n := 0
	for _, r := range rules {
		if !strings.HasPrefix(r, "~~OBSOLETO~~") {
			n++
		}
	}
	return n
}

// relativeLossPct returns the percentage of BoltDB active entries missing
// from disk. Returns 0 when disk has ≥ BoltDB (no loss to flag).
func relativeLossPct(activeOnDisk, activeInBoltDB int) int {
	if activeInBoltDB <= 0 || activeOnDisk >= activeInBoltDB {
		return 0
	}
	return (activeInBoltDB - activeOnDisk) * 100 / activeInBoltDB
}

// shouldSkipDestructiveSweep returns true when either guard triggers.
// Absolute-loss guard: disk almost empty while BoltDB has many (truncation).
// Relative-loss guard: disk lost > N% of BoltDB (subtle drift slips abs guard).
func shouldSkipDestructiveSweep(activeOnDisk, activeInBoltDB int) bool {
	if activeOnDisk < syncDestructiveMinDisk && activeInBoltDB > syncDestructiveBoltDBThreshold {
		return true
	}
	if activeInBoltDB >= syncRelativeLossSampleMin && relativeLossPct(activeOnDisk, activeInBoltDB) > syncDestructiveMaxRelLossPct {
		return true
	}
	return false
}

func runDestructiveSweep(wal *WAL, existing []string, diskSet map[string]string) int {
	obsoleted := 0
	for i, r := range existing {
		if strings.HasPrefix(r, "~~OBSOLETO~~") {
			continue
		}
		norm := normalizeDirective(r)
		if norm == "" {
			continue
		}
		if _, inDisk := diskSet[norm]; inDisk {
			continue
		}
		if err := wal.DeprecateDirective(i+1, 0); err == nil {
			obsoleted++
		}
	}
	return obsoleted
}

func runAdditiveUpsertFromDisk(wal *WAL, diskSet map[string]string) {
	existing, _ := wal.GetDirectives()
	existingSet := make(map[string]struct{}, len(existing))
	for _, r := range existing {
		existingSet[normalizeDirective(r)] = struct{}{}
	}
	for normalized, line := range diskSet {
		if _, dupe := existingSet[normalized]; dupe || normalized == "" {
			continue
		}
		_ = wal.SaveDirective(line)
		existingSet[normalized] = struct{}{}
	}
}

// parseDirectivesFromMarkdown extracts active (non-soft-deleted) directives
// from the on-disk neo-synced-directives.md file. Returns map[normalized]rawLine.
// Soft-deleted entries (~~...~~ wrapping) are skipped — they should remain
// soft-deleted in BoltDB too and the destructive sweep will handle them.
func parseDirectivesFromMarkdown(data []byte) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		if len(line) <= 3 || line[0] < '0' || line[0] > '9' {
			continue
		}
		idx := strings.Index(line, ". ")
		if idx <= 0 || idx >= 5 {
			continue
		}
		rule := line[idx+2:]
		if strings.HasPrefix(rule, "~~") {
			continue // soft-deleted on disk
		}
		norm := normalizeDirective(rule)
		if norm == "" {
			continue
		}
		out[norm] = rule
	}
	return out
}

func (wal *WAL) GetDirectives() ([]string, error) {
	var rules []string
	err := wal.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketDirectives)
		if bucket == nil {
			return fmt.Errorf("bucket_directives not found")
		}

		if data := bucket.Get([]byte("global_rules")); data != nil {
			if err := json.Unmarshal(data, &rules); err != nil {
				return err
			}
		}
		return nil
	})
	return rules, err
}

// SnapshotDirectives writes the current BoltDB directives state to a JSON
// file at snapshotPath. Recovery option before destructive operations
// (CompactDirectives hard-purge, mass-deprecation). Format: timestamp +
// active/deprecated counts + raw entries (including ~~OBSOLETO~~ markers).
// Caller decides whether write failure aborts the destructive op.
//
// Born from 2026-05-13 7-directive drift incident — git history of the
// .md file was the only recovery option since .neo/db/ is gitignored.
func (wal *WAL) SnapshotDirectives(snapshotPath string) error {
	rules, err := wal.GetDirectives()
	if err != nil {
		return fmt.Errorf("SnapshotDirectives: read: %w", err)
	}
	active, deprecated := 0, 0
	for _, r := range rules {
		if strings.HasPrefix(r, "~~OBSOLETO~~") {
			deprecated++
		} else {
			active++
		}
	}
	payload := struct {
		SnapshotAtUnix  int64    `json:"snapshot_at_unix"`
		ActiveCount     int      `json:"active_count"`
		DeprecatedCount int      `json:"deprecated_count"`
		Directives      []string `json:"directives"`
	}{
		SnapshotAtUnix:  time.Now().Unix(),
		ActiveCount:     active,
		DeprecatedCount: deprecated,
		Directives:      rules,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("SnapshotDirectives: marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0755); err != nil {
		return fmt.Errorf("SnapshotDirectives: mkdir: %w", err)
	}
	return os.WriteFile(snapshotPath, data, 0600)
}

// RestoreDirectivesFromSnapshot reads a JSON snapshot (produced by
// SnapshotDirectives) and adds entries to BoltDB that are NOT already
// present (by normalized text). Returns the count of entries added.
//
// Conservative semantics: does NOT delete or modify existing BoltDB
// entries; does NOT re-activate ~~OBSOLETO~~ entries from the snapshot
// (operator can update those explicitly if intended). Only fills gaps
// — closes the snapshot loop so the file produced by SnapshotDirectives
// is usable, not write-only.
func (wal *WAL) RestoreDirectivesFromSnapshot(snapshotPath string) (int, error) {
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		return 0, fmt.Errorf("RestoreDirectivesFromSnapshot: read: %w", err)
	}
	var payload struct {
		Directives []string `json:"directives"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return 0, fmt.Errorf("RestoreDirectivesFromSnapshot: parse: %w", err)
	}
	existing, _ := wal.GetDirectives()
	existingSet := make(map[string]struct{}, len(existing))
	for _, r := range existing {
		existingSet[normalizeDirective(r)] = struct{}{}
	}
	added := 0
	for _, line := range payload.Directives {
		if strings.HasPrefix(line, "~~OBSOLETO~~") {
			continue
		}
		norm := normalizeDirective(line)
		if norm == "" {
			continue
		}
		if _, exists := existingSet[norm]; exists {
			continue
		}
		if err := wal.SaveDirective(line); err != nil {
			return added, fmt.Errorf("RestoreDirectivesFromSnapshot: save: %w", err)
		}
		existingSet[norm] = struct{}{}
		added++
	}
	return added, nil
}

// CompactDirectives hard-purges all ~~OBSOLETO~~ entries and deduplicates
// the active set. Tag-based dedup keeps the LAST (most recent) entry when
// multiple entries share the same leading [TAG]. Returns (removed, kept).
//
// DESTRUCTIVE. Callers should invoke SnapshotDirectives first for
// recovery — see handleCompactDirectives in cmd/neo-mcp/tools.go.
func (wal *WAL) CompactDirectives() (int, int, error) {
	var removed, kept int
	err := wal.db.Batch(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketDirectives)
		if bucket == nil {
			return fmt.Errorf("bucket_directives not found")
		}
		key := []byte("global_rules")
		var rules []string
		if data := bucket.Get(key); data != nil {
			if err := json.Unmarshal(data, &rules); err != nil {
				return err
			}
		}
		// Pass 1: drop deprecated and empty.
		active := make([]string, 0, len(rules))
		for _, r := range rules {
			if strings.Contains(r, "~~OBSOLETO~~") || strings.TrimSpace(r) == "" {
				removed++
				continue
			}
			active = append(active, r)
		}
		// Pass 2: tag-based dedup — keep last occurrence per tag.
		tagIndex := make(map[string]int, len(active))
		for i, r := range active {
			tag := extractDirectiveTag(r)
			if tag != "" {
				if prev, exists := tagIndex[tag]; exists {
					active[prev] = ""
					removed++
				}
				tagIndex[tag] = i
			}
		}
		// Pass 3: exact dedup on remaining entries.
		seen := make(map[string]struct{}, len(active))
		compacted := make([]string, 0, len(active))
		for _, r := range active {
			if r == "" {
				continue
			}
			norm := strings.TrimSpace(r)
			if _, dup := seen[norm]; dup {
				removed++
				continue
			}
			seen[norm] = struct{}{}
			compacted = append(compacted, r)
		}
		kept = len(compacted)
		return sre.ZeroAllocJSONMarshal(compacted, func(data []byte) error {
			return bucket.Put(key, data)
		})
	})
	return removed, kept, err
}

// extractDirectiveTag returns the leading [TAG] from a directive string,
// e.g. "[SRE-BRIEFING]" or "[SCOPE: GLOBAL] TOKEN-BUDGET:". Returns
// empty string if no tag is found.
func extractDirectiveTag(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 0 || s[0] != '[' {
		return ""
	}
	end := strings.Index(s, "]")
	if end < 0 {
		return ""
	}
	tag := s[:end+1]
	// For "[SCOPE: X] NAME:" patterns, include the NAME part as the key.
	rest := strings.TrimSpace(s[end+1:])
	if colon := strings.Index(rest, ":"); colon > 0 && colon < 40 {
		tag += " " + rest[:colon]
	}
	return tag
}

func (wal *WAL) Vacuum(ctx context.Context, workspaceRoot string, ignoreDirs []string) (int, error) {
	deletedCount := 0

	err := wal.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketDocs)
		if bucket == nil {
			return fmt.Errorf("bucket_docs not found")
		}

		c := bucket.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			var meta DocMeta
			if err := json.Unmarshal(v, &meta); err == nil {
				filePath := meta.Path
				shouldPurge := false

				_, statErr := os.Stat(filePath)
				if os.IsNotExist(statErr) {
					shouldPurge = true
				}

				if !shouldPurge {
					for _, dir := range ignoreDirs {
						if dir != "" && strings.Contains(filePath, string(os.PathSeparator)+dir+string(os.PathSeparator)) {
							shouldPurge = true
							break
						}
					}
				}

				if shouldPurge {
					if err := c.Delete(); err != nil {
						log.Printf("[SRE-WARN] Vacuum could not delete ghost file %s: %v", filePath, err)
					} else {
						deletedCount++
					}
				}
			}
		}
		return nil
	})

	return deletedCount, err
}

func (w *WAL) Sync() error {
	return w.db.Sync()
}

func (w *WAL) Close() error {
	return w.db.Close()
}

// --- Session State (Épica 78/79) ---

var bucketSessionState = []byte("session_state")

// GetSessionMutations returns the paths certified in the given session.
// Returns nil (no error) if the bucket or key doesn't exist yet.
func (wal *WAL) GetSessionMutations(sessionID string) ([]string, error) {
	var paths []string
	err := wal.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSessionState)
		if b == nil {
			return nil
		}
		v := b.Get([]byte(sessionID))
		if v == nil {
			return nil
		}
		return json.Unmarshal(v, &paths)
	})
	return paths, err
}

// sessionPathAnchoredInWorkspace is the defense-in-depth twin of
// cmd/neo-mcp.isPathInWorkspace — kept local to pkg/rag to avoid a circular
// import. Returns true when path resolves inside workspace after both are
// absolutized + cleaned. [Épica 330.L]
func sessionPathAnchoredInWorkspace(workspace, path string) bool {
	absWs, err := filepath.Abs(workspace)
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(absWs), filepath.Clean(absPath))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// AppendSessionCertified records a certified file path into the session_state bucket.
// Idempotent — duplicate paths are deduplicated. Also writes a "<sessionID>:ts"
// meta-key with the current unix timestamp so PurgeOldSessions can age-out old
// sessions during Vacuum_Memory. [SRE-108.B fix]
//
// [Épica 330.L] Defense-in-depth ownership check: sessionID format is
// "<workspace>|<boot-unix>". If path doesn't anchor inside <workspace>, reject
// silently with log — prevents BoltDB pollution even if a future caller
// forgets the certifyOneFile-level guard.
func (wal *WAL) AppendSessionCertified(sessionID, path string) error {
	if idx := strings.Index(sessionID, "|"); idx > 0 {
		workspace := sessionID[:idx]
		if !sessionPathAnchoredInWorkspace(workspace, path) {
			log.Printf("[SRE-OWN] WAL reject cross-workspace path: workspace=%s path=%s", workspace, path)
			return nil
		}
	}
	return wal.db.Batch(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketSessionState)
		if err != nil {
			return err
		}
		var existing []string
		if v := b.Get([]byte(sessionID)); v != nil {
			_ = json.Unmarshal(v, &existing)
		}
		for _, p := range existing {
			if p == path {
				return nil // already recorded
			}
		}
		existing = append(existing, path)
		if err := sre.ZeroAllocJSONMarshal(existing, func(data []byte) error {
			return b.Put([]byte(sessionID), data)
		}); err != nil {
			return err
		}
		// [SRE-108.B] Write timestamp meta-key for PurgeOldSessions.
		tsKey := sessionID + ":ts"
		return sre.ZeroAllocJSONMarshal(time.Now().Unix(), func(data []byte) error {
			return b.Put([]byte(tsKey), data)
		})
	})
}

// PurgeForeignSessionPaths scrubs session_state entries of paths NOT anchored
// in workspace. One-shot cleanup for buckets polluted BEFORE the ownership
// guard (Épica 330.L) was in place — idempotent, safe to call at every boot.
//
// For each session key (non-":ts"), rewrites its JSON value with only the
// owned paths. If the filtered list is empty, the session key is preserved
// (may still have a valid ":ts") — PurgeOldSessions will age it out.
//
// Returns the number of foreign paths removed.
func (wal *WAL) PurgeForeignSessionPaths(workspace string) (int, error) {
	if workspace == "" {
		return 0, nil
	}
	absWs, err := filepath.Abs(workspace)
	if err != nil {
		return 0, err
	}
	absWs = filepath.Clean(absWs)

	var removed int
	err = wal.db.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSessionState)
		if b == nil {
			return nil
		}
		type rewrite struct {
			key  []byte
			data []byte
		}
		var toRewrite []rewrite

		_ = b.ForEach(func(k, v []byte) error {
			key := string(k)
			if strings.HasSuffix(key, ":ts") {
				return nil
			}
			// Only scrub sessions that belong to THIS workspace (sessionID =
			// "<workspace>|<unix>"). Foreign sessions — if any — are another
			// process's problem to clean.
			if idx := strings.Index(key, "|"); idx > 0 {
				sessionWS := key[:idx]
				absSessionWS, _ := filepath.Abs(sessionWS)
				if filepath.Clean(absSessionWS) != absWs {
					return nil
				}
			}
			var paths []string
			if jerr := json.Unmarshal(v, &paths); jerr != nil {
				return nil
			}
			clean := paths[:0]
			for _, p := range paths {
				if sessionPathAnchoredInWorkspace(absWs, p) {
					clean = append(clean, p)
				} else {
					removed++
				}
			}
			if len(clean) == len(paths) {
				return nil // no-op
			}
			data, mErr := json.Marshal(clean)
			if mErr != nil {
				return nil
			}
			// Defer actual writes to after ForEach (mutations during iteration
			// are unsafe in bbolt).
			keyCopy := make([]byte, len(k))
			copy(keyCopy, k)
			toRewrite = append(toRewrite, rewrite{key: keyCopy, data: data})
			return nil
		})

		for _, r := range toRewrite {
			if werr := b.Put(r.key, r.data); werr != nil {
				return werr
			}
		}
		return nil
	})
	return removed, err
}

// peer_session_state bucket stores LRU lists of mutations from peer workspaces. [335.A]
var bucketPeerSession = []byte("peer_session_state")

// peerSessionEntry is one peer mutation record. [335.A]
type peerSessionEntry struct {
	File        string `json:"file"`
	CertifiedAt int64  `json:"certified_at"`
}

// StorePeerSessionMutation records a file certified by a peer workspace.
// Keeps at most cap entries per peer (LRU — oldest dropped first). [335.A]
func (wal *WAL) StorePeerSessionMutation(peerWSID, file string, certifiedAt int64, cap int) error {
	if cap <= 0 {
		cap = 50
	}
	return wal.db.Batch(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketPeerSession)
		if err != nil {
			return err
		}
		var entries []peerSessionEntry
		if v := b.Get([]byte(peerWSID)); v != nil {
			_ = json.Unmarshal(v, &entries)
		}
		// Deduplicate by file.
		for i, e := range entries {
			if e.File == file {
				entries[i].CertifiedAt = certifiedAt
				return sre.ZeroAllocJSONMarshal(entries, func(data []byte) error {
					return b.Put([]byte(peerWSID), data)
				})
			}
		}
		entries = append(entries, peerSessionEntry{File: file, CertifiedAt: certifiedAt})
		// LRU: keep only the cap most recent entries.
		if len(entries) > cap {
			entries = entries[len(entries)-cap:]
		}
		return sre.ZeroAllocJSONMarshal(entries, func(data []byte) error {
			return b.Put([]byte(peerWSID), data)
		})
	})
}

// GetAllPeerSessionMutations returns a map of peerWSID → []file for all peers. [335.A]
func (wal *WAL) GetAllPeerSessionMutations() (map[string][]string, error) {
	result := map[string][]string{}
	err := wal.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketPeerSession)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var entries []peerSessionEntry
			if jsonErr := json.Unmarshal(v, &entries); jsonErr != nil {
				return nil
			}
			files := make([]string, len(entries))
			for i, e := range entries {
				files[i] = e.File
			}
			result[string(k)] = files
			return nil
		})
	})
	return result, err
}

// PurgeOldSessions removes session_state entries older than maxAge.
// Called by Vacuum_Memory (Épica 79.4).
func (wal *WAL) PurgeOldSessions(maxAge time.Duration) error {
	cutoff := time.Now().Add(-maxAge).Unix()
	return wal.db.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSessionState)
		if b == nil {
			return nil
		}
		// Meta-key "<sessionID>:ts" holds the unix creation timestamp.
		return b.ForEach(func(k, v []byte) error {
			key := string(k)
			if strings.HasSuffix(key, ":ts") {
				return nil // skip meta keys
			}
			tsKey := key + ":ts"
			tsBytes := b.Get([]byte(tsKey))
			if tsBytes == nil {
				return nil // no timestamp — don't purge
			}
			var ts int64
			if err := json.Unmarshal(tsBytes, &ts); err == nil && ts < cutoff {
				_ = b.Delete(k)
				_ = b.Delete([]byte(tsKey))
			}
			return nil
		})
	})
}

// agentIDKey is the stable key for the session_agent_id in session_state. [336.A]
const agentIDKey = "__session_agent_id__"

// SetSessionAgentID persists the MCP session identity to the session_state bucket. [336.A]
// Format: "<workspace-id>:<boot-unix>:<client-name@version>"
func (wal *WAL) SetSessionAgentID(id string) error {
	return wal.db.Batch(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketSessionState)
		if err != nil {
			return err
		}
		return b.Put([]byte(agentIDKey), []byte(id))
	})
}

// GetSessionAgentID returns the stored session_agent_id, or "" if not set. [336.A]
func (wal *WAL) GetSessionAgentID() (string, error) {
	var id string
	err := wal.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSessionState)
		if b == nil {
			return nil
		}
		if v := b.Get([]byte(agentIDKey)); v != nil {
			id = string(v)
		}
		return nil
	})
	return id, err
}

// --- Remediation Tracking (Épica 88.B.3) ---

var bucketRemediations = []byte("remediations")

// RemediationRecord tracks an auto-remediation suggestion or application. [SRE-88.B.3]
type RemediationRecord struct {
	SessionID   string `json:"session_id"`
	File        string `json:"file"`
	Rule        string `json:"rule"`         // MAKE_IN_LOOP, INTERFACE_ANY, etc.
	AutoApplied bool   `json:"auto_applied"`
	At          int64  `json:"at"`           // unix timestamp
}

// AppendRemediation records a remediation event in BoltDB. [SRE-88.B.3]
func (wal *WAL) AppendRemediation(rec RemediationRecord) error {
	return wal.db.Batch(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketRemediations)
		if err != nil {
			return err
		}
		key := fmt.Sprintf("%s:%s:%d", rec.SessionID, rec.File, rec.At)
		return sre.ZeroAllocJSONMarshal(rec, func(data []byte) error {
			return b.Put([]byte(key), data)
		})
	})
}

// GetSessionRemediations returns all remediations for a session. [SRE-88.B.3]
func (wal *WAL) GetSessionRemediations(sessionID string) ([]RemediationRecord, error) {
	var records []RemediationRecord
	err := wal.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketRemediations)
		if b == nil {
			return nil
		}
		prefix := []byte(sessionID + ":")
		return b.ForEach(func(k, v []byte) error {
			if len(k) >= len(prefix) && string(k[:len(prefix)]) == string(prefix) {
				var rec RemediationRecord
				if err := json.Unmarshal(v, &rec); err == nil {
					records = append(records, rec)
				}
			}
			return nil
		})
	})
	return records, err
}
