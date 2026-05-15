// cmd/neo-mcp/cache_setup.go — helper that bundles the PILAR XXV cache
// construction + warm-load sequence so main.go keeps its boot story
// readable. [PILAR-XXV/221]
//
// Lifts ~40 lines of per-cache boilerplate out of main() and consolidates
// the three snapshot-path constants + the log line format into one place.
// Mirrors the pattern used by `mustRegister` — main declares the variables,
// this helper does the heavy lifting.

package main

import (
	"log"
	"path/filepath"

	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/rag"
)

// cacheStack holds the three RAG caches wired together at boot.
// Returned by setupCaches so callers can pass the bundle around (or
// individual fields) instead of 3 separate args.
type cacheStack struct {
	query    *rag.QueryCache
	text     *rag.TextCache
	emb      *rag.Cache[[]float32]
	hotFiles *rag.HotFilesCache // [LARGE-PROJECT/A] optional; set by main when RadarTool wires it
}

// setupCaches constructs all three cache layers from cfg.RAG capacities,
// then attempts to warm-load each from its snapshot file. Snapshot loads
// are non-fatal — boot never blocks on cache warmup. Returns the bundle
// for callers that want to pass it as a unit (main uses individual
// fields via s.query / s.text / s.emb).
//
// currentGen is Graph.Gen.Load() at the time of wiring — the caller owns
// that pointer's lifetime.
func setupCaches(cfg *config.NeoConfig, workspace string, currentGen uint64) cacheStack {
	s := cacheStack{
		query: rag.NewQueryCache(cfg.RAG.QueryCacheCapacity),
		text:  rag.NewTextCache(cfg.RAG.QueryCacheCapacity),
		emb:   rag.NewCache[[]float32](cfg.RAG.EmbeddingCacheCapacity),
	}

	warm := func(label string, loader func(string, uint64) (int, error), rel string) {
		path := filepath.Join(workspace, rel)
		n, err := loader(path, currentGen)
		if err != nil {
			log.Printf("[BOOT] %s cache snapshot load failed: %v (continuing cold)", label, err)
			return
		}
		if n > 0 {
			log.Printf("[BOOT] %s cache warm-loaded %d entries from snapshot", label, n)
		}
	}

	warm("query", s.query.LoadSnapshot, cacheSnapshotRelPath)
	warm("text", s.text.LoadSnapshot, textCacheSnapshotRelPath)
	warm("embedding", func(path string, gen uint64) (int, error) {
		return s.emb.LoadSnapshotJSON(path, gen, "EMBED|")
	}, embCacheSnapshotRelPath)
	// hot_files is wired AFTER RadarTool init in main (caller mutates s.hotFiles
	// then invokes warmHotFilesCacheSnapshot) — the cache instance does not
	// exist at this point in the boot sequence.

	// [Phase 0.D / Speed-First] symbolMapCache (radar_compile.go package var)
	// — not part of cacheStack because it's a package-level memo, but the
	// boot warm-load belongs alongside the other snapshots. Self-invalidating
	// via aggregated-mtime keys, so stale entries cannot serve incorrect data.
	if n, err := loadSymbolMapSnapshot(filepath.Join(workspace, symbolMapSnapshotRelPath)); err != nil {
		log.Printf("[BOOT] symbol_map cache snapshot load failed: %v (continuing cold)", err)
	} else if n > 0 {
		log.Printf("[BOOT] symbol_map cache warm-loaded %d package(s) from snapshot", n)
	}

	return s
}

// warmHotFilesCacheSnapshot loads the on-disk hot-files snapshot into
// the provided cache, if any entries' mtime+size still match disk state.
// Called by main() AFTER RadarTool is constructed (since hotFiles lives
// on RadarTool, not in the early cacheStack). [LARGE-PROJECT/A 2026-05-13]
func warmHotFilesCacheSnapshot(cache *rag.HotFilesCache, workspace string) {
	if cache == nil {
		return
	}
	path := filepath.Join(workspace, hotFilesCacheSnapshotRelPath)
	n, err := cache.LoadSnapshotJSON(path)
	if err != nil {
		log.Printf("[BOOT] hot_files cache snapshot load failed: %v (continuing cold)", err)
		return
	}
	if n > 0 {
		log.Printf("[BOOT] hot_files cache warm-loaded %d entries from snapshot", n)
	}
}

// persistCachesOnShutdown writes all three snapshots to disk. Fixed N
// per layer (query=32, text=16, embedding=64) — operators who want a
// different split should call neo_cache_persist explicitly before
// signalling shutdown. Failures are logged, never blocking — caches
// are a latency optimisation, not a durability requirement. [Épica 222]
func persistCachesOnShutdown(s cacheStack, workspace string) {
	type persistOp struct {
		label string
		fn    func() error
		path  string
	}
	ops := []persistOp{
		{"query", func() error {
			return s.query.SaveSnapshot(filepath.Join(workspace, cacheSnapshotRelPath), 32)
		}, filepath.Join(workspace, cacheSnapshotRelPath)},
		{"text", func() error {
			return s.text.SaveSnapshot(filepath.Join(workspace, textCacheSnapshotRelPath), 16)
		}, filepath.Join(workspace, textCacheSnapshotRelPath)},
		{"embedding", func() error {
			return s.emb.SaveSnapshotJSON(filepath.Join(workspace, embCacheSnapshotRelPath), 64)
		}, filepath.Join(workspace, embCacheSnapshotRelPath)},
	}
	if s.hotFiles != nil {
		ops = append(ops, persistOp{"hot_files", func() error {
			return s.hotFiles.SaveSnapshotJSON(filepath.Join(workspace, hotFilesCacheSnapshotRelPath), 64)
		}, filepath.Join(workspace, hotFilesCacheSnapshotRelPath)})
	}
	// [Phase 0.D / Speed-First] symbolMapCache snapshot — saved unconditionally
	// (no top-N trim; the cache is bounded by # of packages × # of source files,
	// not by query volume). Mirror of the load wired in setupCaches.
	ops = append(ops, persistOp{"symbol_map", func() error {
		_, err := saveSymbolMapSnapshot(filepath.Join(workspace, symbolMapSnapshotRelPath))
		return err
	}, filepath.Join(workspace, symbolMapSnapshotRelPath)})
	for _, op := range ops {
		if err := op.fn(); err != nil {
			log.Printf("[SHUTDOWN] %s cache persist failed: %v", op.label, err)
		} else {
			log.Printf("[SHUTDOWN] %s cache persisted → %s", op.label, op.path)
		}
	}
}
