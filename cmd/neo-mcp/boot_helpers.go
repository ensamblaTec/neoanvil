// boot_helpers.go — [ÉPICA 269.A / 302.C] boot phase helpers to reduce main() CC to ≤ 10.
//
// Each helper extracts a branch-heavy setup block so every responsibility
// compiles to a separately auditable unit (CC ≤ 15 each).
// Every helper that opens a file handle returns a cleanup closure; call with defer.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/ensamblatec/neoanvil/pkg/auth"
	"github.com/ensamblatec/neoanvil/pkg/coldstore"
	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/consensus"
	"github.com/ensamblatec/neoanvil/pkg/cpg"
	"github.com/ensamblatec/neoanvil/pkg/dba"
	"github.com/ensamblatec/neoanvil/pkg/federation"
	"github.com/ensamblatec/neoanvil/pkg/hardware"
	"github.com/ensamblatec/neoanvil/pkg/inference"
	"github.com/ensamblatec/neoanvil/pkg/knowledge"
	"github.com/ensamblatec/neoanvil/pkg/observability"
	"github.com/ensamblatec/neoanvil/pkg/pubsub"
	"github.com/ensamblatec/neoanvil/pkg/rag"
	"github.com/ensamblatec/neoanvil/pkg/sre"
	"github.com/ensamblatec/neoanvil/pkg/state"
	workspacereg "github.com/ensamblatec/neoanvil/pkg/workspace"
)

// bootRAG opens the WAL, recovers the HNSW graph, and populates optional quant companions.
// Fatal on WAL or graph recovery failure — the orchestrator cannot operate without memory.
// Caller must defer the returned cleanup to close the WAL on normal exit.
func bootRAG(ctx context.Context, cfg *config.NeoConfig, workspace string) (wal *rag.WAL, graph *rag.Graph, cleanup func()) {
	walFile := filepath.Join(workspace, cfg.RAG.DBPath)
	dbDir := filepath.Dir(walFile)
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		log.Fatalf("[SRE-FATAL] failed to create db directory: %v", err)
	}
	var err error
	wal, err = rag.OpenWAL(walFile)
	if err != nil {
		log.Fatalf("[SRE-FATAL] cannot initialize WAL — orchestrator cannot operate without memory: %v", err)
	}
	if err = rag.InitGraphRAG(wal); err != nil {
		log.Printf("[SRE-WARN] failed to initialize Graph-RAG structures: %v", err)
	}
	// [ÉPICA 149] HNSW fast-boot snapshot — try the binary blob first.
	// When fresh: skip cold WAL load AND skip the big-bucket sanitizer
	// (149.J — sanitizer page-fault scan accounts for ~10-15s of boot
	// time on multi-GB WALs). The snapshot's bit-exact graph already
	// represents valid state for nodes/edges/vectors; only the small
	// metadata buckets need re-validation at boot.
	bootedFastHNSW := false
	persistPath := filepath.Join(workspace, cfg.RAG.HNSWPersistPath)
	loadStart := time.Now()
	if snap, snapErr := rag.LoadHNSWSnapshot(persistPath); snapErr == nil {
		if stale, reason := rag.IsHNSWSnapshotStale(wal, &snap.Header); !stale {
			graph = snap.Graph
			bootedFastHNSW = true
			globalBootProgress.MarkHNSWBootedFast()
			log.Printf("[BOOT] HNSW fast-boot from snapshot: %d nodes %d-dim in %v",
				len(graph.Nodes), graph.VecDim, time.Since(loadStart))
		} else {
			log.Printf("[BOOT] HNSW snapshot stale (%s) — cold rebuild", reason)
		}
	} else if !errors.Is(snapErr, rag.ErrHNSWSnapshotMissing) {
		log.Printf("[BOOT] HNSW snapshot rejected: %v — cold rebuild", snapErr)
	}

	if bootedFastHNSW {
		// [149.J] Lightweight WAL prep — metadata buckets only. Big-bucket
		// validation deferred to background goroutine to avoid the boot-time
		// page-fault storm.
		bootWALFast(wal, workspace)
		runBackgroundSanitize(ctx, wal, 30*time.Second)
	} else {
		// Cold path needs full sanitize before LoadGraph (the snapshot
		// wasn't usable, so we can't trust the in-memory state until the
		// WAL has been validated).
		bootWAL(wal, workspace)
		// [ÉPICA 148.A] Boot progress observability — only meaningful for the
		// cold path; fast-boot finishes in <5s without need for ticker noise.
		globalBootProgress.StartHNSWLoad(walFile)
		tickerDone := make(chan struct{})
		go runBootProgressTicker(tickerDone)
		graph, err = wal.LoadGraph(ctx)
		close(tickerDone)
		globalBootProgress.FinishHNSWLoad()
		if err != nil {
			_ = wal.Close()
			log.Fatalf("[SRE-FATAL] failed to recover HNSW graph from WAL: %v", err)
		}
		log.Printf("[BOOT] vector load: %d nodes %d-dim in %v", len(graph.Nodes), graph.VecDim, time.Since(loadStart))
		// Async populate snapshot for next boot. Doesn't block startup.
		// Concurrency is safe via Graph.snapshotMu RLock inside SaveHNSWSnapshot.
		go func(g *rag.Graph, w *rag.WAL, path string) {
			if err := rag.SaveHNSWSnapshot(g, w, path); err != nil {
				log.Printf("[BOOT] async HNSW snapshot save failed: %v", err)
				return
			}
			log.Printf("[BOOT] HNSW snapshot saved to %s", path)
		}(graph, wal, persistPath)
	}
	populateQuantCompanion(graph, cfg.RAG.VectorQuant)
	return wal, graph, func() { _ = wal.Close() }
}

