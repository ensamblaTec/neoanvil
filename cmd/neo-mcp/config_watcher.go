package main

// config_watcher.go — Hot-reload of safe neo.yaml fields via fsnotify.
//
// Only "safe" fields (inference, governance, sre thresholds) are reloaded at
// runtime. Unsafe fields (ports, db paths, certs, provider) remain pinned to
// their boot-time values — changing them requires a full restart.
//
// Usage: call WatchConfig once from main() after loading the boot config.
// Components read hot values via LiveConfig(bootCfg) instead of cfg directly.

import (
	"context"
	"log"
	"path/filepath"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/pubsub"
	"github.com/ensamblatec/neoanvil/pkg/rag"
)

// liveConfigPtr holds the latest atomically swapped hot-reloaded config.
// nil until the first successful reload after WatchConfig starts.
var liveConfigPtr atomic.Pointer[config.NeoConfig]

// LiveConfig returns the latest hot-reloaded config if available,
// falling back to bootCfg for unsafe fields not yet reloaded.
func LiveConfig(bootCfg *config.NeoConfig) *config.NeoConfig {
	if p := liveConfigPtr.Load(); p != nil {
		return p
	}
	return bootCfg
}

// CacheResizer is the minimal interface WatchConfig needs to retune a
// cache layer on hot reload. Both QueryCache, TextCache, and Cache[T]
// satisfy it via their Resize + Capacity methods. [Épica 206]
type CacheResizer interface {
	Resize(int)
	Capacity() int
}

