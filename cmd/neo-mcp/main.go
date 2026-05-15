package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // Register "pgx" SQL driver for DB_SCHEMA PostgreSQL support
	_ "github.com/lib/pq"              // Register "postgres" SQL driver for DB_SCHEMA PostgreSQL support

	"github.com/ensamblatec/neoanvil/pkg/finops"
	"github.com/ensamblatec/neoanvil/pkg/pubsub"

	"github.com/ensamblatec/neoanvil/pkg/integrations"
	"github.com/ensamblatec/neoanvil/pkg/observability"
	"github.com/ensamblatec/neoanvil/pkg/state"
	"github.com/ensamblatec/neoanvil/pkg/wasm"

	"github.com/ensamblatec/neoanvil/pkg/astx"
	"github.com/ensamblatec/neoanvil/pkg/coldstore"
	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/incidents"
	"github.com/ensamblatec/neoanvil/pkg/cpg"
	"github.com/ensamblatec/neoanvil/pkg/federation"
	"github.com/ensamblatec/neoanvil/pkg/mctx"
	"github.com/ensamblatec/neoanvil/pkg/memx"
	"github.com/ensamblatec/neoanvil/pkg/mesh"
	"github.com/ensamblatec/neoanvil/pkg/otelx"
	"github.com/ensamblatec/neoanvil/pkg/phoenix"
	"github.com/ensamblatec/neoanvil/pkg/rag"
	"github.com/ensamblatec/neoanvil/pkg/sre"
	"github.com/ensamblatec/neoanvil/pkg/telemetry"
	"github.com/ensamblatec/neoanvil/pkg/tensorx"
	"github.com/ensamblatec/neoanvil/pkg/wasmx"
	workspacereg "github.com/ensamblatec/neoanvil/pkg/workspace"
)

var (
	jsImportRegex = regexp.MustCompile(`(?m)(?:from|require\()\s*['"]([^'"]+)['"]`)
	pyImportRegex = regexp.MustCompile(`(?m)^(?:import|from)\s+([a-zA-Z0-9_\.]+)`)

	// [SRE-46] Package-level distiller and FPI for REM sleep GC + compression.
	remDistiller = rag.NewDistiller()
	remFPI       = rag.NewFlashbackFPI()

	// [SRE-41] Active Dreaming engine — DreamCycle called during REM sleep.
	dreamEngine *sre.DreamEngine
	// [SRE-40] Policy engine — Sentinel constitutional rules enforced at dispatch.
	policyEngine *sre.PolicyEngine
	// [SRE-45] Global HyperGraph instance — feeds Holographic Trace API on HUD.
	globalHyperGraph *rag.HyperGraph
)


// [SRE-6.2] FinOps Wrapper Zero-Alloc
func ExecuteWithFinops[T any](executeFn func() (T, error)) (T, error) {
	start := time.Now()
	res, err := executeFn()
	finops.IngestHardwareMetric(time.Since(start).Nanoseconds())
	return res, err
}

func initPlannerAndSubsystems(workspace string, cfg *config.NeoConfig) {
	if err := state.InitPlanner(workspace); err != nil {
		log.Printf("[SRE-WARN] Fallo al inicializar Planner DB: %v", err)
		return
	}
	// [138.C.7] Migrate any legacy SRETask records (pre-trust-system)
	// so the daemon's iterative loop has a stable starting point. Idempotent
	// — re-runs on every boot are no-ops once tasks carry migrated_at.
	if migrated, mErr := state.MigrateLegacyTasks(); mErr != nil {
		log.Printf("[SRE-WARN] migration shim failed (non-fatal): %v", mErr)
	} else if migrated > 0 {
		log.Printf("[SRE-BOOT] migrated %d legacy task(s) for trust system", migrated)
	}
	// [138.E.3] Pair-mode audit-event reaper. Every 5 minutes, mark
	// unresolved events past PairAuditEventTTL (30 min) as
	// OutcomeSuccess (conservative no-penalty). Goroutine for the
	// lifetime of the process — neo-mcp is long-running so the leak
	// is intentional. Failures are logged + skipped.
	go runPairAuditReaper()
	listnr := integrations.NewSREListener(fmt.Sprintf(":%d", cfg.Server.SREListenerPort), func(payload string) {
		taskDesc := "[SRE INCIDENT] Responder a alarma externa: " + payload
		state.EnqueueTasks([]state.SRETask{
			{Description: taskDesc, TargetFile: "GLOBAL SRE CONTEXT"},
		})
		log.Printf("[SRE-AUTONOMY] Incidente autogenerado en BoltDB: %s\n", payload)
	})
	listnr.Start(context.Background())
	bpfTracer := sre.NewEBpfTracer(nil)
	bpfTracer.Start()
	meshRouter := mesh.NewRouter(100)
	_ = meshRouter
	wasmEngine := wasm.NewWasmHypervisor()
	_ = wasmEngine
}

// validateEmbedderAtBoot runs a one-shot Ollama model presence check against
// every configured embed endpoint. Non-fatal: logs a single actionable warning
// and continues boot (tools that don't need embedding still work). [INC-20260424-133023]
func validateEmbedderAtBoot(ctx context.Context, cfg *config.NeoConfig) {
	if cfg == nil || cfg.AI.EmbeddingModel == "" {
		return
	}
	urls := cfg.AI.EmbeddingURLs
	if len(urls) == 0 && cfg.AI.EmbedBaseURL != "" {
		urls = []string{cfg.AI.EmbedBaseURL}
	}
	if len(urls) == 0 && cfg.AI.BaseURL != "" {
		urls = []string{cfg.AI.BaseURL}
	}
	if len(urls) == 0 {
		return // no embed endpoint configured → nothing to validate
	}
	for _, url := range urls {
		err := rag.ValidateWithRetry(ctx, url, cfg.AI.EmbeddingModel, 3, 2*time.Second)
		if err == nil {
			log.Printf("[BOOT-OLLAMA] embed model %q present on %s", cfg.AI.EmbeddingModel, url)
			continue
		}
		if errors.Is(err, rag.ErrOllamaModelNotFound) {
			log.Printf("[BOOT-WARN] embed model %q MISSING on %s — run `ollama pull %s` (ingestion workers will loop on HTTP 404 until resolved)",
				cfg.AI.EmbeddingModel, url, cfg.AI.EmbeddingModel)
			continue
		}
		log.Printf("[BOOT-WARN] embed endpoint %s unreachable: %v (tools requiring embed will degrade to grep fallback)", url, err)
	}
}

// bootWAL is the cold-path WAL warmup. PurgeForeignSessionPaths +
// FULL SanitizeWAL + LoadDirectivesFromDisk. Used by bootRAG when
// fast-boot snapshot was unavailable / stale / corrupt — at that point
// the cold rebuild needs guaranteed-clean buckets.
func bootWAL(wal *rag.WAL, workspace string) {
	// [Épica 330.L] One-shot scrub of cross-workspace paths that landed in
	// session_state BEFORE the ownership guard (2026-04-23). Idempotent +
	// cheap when clean (< 1ms for typical session counts).
	if removed, err := wal.PurgeForeignSessionPaths(workspace); err != nil {
		log.Printf("[SRE-OWN] PurgeForeignSessionPaths: %v", err)
	} else if removed > 0 {
		log.Printf("[SRE-OWN] scrubbed %d cross-workspace paths from session_state (bug 330.L remediation)", removed)
	}
	if purged, sanitizeErr := wal.SanitizeWAL(); sanitizeErr != nil {
		log.Printf("[SRE-WARN] WAL sanitization error: %v", sanitizeErr)
	} else if purged > 0 {
		log.Printf("[SRE-BOOT] WAL sanitized: %d corrupted entries purged.", purged)
	}
	if err := wal.LoadDirectivesFromDisk(workspace); err != nil {
		log.Printf("[SRE-WARN] Failed to load disk directives: %v", err)
	}
}

// bootWALFast is the fast-boot-path WAL warmup. Same prep as bootWAL but
// uses SanitizeWALMetadataOnly (skips nodes/edges/vectors) because the
// snapshot already represents valid graph state. Saves ~10-15s on multi-GB
// WALs by avoiding the page-fault storm of iterating big buckets.
// [ÉPICA 149.J / DS audit-mitigated]
//
// Big-bucket sanitization still happens — runBackgroundSanitize schedules
// a full SanitizeWAL pass ~30s after boot completes, so corruption can't
// accumulate silently across sessions.
func bootWALFast(wal *rag.WAL, workspace string) {
	if removed, err := wal.PurgeForeignSessionPaths(workspace); err != nil {
		log.Printf("[SRE-OWN] PurgeForeignSessionPaths: %v", err)
	} else if removed > 0 {
		log.Printf("[SRE-OWN] scrubbed %d cross-workspace paths from session_state (fast-boot)", removed)
	}
	if purged, sanitizeErr := wal.SanitizeWALMetadataOnly(); sanitizeErr != nil {
		log.Printf("[SRE-WARN] WAL meta sanitization error: %v", sanitizeErr)
	} else if purged > 0 {
		log.Printf("[SRE-BOOT] WAL meta sanitized: %d corrupted entries purged (fast-boot path).", purged)
	}
	if err := wal.LoadDirectivesFromDisk(workspace); err != nil {
		log.Printf("[SRE-WARN] Failed to load disk directives: %v", err)
	}
}

// runBackgroundSanitize schedules a full SanitizeWAL pass after a delay.
// Used on the fast-boot path so big-bucket validation still happens
// (catches accumulated corruption) without blocking boot. Runs as a
// goroutine; ctx cancels it cleanly on shutdown.
// [ÉPICA 149.J]
func runBackgroundSanitize(ctx context.Context, wal *rag.WAL, delay time.Duration) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		log.Printf("[SRE-BOOT] running deferred full WAL sanitize (post fast-boot)")
		start := time.Now()
		if purged, err := wal.SanitizeWAL(); err != nil {
			log.Printf("[SRE-WARN] background WAL sanitize error: %v", err)
		} else {
			log.Printf("[SRE-BOOT] deferred sanitize done in %v (%d purged)", time.Since(start), purged)
		}
	}()
}

func initEmbedder(ctx context.Context, cfg *config.NeoConfig, sandbox *wasmx.Sandbox, workspace string) rag.Embedder {
	if cfg.AI.Provider == "wasm" {
		log.Println("[BOOT] using WASM Local Embedder (CPU bound)")
		return wasmx.NewLocalEmbedder(ctx, sandbox.Runtime(), workspace)
	}
	log.Println("[BOOT] using external Ollama Embedder (HTTP bound)")
	// [303.E] Pass embedding_urls pool for round-robin; falls back to embed_base_url when empty.
	return rag.NewOllamaEmbedder(cfg.AI.EmbedBaseURL, cfg.AI.EmbeddingModel, cfg.AI.EmbedTimeoutSeconds, cfg.RAG.EmbedConcurrency, cfg.RAG.MaxEmbedChars, cfg.AI.EmbeddingURLs...)
}