// federationBundle holds the project-shared resources opened during boot. [269.A]
type federationBundle struct {
	KS      *knowledge.KnowledgeStore
	HC      *knowledge.HotCache
	SG      *rag.SharedGraph
	Cleanup func()
}

// bootProjectFederation opens the KnowledgeStore and SharedGraph for project-mode workspaces.
// resolveProjectPath canonicalises a path from .neo-project/neo.yaml.
// Relative paths are joined against projRoot; absolute paths are returned as-is.
func resolveProjectPath(projRoot, p string) string {
	if !filepath.IsAbs(p) {
		return filepath.Clean(filepath.Join(projRoot, p))
	}
	return filepath.Clean(p)
}

// KS and SG are nil when the workspace is not in a project (graceful degradation).
// Caller must defer bundle.Cleanup() to close open file handles.
func bootProjectFederation(cfg *config.NeoConfig, workspace string) *federationBundle {
	hc := knowledge.NewHotCache()
	fb := &federationBundle{HC: hc, Cleanup: func() {}}

	projDir := findNeoProjectDir(workspace)
	// projRoot is the parent of the .neo-project/ directory — relative paths in
	// .neo-project/neo.yaml are expressed from that root, not from projDir itself.
	projRoot := ""
	if projDir != "" {
		projRoot = filepath.Dir(projDir)
	}

	fb.KS = bootKnowledgeStore(cfg, workspace, projDir, projRoot, hc)
	if projDir != "" {
		fb.SG = bootSharedGraph(cfg, workspace, projDir, projRoot)
	}

	fb.Cleanup = func() {
		if fb.KS != nil {
			_ = fb.KS.Close()
		}
		if fb.SG != nil {
			_ = fb.SG.Close()
		}
	}
	return fb
}

// bootKnowledgeStore opens the KnowledgeStore, wires the sync dir and watcher,
// and pre-loads the HotCache. Returns nil on open failure. [302.C]
//
// [354.Z-redesign] When a project has coordinator_workspace configured and
// this workspace is NOT the coordinator, skip opening entirely — the
// coordinator owns the bbolt flock and non-coordinators proxy tier:"project"
// ops via MCP to the coordinator. Keeps boot deterministic and fast.
func bootKnowledgeStore(cfg *config.NeoConfig, workspace, projDir, projRoot string, hc *knowledge.HotCache) *knowledge.KnowledgeStore {
	if projDir != "" && cfg.Project != nil && cfg.Project.CoordinatorWorkspace != "" && !isCoordinatorWorkspace(workspace, cfg) {
		log.Printf("[NEO-BOOT] knowledge: non-coordinator workspace — tier:\"project\" will proxy to %q", cfg.Project.CoordinatorWorkspace)
		return nil
	}
	ksPath := filepath.Join(workspace, ".neo", "db", "knowledge.db")
	if projDir != "" {
		ksPath = filepath.Join(projDir, "db", "knowledge.db")
	}
	ks, ksErr := knowledge.Open(ksPath)
	if ksErr != nil {
		log.Printf("[SRE-WARN] Knowledge Store open failed (knowledge ops disabled): %v", ksErr)
		return nil
	}
	if projDir != "" {
		knowledgeDir := filepath.Join(projDir, "knowledge")
		if cfg.Project != nil && cfg.Project.KnowledgePath != "" {
			knowledgeDir = resolveProjectPath(projRoot, cfg.Project.KnowledgePath)
		}
		ks.SetSyncDir(knowledgeDir)
		if ensureErr := knowledge.EnsureSyncDir(knowledgeDir); ensureErr != nil {
			log.Printf("[SRE-WARN] Knowledge sync dir ensure failed: %v", ensureErr)
		}
		if _, watchErr := knowledge.StartWatcher(knowledgeDir, ks, hc); watchErr != nil {
			log.Printf("[SRE-WARN] Knowledge watcher start failed: %v", watchErr)
		}
		go knowledge.BootstrapFromFiles(knowledgeDir, ks, hc)
	}
	if loadErr := hc.Load(ks); loadErr != nil {
		log.Printf("[SRE-WARN] HotCache load failed: %v", loadErr)
	}
	hot, total := hc.Stats()
	log.Printf("[NEO-BOOT] knowledge: %d hot / %d total entries loaded", hot, total)
	return ks
}

