package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"log"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/astx"
	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/incidents"
	"github.com/ensamblatec/neoanvil/pkg/kanban"
	"github.com/ensamblatec/neoanvil/pkg/pubsub"
	"github.com/ensamblatec/neoanvil/pkg/rag"
	"github.com/ensamblatec/neoanvil/pkg/sre"
	"github.com/ensamblatec/neoanvil/pkg/tensorx"
	workspacereg "github.com/ensamblatec/neoanvil/pkg/workspace"
)

// extractGoImports parses Go import declarations from content. [SRE-123.A]
func extractGoImports(content string) []string {
	var results []string
	lines := strings.Split(content, "\n")
	inBlock := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "import (" {
			inBlock = true
			continue
		}
		if inBlock && line == ")" {
			inBlock = false
			continue
		}
		if inBlock || strings.HasPrefix(line, "import ") {
			if start := strings.Index(line, `"`); start != -1 {
				if end := strings.Index(line[start+1:], `"`); end != -1 {
					results = append(results, line[start+1:start+1+end])
				}
			}
		}
	}
	return results
}

func extractImports(content string, ext string) []string {
	var results []string
	switch ext {
	case ".go":
		results = extractGoImports(content)
	case ".ts", ".js", ".tsx", ".jsx":
		matches := jsImportRegex.FindAllStringSubmatch(content, -1)
		for _, m := range matches {
			if len(m) > 1 {
				results = append(results, m[1])
			}
		}
	case ".py":
		matches := pyImportRegex.FindAllStringSubmatch(content, -1)
		for _, m := range matches {
			if len(m) > 1 {
				results = append(results, m[1])
			}
		}
	}
	return results
}

func recordTechDebt(workspace, title, description, priority string) {
	if err := kanban.AppendTechDebt(workspace, title, description, priority); err != nil {
		log.Printf("[SRE-DEBT] Failed to record debt: %v", err)
	}
}

func setupLogging(workspace string) (*os.File, *sre.TriageEngine) {
	logsPath := filepath.Join(workspace, ".neo", "logs")

	_ = os.MkdirAll(logsPath, 0755)
	logFile, err := os.OpenFile(filepath.Join(logsPath, "mcp.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		log.SetOutput(os.Stderr)
		log.Println("[SRE-WARN] Failed to open log file, using only stderr")
		return nil, nil
	}
	// NUEVO: Instanciar el Auto-Triage con un buffer histórico de 150 líneas
	triageEngine := sre.NewTriageEngine(workspace, 150)

	// MultiWriter bifurca el output: a la terminal, al archivo físico, y al motor de IA SRE
	log.SetOutput(io.MultiWriter(os.Stderr, logFile, triageEngine))
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)
	return logFile, triageEngine
}

func fnvHash64_Chunk(path string, chunkIndex int) uint64 {
	h := fnv.New64a()
	h.Write([]byte(path))
	h.Write(fmt.Appendf(nil, "_%d", chunkIndex))
	return h.Sum64()
}

func isSupportedExt(ext string, allowed []string) bool {
	return slices.Contains(allowed, ext)
}

// bootPostIncidentTasks wraps the two async boot-tail tasks (HNSW index retry +
// Nexus debt check) into a single main()-visible call. Keeps the entrypoint CC
// bounded while grouping operations that must run after BM25 indexing but
// before the MCP server starts serving traffic. [353.A]
func bootPostIncidentTasks(ctx context.Context, workspace string, cfg *config.NeoConfig,
	embedder rag.Embedder, graph *rag.Graph, wal *rag.WAL, cpu tensorx.ComputeDevice, bus *pubsub.Bus) {
	go retryIndexIncidents(ctx, workspace, embedder, graph, wal, cpu)
	go checkNexusDebtAtBoot(ctx, workspace, cfg, bus)
}