// populateQuantCompanion builds the int8 or binary companion arrays on
// the HNSW graph when cfg.RAG.VectorQuant requests it. The float32
// Vectors slice stays authoritative; companion arrays are derived views
// rebuilt on every boot in O(N·D). [Épica 170.C]
//
// Costs at 50 k nodes × 768 dims:
//   - float32 baseline: ~150 MB, ~234 ns/distance (v3 GOAMD64)
//   - int8 companion:   +38 MB RAM overhead, ~582 ns/distance (slower due
//     to missing VNNI in Go auto-vectorizer — see pkg/rag/quantize.go)
//   - binary companion: +4.6 MB RAM overhead, ~3 ns/distance (POPCNT
//     intrinsic), <5% recall loss after coarse+re-rank hybrid
func populateQuantCompanion(g *rag.Graph, mode string) {
	if len(g.Nodes) == 0 {
		return
	}
	switch mode {
	case "int8":
		g.PopulateInt8()
		log.Printf("[BOOT] int8 companion populated: %d nodes, +%d KB RAM overhead",
			len(g.Nodes), (len(g.Int8Vectors)+len(g.Int8Scales)*4)/1024)
	case "binary":
		g.PopulateBinary()
		log.Printf("[BOOT] binary companion populated: %d nodes, +%d KB RAM overhead",
			len(g.Nodes), len(g.BinaryVectors)*8/1024)
	case "hybrid":
		// Hybrid uses binary as candidate filter + float32 rerank — only the
		// binary side needs population. Float32 stays authoritative by design.
		// Empirical recall on the operator's own corpus: 1.000 across 50 queries
		// at top-10 (recall_measure_live_test.go). [Épica 170.C-hybrid]
		g.PopulateBinary()
		log.Printf("[BOOT] hybrid companion populated: %d nodes, +%d KB RAM overhead (binary candidate filter + float32 rerank)",
			len(g.Nodes), len(g.BinaryVectors)*8/1024)
	case "", "float32":
		// default — no companion storage
	default:
		log.Printf("[BOOT-WARN] unknown rag.vector_quant=%q — defaulting to float32 (valid: float32|int8|binary|hybrid)", mode)
	}
}

func startGossipIfEnabled(ctx context.Context, cfg *config.NeoConfig, workspace string, embedder rag.Embedder, hnswGraph *rag.Graph, cpuEngine *tensorx.CPUDevice, wal *rag.WAL) {
	if !cfg.Server.Tailscale || len(cfg.Server.GossipPeers) == 0 {
		return
	}
	searchFn := func(gCtx context.Context, query string) []string {
		queryVec, err := embedder.Embed(gCtx, query)
		if err != nil {
			return nil
		}
		idxs, err := hnswGraph.SearchAuto(gCtx, queryVec, 3, cpuEngine, cfg.RAG.VectorQuant)
		if err != nil {
			return nil
		}
		snippets := make([]string, 0, len(idxs))
		for _, idx := range idxs {
			if int(idx) < len(hnswGraph.Nodes) {
				_, content, _, _ := wal.GetDocMeta(hnswGraph.Nodes[idx].DocID)
				if content != "" {
					snippets = append(snippets, content)
				}
			}
		}
		return snippets
	}
	mctx.StartGossipListener(ctx, cfg.Server.GossipPort, mctx.GossipHandler{
		WAL:      wal,
		NodeID:   workspace,
		SearchFn: searchFn,
	})
}

func startingWorkspaceFromArgs() string {
	if len(os.Args) > 1 {
		return os.Args[1]
	}
	workspace, err := os.Getwd()
	if err != nil {
		log.Fatalf("[SRE-FATAL] Cannot determine working directory: %v", err)
	}
	return workspace
}

// resolveWorkspace walks up the directory tree from startingDir looking for neo.yaml.
// Falls back to startingDir (as absolute path) if not found.
func resolveWorkspace(startingDir string) string {
	workspace := startingDir
	for {
		if _, err := os.Stat(filepath.Join(workspace, "neo.yaml")); err == nil {
			break
		}
		parent := filepath.Dir(workspace)
		if parent == workspace {
			workspace = startingDir
			break
		}
		workspace = parent
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		log.Fatalf("[SRE-FATAL] Cannot resolve workspace absolute path: %v", err)
	}
	return abs
}

func registerWorkspace(workspace string) {
	reg, regErr := workspacereg.LoadRegistry()
	if regErr != nil {
		log.Printf("[SRE-WARN] workspace registry load: %v", regErr)
		return
	}
	entry, addErr := reg.Add(workspace)
	if addErr != nil {
		return
	}
	_ = reg.Select(entry.ID)
	if os.Getenv("NEO_NEXUS_CHILD") != "1" {
		if saveErr := reg.Save(); saveErr != nil {
			log.Printf("[SRE-WARN] workspace registry save: %v", saveErr)
		}
	}
}

func computeTTLSeconds(cfg *config.NeoConfig) (int, int) {
	pairTTL := cfg.SRE.CertifyTTLMinutes * 60
	if pairTTL <= 0 {
		pairTTL = 1800
	}
	fastTTL := cfg.SRE.CertifyTTLMinutes * 60
	if fastTTL <= 0 {
		fastTTL = 300
	}
	return pairTTL, fastTTL
}

func newConfiguredOracleEngine(cfg *config.NeoConfig) *sre.OracleEngine {
	e := sre.NewOracleEngine()
	if cfg.SRE.OracleAlertThreshold > 0 {
		e.AlertThreshold = cfg.SRE.OracleAlertThreshold
	}
	if cfg.SRE.OracleHeapLimitMB > 0 {
		e.HeapLimitMB = cfg.SRE.OracleHeapLimitMB
	}
	if cfg.SRE.OraclePowerLimitW > 0 {
		e.PowerLimitW = cfg.SRE.OraclePowerLimitW
	}
	return e
}