// bootSharedGraph opens the project shared HNSW tier in the correct mode:
// read-write for the coordinator workspace, read-only for all others. [287.C / 314.B]
func bootSharedGraph(cfg *config.NeoConfig, workspace, projDir, projRoot string) *rag.SharedGraph {
	sharedPath := filepath.Join(projDir, "db", "shared.db")
	if cfg.Project != nil && cfg.Project.SharedMemoryPath != "" {
		sharedPath = resolveProjectPath(projRoot, cfg.Project.SharedMemoryPath)
	}
	isCoord := isCoordinatorWorkspace(workspace, cfg)
	var sg *rag.SharedGraph
	var sgErr error
	if isCoord {
		sg, sgErr = rag.OpenSharedGraph(sharedPath)
	} else {
		sg, sgErr = rag.OpenSharedGraphReadOnly(sharedPath)
	}
	if sgErr != nil {
		log.Printf("[SRE-WARN] SharedGraph open failed (shared tier disabled): %v", sgErr)
		return nil
	}
	mode := "rw"
	if !isCoord {
		mode = "ro"
	}
	log.Printf("[NEO-BOOT] shared HNSW tier: %s [%s]", sharedPath, mode)
	return sg
}

// isCoordinatorWorkspace returns true when workspace matches the coordinator_workspace
// field in .neo-project/neo.yaml (exact path or basename match). [314.B]
// When no coordinator is configured, every workspace behaves as coordinator (legacy mode).
func isCoordinatorWorkspace(workspace string, cfg *config.NeoConfig) bool {
	if cfg.Project == nil || cfg.Project.CoordinatorWorkspace == "" {
		return true // legacy: no coordinator configured → all open read-write (pre-314.B)
	}
	coord := cfg.Project.CoordinatorWorkspace
	return workspace == coord || filepath.Base(workspace) == coord || filepath.Base(workspace) == filepath.Base(coord)
}

// resolveCoordinatorWSID returns the workspace registry ID for a coord string
// (an absolute path or basename from .neo-project/neo.yaml coordinator_workspace).
// Returns "" when no match is found. [354.Z-redesign piece 2]
func resolveCoordinatorWSID(coord string) string {
	reg, err := workspacereg.LoadRegistry()
	if err != nil {
		return ""
	}
	coordBase := filepath.Base(coord)
	for _, e := range reg.Workspaces {
		if e.Path == coord || filepath.Base(e.Path) == coord || filepath.Base(e.Path) == coordBase {
			return e.ID
		}
	}
	return ""
}

// sreBundle holds SRE subsystem instances wired during boot. [269.A]
// policyEngine, dreamEngine, and globalHyperGraph are set as package-level vars inside bootSRE
// because several tool handlers reference them directly without the bundle.
type sreBundle struct {
	Ghost     *GhostInterceptor
	Consensus *consensus.ConsensusEngine
	Oracle    *sre.OracleEngine
	InfGW     *inference.Gateway
}