// WatchConfig starts a background fsnotify watcher on configPath.
// On Write/Create events, safe fields are reloaded atomically and
// EventConfigReloaded is published to the SSE bus.
// Must be called once from main() after the boot config is loaded.
// [Épica 206] Extra args let the watcher also Resize the RAG caches
// when neo.yaml's query_cache_capacity or embedding_cache_capacity
// change — no restart required.
func WatchConfig(ctx context.Context, configPath string, bootCfg *config.NeoConfig, bus *pubsub.Bus,
	queryCache *rag.QueryCache, textCache *rag.TextCache, embCache *rag.Cache[[]float32], sg *rag.SharedGraph) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[CONFIG-WATCH] fsnotify unavailable: %v — hot-reload disabled", err)
		return
	}
	// [Épica 229.4b] Watch the PARENT directory, not the file itself.
	// Watching the file by inode breaks on every editor that writes via
	// temp-file + rename (sed -i, vim :w, most IDEs) — the old inode is
	// unlinked and events stop firing even if we re-add on Rename/Remove.
	// Directory watch sees Create/Write events regardless of how the new
	// content arrives. We filter by basename so we only react to neo.yaml.
	watchDir := filepath.Dir(configPath)
	watchBase := filepath.Base(configPath)
	if err := w.Add(watchDir); err != nil {
		log.Printf("[CONFIG-WATCH] cannot watch %s: %v — hot-reload disabled", watchDir, err)
		_ = w.Close()
		return
	}
	log.Printf("[CONFIG-WATCH] hot-reload active: %s (dir=%s)", configPath, watchDir)

	go func() {
		defer w.Close() //nolint:errcheck
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				// Filter: only care about events targeting our config file.
				if filepath.Base(ev.Name) != watchBase {
					continue
				}
				if !ev.Has(fsnotify.Write) && !ev.Has(fsnotify.Create) {
					continue
				}
				fresh, loadErr := config.LoadConfig(configPath)
				if loadErr != nil {
					log.Printf("[CONFIG-WATCH] reload parse error: %v", loadErr)
					continue
				}
				// Build a merged copy: start from boot values, overwrite only safe fields.
				merged := *bootCfg
				merged.Inference               = fresh.Inference
				merged.Governance              = fresh.Governance
				merged.Sentinel                = fresh.Sentinel
				merged.Cognitive.Strictness    = fresh.Cognitive.Strictness
				merged.SRE.SafeCommands         = fresh.SRE.SafeCommands
				merged.SRE.UnsupervisedMaxCycles = fresh.SRE.UnsupervisedMaxCycles
				merged.SRE.KineticMonitoring    = fresh.SRE.KineticMonitoring
				merged.SRE.KineticThreshold     = fresh.SRE.KineticThreshold
				merged.SRE.DigitalTwinTesting   = fresh.SRE.DigitalTwinTesting
				merged.SRE.ConsensusEnabled     = fresh.SRE.ConsensusEnabled
				merged.SRE.ConsensusQuorum      = fresh.SRE.ConsensusQuorum
				// [Épica 206] Cache capacities are safe to retune live.
				merged.RAG.QueryCacheCapacity     = fresh.RAG.QueryCacheCapacity
				merged.RAG.EmbeddingCacheCapacity = fresh.RAG.EmbeddingCacheCapacity
				// [Épica 229.4] CPG OOM limit — consumed live by cpg.Manager
				// via MaxHeapMBFn closure in main.go. Updating here lets a
				// neo.yaml edit take effect on the next Graph() call.
				merged.CPG.MaxHeapMB = fresh.CPG.MaxHeapMB
				liveConfigPtr.Store(&merged)

				// [287.G] Prune shared graph entries for workspaces removed from project.
				if sg != nil && fresh.Project != nil {
					knownIDs := make(map[string]struct{}, len(fresh.Project.MemberWorkspaces))
					for _, ws := range fresh.Project.MemberWorkspaces {
						knownIDs[ws] = struct{}{}
					}
					if removed, pruneErr := sg.Prune(knownIDs); pruneErr != nil {
						log.Printf("[287.G] shared graph prune error: %v", pruneErr)
					} else if removed > 0 {
						log.Printf("[287.G] shared graph pruned %d entries", removed)
					}
				}

				// [Épica 206] Apply capacity deltas immediately. Resize is
				// O(1) grow, O(evicted) shrink. Log only when the value
				// actually changed so noise stays minimal.
				cacheDeltas := map[string]any{}
				if queryCache != nil {
					newCap := fresh.RAG.QueryCacheCapacity
					if oldCap := queryCache.Capacity(); oldCap != newCap {
						queryCache.Resize(newCap)
						cacheDeltas["query_cache"] = []int{oldCap, newCap}
						log.Printf("[CONFIG-WATCH] query cache resized %d → %d", oldCap, newCap)
					}
				}
				if textCache != nil {
					newCap := fresh.RAG.QueryCacheCapacity // shares capacity with query cache
					if oldCap := textCache.Capacity(); oldCap != newCap {
						textCache.Resize(newCap)
						cacheDeltas["text_cache"] = []int{oldCap, newCap}
						log.Printf("[CONFIG-WATCH] text cache resized %d → %d", oldCap, newCap)
					}
				}
				if embCache != nil {
					newCap := fresh.RAG.EmbeddingCacheCapacity
					if oldCap := embCache.Capacity(); oldCap != newCap {
						embCache.Resize(newCap)
						cacheDeltas["embedding_cache"] = []int{oldCap, newCap}
						log.Printf("[CONFIG-WATCH] embedding cache resized %d → %d", oldCap, newCap)
					}
				}

				payload := map[string]any{
					"path":         configPath,
					"ghost_mode":   fresh.Governance.GhostMode,
					"offline_mode": fresh.Inference.OfflineMode,
					"consensus":    fresh.SRE.ConsensusEnabled,
				}
				if len(cacheDeltas) > 0 {
					payload["cache_deltas"] = cacheDeltas
				}
				bus.Publish(pubsub.Event{
					Type:    pubsub.EventConfigReloaded,
					Payload: payload,
				})
				log.Printf("[CONFIG-WATCH] reloaded — ghost=%v offline=%v consensus=%v",
					fresh.Governance.GhostMode,
					fresh.Inference.OfflineMode,
					fresh.SRE.ConsensusEnabled,
				)

			case wErr, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Printf("[CONFIG-WATCH] watcher error: %v", wErr)
			}
		}
	}()
}