// checkNexusDebtAtBoot queries Nexus for debt events affecting this workspace
// after neo-mcp finishes local init. Runs in a goroutine; completion is
// non-blocking. On detection: log.Printf warnings (P0 = SRE-WARN, P1+ = INFO)
// and publish EventNexusDebtWarning to the event bus so the HUD displays a
// banner. Graceful degradation: Nexus offline or debt disabled → silent. [353.A]
func checkNexusDebtAtBoot(_ context.Context, workspace string, cfg *config.NeoConfig, bus *pubsub.Bus) {
	if cfg == nil || bus == nil {
		return
	}
	wsID := lookupWorkspaceID(workspace)
	if wsID == "" {
		return
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.NexusDispatcherPort)
	url := fmt.Sprintf("%s/internal/nexus/debt/affecting?workspace_id=%s", base, wsID)
	client := sre.SafeInternalHTTPClient(2)
	resp, err := client.Get(url) //nolint:gosec // G107-TRUSTED-CONFIG-URL: nexus base from cfg
	if err != nil || resp == nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return // 404 = debt disabled; other codes = no-op
	}
	var events []struct {
		ID       string `json:"id"`
		Priority string `json:"priority"`
		Title    string `json:"title"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil || len(events) == 0 {
		return
	}
	p0 := 0
	for _, e := range events {
		if e.Priority == "P0" {
			p0++
		}
	}
	if p0 > 0 {
		log.Printf("[SRE-WARN] Nexus reports %d P0 debt affecting this workspace — see BRIEFING", p0)
	} else {
		log.Printf("[SRE-INFO] Nexus reports %d debt events affecting this workspace", len(events))
	}
	bus.Publish(pubsub.Event{
		Type: pubsub.EventNexusDebtWarning,
		Payload: map[string]any{
			"count":        len(events),
			"p0_count":     p0,
			"workspace_id": wsID,
			"events":       events,
		},
	})
}

// lookupWorkspaceID resolves the Nexus workspace ID for a given absolute path
// by scanning ~/.neo/workspaces.json. Returns empty string on miss. Used by
// tools (e.g. neo_debt) that need to self-identify for Nexus routing. [351.C]
func lookupWorkspaceID(workspace string) string {
	reg, err := workspacereg.LoadRegistry()
	if err != nil {
		return ""
	}
	for _, e := range reg.Workspaces {
		if e.Path == workspace {
			return e.ID
		}
	}
	return ""
}

func findNeoProjectDir(startDir string) string {
	const maxWalk = 5
	dir := startDir
	for i := 0; i < maxWalk; i++ {
		candidate := filepath.Join(dir, ".neo-project")
		neoYAML := filepath.Join(candidate, "neo.yaml")
		if _, err := os.Stat(neoYAML); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func retryIndexIncidents(ctx context.Context, workspace string, embedder rag.Embedder, graph *rag.Graph, wal *rag.WAL, cpu tensorx.ComputeDevice) {
	if incidents.CountIncidentFiles(workspace) == 0 {
		return
	}
	delays := []time.Duration{0, time.Minute, 5 * time.Minute, 15 * time.Minute}
	for _, delay := range delays {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
		indexCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		incidents.IndexIncidents(indexCtx, workspace, embedder, graph, wal, cpu)
		cancel()
		if incidents.IndexedCount() > 0 || incidents.SkippedCount() > 0 {
			return
		}
		log.Printf("[INC-RETRY] embedder may be unavailable — next retry in %v", nextRetryDelay(delay))
	}
}

func nextRetryDelay(current time.Duration) time.Duration {
	switch current {
	case 0:
		return time.Minute
	case time.Minute:
		return 5 * time.Minute
	default:
		return 0
	}
}

func bootstrapWorkspace(ctx context.Context, workspace string, graph *rag.Graph, wal *rag.WAL, embedder rag.Embedder, cpu tensorx.ComputeDevice, lexicalIdx *rag.LexicalIndex, cfg *config.NeoConfig, jobs chan string, bus *pubsub.Bus) {
	log.Printf("[BOOTSTRAP] starting bulk ingestion for workspace: %s", workspace)

	// [SRE-36.1.3] Snapshot GC counter before ingestion starts.
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	// [SRE-36.1.2] vecPool recycles []float32 slabs between chunks of the same
	// file. After InsertBatch deep-copies data into graph.Vectors, the original
	// slices are safe to return here so the GC doesn't need to collect them.
	vecDim := embedder.Dimension()
	vecPool := &sync.Pool{
		New: func() any { s := make([]float32, 0, vecDim); return &s },
	}

	existingDocs := make(map[uint64]bool)
	for _, node := range graph.Nodes {
		existingDocs[node.DocID] = true
		_, content, _, err := wal.GetDocMeta(node.DocID)
		if err == nil {
			lexicalIdx.AddDocument(node.DocID, content)
		}
	}

	type indexPayload struct {
		docID   uint64
		vec     []float32
		path    string
		snippet string
	}
	results := make(chan indexPayload, 100)

	go func() {
		batchLimit := cfg.RAG.BatchSize
		if batchLimit <= 0 {
			batchLimit = 100
		}

		docIDs := make([]uint64, 0, batchLimit)
		vecs := make([][]float32, 0, batchLimit)
		snippets := make([]string, 0, batchLimit)
		paths := make([]string, 0, batchLimit)

		flush := func() {
			if len(docIDs) == 0 {
				return
			}
			if err := graph.InsertBatch(ctx, docIDs, vecs, 5, cpu, wal); err != nil {
				log.Printf("[SRE-WARN] batch insert failed: %v", err)
			} else {
				for i, id := range docIDs {
					lexicalIdx.AddDocument(id, snippets[i])
					if err := wal.SaveDocMeta(id, paths[i], snippets[i], 0); err != nil {
						log.Printf("[SRE-WARN] failed to save metadata for %d: %v", id, err)
					}
				}
			}

			// [SRE-36.1.2] InsertBatch deep-copies vecs into graph.Vectors; return
			// the original slices to the pool so they can be reused and avoid GC.
			for i := range vecs {
				s := vecs[i][:0]
				vecPool.Put(&s)
				vecs[i] = nil
			}

			docIDs = docIDs[:0]
			vecs = vecs[:0]
			snippets = snippets[:0]
			paths = paths[:0]
		}

		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case res, ok := <-results:
				if !ok {
					flush()
					return
				}

				if len(docIDs) == cap(docIDs) {
					flush()
				}

				docIDs = append(docIDs, res.docID)
				vecs = append(vecs, res.vec)
				snippets = append(snippets, res.snippet)
				paths = append(paths, res.path)

			case <-ticker.C:
				flush()
			case <-ctx.Done():
				flush()
				return
			}
		}
	}()

	workers := cfg.RAG.IngestionWorkers
	if workers <= 0 {
		workers = 1
	}

	// [SRE-EMBED-SEM] Semaphore that caps concurrent Ollama embed calls.
	// Separates I/O-bound workers (file read + chunking) from GPU-bound embed slots.
	// Without this, 12 workers fire simultaneously → Ollama HTTP 500 (queue overflow).
	embedConcurrency := cfg.RAG.OllamaConcurrency
	if embedConcurrency <= 0 {
		embedConcurrency = 4
	}
	embedSem := make(chan struct{}, embedConcurrency)
	log.Printf("[SRE] Unleashing %d concurrent workers for mass ingestion (Provider: %s, embed_slots: %d)", workers, cfg.AI.Provider, embedConcurrency)

	for w := 0; w < workers; w++ {
		go func(workerID int, jobsChan <-chan string, resChan chan<- indexPayload) {
			for path := range jobsChan {
				if ctx.Err() != nil {
					return
				}
				data, err := os.ReadFile(path)
				if err != nil {
					continue
				}

				ext := filepath.Ext(path)
				imports := extractImports(string(data), ext)
				for _, imp := range imports {
					if err := wal.SaveDependencies(imp, []string{path}); err != nil {
						log.Printf("[SRE-WARN] failed to link dependency %s -> %s: %v", imp, path, err)
					}
				}

				chunks := astx.SemanticChunk(ctx, data, ext)
				if len(chunks) == 0 {
					chunkSize := cfg.RAG.ChunkSize
					overlap := cfg.RAG.Overlap
					for start := 0; start < len(data); start += chunkSize - overlap {
						end := min(start+chunkSize, len(data))
						chunks = append(chunks, data[start:end])
					}
				}

				// [BATCH-EMBED-FAST-PATH] On healthy Ollama with 4+ chunks, a single
				// /api/embed (plural) call is 1.5-3.7× faster than the per-chunk
				// retry loop below. Try it first; on ANY error fall through to the
				// established per-chunk path so crash/busy/transient backoff is
				// preserved verbatim. Single-chunk files skip the batch attempt —
				// EmbedBatch short-circuits them anyway and sequential is the same
				// HTTP round-trip cost.
				if len(chunks) > 1 {
					texts := make([]string, len(chunks))
					for i, c := range chunks {
						texts[i] = string(c)
					}
					select {
					case embedSem <- struct{}{}:
					case <-ctx.Done():
						return
					}
					vecs, batchErr := rag.EmbedMany(ctx, embedder, texts)
					<-embedSem
					if batchErr == nil && len(vecs) == len(chunks) {
						for i, vec := range vecs {
							docID := fnvHash64_Chunk(path, i)
							select {
							case resChan <- indexPayload{docID: docID, vec: vec, path: path, snippet: texts[i]}:
							case <-ctx.Done():
								return
							}
						}
						continue
					}
					if batchErr != nil {
						log.Printf("[SRE-INFO] Worker %d: batch embed failed for %s (%d chunks): %v — falling back to per-chunk retry path", workerID, path, len(chunks), batchErr)
					}
				}

				for chunkIndex, chunkSlice := range chunks {

					if ctx.Err() != nil {
						return
					}

					// Acquire embed slot — blocks if embedConcurrency slots are in use.
					// Prevents HTTP 500 from Ollama when workers > embed capacity.
					select {
					case embedSem <- struct{}{}:
					case <-ctx.Done():
						return
					}

					var vec []float32
					var embedErr error
					maxRetries := 3
					crashStreak := 0 // [SRE-98.B] consecutive 500/502/504 responses

					// [SRE-98.A] Retry backoff tiers. Crash = Ollama process blew up
					// (OOM, segfault) — needs long recovery. Busy = queue saturated —
					// short backoff suffices. Transient = network hiccup, minimal delay.
					crashBackoff := []time.Duration{10 * time.Second, 30 * time.Second, 60 * time.Second}
					busyBackoff := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
					transientBackoff := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}

					for attempt := 1; attempt <= maxRetries; attempt++ {
						vec, embedErr = embedder.Embed(ctx, string(chunkSlice))
						if embedErr == nil {
							break
						}
						if attempt >= maxRetries {
							break
						}

						// [INC-20260424-133023] Permanent errors (HTTP 404 = model not
						// loaded, 400/401/403 = bad request/auth) will not recover by
						// retrying the same request. Skip the retry loop entirely so
						// the worker pool doesn't hammer Ollama with 87+ warnings per
						// minute. A single log line surfaces the issue once per chunk.
						if rag.IsPermanent(embedErr) {
							log.Printf("[SRE-WARN] Worker %d: permanent embed error on %s (chunk %d): %v — skipping (not retrying)",
								workerID, path, chunkIndex, embedErr)
							break
						}

						// [SRE-35-hotfix] Circuit breaker open: do NOT burn retry budget
						// with fast-fails (µs). Wait the full recovery window for the breaker
						// to transition to HalfOpen, then probe once. If still open, give up.
						if errors.Is(embedErr, sre.ErrCircuitOpen) {
							if attempt == 1 {
								log.Printf("[SRE-INFO] Circuit open for %s (chunk %d) — waiting %v for Ollama recovery...",
									path, chunkIndex, sre.BreakerResetTimeout)
								select {
								case <-ctx.Done():
									<-embedSem
									return
								case <-time.After(sre.BreakerResetTimeout + 2*time.Second):
								}
							} else {
								// Already waited once — Ollama still not recovered. Skip chunk.
								log.Printf("[SRE-WARN] Worker %d: circuit still open after recovery wait, skipping chunk %d of %s",
									workerID, chunkIndex, path)
								break
							}
							continue
						}

						// [SRE-98.A] Pick backoff tier by error class.
						idx := attempt - 1
						var base time.Duration
						var tier string
						switch {
						case rag.IsCrash(embedErr):
							base = crashBackoff[idx]
							tier = "crash"
							crashStreak++
						case rag.IsBusy(embedErr):
							base = busyBackoff[idx]
							tier = "busy"
							crashStreak = 0
						default:
							base = transientBackoff[idx]
							tier = "transient"
							crashStreak = 0
						}

						// [SRE-98.B] 3 consecutive crashes = Ollama likely OOM.
						// Surface to HUD via EventGCPressure so operator sees the alert.
						if crashStreak >= 3 && bus != nil {
							bus.Publish(pubsub.Event{
								Type: pubsub.EventGCPressure,
								Payload: map[string]any{
									"reason":    "embed_oom",
									"path":      path,
									"workspace": workspace,
									"attempts":  crashStreak,
								},
							})
							log.Printf("[SRE-ALERT] embed_oom suspected on %s after %d crash-class failures", path, crashStreak)
							crashStreak = 0 // throttle repeated alerts
						}

						jitter := time.Duration(rand.Int63n(int64(500 * time.Millisecond)))
						delay := base + jitter
						log.Printf("[SRE-INFO] Ollama %s on %s (%v). Retrying in %v (%d/%d)...", tier, path, embedErr, delay, attempt, maxRetries)
						select {
						case <-ctx.Done():
							<-embedSem
							log.Printf("[SRE-WARN] Worker %d cancelled during retries for %s", workerID, path)
							return
						case <-time.After(delay):
						}
					}

					<-embedSem // release slot regardless of success or failure

					if embedErr != nil {
						log.Printf("[SRE-WARN] Worker %d failed to embed chunk %d of %s after %d retries: %v", workerID, chunkIndex, path, maxRetries, embedErr)
						continue
					}

					docID := fnvHash64_Chunk(path, chunkIndex)
					resChan <- indexPayload{docID: docID, vec: vec, path: path, snippet: string(chunkSlice)}
				}
			}
		}(w, jobs, results)
	}

	var filesQueued int
	err := filepath.WalkDir(workspace, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			for _, ignore := range cfg.Workspace.IgnoreDirs {
				if name == ignore || name == ".neo" {
					return filepath.SkipDir
				}
			}
			return nil
		}

		ext := filepath.Ext(path)
		if !isSupportedExt(ext, cfg.Workspace.AllowedExtensions) {
			return nil
		}

		chunkZeroID := fnvHash64_Chunk(path, 0)
		if !existingDocs[chunkZeroID] {
			jobs <- path
			filesQueued++
		}
		return nil
	})

	if err != nil {
		log.Printf("[SRE-ERROR] walker failed: %v", err)
	}

	// [SRE-36.1.3] Measure GC pressure introduced by the bulk bootstrap walk.
	// We compare NumGC before the walk to after to isolate ingestion-induced GC.
	if filesQueued > 0 && bus != nil {
		var memAfter runtime.MemStats
		runtime.ReadMemStats(&memAfter)
		gcDelta := memAfter.NumGC - memBefore.NumGC
		gcPerFile := float64(gcDelta) / float64(filesQueued)
		if gcPerFile > cfg.RAG.GCPressureThreshold {
			bus.Publish(pubsub.Event{
				Type: pubsub.EventGCPressure,
				Payload: map[string]any{
					"gc_delta":    gcDelta,
					"files":       filesQueued,
					"gc_per_file": gcPerFile,
					"workspace":   workspace,
				},
			})
			log.Printf("[SRE-36.1.3] GC pressure alert: %.1f GC runs/file (%d GC in %d files)", gcPerFile, gcDelta, filesQueued)
		}
	}

	log.Println("[BOOTSTRAP] mass ingestion completed. Channels remain open.")
}

func installPreCommitHook(workspace string, pairTTLSeconds, fastTTLSeconds int) {
	hookDir := filepath.Join(workspace, ".git", "hooks")
	hookPath := filepath.Join(hookDir, "pre-commit")

	// Don't overwrite if user has a custom hook
	if _, err := os.Stat(hookPath); err == nil {
		existing, _ := os.ReadFile(hookPath)
		if !strings.Contains(string(existing), "SRE-21.3.3") {
			return // User has a custom hook, don't overwrite
		}
	}

	// [SRE-21.3.3] Hook logic: verify staged files against certified_state.lock
	// [SRE-76.2] TTL injected from neo.yaml sre.certify_ttl_minutes at boot time.
	hookScript := fmt.Sprintf(`#!/bin/bash
# [SRE-21.3.3] NeoAnvil Pre-Commit SRE Hook — auto-installed by orchestrator
REPO_ROOT="$(git rev-parse --show-toplevel)"
LOCK_FILE="$REPO_ROOT/.neo/db/certified_state.lock"

# [SRE-107.B] Binary-staleness warning: if staged .go files are newer than
# bin/neo-mcp, the running daemon may not have the changes loaded. Warn but
# never block — staleness is a runtime concern, not a commit concern.
BIN_MCP="$REPO_ROOT/bin/neo-mcp"
if [ -f "$BIN_MCP" ]; then
    STALE_GO=$(git diff --cached --name-only --diff-filter=ACM | grep -E '\.go$' | while read -r f; do
        if [ -f "$REPO_ROOT/$f" ] && [ "$REPO_ROOT/$f" -nt "$BIN_MCP" ]; then
            echo "$f"
        fi
    done)
    if [ -n "$STALE_GO" ]; then
        echo "⚠️  bin/neo-mcp is older than staged changes — consider 'make rebuild-restart'"
        echo "$STALE_GO" | head -5 | sed 's/^/     /'
    fi
fi

# [SRE-101.A] Escape hatch: NEO_CERTIFY_BYPASS=1 skips the veto. Warns but
# does not block — for CI/CD and for when neo-mcp/Nexus is offline. Intended
# for emergency use; regular workflow should still go through certify.
if [ "${NEO_CERTIFY_BYPASS}" = "1" ]; then
    STAGED=$(git diff --cached --name-only --diff-filter=ACM | grep -E '\.(go|ts|tsx|js|jsx|css|rs|py|html)$')
    if [ -n "$STAGED" ]; then
        echo "🟡 [SRE BYPASS] NEO_CERTIFY_BYPASS=1 — skipping certification check for:"
        echo "$STAGED" | sed 's/^/     /'
        echo "   (regular workflow: run neo_sre_certify_mutation before committing)"
    fi
    exit 0
fi

# [SRE-76.2] TTL from .neo/mode file (written by neo-mcp at boot). Fallback to env var.
NEO_MODE="${NEO_SERVER_MODE:-$(cat "$REPO_ROOT/.neo/mode" 2>/dev/null)}"
if [ "$NEO_MODE" = "pair" ]; then
    STALE_SECONDS=%d  # pair mode TTL
else
    STALE_SECONDS=%d  # fast/daemon mode TTL
fi

if [ ! -f "$LOCK_FILE" ]; then
    CODE_FILES=$(git diff --cached --name-only --diff-filter=ACM | grep -E '\.(go|ts|tsx|js|jsx|css|rs|py|html)$')
    if [ -n "$CODE_FILES" ]; then
        echo "🔴 [SRE VETO] Code not certified by NeoAnvil. Run neo_sre_certify_mutation before committing."
        echo "   Uncertified files:"
        echo "$CODE_FILES" | sed 's/^/     /'
        exit 1
    fi
    exit 0
fi

NOW=$(date +%%s)
FAILED=""

for FILE in $(git diff --cached --name-only --diff-filter=ACM | grep -E '\.(go|ts|tsx|js|jsx|css|rs|py|html)$'); do
    ABS_FILE="$(git rev-parse --show-toplevel)/$FILE"
    SEAL=$(grep "^${ABS_FILE}|" "$LOCK_FILE" 2>/dev/null | tail -1)
    if [ -z "$SEAL" ]; then
        FAILED="$FAILED\n     $FILE (not certified)"
    else
        STAMP=$(echo "$SEAL" | cut -d'|' -f2)
        AGE=$((NOW - STAMP))
        if [ "$AGE" -gt "$STALE_SECONDS" ]; then
            FAILED="$FAILED\n     $FILE (seal expired: ${AGE}s ago)"
        fi
    fi
done

if [ -n "$FAILED" ]; then
    echo "🔴 [SRE VETO] Code not certified by NeoAnvil. Run neo_sre_certify_mutation before committing."
    echo -e "   Uncertified files:$FAILED"
    exit 1
fi

> "$LOCK_FILE"
exit 0
`, pairTTLSeconds, fastTTLSeconds)
	_ = os.MkdirAll(hookDir, 0755)
	_ = os.WriteFile(hookPath, []byte(hookScript), 0755)
	log.Printf("[SRE-21.3.2] Pre-commit hook installed at %s", hookPath)
}

func daemonPIDPath(workspace string) string {
	return filepath.Join(workspace, ".neo", "daemon.pid")
}

// writeDaemonPID writes a JSON file that lets the `neo` CLI discover the running daemon.
// [SRE-33.1.2] Also stores sse_port for the pre-commit hook to call record_hotspot_bypass. [159.A]
func writeDaemonPID(workspace string, pid, dashPort, ssePort int, mode string) {
	data, _ := json.Marshal(map[string]any{
		"pid":      pid,
		"port":     dashPort,
		"sse_port": ssePort,
		"mode":     mode,
		"started":  time.Now().UTC().Format(time.RFC3339),
	})
	_ = os.WriteFile(daemonPIDPath(workspace), data, 0644)
}

// deleteDaemonPID removes the daemon PID file on clean shutdown.
func deleteDaemonPID(workspace string) {
	_ = os.Remove(daemonPIDPath(workspace))
}