// bootSRE creates all SRE subsystems. Sets the package-level policyEngine, dreamEngine,
// and globalHyperGraph vars that handlers reference without going through a parameter.
func bootSRE(cfg *config.NeoConfig, workspace string) *sreBundle {
	ghost := NewGhostInterceptor(cfg, workspace)
	log.Printf("[BOOT] GhostInterceptor initialized (ghost_mode=%v max_cycles=%d)",
		cfg.Governance.GhostMode, cfg.Governance.GhostModeMaxCycles)

	policyEngine = sre.NewPolicyEngine(cfg.Sentinel)
	log.Printf("[BOOT] PolicyEngine initialized with constitutional rules")

	dreamEngine = sre.NewDreamEngine(cfg.Sentinel.ImmunityActivationMin, cfg.Sentinel.ImmunityConfidenceInit)

	globalHyperGraph = rag.NewHyperGraph(
		cfg.HyperGraph.MaxImpactDepth,
		cfg.HyperGraph.RiskDecayFactor,
		cfg.HyperGraph.MinRiskThreshold,
	)

	infGW := inference.NewGateway(cfg.Inference, cfg.AI.BaseURL)
	infGW.SetTokenReporter(func(agent, tool, promptType string, inTokens, outTokens int) {
		if observability.GlobalStore == nil {
			return
		}
		liveCfg := LiveConfig(cfg)
		cost := 0.0
		if liveCfg != nil {
			cost = liveCfg.Inference.UsageCost(agent, inTokens, outTokens)
		}
		observability.GlobalStore.RecordTokens(observability.TokenEntry{
			Source:       observability.SourceInternalInference,
			Agent:        agent,
			Tool:         tool,
			PromptType:   promptType,
			Model:        agent,
			InputTokens:  inTokens,
			OutputTokens: outTokens,
			Calls:        1,
			CostUSD:      cost,
		})
	})
	log.Printf("[BOOT] Inference Gateway initialized (mode=%s, cloud_model=%s, budget=%d)",
		cfg.Inference.Mode, cfg.Inference.CloudModel, cfg.Inference.CloudTokenBudgetDaily)

	return &sreBundle{
		Ghost:     ghost,
		Consensus: consensus.NewConsensusEngine(cfg),
		Oracle:    newConfiguredOracleEngine(cfg),
		InfGW:     infGW,
	}
}