// wireCPGTriageHook attaches a CPG correlator to the triage engine post-write hook. [165.C]
func wireCPGTriageHook(triageEngine *sre.TriageEngine, cpgMgr *cpg.Manager) {
	if triageEngine == nil {
		return
	}
	triageEngine.SetPostWriteHook(func(path string, content []byte) {
		corrs := incidents.CorrelateWithCPG(content, cpgMgr, 5)
		section := incidents.FormatCPGSection(corrs)
		if section == "" {
			return
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(section)
	})
}

func main() { //nolint:complexity // entrypoint — high CC is inherent to wiring all subsystems
	workspace := ""
	// [SRE] Crono-Depuracion: Captor Gob de Pánicos Global
	defer func() {
		if r := recover(); r != nil {
			panicMsg := fmt.Sprintf("%v", r)
			log.Printf("[SRE-FATAL] Colapso sistémico interceptado: %s", panicMsg)
			_ = sre.DumpSnapshot(workspace)

			// [SRE-14.1.1] Fénix Protocol — only armed when explicitly enabled
			if (strings.Contains(panicMsg, "eBPF-Kernel-Compromise") || strings.Contains(panicMsg, "SRE-SSRF")) &&
				os.Getenv("SRE_PHOENIX_ARMED") == "true" {
				phoenix.TriggerPhoenixProtocol("Detección de intrusión a nivel Kernel / SSRF Crítica")
			}
			log.Fatalf("[SRE-FATAL] Syscall violenta interceptada.")
		}
	}()
	workspace = resolveWorkspace(startingWorkspaceFromArgs())

	// [SRE-30+] Ensure all .neo/ directories and template files exist before any subsystem boots.
	if err := config.EnsureWorkspace(workspace); err != nil {
		log.Printf("[SRE-WARN] workspace bootstrap incomplete: %v", err)
	}

	// [SRE-BUG-FIX] Initialize LastActivityTimestamp at boot so REM sleep can evaluate idle.
	// Without this, ts=0 causes the idle check to skip forever (deadlock).
	LastActivityTimestamp.Store(time.Now().Unix())

	// [SRE-37] Auto-register current workspace in global ~/.neo/workspaces.json on every boot.
	// [SRE-81.B.4] When running as a Nexus child (NEO_NEXUS_CHILD=1), only read the registry
	// to obtain the workspace entry — never write it. Nexus is the sole owner of
	// ~/.neo/workspaces.json; children writing it races with the dispatcher.
	registerWorkspace(workspace)

	cfgPath := filepath.Join(workspace, "neo.yaml")
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		log.Printf("[SRE-WARN] failed to load or create neo.yaml: %v", err)
	}
	bootTenant(cfg) // [Épica 265.A/B] load credentials, inject TenantID into cfg.Auth + session state

	// [SRE-20.2.2] Materialize server mode for child tools
	os.Setenv("NEO_SERVER_MODE", cfg.Server.Mode)
	// Write mode to file so pre-commit hook can read it without relying on shell env.
	modeFile := filepath.Join(workspace, ".neo", "mode")
	_ = os.MkdirAll(filepath.Dir(modeFile), 0755)
	_ = os.WriteFile(modeFile, []byte(cfg.Server.Mode), 0644)

	// [GPU-AWARE] Detect GPU and apply adaptive config overrides.
	bootGPU := applyGPUConfig(cfg)

	// Initialize SSRF trusted ports from neo.yaml (Zero-Hardcoding)
	sre.InitTrustedPorts(cfg.SRE.TrustedLocalPorts)

	// [SRE-21.3.2/101.B] Auto-install pre-commit hook with configurable TTL.
	// Pair default raised from 900s to 1800s so multi-file commits survive the
	// typical session pause between certify and git commit.
	pairTTL, fastTTL := computeTTLSeconds(cfg)
	installPreCommitHook(workspace, pairTTL, fastTTL)

	jobs := make(chan string, 100)

	initPlannerAndSubsystems(workspace, cfg)

	go telemetry.StartHardwareTelemetry(workspace)
	// InitFirehose is started after ctx below to enable graceful shutdown.

	logFile, triageEngine := setupLogging(workspace)
	if logFile != nil {
		defer logFile.Close()
	}

	log.Println("[BOOT] initialize NeoAnvil MCP Orchestrator")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	telemetry.InitFirehose(ctx)
	startOrphanScanner(ctx, workspace, cfg) // [362.A] background orphan detection for delegate tasks

	// [SRE-33.1.2] Write daemon PID file so `neo` CLI can discover the dashboard port.
	writeDaemonPID(workspace, os.Getpid(), cfg.Server.DashboardPort, cfg.Server.SSEPort, cfg.Server.Mode)
	defer deleteDaemonPID(workspace)

	// [ÉPICA 271.C] Plain PID lock file — shell-readable integer for rebuild-restart to kill
	// by exact PID (complement to pkill; catches processes with non-standard argv[0]).
	pidLockPath := filepath.Join(workspace, ".neo", "neo-mcp.pid")
	_ = os.WriteFile(pidLockPath, []byte(strconv.Itoa(os.Getpid())), 0644)
	defer os.Remove(pidLockPath)

	// [SRE-32.1.1] Central event bus — feeds the Operator HUD dashboard via SSE.
	eventBus := pubsub.NewBus()
	snapshots := NewSnapshotBuffer(50)

	// [SRE-56.1] Epic closure hook — fires EventSuggestCommit when all Kanban tasks DONE.
	state.OnEpicClose = func() {
		log.Printf("[KANBAN] Epic complete — all tasks DONE. Emitting EventSuggestCommit.")
		eventBus.Publish(pubsub.Event{
			Type: pubsub.EventSuggestCommit,
			Payload: map[string]any{
				"message": "All Kanban tasks complete. Consider neo_memory_commit + git commit to persist this epic.",
			},
		})
	}

	server := NewMCPServer()

	log.Println("[BOOT] memory subsystem is being instanced (memx)")
	f32Pool := memx.NewObservablePool(
		func() *memx.F32Slab { return &memx.F32Slab{Data: make([]float32, 0, memx.BlockSize)} },
		func(slab *memx.F32Slab) { slab.Data = slab.Data[:0] },
		memx.BlockSize*4,
	)

	arenaSize := cfg.Cognitive.ArenaSize
	treeMemory := make([]mctx.Node, arenaSize)
	mctsEngine := mctx.NewTree(treeMemory)
	log.Printf("[BOOT] MCTS cognitive engine ready: %d node arena", arenaSize)

	log.Println("[BOOT] long-term memory subsystem (RAG WAL)")
	wal, hnswGraph, cleanupRAG := bootRAG(ctx, cfg, workspace) // [269.A]
	defer cleanupRAG()
	hnswGraph.SetAffinityConfig(cfg.SRE.CPUAffinityEnabled, cfg.SRE.CPUAffinityCores) // [367.A]

	// [SRE] Fase 7.2: Protección ACID contra SIGKILL / SIGINT
	// [BUG-FIX 2026-05-13 v2] Previously a SIGTERM goroutine ran in parallel
	// with main()'s own <-ctx.Done() shutdown path. Both fired on signal and
	// raced — the goroutine called os.Exit(0) which killed the process
	// BEFORE main()'s synchronous HNSW save could run. Symptom: every boot
	// detected snapshot stale and rebuilt cold despite HNSW save being
	// "implemented" twice (in goroutine AND in main). Resolution: the
	// goroutine is gone; all teardown is consolidated synchronously in
	// main()'s post-<-ctx.Done() block at the bottom of this function (see
	// "MCP server is shutting down..." section near end of main). The
	// `defer cleanupRAG()` registered above still runs wal.Close() when
	// main returns, so the close order is well-defined.

	log.Println("[BOOT] compute engine (tensorx CPUDevice)")
	cpuEngine := tensorx.NewCPUDevice(f32Pool)
	if cfg.RAG.HNSWBatchEnabled { // [367.C]
		hnswGraph.EnableBatcher(cpuEngine, cfg.RAG.HNSWBatchWindowMS, cfg.RAG.HNSWBatchMaxSize)
		defer hnswGraph.DisableBatcher()
		log.Printf("[BOOT] HNSW query batcher enabled (window=%dms, maxSize=%d)", cfg.RAG.HNSWBatchWindowMS, cfg.RAG.HNSWBatchMaxSize)
	}

	// [ÉPICA 149] Periodic HNSW snapshot save. Default every 30 min;
	// 0 disables (only the SIGTERM hook above writes the snapshot then).
	// Save acquires Graph.snapshotMu.RLock so it never corrupts state
	// vs concurrent inserts; the cost is that inserts queue for the
	// duration of serialization. With infrequent saves (30 min) and
	// fast serialization (3 GB ≈ ~5s), the throughput hit is invisible.
	if interval := cfg.RAG.HNSWPersistIntervalMinutes; interval > 0 && cfg.RAG.HNSWPersistPath != "" {
		go func(g *rag.Graph, w *rag.WAL, persistRel string, intervalMin int) {
			persistPath := filepath.Join(workspace, persistRel)
			ticker := time.NewTicker(time.Duration(intervalMin) * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := rag.SaveHNSWSnapshot(g, w, persistPath); err != nil {
						log.Printf("[HNSW-PERSIST] periodic save failed: %v", err)
					} else {
						log.Printf("[HNSW-PERSIST] periodic save ok → %s", persistPath)
					}
				}
			}
		}(hnswGraph, wal, cfg.RAG.HNSWPersistPath, interval)
	}

	log.Println("[BOOT] initializing WebAssembly Sandbox (wazero)")

	hypervisorPath := filepath.Join(workspace, ".neo", "models", "hypervisor.wasm")
	wasmBinary, err := os.ReadFile(hypervisorPath)
	if err != nil {
		log.Printf("[SRE-WAR] Hypervisor %s no encontrado, forzando dummy fallback: %v", hypervisorPath, err)
		wasmBinary = []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	}
	sandbox, err := wasmx.NewSandbox(ctx, wasmBinary, cpuEngine)
	if err != nil {
		log.Fatalf("[SRE-FATAL] failed to initialize wasm sandbox: %v", err)
	}
	defer sandbox.Close(ctx)

	embedder := initEmbedder(ctx, cfg, sandbox, workspace)

	// [INC-20260424-133023] Preflight: confirm the Ollama embedding model is
	// actually loaded before we spin up ingestion workers. If absent, emit
	// a single actionable warning and skip the loud circuit-breaker loop
	// that would otherwise produce 100+ SRE-WARN lines per minute.
	validateEmbedderAtBoot(ctx, cfg)

	lexicalIdx := rag.NewLexicalIndex()
	// [Épica 169.A] Dedicated BM25 index for .neo/incidents/ — never touches
	// Ollama so INCIDENT_SEARCH is always available even when the embedder
	// is down or the INC corpus exceeds the embed context window.
	incLexIdx := rag.NewLexicalIndex()
	// [Épica 175/179/199/221] Construct all three cache layers + warm-load
	// from disk snapshots in a single helper. See cache_setup.go for the
	// per-layer capacity sourcing and snapshot paths.
	caches := setupCaches(cfg, workspace, hnswGraph.Gen.Load())
	queryCache := caches.query
	textCache := caches.text
	embCache := caches.emb
	// [Épica 178] Per-tool MCP latency histogram (512-sample ring buffer
	// per tool). Feeds p50/p95/p99 into BRIEFING full mode and HUD_STATE.
	toolLatency := observability.NewToolLatencyTracker(512)

	log.Println("[BOOT] awakening MLP Neural Network from BoltDB")
	wasmx.InitMLP(wal)

	registry := NewToolRegistry()
	mustRegister := func(tool Tool) {
		if err := registry.Register(tool); err != nil {
			log.Fatalf("[SRE-FATAL] %v", err)
		}
	}

	// [SRE] Database ORM + Cold Storage OLAP — [302.C] extracted to bootDBAEngines.
	dbab := bootDBAEngines(workspace, cfg)
	defer dbab.Cleanup()
	dbaEngine := dbab.DBA
	coldStoreEngine := dbab.Cold

	// [Épica 294 / 287.C / 269.A] Project Knowledge Store + shared HNSW tier — extracted to bootProjectFederation.
	fed := bootProjectFederation(cfg, workspace)
	defer fed.Cleanup()
	knowledgeStore := fed.KS
	hotCache := fed.HC
	sharedGraph := fed.SG

	// [Épica 365.A] Apply runtime heap cap BEFORE heavy subsystems load.
	// Opt-in via sre.runtime_memory_limit_mb (0 = uncapped default).
	sre.ApplyMemoryLimit(cfg.SRE.RuntimeMemoryLimitMB)

	// [SRE-50/48/61/40/41/34/45 — 269.A] SRE subsystems extracted to bootSRE.
	sreb := bootSRE(cfg, workspace)
	ghostInterceptor := sreb.Ghost
	consensusEngine := sreb.Consensus
	oracleEngine := sreb.Oracle
	inferenceGW := sreb.InfGW

	// [PILAR-XX / Épica 145 / 269.A] CPG Manager extracted to bootCPGManager.
	cpgMgr, cleanupCPG := bootCPGManager(ctx, cfg, workspace)
	defer cleanupCPG()

	// [PILAR-XXI/152.C] Wire CPG blast-radius into auto-triage incident reports. [165.C]
	wireCPGTriageHook(triageEngine, cpgMgr)

	knowledgeBootUnix := time.Now().Unix() // [298.C] session start reference for stale-contract detection
	radarTool := NewRadarTool(hnswGraph, cpuEngine, f32Pool, embedder, wal, lexicalIdx, dbaEngine, cfg, workspace).WithCPGManager(cpgMgr).WithIncidentLex(incLexIdx).WithQueryCache(queryCache).WithTextCache(textCache).WithEmbeddingCache(embCache).WithKnowledgeStats(hotCache.Stats).WithKnowledgeStore(knowledgeStore).WithContractHotFetch(func(ns, key string) (string, bool) {
		e, ok := hotCache.Get(ns, key)
		if !ok {
			return "", false
		}
		return e.Content, true
	}).WithKnowledgeStaleContracts(func() []string {
		if knowledgeStore == nil {
			return nil
		}
		entries, err := knowledgeStore.List("contracts", "")
		if err != nil || len(entries) == 0 {
			return nil
		}
		var stale []string
		for _, e := range entries {
			if e.UpdatedAt > knowledgeBootUnix {
				stale = append(stale, e.Key)
			}
		}
		return stale
	})
	radarTool.WithSharedGraph(sharedGraph) // [287.E] project shared HNSW tier
	radarTool.WithGPUInfo(bootGPU)        // [GPU-AWARE] boot-time GPU snapshot for BRIEFING display
	// [LARGE-PROJECT/A 2026-05-13] Wire HotFilesCache into the persist/load
	// stack so the cache warms on next boot. RadarTool owns the instance
	// (initialised in NewRadarTool), the cacheStack just borrows a pointer
	// for persistCachesOnShutdown to find it at teardown.
	caches.hotFiles = radarTool.hotFiles
	warmHotFilesCacheSnapshot(radarTool.hotFiles, workspace)
	mustRegister(radarTool)
	// [Épica 239] Unified cache observability + control tool. Dispatches via
	// `action` to the six sub-handlers kept intact as *Tool types (still
	// individually unit-testable). Replaces 6 previous MCP registrations.
	warmupTool := &CacheWarmupTool{radar: radarTool, queryCache: queryCache, textCache: textCache}
	mustRegister(&CacheTool{
		stats:   &CacheStatsTool{queryCache: queryCache, textCache: textCache, embCache: embCache, hotFiles: radarTool.hotFiles, workspace: workspace, knowledgeStats: hotCache.Stats},
		flush:   &CacheFlushTool{graph: hnswGraph},
		resize:  &CacheResizeTool{queryCache: queryCache, textCache: textCache},
		warmup:  warmupTool,
		persist: &CachePersistTool{queryCache: queryCache, textCache: textCache, embCache: embCache, workspace: workspace},
		inspect: &CacheInspectTool{queryCache: queryCache, textCache: textCache, embCache: embCache, graph: hnswGraph},
	})
	// [Phase 0.A / Speed-First] Auto-warmup at boot. Uses the recent-miss
	// rings rehydrated from the cache snapshot (cache_persist.go restores
	// them now), so we close the observe→persist→warm loop without the
	// operator copy-pasting target lists after every rebuild-restart.
	// Runs detached: warmup is a latency optimisation, not a boot prereq.
	go func() {
		if _, werr := warmupTool.Execute(ctx, map[string]any{"from_recent": true}); werr != nil {
			log.Printf("[BOOT] auto-warmup err: %v", werr)
		}
	}()
	// [Épica 207] Per-tool latency / error observability — distinct domain
	// (tool latency, not cache), kept as its own tool.
	mustRegister(&ToolStatsTool{workspace: workspace})
	// [345.A] Load registry for cross-workspace certify routing. Nil-safe: if load fails, certify
	// degrades gracefully (cross-ws paths get the existing rejection message).
	crossWSReg, crossWSRegErr := workspacereg.LoadRegistry()
	if crossWSRegErr != nil {
		log.Printf("[BOOT] cross-workspace routing disabled: registry load: %v", crossWSRegErr)
		crossWSReg = nil
	}
	radarTool.WithRegistry(crossWSReg) // [347.A] cross-workspace GRAPH_WALK scatter
	mustRegister(NewCertifyMutationTool(wal, hnswGraph, cpuEngine, embedder, workspace, cfg).WithBus(eventBus).WithInferenceGW(inferenceGW).WithRadar(radarTool).WithRegistry(crossWSReg)) // [SRE-26.2.2][SRE-32.2.2][SRE-86.A][292.A][345.A][347.A]
	mustRegister(&ApplyMigrationTool{dbaEngine: dbaEngine})
	// [Épica 239] Unified command dispatcher. Replaces 3 individual
	// registrations (neo_run_command + neo_approve_command + neo_kill_command).
	mustRegister(&CommandTool{
		run:       NewRunCommandTool(cfg.Cognitive.AutoApprove),
		approve:   NewApproveCommandTool(),
		kill:      &KillCommandTool{},
		workspace: workspace,
		contracts: func() []cpg.ContractNode { c, _ := radarTool.resolveContracts(); return c }, // [291.C]
	})

	mustRegister(NewContextCompressorTool().WithWAL(wal, workspace)) // [130.1]
	// [SRE-42.1] neo_inspect_matrix and neo_inspect_dom merged into neo_radar (HUD_STATE, FRONTEND_ERRORS)
	mustRegister(NewModelDownloaderTool(workspace))
	mustRegister(NewDaemonTool(wal, workspace).WithConfig(cfg).WithRegistry(crossWSReg)) // [348.A]
	mustRegister(NewChaosDrillTool(workspace, cfg).WithRegistry(crossWSReg))           // [346.A]
	// [SRE-42.1] neo_inject_fault merged into neo_chaos_drill
	// [Épica 239] Unified brain-state tool. Replaces 4 individual tools
	// (neo_memory_commit + neo_learn_directive + neo_rem_sleep + neo_load_snapshot).
	// [LXVII-refined] Nexus-global shared store at ~/.neo/shared/db/global.db.
	// [354.Z-redesign]
	// Piece 1: Nexus (dispatcher) is now the single owner of the nexus-global
	// store. Children proxy tier:"nexus" via HTTP to /api/v1/shared/nexus/*.
	// Piece 2: project-tier ownership is deterministic — coordinator workspace
	// holds the flock on .neo-project/db/knowledge.db. Non-coordinators boot
	// with ks=nil and proxy tier:"project" to the coordinator via Nexus MCP
	// routing. coordinatorWSID is resolved from the workspace registry here.
	// [354.Z-redesign / PILAR LXVII / 355.A-B] Coordinator + org-tier boot, extracted to reduce main CC.
	coord := bootCoordinatorTier(workspace, cfg)
	coordWSID := coord.CoordWSID
	orgKS := coord.OrgKS
	orgWriters := coord.OrgWriters
	mustRegister(&MemoryTool{
		commit:          NewMemoryCommitTool(workspace),
		learn:           NewLearnDirectiveTool(wal, workspace).WithConfig(func() *config.NeoConfig { return cfg }),
		remSleep:        NewRemSleepTool(wal),
		loadSnapshot:    &LoadSnapshotTool{workspace: workspace},
		ks:              knowledgeStore,
		hc:              hotCache,
		bus:             eventBus,
		workspace:       workspace,
		coordinatorWSID: coordWSID,
		orgKS:           orgKS,
		orgWriters:      orgWriters, // [361.A] RBAC writers allowlist from .neo-org/neo.yaml
	})
	mustRegister(NewLogAnalyzerTool(embedder, hnswGraph, wal, cpuEngine)) // [PILAR-XXI/151] Semantic log analysis + incident correlation
	// [PILAR LXVI / 351.C] 4-tier debt registry access (workspace/project/nexus/org).
	// Nexus tier proxies to Nexus dispatcher HTTP endpoints; workspace/project are
	// local file access; org is reserved for PILAR LXVII.
	mustRegister(NewDebtTool(workspace, lookupWorkspaceID(workspace), cfg))
	// [ADR-013] Local LLM tool — routes prompts to operator's GPU via Ollama.
	// Default model resolved from cfg.AI.LocalModel; falls back to the tool's
	// portable default (qwen2.5-coder:7b) when unset. $0/call complement to
	// plugin-deepseek for daemon mode + non-frontier audit tasks.
	mustRegister(NewLocalLLMTool(cfg.AI.BaseURL, cfg.AI.LocalModel))

	telemetry.SetAutoApprove(cfg.Cognitive.AutoApprove)

	// [SRE-27.1.1] Start Gossip P2P listener when Tailscale mode is enabled.
	startGossipIfEnabled(ctx, cfg, workspace, embedder, hnswGraph, cpuEngine, wal)

	// [PILAR LXIX / 364.C] Continuous PGO capture when sre.pgo_capture_interval_minutes > 0.
	// Writes .neo/pgo/profile-<unix>.pgo every N minutes; rotates >24h. `make build-pgo`
	// picks the newest. Default 0 = disabled (opt-in).
	go sre.ContinuousPGOCapture(ctx, workspace, cfg.SRE.PGOCaptureIntervalMinutes)

	if err := telemetry.InitHeatmap(workspace); err != nil {
		log.Printf("[SRE-WARN] Failed to initialize AST Heatmap: %v", err)
	}
	defer telemetry.CloseHeatmap()

	// [PILAR-XXVII/243 — 302.C] Observability store + goroutines extracted to bootObservability.
	defer bootObservability(ctx, workspace, cpgMgr, queryCache, textCache, embCache, eventBus)()

	// [SRE-85.A] Dashboard data opts — closures for internal API endpoints.
	// The SPA and standalone server were moved to cmd/neo-nexus (Épica 85).
	// These endpoints are registered on sseMux below, alongside /health and /mcp/*.
	dashOpts := DashboardOpts{
		Bus:       eventBus,
		MCTSFn:    func() map[string]any { return mctsEngine.ExportTopKGraph(100) },
		ChaosFn:   func(drillCtx context.Context) string {
			chaosTool := NewChaosDrillTool(workspace, cfg)
			result, err := chaosTool.Execute(drillCtx, map[string]any{
				"target":          cfg.Integrations.ChaosDrillTarget,
				"aggression_level": 3,
				"inject_faults":   false,
			})
			if err != nil {
				return "Chaos drill error: " + err.Error()
			}
			if m, ok := result.(map[string]any); ok {
				if content, ok2 := m["content"].([]map[string]any); ok2 && len(content) > 0 {
					return fmt.Sprintf("%v", content[0]["text"])
				}
			}
			return fmt.Sprintf("%v", result)
		},
		Snapshots: snapshots,
		Mode:      cfg.Server.Mode,
		Version:   "6.3.0",
		AuditFn: func(path string) (string, error) {
			src, readErr := os.ReadFile(path)
			if readErr != nil {
				return "", readErr
			}
			findings, auditErr := astx.AuditGoFile(path, src)
			if auditErr != nil {
				return "", auditErr
			}
			return astx.FormatAuditReport(findings), nil
		},
		RecallFn: func(recallCtx context.Context, query string) []string {
			queryVec, vecErr := embedder.Embed(recallCtx, query)
			if vecErr != nil {
				return nil
			}
			idxs, searchErr := hnswGraph.SearchAuto(recallCtx, queryVec, 5, cpuEngine, cfg.RAG.VectorQuant)
			if searchErr != nil {
				return nil
			}
			results := make([]string, 0, len(idxs))
			for _, idx := range idxs {
				if int(idx) < len(hnswGraph.Nodes) {
					_, content, _, _ := wal.GetDocMeta(hnswGraph.Nodes[idx].DocID)
					if content != "" {
						results = append(results, content)
					}
				}
			}
			return results
		},
		RemFn: func(remCtx context.Context) error {
			remTool := NewRemSleepTool(wal)
			_, err := remTool.Execute(remCtx, map[string]any{
				"learning_rate":         defaultRemLearningRate,
				"session_success_ratio": defaultRemSuccessRatio,
			})
			return err
		},
		PendingTasksFn: func() int {
			pending, _ := state.GetPlannerState()
			return pending
		},
		DiagnoseFn: func(diagCtx context.Context, path string) (map[string]any, error) {
			result, err := inferenceGW.Diagnose(diagCtx, path, "", nil)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"level":      result.Level.String(),
				"confidence": result.Confidence,
				"risk":       result.Risk,
				"summary":    result.Summary,
				"suggestion": result.Suggestion,
				"tokens":     result.Tokens,
			}, nil
		},
		HyperGraphFn: func() *rag.HyperGraph { return globalHyperGraph },
		MerkleFn:     func() string { return computeBrainMerkle(hnswGraph) },
		Workspace:    workspace,
		OracleFn: func() map[string]any {
			r := oracleEngine.Risk()
			return map[string]any{
				"heap_trend_mb_per_min": r.HeapTrendMBPerMin,
				"power_trend_w_per_min": r.PowerTrendWPerMin,
				"fail_prob_24h":         r.FailProb24h,
				"dominant_signal":       r.DominantSignal,
				"alert":                 r.Alert,
				"alert_message":         r.AlertMessage,
				"samples_collected":     r.SamplesCollected,
				"at":                    r.At,
			}
		},
		// [PILAR-XXVII/244.A] Workspace identity + boot ts for Snapshot.
		WorkspaceName: filepath.Base(workspace),
		BootUnix:      time.Now().Unix(),
		// [PILAR-XXXIV/268] Extended metrics: index coverage + dominant lang.
		Graph:        hnswGraph,
		DominantLang: cfg.Workspace.DominantLang,
	}

	// [SRE-HOT-RELOAD] Watch neo.yaml for live config changes (inference, governance, sre thresholds).
	WatchConfig(ctx, cfgPath, cfg, eventBus, queryCache, textCache, embCache, sharedGraph)

	log.Println("[BOOT] MCP server is online — headless RPC mode")

	// MCP protocol handler — shared between stdio and SSE transports
	mcpHandler := func(toolCtx context.Context, req RPCRequest) (any, error) {
		switch req.Method {
		case "initialize":
			// [PILAR-XXVII/243.E] Capture the caller agent so MCP-traffic
			// token accounting knows which client is spending. Single
			// client assumption — each workspace has one active Claude
			// Code / Gemini / MCP-inspector session at a time.
			if len(req.Params) > 0 {
				var init struct {
					ClientInfo struct {
						Name    string `json:"name"`
						Version string `json:"version"`
					} `json:"clientInfo"`
				}
				if err := json.Unmarshal(req.Params, &init); err == nil && init.ClientInfo.Name != "" {
					agentStr := init.ClientInfo.Name + "@" + init.ClientInfo.Version
					setMCPClientAgent(agentStr)
					// [336.A] Build session_agent_id: <workspace>:<boot-unix>:<client>
					sid := fmt.Sprintf("%s:%d:%s", workspace, serverBootTime.Unix(), agentStr)
					setSessionAgentID(sid)
					if werr := wal.SetSessionAgentID(sid); werr != nil {
						log.Printf("[SRE-WARN] persist session_agent_id: %v", werr)
					}
				}
			}
			return map[string]any{
				"protocolVersion": "2025-03-26",
				// [SRE-54] listChanged:false signals stable tool inventory — client may cache schemas without re-fetching.
				"capabilities": map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo": map[string]any{
					"name":    "neoanvil",
					"version": "6.2.0",
				},
			}, nil
		case "notifications/initialized":
			log.Println("[MCP] client handshake complete")
			return nil, nil
		case "tools/list", "tool/list":
			return map[string]any{"tools": registry.List()}, nil
		case "tools/call":
			var call struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &call); err != nil {
				return nil, fmt.Errorf("invalid call parameters: %w", err)
			}

			// [SRE-22.1.3] CanMutate guard — applies to both stdio and SSE transports
			if (call.Name == "neo_apply_patch" || call.Name == "neo_write_safe_file") && !state.CanMutate() {
				msg := "[SRE-VETO] Operación denegada (Firewall de Bloom). La fase cognitiva actual no autoriza mutación de código. Usa la herramienta de transición de fase 'neo_set_cognitive_stage' para avanzar al nivel 4 antes de intentar esto de nuevo."
				return map[string]any{"content": []map[string]any{{"type": "text", "text": msg}}, "isError": true}, nil
			}

			LastActivityTimestamp.Store(time.Now().Unix()) // [SRE-28.3.1]

			// [SRE-50] Ghost Mode governance — cycle tracking, divergence guard, suspension.
			if decision := ghostInterceptor.ShouldAutoApprove(toolCtx, call.Name, fmt.Sprintf("%v", call.Arguments)); decision.Suspended {
				return map[string]any{"content": []map[string]any{{"type": "text", "text": "[SRE-GHOST] " + decision.Reason}}, "isError": true}, nil
			} else if decision.AutoApproved {
				eventBus.Publish(pubsub.Event{Type: pubsub.EventGhostCycle, Payload: map[string]any{
					"tool": call.Name, "cycle": decision.CycleCount,
				}})
			}

			// [SRE-48/62] Synthetic Debate veto — 3-personality consensus before certify. [SRE-62.2/64.2]
			liveCfg := LiveConfig(cfg)
			if call.Name == "neo_sre_certify_mutation" && liveCfg.SRE.ConsensusEnabled {
				mutation := fmt.Sprintf("%v", call.Arguments["mutated_files"])
				debate := consensusEngine.RunDebate(toolCtx, mutation)
				if !debate.Consensus {
					return map[string]any{"content": []map[string]any{{"type": "text", "text": fmt.Sprintf(
						"[SRE-VETO/DEBATE] agreement=%.0f%% < 66%%. Vetoed by: %s",
						debate.WeightedAgreement*100, strings.Join(debate.VetoBy, ", "),
					)}}, "isError": true}, nil
				}
				// Inject certified_by so certify tool can include it in output. [SRE-62.3]
				if call.Arguments == nil {
					call.Arguments = make(map[string]any)
				}
				call.Arguments["_certified_by"] = debate.CertifiedBy
			}

			// [SRE-40] Policy veto — constitutional rules enforced for dangerous operations.
			if policyEngine != nil {
				d := policyEngine.Evaluate(call.Name, map[string]string{"mode": cfg.Server.Mode})
				if !d.Allowed {
					eventBus.Publish(pubsub.Event{Type: pubsub.EventPolicyVeto, Payload: map[string]any{
						"tool": call.Name, "rule": d.Rule, "reason": d.Reason,
					}})
					return map[string]any{"content": []map[string]any{{"type": "text", "text": "[SRE-POLICY] " + d.Reason}}, "isError": true}, nil
				}
			}

			// [PILAR-XXIII] Tool call observability — start/end trace + cooperative timeout.
			// Without this, a hung handler blocks the MCP SDK indefinitely with no log feedback.
			callCtx := toolCtx
			if timeoutSec := cfg.Server.ToolTimeoutSeconds; timeoutSec > 0 {
				var cancel context.CancelFunc
				callCtx, cancel = context.WithTimeout(toolCtx, time.Duration(timeoutSec)*time.Second)
				defer cancel()
			}
			log.Printf("[MCP-TOOL] start name=%s id=%v", call.Name, req.ID)
			start := time.Now()
			res, err := ExecuteWithFinops(func() (any, error) { return registry.Call(callCtx, call.Name, call.Arguments) })
			dur := time.Since(start)
			log.Printf("[MCP-TOOL] end name=%s id=%v dur=%s err=%v", call.Name, req.ID, dur.Truncate(time.Millisecond), err)
			observability.GlobalMetrics.RecordCall(call.Name, dur, err != nil)
			// [274.A/B] Extract sub-tool key (intent/action) before recording so
			// latency histogram tracks both coarse ("neo_radar") and fine-grained
			// ("neo_radar/BLAST_RADIUS") keys. HUD ToolsTab and neo_tool_stats
			// automatically surface the sub-tool rows via the same tracker.
			action, _ := call.Arguments["action"].(string)
			if action == "" {
				if intent, ok := call.Arguments["intent"].(string); ok {
					action = intent
				}
			}
			// [Épica 178/188] Per-tool latency histogram — 512-sample ring
			// buffer + error counter. Feeds p50/p95/p99 + err_rate into
			// BRIEFING / HUD_STATE / neo_cache_stats.
			toolLatency.RecordErr(call.Name, dur, err != nil)
			if action != "" {
				toolLatency.RecordErr(call.Name+"/"+action, dur, err != nil)
			}
			telemetry.ReportToolUsage(call.Name, dur.Seconds(), err != nil)
			// [PILAR-XXVII/243.B] Persist per-tool call metrics to BoltDB
			// so the web HUD + TUI survive neo-mcp restarts. The ring
			// buffer hot-path is < 100 ns, far below the tool handler's
			// own latency — no observable overhead.
			status := "ok"
			errCat := ""
			if err != nil {
				status = "error"
				errCat = "tool_error"
			}
			argsBytes := estimateJSONBytes(call.Arguments)
			resBytes := estimateResultBytes(res)
			observability.GlobalStore.RecordCall(call.Name, action, dur, status, errCat, argsBytes, resBytes)
			// [PILAR-XXVII/243.E] Dual token-tracking — MCP traffic side.
			// Bytes/4 heuristic documented in obs_wire.go:tokensPerChar.
			recordMCPTokens(cfg, call.Name, action, argsBytes, resBytes)
			return res, err
		case "ping":
			return "pong", nil
		default:
			return nil, fmt.Errorf("unsupported method: %s", req.Method)
		}
	}

	// [SRE-24.4] MCP 2025-03-26 spec: OAuth 2.1 discovery endpoints for SSE transport.
	// Claude Code SDK hits /.well-known/oauth-authorization-server before connecting.
	// This is a local-only server — we return valid metadata with a no-op token endpoint.
	// [Area 4.2.A] Build the OpenAPI serve cache once tools are registered.
	// Lazy build inside the cache means contracts are only scanned on the
	// first /openapi.json hit — boot stays fast.
	setupOpenAPIServeCache(workspace, registry)

	go func() {
		sseMux := http.NewServeMux()
		sseAddr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.SSEPort)
		baseURL := "http://" + sseAddr
		// When running behind Nexus, use the dispatcher URL so the MCP SDK's
		// oauth-protected-resource validation matches the client-facing URL.
		if extURL := os.Getenv("NEO_EXTERNAL_URL"); extURL != "" {
			baseURL = extURL
		}

		// OAuth 2.0 Authorization Server Metadata (RFC 8414) — includes registration_endpoint
		sseMux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                          baseURL,
				"authorization_endpoint":          baseURL + "/oauth/authorize",
				"token_endpoint":                  baseURL + "/oauth/token",
				"registration_endpoint":           baseURL + "/oauth/register",
				"response_types_supported":        []string{"code"},
				"grant_types_supported":           []string{"authorization_code", "client_credentials"},
				"code_challenge_methods_supported": []string{"S256"},
			})
		})

		// [Épica 229.2] OAuth 2.0 Protected Resource Metadata (RFC 9728).
		// Claude Code SDK hits this before oauth-authorization-server when using the
		// Streamable-HTTP transport. Returning 404 text/plain made the SDK fail with
		// "Invalid OAuth error response: JSON Parse error" — it couldn't decode the
		// body as JSON. Returning a valid JSON metadata document lets the SDK
		// discover the local authorization server and proceed to the token flow.
		sseMux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resource":              baseURL,
				"authorization_servers": []string{baseURL},
				"bearer_methods_supported": []string{"header"},
				"scopes_supported":      []string{"mcp"},
			})
		})

		// RFC 7591 Dynamic Client Registration — echo redirect_uris from request, fill defaults
		sseMux.HandleFunc("/oauth/register", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, `{"error":"method_not_allowed"}`, http.StatusMethodNotAllowed)
				return
			}
			// Parse the registration request to echo back redirect_uris
			var req struct {
				RedirectURIs []string `json:"redirect_uris"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if len(req.RedirectURIs) == 0 {
				req.RedirectURIs = []string{"http://localhost"}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"client_id":                  "neo-local-client",
				"client_secret":              "neo-local-secret",
				"client_id_issued_at":        time.Now().Unix(),
				"client_secret_expires_at":   0,
				"redirect_uris":              req.RedirectURIs,
				"grant_types":                []string{"authorization_code", "client_credentials"},
				"response_types":             []string{"code"},
				"token_endpoint_auth_method": "client_secret_post",
			})
		})

		// No-op token endpoint — issues a static local token (localhost only, no real auth)
		sseMux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "neo-local-token",
				"token_type":   "bearer",
				"expires_in":   86400,
			})
		})

		// No-op authorize endpoint — redirects immediately with code
		sseMux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
			redirectURI := r.URL.Query().Get("redirect_uri")
			state := r.URL.Query().Get("state")
			if redirectURI == "" {
				http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
				return
			}
			sep := "?"
			if strings.Contains(redirectURI, "?") {
				sep = "&"
			}
			target := redirectURI + sep + "code=neo-local-code&state=" + state
			http.Redirect(w, r, target, http.StatusFound)
		})

		// [SRE-85.C] Single RPC endpoint — Nexus proxies /mcp/message here.
		// [Area 6.1.D] Wrap with otelx span: extract Traceparent from
		// the request, start a child span tagged with the upstream
		// trace ID. Noop tracer = zero-cost wrapping.
		mcpHandlerFn := server.HandleMessage(mcpHandler)
		sseMux.HandleFunc(cfg.Server.SSEMessagePath, func(w http.ResponseWriter, r *http.Request) {
			ctx, span := otelx.StartSpan(r.Context(), "mcp.message")
			defer span.End()
			if tp := r.Header.Get(otelx.TraceParentHeader); tp != "" {
				if tid := otelx.ParseTraceParent(tp); tid != "" {
					span.SetAttribute("upstream.trace_id", tid)
				}
			}
			mcpHandlerFn(w, r.WithContext(ctx))
		})

		// [SRE-85.A.4] Register dashboard data endpoints on sseMux (headless worker).
		// Nexus proxies these to the operator HUD.
		RegisterDashboardAPI(sseMux, dashOpts)

		// [285.B] GET /internal/memex/recent?n=<N>&since=<unix>
		// Returns recent memex entries for cross-workspace sync (Nexus-internal only).
		sseMux.HandleFunc("/internal/memex/recent", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			n := 20
			if v := r.URL.Query().Get("n"); v != "" {
				if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
					n = parsed
				}
			}
			var since time.Time
			if v := r.URL.Query().Get("since"); v != "" {
				if ts, err := strconv.ParseInt(v, 10, 64); err == nil {
					since = time.Unix(ts, 0)
				}
			}
			memEntries, err := state.MemexRead(since)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if len(memEntries) > n {
				memEntries = memEntries[len(memEntries)-n:]
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(memEntries); err != nil {
				log.Printf("[285.B] encode error: %v", err)
			}
		})

		// [285.C] POST /internal/memex/import
		// Imports memex entries from siblings; deduplicates by SHA256(topic+content).
		sseMux.HandleFunc("/internal/memex/import", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var incoming []state.MemexEntry
			if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			imported := 0
			for _, e := range incoming {
				h := sha256.Sum256([]byte(e.Topic + "\x00" + e.Content))
				key := fmt.Sprintf("%x", h)
				if state.MemexHasKey(key) {
					continue
				}
				if err := state.MemexImport(e, key); err != nil {
					log.Printf("[285.C] import error topic=%q: %v", e.Topic, err)
					continue
				}
				imported++
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"imported":%d,"received":%d}`, imported, len(incoming))
		})

		// [292.C] POST /internal/contract/alert — receives breaking changes from peer workspaces.
		// Stores in BoltDB bucket "contract_alerts" for BRIEFING to display.
		sseMux.HandleFunc("/internal/contract/alert", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var payload struct {
				Breaking      []BreakingChange `json:"breaking"`
				FromWorkspace string           `json:"from_workspace"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if len(payload.Breaking) == 0 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			key := payload.FromWorkspace + ":" + fmt.Sprintf("%d", time.Now().UnixNano())
			if err := state.ContractAlertWrite(key, payload.Breaking); err != nil {
				log.Printf("[292.C] contract_alert write error: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			log.Printf("[292.C] contract alert stored: from=%s breaking=%d", payload.FromWorkspace, len(payload.Breaking))
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"stored":%d}`, len(payload.Breaking))
		})

		// [Épica 296.C] Sibling refresh — Nexus broadcasts after any knowledge store write.
		sseMux.HandleFunc("/internal/knowledge/refresh", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if knowledgeStore == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if loadErr := hotCache.Load(knowledgeStore); loadErr != nil {
				log.Printf("[296.C] knowledge refresh: hotcache reload failed: %v", loadErr)
				http.Error(w, "reload failed", http.StatusInternalServerError)
				return
			}
			hot, total := hotCache.Stats()
			log.Printf("[296.C] knowledge hot cache refreshed: %d hot / %d total", hot, total)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"hot":%d,"total":%d}`, hot, total)
		})

		// [335.A] POST /internal/session/broadcast — Nexus fan-outs a peer workspace's
		// certified mutation so this workspace can mirror it in peer_session_state.
		sseMux.HandleFunc("/internal/session/broadcast", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var payload struct {
				WorkspaceID string `json:"workspace_id"`
				MutatedFile string `json:"mutated_file"`
				CertifiedAt int64  `json:"certified_at"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.WorkspaceID == "" || payload.MutatedFile == "" {
				http.Error(w, "invalid payload", http.StatusBadRequest)
				return
			}
			cap := 50
			if cfg.SRE.PeerSessionMirrorCap > 0 {
				cap = cfg.SRE.PeerSessionMirrorCap
			}
			if err := wal.StorePeerSessionMutation(payload.WorkspaceID, payload.MutatedFile, payload.CertifiedAt, cap); err != nil {
				log.Printf("[335.A] peer session store error: %v", err)
				http.Error(w, "store failed", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})

		// [334.B] POST /internal/openapi/refresh — Nexus calls this on siblings when the
		// spec file hash changes in any member workspace. Invalidates the local HotCache
		// so the next ParseOpenAPIContracts call re-reads the spec from disk.
		// [Area 4.2.A] Also drops the rendered openapi.json cache so the next GET re-builds.
		sseMux.HandleFunc("/internal/openapi/refresh", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			cpg.InvalidateOpenAPICache(workspace)
			openAPIServeCache.InvalidateCache()
			w.WriteHeader(http.StatusNoContent)
		})

		// [Area 4.2.A] GET /openapi.json — auto-generated OpenAPI 3.0 spec
		// covering the dispatcher's HTTP routes + the MCP tool registry
		// (under x-mcp-tools). Pass `?include_internal=true` to surface
		// /internal/* endpoints (default excludes them — loopback only).
		sseMux.Handle("/openapi.json", openAPIServeCache.Handler())

		// [Area 4.2.C] GET /docs — Swagger UI rendering /openapi.json.
		// Loads swagger-ui-dist from CDN at view time so the binary
		// stays small (no 3MB of embedded JS). For air-gapped operators,
		// edit pkg/openapi/docs.go to point at an internal mirror.
		sseMux.Handle("/docs", openapiDocsHandler())

		// [287.C] GET /internal/rag/shared/query?k=N  body: {"vector":[…float32]}
		// Returns top-K DocMeta hits from the shared graph (loopback-only, no lock needed).
		sseMux.HandleFunc("/internal/rag/shared/query", func(w http.ResponseWriter, r *http.Request) {
			if sharedGraph == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if !isLoopback(r.RemoteAddr) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			kParam := r.URL.Query().Get("k")
			k := 5
			if n, err := strconv.Atoi(kParam); err == nil && n > 0 {
				k = n
			}
			var req struct {
				Vector []float32 `json:"vector"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Vector) == 0 {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			hits, err := sharedGraph.Search(req.Vector, k)
			if err != nil || len(hits) == 0 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(hits)
		})

		// [287.C] POST /internal/rag/shared/insert  body: {wal_path: "/path/to/hnsw.db"}
		// Merges new docs from the caller's WAL into the shared graph under exclusive lock.
		sseMux.HandleFunc("/internal/rag/shared/insert", func(w http.ResponseWriter, r *http.Request) {
			if sharedGraph == nil || r.Method != http.MethodPost {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if !isLoopback(r.RemoteAddr) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			var req struct {
				WALPath string `json:"wal_path"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.WALPath == "" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			srcWAL, err := rag.OpenWAL(req.WALPath)
			if err != nil {
				http.Error(w, "open wal: "+err.Error(), http.StatusInternalServerError)
				return
			}
			defer srcWAL.Close()
			if lockErr := sharedGraph.TryLock(); lockErr != nil {
				http.Error(w, "lock busy", http.StatusConflict)
				return
			}
			defer sharedGraph.Unlock()
			added, err := sharedGraph.MergeFrom(srcWAL)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			log.Printf("[287.C] shared insert: +%d docs from %s", added, req.WALPath)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"added":%d}`, added)
		})

		// Health probe for Nexus watchdog / verifyBoot — must return 200 so the
		// dispatcher marks this child StatusRunning and enables proxy routing.
		sseMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"status":"ok","mode":"%s"}`, cfg.Server.Mode)
		})
		// [ÉPICA 148.B] Boot progress endpoint — disambiguates "starting"
		// from "stuck" while LoadGraph is still running on a multi-GB WAL.
		sseMux.HandleFunc("/boot_progress", handleBootProgress)

		// [145.J] If Nexus pre-reserved the port and passed it via ExtraFiles,
		// accept on the inherited fd to close the TOCTOU window. Otherwise,
		// bind normally (standalone / direct invocation).
		var sseListener net.Listener
		if fdStr := os.Getenv("NEO_INHERITED_LISTENER_FD"); fdStr != "" {
			fd, _ := strconv.Atoi(fdStr)
			f := os.NewFile(uintptr(fd), "listener")
			inherited, lerr := net.FileListener(f)
			f.Close() // net.FileListener dups the fd; close the original
			if lerr == nil {
				sseListener = inherited
			} else {
				log.Printf("[SRE-WARN] net.FileListener fd=%d failed (%v) — falling back to net.Listen", fd, lerr)
			}
		}
		if sseListener == nil {
			var lerr error
			sseListener, lerr = net.Listen("tcp", sseAddr)
			if lerr != nil {
				log.Printf("[SRE-WARN] SSE listen %s: %v", sseAddr, lerr)
				return
			}
		}
		log.Printf("[SRE-85] Worker HTTP available at http://%s (headless RPC + dashboard APIs)", sseAddr)
		if err := http.Serve(sseListener, sseMux); err != nil {
			log.Printf("[SRE-WARN] SSE server failed: %v", err)
		}
	}()

	go bootstrapWorkspace(ctx, workspace, hnswGraph, wal, embedder, cpuEngine, lexicalIdx, cfg, jobs, eventBus)
	// [BLAST_RADIUS dep-graph fix 3/3] Snapshot-boot safety net — populates the
	// GRAPH_EDGES dep-graph from a cheap import-only walk when it is empty, so
	// BLAST_RADIUS doesn't stay on graph_status:empty after a fast-boot that
	// re-embeds nothing. No-ops once the graph is populated.
	go backfillDepGraph(workspace, wal, hnswGraph, cfg)

	// [Épica 330.C] Archive INC-*.md older than sre.inc_archive_days BEFORE indexing
	// so BM25 and HNSW only see the recent corpus. Synchronous, cheap (mtime scan + rename).
	incidents.ArchiveOldIncidents(workspace, cfg.SRE.INCArchiveDays)

	// [Épica 169.B] Build BM25-only incident index synchronously — guaranteed
	// to succeed regardless of embedder state. Runs before the HNSW retry so
	// INCIDENT_SEARCH has working results from the first tool call.
	incidents.IndexIncidentsBM25Only(workspace, incLexIdx)

	// [353.A] Consolidated post-init async bundle: incident HNSW index retry +
	// Nexus debt check. Spawning both goroutines here keeps main()'s CC bounded.
	bootPostIncidentTasks(ctx, workspace, cfg, embedder, hnswGraph, wal, cpuEngine, eventBus)

	// [SRE-22.2.1] Passive validation hook: validate .go files via AST before indexing
	passiveValidator := rag.ValidateFunc(func(filename string) error {
		if filepath.Ext(filename) == ".go" {
			src, err := os.ReadFile(filename)
			if err != nil {
				return err
			}
			return astx.ValidateSyntax(context.Background(), src, filename)
		}
		return nil
	})
	watcher, err := rag.NewAutoIndexer(workspace, lexicalIdx, cfg.Workspace.IgnoreDirs, cfg.Workspace.AllowedExtensions, jobs, passiveValidator)
	if err != nil {
		log.Printf("[SRE-WARN] failed to initialize AutoIndexer: %v", err)
	} else {
		watcher.Start(ctx)
		defer watcher.Stop()
	}

	log.Println("[BOOT] 0. INICIALIZAR BIOSENSORES Y PROFILING (Aislado en Localhost)")
	diagServer := sre.NewDiagnosticsServer(fmt.Sprintf("127.0.0.1:%d", cfg.Server.DiagnosticsPort))
	diagServer.Start()
	sre.InitPulseEmitter() // [SRE] Hook para latido TUI de Termodinámica
	defer func() {
		log.Println("[SRE] Apagando panel de diagnósticos...")
		_ = diagServer.Shutdown(ctx)
	}()

	// =====================================================================
	// [SRE-DECOUPLED] La telemetría industrial (puerto 8081) ahora vive en cmd/sandbox/main.go
	log.Println("[BOOT] 💤 Demonio MCP operando como Agente Cognitivo Puro. Servidor Industrial desacoplado.")
	// =====================================================================

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		client := sre.SafeHTTPClient()
		for {
			<-ticker.C
			// [SRE-24.1.2] Sync MCP IO counters into telemetry state for TUI/HUD
			telemetry.SetIOStats(bytesReceived.Load(), bytesSent.Load())

			payload := mctsEngine.ExportTopKGraph(100)
			body, err := json.Marshal(payload)
			if err == nil {
				resp, _ := client.Post(cfg.Integrations.SandboxBaseURL+"/api/v1/sre/mcts_sync", "application/json", bytes.NewBuffer(body))
				if resp != nil {
					resp.Body.Close()
				}
			}

			toolsPayload := telemetry.GetActiveTools()
			if len(toolsPayload) > 0 {
				toolsBody, err := json.Marshal(toolsPayload)
				if err == nil {
					respTool, _ := client.Post(cfg.Integrations.SandboxBaseURL+"/api/v1/sre/tools_sync", "application/json", bytes.NewBuffer(toolsBody))
					if respTool != nil {
						respTool.Body.Close()
					}
				}
			}
		}
	}()

	// [337.A] Presence heartbeat — POSTs session identity to Nexus every 30s
	// so peer workspaces can see this agent is active.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		hbClient := sre.SafeInternalHTTPClient(3)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				base := nexusDispatcherBase()
				if base == "" {
					continue
				}
				payload, err := json.Marshal(map[string]any{
					"workspace_id":       workspace,
					"session_agent_id":   currentSessionAgentID(),
					"last_activity_unix": time.Now().Unix(),
					"active_tools":       []string{},
				})
				if err != nil {
					continue
				}
				url := base + "/internal/presence" //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: url derived from nexusDispatcherBase
				resp, herr := hbClient.Post(url, "application/json", bytes.NewReader(payload))
				if herr == nil {
					resp.Body.Close()
				}
			}
		}
	}()

	// [PILAR-XXV/186] Cache pulse — emits an aggregated EventCachePulse
	// every 10 s with QueryCache + TextCache stats + search-path counters.
	// Rate-limited by design: per-call pub/sub would flood the bus with
	// hot-path noise. The 10-second cadence is cheap enough for the HUD
	// dashboard to graph over time and long enough that aggregate ratios
	// stabilise before each sample.
	// [PILAR-XXV/193] The same goroutine also fires a one-shot
	// EventCacheThrash when evict_rate > 30% — at most once per 10 min
	// so the HUD banner is informative without being spammy.
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		const thrashThreshold = 0.30
		const thrashCooldown = 10 * time.Minute
		var lastThrashEmit time.Time
		emitThrash := func(scope string, evicts, requests uint64, ratio float64) {
			if time.Since(lastThrashEmit) < thrashCooldown {
				return
			}
			lastThrashEmit = time.Now()
			eventBus.Publish(pubsub.Event{Type: pubsub.EventCacheThrash, Payload: map[string]any{
				"scope":       scope,
				"evict_rate":  ratio,
				"evictions":   evicts,
				"requests":    requests,
				"suggestion":  "raise query_cache_capacity in neo.yaml or call neo_cache_resize",
			}})
		}
		for range ticker.C {
			payload := map[string]any{}
			if queryCache != nil {
				h, m, e, sz := queryCache.Stats()
				payload["qcache"] = map[string]any{
					"hits": h, "misses": m, "evicts": e, "size": sz,
					"hit_ratio": queryCache.HitRatio(),
				}
				if req := h + m; req > 20 && float64(e)/float64(req) > thrashThreshold {
					emitThrash("query_cache", e, req, float64(e)/float64(req))
				}
			}
			if textCache != nil {
				h, m, e, sz := textCache.Stats()
				payload["tcache"] = map[string]any{
					"hits": h, "misses": m, "evicts": e, "size": sz,
					"hit_ratio": textCache.HitRatio(),
				}
				if req := h + m; req > 20 && float64(e)/float64(req) > thrashThreshold {
					emitThrash("text_cache", e, req, float64(e)/float64(req))
				}
			}
			payload["search_paths"] = map[string]any{
				"binary": rag.SearchBinaryCount(),
				"hybrid": rag.HybridSearchCount(),
				"int8":   rag.SearchInt8Count(),
			}
			eventBus.Publish(pubsub.Event{Type: pubsub.EventCachePulse, Payload: payload})
		}
	}()

	// [SRE-57] Self-Healer Daemon — Panic Reaper + Thermal Rollback + OOM Guard.
	healer := sre.NewSelfHealerDaemon(workspace)

	// [SRE-9.2.1] Homeostasis Térmica (Cronjob de Estado Idle)
	go func() {
		sensor, err := finops.MountRAPL()
		if err != nil {
			log.Printf("[SRE-WARN] failed to mount RAPL sensor: %v", err)
		}
		// [SRE-44] Kinetic SRE sensor — DFT-based hardware bio-feedback.
		kSensor := sre.NewKineticSensor(cfg.Kinetic)
		kSensor.Calibrate() // establish baseline (safe with empty window)
		defer func() {
			if sensor != nil {
				sensor.Close()
			}
		}()

		// [SRE-97.C] Homeostasis jitter — desincroniza ticks entre children para
		// evitar thundering herd contra Ollama cuando N workspaces despiertan en
		// la misma ventana. Sleep derivado del hash del workspace path (0-29s).
		h := fnv.New32a()
		_, _ = h.Write([]byte(workspace))
		jitter := time.Duration(h.Sum32()%30) * time.Second
		log.Printf("[SRE-HOMEOSTASIS] maintenance jitter offset: %v (workspace=%s)", jitter, workspace)
		select {
		case <-time.After(jitter):
		case <-ctx.Done():
			return
		}

		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				log.Println("[SRE-HOMEOSTASIS] Starting maintenance routine (Idle)...")

				// 9.2.2: wal.Vacuum()
				_, _ = wal.Vacuum(context.Background(), workspace, cfg.Workspace.IgnoreDirs)

				// [SRE-72.2] Coldstore: archive RED metrics to cold storage every 5min maintenance tick
				if coldStoreEngine != nil {
					redSummary := observability.GlobalMetrics.EmitSummary(workspace)
					var metrics []coldstore.MetricRecord
					for toolName, m := range redSummary {
						metrics = append(metrics, coldstore.MetricRecord{
							Timestamp: time.Now().Unix(), Category: "tool_latency", MetricName: toolName,
							Value: float64(m["duration"]) / 1000, WorkspaceID: workspace,
						})
					}
					if n, archErr := coldStoreEngine.ArchiveMetrics(ctx, metrics); archErr != nil {
						log.Printf("[COLDSTORE] metric archive error: %v", archErr)
					} else if n > 0 {
						log.Printf("[COLDSTORE] Archived %d tool metrics to cold storage", n)
					}
				}

				// [SRE-28.3.2] REM Sleep: consolidate episodic memory after 5min idle.
				if ts := LastActivityTimestamp.Load(); ts > 0 {
					if time.Since(time.Unix(ts, 0)) > 5*time.Minute {
						TriggerREMSleep(ctx, workspace, wal, hnswGraph, embedder, cpuEngine, coldStoreEngine, sharedGraph)

						// [SRE-94] Federation Dream Synthesis — export local memex vectors to fleet after REM consolidation.
						liveCfg := LiveConfig(cfg)
						if liveCfg.Federation.HarvestTimeoutSec > 0 || liveCfg.Federation.DreamSchedule != "" {
							dreamCfg := federation.NewDreamConfigFromYAML(
								liveCfg.Federation.DreamSchedule,
								liveCfg.Federation.DedupThreshold,
								liveCfg.LLM.OllamaURL,
								liveCfg.LLM.Model,
								liveCfg.Federation.HarvestTimeoutSec,
								liveCfg.Federation.MaxVectorsPerNode,
							)
							nodeID := filepath.Base(workspace)
							go func() {
								if err := federation.DreamSynthesisPipeline(dreamCfg, nil, &ragMemexAdapter{}, nodeID); err != nil {
									log.Printf("[FEDERATION] DreamSynthesis error: %v", err)
								}
							}()
						}
					}
				}

				// [SRE-44] Kinetic SRE: sample hardware state every homeostasis tick.
				kSensor.Sample()
				if LiveConfig(cfg).SRE.KineticMonitoring {
					if reports := kSensor.Analyze(); len(reports) > 0 {
						for _, r := range reports {
							eventBus.Publish(pubsub.Event{Type: pubsub.EventKineticAnomaly, Payload: map[string]any{
								"type": r.Type, "severity": r.Severity, "value": r.CurrentValue, "action": r.Action,
							}})
							log.Printf("[SRE-44] KineticAnomaly: %s severity=%.2fσ — %s", r.Type, r.Severity, r.Action)
							// [SRE-72.1] HyperGraph: record error→hardware edge on anomaly
							if globalHyperGraph != nil {
								var hmem runtime.MemStats
								runtime.ReadMemStats(&hmem)
								globalHyperGraph.RecordErrorHardware(r.Type, float64(hmem.HeapAlloc)/(1024*1024), hmem.NumGC)
							}
							// [SRE-TECH-DEBT] Severe anomalies (>3σ) auto-detected as tech debt
							if r.Severity > 3.0 {
								recordTechDebt(workspace,
									fmt.Sprintf("Kinetic anomaly: %s (%.1fσ)", r.Type, r.Severity),
									fmt.Sprintf("%s\nRecommended action: %s", r.Description, r.Action), "alta")
							}
						}
					}
				}

				// [SRE-40] Sentinel invariant verification every homeostasis tick.
				if policyEngine != nil {
					for _, c := range policyEngine.VerifyInvariants() {
						if !c.Holds {
							log.Printf("[SRE-40] Invariant VIOLATED: %s (%d violations) — %s", c.Name, c.Violated, c.Example)
						}
					}
				}

				// [SRE-13.2.2] MCTS Arena Purge — prevent OOM on 100k node arena
				mctsEngine.ResetArena()

				// 9.2.2: neo_rem_sleep evaluation
				if sensor != nil {
					watts := sensor.ReadWatts_O1(300.0) // 5 min
					log.Printf("[SRE-HOMEOSTASIS] Consumption: %.2f Watts. Adjusting MLP weights...", watts)

					// [SRE-31.3.2] STABILIZING mode: suspend DaemonTool tasks above 60W.
					if overrideWatts := os.Getenv("NEO_RAPL_OVERRIDE_WATTS"); overrideWatts != "" {
						if ow, parseErr := strconv.ParseFloat(overrideWatts, 64); parseErr == nil {
							watts = ow
						}
					}
					// [132.E] Thermal pressure check (sysctl/powermetrics on darwin, /sys/class/thermal on linux).
					thermalCritical := cfg.SRE.ThermalPressureCheck && sre.ThermalPressure() == "critical"
					if watts > 60.0 || thermalCritical {
						if ThermicStabilizing.CompareAndSwap(0, 1) {
							log.Printf("[SRE-STABILIZING] RAPL %.2fW > 60W | thermal_critical=%v — DaemonTool suspended.", watts, thermalCritical)
						}
					} else {
						ThermicStabilizing.Store(0)
					}

					// [SRE-32.1.3] Emit heartbeat with current vitals to Operator HUD.
					EmitHeartbeat(eventBus, watts, cfg.Server.Mode, snapshots)

					// [SRE-36.3.2] Arena thrashing alarm — threshold from neo.yaml rag.arena_miss_rate_threshold.
					if mr := f32Pool.MissRate(); mr > cfg.RAG.ArenaMissRateThreshold {
						_, totalGet, _, totalMiss := f32Pool.Metrics()
						eventBus.Publish(pubsub.Event{
							Type: pubsub.EventArenaThresh,
							Payload: map[string]any{
								"miss_rate":  mr,
								"total_get":  totalGet,
								"total_miss": totalMiss,
							},
						})
						log.Printf("[SRE-36.3.2] Arena thrashing: miss_rate=%.2f%% (get=%d miss=%d) — pool too small", mr*100, totalGet, totalMiss)
					}

					// Use a small adjustment based on power efficiency
					success := watts < 50.0 // Arbitrary threshold

					mlp := wasmx.GetMathMLP()
					if mlp != nil {
						mlp.AdjustWeights(0.01, success)
					}

					// [SRE-61.1] Oracle feed — sample current runtime stats for trend analysis.
					oracleEngine.Feed(sre.SampleFromRuntime(watts))
					// [SRE-61.3] Oracle alert — emit EventOracleAlert when failure probability is high.
					if risk := oracleEngine.Risk(); risk.Alert {
						eventBus.Publish(pubsub.Event{
							Type: pubsub.EventOracleAlert,
							Payload: map[string]any{
								"fail_prob_24h":  risk.FailProb24h,
								"dominant":       risk.DominantSignal,
								"message":        risk.AlertMessage,
							},
						})
					}

					// [SRE-57.2] Thermal Emergency Rollback — git stash after 3 critical ticks.
					if msg := healer.ThermalRollback(watts); msg != "" {
						eventBus.Publish(pubsub.Event{
							Type:    pubsub.EventThermalRollback,
							Payload: map[string]any{"message": msg, "watts": watts},
						})
					}
				}

				// [SRE-57.1] Panic Reaper — restart zombie supervised goroutines.
				if n := healer.RunReaper(); n > 0 {
					log.Printf("[REAPER] Restarted %d zombie goroutine(s).", n)
				}

				// [SRE-57.3] OOM Guard — force GC + FreeOSMemory when heap too large.
				if healer.OOMGuard(f32Pool) {
					eventBus.Publish(pubsub.Event{
						Type:    pubsub.EventOOMGuard,
						Payload: map[string]any{"threshold_mb": healer.OOMThresholdMB},
					})
				}

				// [SRE-58.1] Proactive Context Compression — emit suggest_compress when IO > threshold.
				liveCfgNow := LiveConfig(cfg)
				threshKB := int64(liveCfgNow.SRE.ContextCompressThresholdKB)
				if threshKB <= 0 {
					threshKB = 600
				}
				totalIO := (bytesReceived.Load() + bytesSent.Load()) / 1024
				if totalIO > threshKB {
					eventBus.Publish(pubsub.Event{
						Type: pubsub.EventSuggestCompress,
						Payload: map[string]any{
							"total_kb":     totalIO,
							"threshold_kb": threshKB,
							"message":      "Session IO exceeds threshold — run neo_compress_context to reduce context pressure.",
						},
					})
				}
			}
		}
	}()

	// [SRE-85.C] Headless worker — no stdio transport. All tool calls arrive via
	// HTTP POST /mcp/message from Nexus. Wait for shutdown signal.
	log.Println("[BOOT] Headless RPC worker — waiting for requests via HTTP")
	<-ctx.Done()

	log.Println("[SHUTDOWN] MCP server is shutting down...")
	log.Println("[SRE-SHUTDOWN] OS Signal received. Executing ACID Graceful Teardown...")
	observability.GlobalMetrics.EmitSummary(workspace)
	_ = wal.Sync()

	// [PILAR-XXV/201/210/222] Auto-persist all three cache layers via the
	// consolidated helper. Bounded N per layer (32/16/64). Failures logged,
	// never blocking.
	persistCachesOnShutdown(caches, workspace)
	// [Épica 263.B] Save CPG snapshot on graceful shutdown so next boot is fast.
	cpgMgr.SaveSnapshot(filepath.Join(workspace, cfg.CPG.PersistPath))
	// [BUG-FIX 2026-05-13 v2] HNSW snapshot save SYNCHRONOUSLY in main shutdown.
	// Must run BEFORE `defer cleanupRAG()` (wal.Close) and BEFORE return — the
	// previously-deleted SIGTERM goroutine raced this and exited the process
	// first, leaving the snapshot frozen at boot-time count. Now: this code
	// is the SOLE path that writes the shutdown-time snapshot, so the next
	// boot's stale-check (NodeKeyN comparison) matches and avoids cold rebuild.
	if cfg.RAG.HNSWPersistPath != "" {
		persistPath := filepath.Join(workspace, cfg.RAG.HNSWPersistPath)
		if err := rag.SaveHNSWSnapshot(hnswGraph, wal, persistPath); err != nil {
			log.Printf("[SRE-SHUTDOWN] HNSW snapshot save failed: %v", err)
		} else {
			log.Printf("[SRE-SHUTDOWN] HNSW snapshot saved to %s", persistPath)
		}
	}

	// [SRE-58.3] Ghost Mode GC — clean up sandbox artifacts on clean shutdown.
	// Moved here from the deleted SIGTERM goroutine.
	if liveCfg := LiveConfig(cfg); liveCfg != nil && liveCfg.Governance.GhostMode {
		GhostGC(workspace)
	}
	// [PILAR-XXVII/243.H] Drain + close observability store before exit
	// so the final ring-buffer batch lands on disk. Moved here from the
	// deleted SIGTERM goroutine.
	if observability.GlobalStore != nil {
		_ = observability.GlobalStore.Close()
	}

	// [291.E] Stop all active mock servers to free ports.
	activeMocksMu.Lock()
	for id, ms := range activeMocks {
		ms.Stop()
		delete(activeMocks, id)
	}
	activeMocksMu.Unlock()
	log.Println("[SRE-SHUTDOWN] NeoAnvil safely terminated. NVMe WAL synchronized.")
	// Main returns here — defer cleanupRAG() runs wal.Close().
}