// bootCPGManager opens the CPG BoltDB, attempts a fast snapshot restore, falls back to a
// cold SSA build, and schedules the periodic snapshot goroutine. Returns the manager and a
// cleanup that closes the BoltDB. [269.A]
func bootCPGManager(ctx context.Context, cfg *config.NeoConfig, workspace string) (*cpg.Manager, func()) {
	// "skip" sentinel disables CPG for non-Go workspaces (e.g. TypeScript frontends).
	if cfg.CPG.PackagePath == "skip" {
		log.Printf("[CPG] disabled via package_path=skip — no Go packages to analyze")
		return cpg.NewManager(), func() {}
	}

	cpgDBPath := filepath.Join(workspace, ".neo", "db", "cpg.db")
	cpgDB, cpgDBErr := bolt.Open(cpgDBPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	cpgMgr := cpg.NewManager()
	if cpgDBErr != nil {
		log.Printf("[CPG-WARN] cannot open CPG cache db (%v); structural rank disabled", cpgDBErr)
		return cpgMgr, func() {}
	}

	cpgMgrCfg := cpg.ManagerConfig{
		PageRankIters:   cfg.CPG.PageRankIters,
		PageRankDamping: cfg.CPG.PageRankDamping,
		ActivationAlpha: cfg.CPG.ActivationAlpha,
		MaxHeapMB:       cfg.CPG.MaxHeapMB,
		MaxHeapMBFn:     func() int { return LiveConfig(cfg).CPG.MaxHeapMB },
	}

	persistPath := filepath.Join(workspace, cfg.CPG.PersistPath)
	if g, hdr, loadErr := cpg.LoadCPG(persistPath); loadErr == nil && !cpg.IsCPGStale(workspace, hdr) {
		cpgMgr.LoadSnapshot(g)
		log.Printf("[CPG] fast-boot: restored snapshot (%d nodes) — skipping SSA rebuild", len(g.Nodes))
	} else {
		if loadErr != nil && loadErr != cpg.ErrSchemaMismatch && !errors.Is(loadErr, os.ErrNotExist) {
			log.Printf("[CPG] snapshot load failed (%v) — cold build", loadErr)
		}
		pkgDir := filepath.Join(workspace, cfg.CPG.PackageDir)
		cpgMgr.Start(ctx, cfg.CPG.PackagePath, workspace, pkgDir, cpgDB, cpgMgrCfg)
		log.Printf("[CPG] Manager started — building SSA graph in background")
	}

	if cfg.CPG.PersistIntervalMinutes > 0 {
		go func() {
			ticker := time.NewTicker(time.Duration(cfg.CPG.PersistIntervalMinutes) * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					cpgMgr.SaveSnapshot(persistPath)
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	return cpgMgr, func() { _ = cpgDB.Close() }
}

// dbaBundle holds SQL DBA + cold-store OLAP engines opened during boot. [302.C]
type dbaBundle struct {
	DBA     *dba.Analyzer
	Cold    *coldstore.Engine
	Cleanup func()
}

// bootDBAEngines opens the SQL DBA Analyzer (fatal on failure) and the cold-store OLAP engine
// (warn-only — orchestrator continues without analytics when cold store is unavailable). [302.C]
func bootDBAEngines(workspace string, cfg *config.NeoConfig) *dbaBundle {
	dbaDBPath := filepath.Join(workspace, ".neo", "db", "auto_schema.db")
	dbaEngine, err := dba.NewAnalyzer(dbaDBPath)
	if err != nil {
		log.Fatalf("[SRE-FATAL] Fallo acoplando Motor Mutacional SQL DBA: %v", err)
	}
	coldStorePath := filepath.Join(workspace, ".neo", "db", "coldstore.db")
	cold, csErr := coldstore.OpenEngine(coldStorePath, cfg.Coldstore)
	if csErr != nil {
		log.Printf("[SRE-WARN] Cold store init failed (analytics disabled): %v", csErr)
		return &dbaBundle{DBA: dbaEngine, Cleanup: func() {}}
	}
	log.Printf("[BOOT] Cold storage OLAP engine initialized at %s", coldStorePath)
	return &dbaBundle{
		DBA:     dbaEngine,
		Cold:    cold,
		Cleanup: func() { _ = cold.Close() },
	}
}

// bootObservability opens the per-workspace observability BoltDB and launches the
// memstats, backup, and SSE-mirror goroutines. Returns a cleanup that closes the store.
// Warn-only on failure — the orchestrator continues without metrics persistence. [302.C]
func bootObservability(ctx context.Context, workspace string, cpgMgr *cpg.Manager, queryCache *rag.QueryCache, textCache *rag.TextCache, embCache *rag.Cache[[]float32], bus *pubsub.Bus) func() {
	obsStore, err := observability.Open(workspace)
	if err != nil {
		log.Printf("[SRE-WARN] failed to initialize observability store: %v", err)
		return func() {}
	}
	observability.GlobalStore = obsStore
	log.Printf("[BOOT] observability store ready at %s", obsStore.Path())
	startMemStatsLoop(ctx, cpgMgr, queryCache, textCache, embCache)
	startBackupLoop(ctx, workspace)
	subscribeEventsToStore(bus)
	return func() { _ = obsStore.Close() }
}

// applyGPUConfig detects GPU availability (honouring hardware.gpu_available override)
// and backfills adaptive config params (ollama_model, embed_concurrency, batch_size)
// so the rest of boot uses GPU-tuned values automatically. [GPU-AWARE / Option A+C]
func applyGPUConfig(cfg *config.NeoConfig) hardware.GPUInfo {
	switch cfg.Hardware.GPUAvailable {
	case "true":
		// forced-on: synthesise minimal GPUInfo without calling nvidia-smi
		gpu := hardware.GPUInfo{Available: true}
		log.Println("[BOOT] GPU: forced-available via hardware.gpu_available=true")
		applyGPUOverrides(cfg)
		return gpu
	case "false":
		log.Println("[BOOT] GPU: disabled via hardware.gpu_available=false")
		return hardware.GPUInfo{}
	default: // "auto"
		gpu := hardware.Detect()
		if !gpu.Available {
			log.Println("[BOOT] GPU: not detected (CPU-only mode)")
			return gpu
		}
		log.Printf("[BOOT] GPU: %s | VRAM %d/%d MiB | driver %s",
			gpu.DeviceName, gpu.VRAMFreeMB, gpu.VRAMTotalMB, gpu.DriverVersion)
		applyGPUOverrides(cfg)
		return gpu
	}
}

// applyGPUOverrides writes GPU-tuned values into cfg when GPU is confirmed available.
func applyGPUOverrides(cfg *config.NeoConfig) {
	if cfg.Hardware.GPUOllamaModel != "" {
		log.Printf("[BOOT] GPU: overriding inference.ollama_model → %s", cfg.Hardware.GPUOllamaModel)
		cfg.Inference.OllamaModel = cfg.Hardware.GPUOllamaModel
	}
	if cfg.Hardware.GPUEmbedConcurrency > 0 {
		log.Printf("[BOOT] GPU: overriding rag.embed_concurrency → %d", cfg.Hardware.GPUEmbedConcurrency)
		cfg.RAG.EmbedConcurrency = cfg.Hardware.GPUEmbedConcurrency
	}
	if cfg.Hardware.GPUBatchSize > 0 {
		log.Printf("[BOOT] GPU: overriding rag.batch_size → %d", cfg.Hardware.GPUBatchSize)
		cfg.RAG.BatchSize = cfg.Hardware.GPUBatchSize
	}
}

// bootTenant loads ~/.neo/credentials.json and injects the default TenantID into
// cfg.Auth + BoltDB session state. No-op when credentials are absent or incomplete.
// Extracted from main() to reduce its CC. [Épica 265.A/B]
func bootTenant(cfg *config.NeoConfig) {
	creds, err := auth.Load(auth.DefaultCredentialsPath())
	if err != nil {
		return
	}
	entry := creds.GetByProvider("default")
	if entry == nil || entry.TenantID == "" {
		return
	}
	cfg.Auth.TenantID = entry.TenantID
	state.SetActiveTenant(entry.TenantID)
}

// coordBootResult holds the outputs of bootCoordinatorTier.
type coordBootResult struct {
	CoordWSID  string
	OrgKS      *knowledge.KnowledgeStore
	OrgWriters []string
}

// bootCoordinatorTier resolves the project-tier coordinator workspace ID and opens
// the org-tier KnowledgeStore when this workspace is the org coordinator. Non-coord
// workspaces get empty CoordWSID / nil OrgKS. Extracted from main() to reduce its CC.
// [354.Z-redesign / PILAR LXVII / 355.A-B]
func bootCoordinatorTier(workspace string, cfg *config.NeoConfig) coordBootResult {
	var res coordBootResult

	// [354.Z / Piece 2] Non-coordinator workspaces proxy tier:"project" to the coord.
	if cfg.Project != nil && cfg.Project.CoordinatorWorkspace != "" && !isCoordinatorWorkspace(workspace, cfg) {
		res.CoordWSID = resolveCoordinatorWSID(cfg.Project.CoordinatorWorkspace)
		if res.CoordWSID == "" {
			log.Printf("[SRE-WARN] coordinator_workspace=%q not found in registry — tier:\"project\" will error until coord boots", cfg.Project.CoordinatorWorkspace)
		} else {
			log.Printf("[NEO-BOOT] tier:\"project\" will proxy to coordinator wsid=%s", res.CoordWSID)
		}
	}

	// [PILAR LXVII / 355.A] Open org-tier store when this is the org coordinator.
	if orgDir, ok := config.FindNeoOrgDir(workspace); ok && cfg.Org != nil {
		projectRoot := workspace
		pdir, hasProject := federation.FindNeoProjectDir(workspace)
		if hasProject {
			projectRoot = filepath.Dir(pdir)
		}
		ks, orgErr := knowledge.OpenOrgStore(knowledge.OrgStoreConfig{
			OrgDir:                orgDir,
			ProjectRoot:           projectRoot,
			CoordinatorProject:    cfg.Org.CoordinatorProject,
			DBPathOverride:        cfg.Org.SharedMemoryPath,
			IsStandaloneWorkspace: !hasProject,
		})
		switch {
		case orgErr == nil && ks != nil:
			res.OrgKS = ks
		case orgErr != nil && orgErr.Error() != "knowledge: org store is read-only from non-coordinator projects":
			log.Printf("[ORG-BOOT] OpenOrgStore: %v", orgErr)
		}
		// [355.B] Mirror org directives into .claude/rules/ with `org-` prefix so the
		// agent picks them up alongside workspace-local rules. Idempotent, orphans logged.
		if sync, serr := federation.SyncOrgDirectivesToWorkspace(orgDir, workspace); serr == nil && sync != nil {
			if sync.Copied > 0 {
				log.Printf("[ORG-DIRS] synced %d org directive(s) to %s/.claude/rules/ (skipped %d identical)",
					sync.Copied, workspace, sync.Skipped)
			}
			if len(sync.OrphansDetected) > 0 {
				log.Printf("[ORG-DIRS] orphan org-rules in workspace (source no longer exists): %v", sync.OrphansDetected)
			}
		}
	}

	if cfg.Org != nil {
		res.OrgWriters = cfg.Org.Writers
	}
	return res
}
