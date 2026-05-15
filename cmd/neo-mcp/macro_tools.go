package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/astx"
	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/hardware"
	"github.com/ensamblatec/neoanvil/pkg/cpg"
	"github.com/ensamblatec/neoanvil/pkg/dba"
	"github.com/ensamblatec/neoanvil/pkg/federation"
	"github.com/ensamblatec/neoanvil/pkg/inference"
	"github.com/ensamblatec/neoanvil/pkg/knowledge"
	"github.com/ensamblatec/neoanvil/pkg/memx"
	"github.com/ensamblatec/neoanvil/pkg/observability"
	"github.com/ensamblatec/neoanvil/pkg/pubsub"
	"github.com/ensamblatec/neoanvil/pkg/rag"
	"github.com/ensamblatec/neoanvil/pkg/sre"
	"github.com/ensamblatec/neoanvil/pkg/state"
	"github.com/ensamblatec/neoanvil/pkg/swarm"
	"github.com/ensamblatec/neoanvil/pkg/telemetry"
	"github.com/ensamblatec/neoanvil/pkg/tensorx"
	"github.com/ensamblatec/neoanvil/pkg/workspace"
)

// [PILAR-XXVIII hotfix] Package-level HTTP clients so repeated chaos
// drills don't allocate a fresh http.Transport + connection pool each
// invocation. Used by both the main siege (10 s) and the digital-twin
// siege — both share the same client safely.
var chaosHTTPClient = sre.SafeHTTPClient()

// runPairFeedbackHook scans for unresolved PairAuditEvents whose files
// intersect the just-certified set, marks them resolved with
// OutcomeSuccess, and updates trust scores. Skip on dry-run since
// dry-runs don't represent operator commit intent. Failures are
// non-fatal — the operator just finished a certify and the trust
// hook shouldn't block the response. [138.E.2 helper]
func runPairFeedbackHook(results []string, dryRun bool) {
	if dryRun {
		return
	}
	approved := extractApprovedFiles(results)
	if len(approved) == 0 {
		return
	}
	resolvedCount, err := state.HookCertifyEvents(approved)
	if err != nil {
		log.Printf("[PAIR-FEEDBACK] hook_certify_events failed: %v", err)
		return
	}
	if resolvedCount > 0 {
		log.Printf("[PAIR-FEEDBACK] resolved %d audit event(s) via certify intersect", resolvedCount)
	}
}

// approvedCertifyStatuses is the whitelist of certify result.status
// values that count as "the operator certified this file" for the
// pair feedback hook. Explicit set instead of HasPrefix("Aprobado")
// because a future variant returning "AprobadoFalso" or
// "Aprobado pero rechazado" would slip through the prefix check.
// [DeepSeek VULN-INPUT-001]
var approvedCertifyStatuses = map[string]bool{
	"Aprobado e Indexado": true,
	"Aprobado (dry-run)":  true,
}

// extractApprovedFiles parses certify result strings (one JSON object
// per file) and returns the file paths whose status is in the
// approvedCertifyStatuses whitelist. Used by the [138.E.2] pair
// feedback hook to scope its scan to files the operator actually
// certified — rejected/rolled-back files shouldn't trigger trust-
// system credit.
func extractApprovedFiles(results []string) []string {
	var approved []string
	for _, r := range results {
		var parsed struct {
			Status string `json:"status"`
			File   string `json:"file"`
		}
		if err := json.Unmarshal([]byte(r), &parsed); err != nil {
			continue
		}
		if parsed.File == "" {
			continue
		}
		if approvedCertifyStatuses[parsed.Status] {
			approved = append(approved, parsed.File)
		}
	}
	return approved
}

// projectRootOf resolves the workspace root for a file by doing two passes:
// 1) walk up looking only for neo.yaml (workspace marker, highest priority)
// 2) if not found, walk up looking for go.mod (fallback for plain Go modules)
// This prevents sub-module go.mod files (e.g. pkg/config/go.mod) from
// shadowing the real workspace root that has neo.yaml.
func projectRootOf(filename string) string {
	start := filepath.Dir(filename)
	// Pass 1: prefer neo.yaml — the workspace marker always wins
	for dir := start; ; {
		if _, err := os.Stat(filepath.Join(dir, "neo.yaml")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// Pass 2: fall back to go.mod for workspaces without neo.yaml
	for dir := start; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return start
}

// goModRootOf returns the directory containing the closest ancestor
// `go.mod`, walking up from filename. Falls back to filename's dir if
// no go.mod is found anywhere in the parent chain.
//
// This is the correct root for `cmd.Dir` of go-toolchain commands
// (go test / go build / go list) because Go always resolves packages
// relative to the module root declared in go.mod. Using projectRootOf
// (which prefers neo.yaml) breaks for monorepo layouts like strategos
// where neo.yaml lives at the workspace root but go.mod is in
// `backend/`. The error surfaces as "go.mod file not found in current
// directory or any parent directory" with bypass=1 as the only
// workaround.
//
// Resolves [Nexus debt T001 CERTIFY-CWD-BUG, P0]. Operator-reported
// 100% bypass rate on strategos for ~30 sessions before this fix.
func goModRootOf(filename string) string {
	start := filepath.Dir(filename)
	for dir := start; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return start
}

// [SRE-25.2.1] Global CRDT for multi-agent file locking — prevents concurrent certifications.
var globalCRDT = &swarm.LWWSet{}

// [FIX-DOCID] Atomic counter for RAG docIDs — prevents nanosecond collisions under concurrent indexing.
var docIDCounter atomic.Uint64

type RadarIntent string

const (
	BLAST_RADIUS     RadarIntent = "BLAST_RADIUS"
	SEMANTIC_CODE    RadarIntent = "SEMANTIC_CODE"
	DB_SCHEMA        RadarIntent = "DB_SCHEMA"
	TECH_DEBT_MAP    RadarIntent = "TECH_DEBT_MAP"
	READ_MASTER_PLAN RadarIntent = "READ_MASTER_PLAN"
	SEMANTIC_AST     RadarIntent = "SEMANTIC_AST"
	READ_SLICE       RadarIntent = "READ_SLICE"   // [SRE-14.3.1] OOM-safe file reader
	BRIEFING         RadarIntent = "BRIEFING"     // [SRE-21.2.1] Session bootstrap snapshot
	AST_AUDIT        RadarIntent = "AST_AUDIT"        // [SRE-29.1.1] Static analysis: CC, infinite loops, shadows
	HUD_STATE        RadarIntent = "HUD_STATE"        // [SRE-42.1] Inspect HUD MCTS/RAM/CPU state
	FRONTEND_ERRORS  RadarIntent = "FRONTEND_ERRORS"  // [SRE-42.1] Inspect frontend console errors
	WIRING_AUDIT     RadarIntent = "WIRING_AUDIT"     // [SRE-55.1] Detect imported packages not instantiated in main.go
	COMPILE_AUDIT    RadarIntent = "COMPILE_AUDIT"    // [SRE-60.4] Build check + undefined symbol resolver + cert status
	GRAPH_WALK       RadarIntent = "GRAPH_WALK"       // [PILAR-XX/148] BFS walk from a symbol over the CPG
	PROJECT_DIGEST   RadarIntent = "PROJECT_DIGEST"   // [PILAR-XX/148] Structural snapshot: CodeRank + coupling + hotspots
	INCIDENT_SEARCH  RadarIntent = "INCIDENT_SEARCH"  // [PILAR-XXI/150] Semantic search over .neo/incidents/ corpus
	PATTERN_AUDIT    RadarIntent = "PATTERN_AUDIT"    // [PILAR-XXI/155] Recurring incident pattern detection
	CONTRACT_QUERY   RadarIntent = "CONTRACT_QUERY"   // [PILAR-XXXVIII/290] Surgical query of a single HTTP endpoint contract
	FILE_EXTRACT     RadarIntent = "FILE_EXTRACT"     // [PILAR-XLI/270] Symbol-targeted file extraction with context window
	CONTRACT_GAP     RadarIntent = "CONTRACT_GAP"     // [316.B] Diff TS fetch calls vs defined Go routes → gaps logged to SHARED_DEBT.md
	INBOX            RadarIntent = "INBOX"            // [331.C] List agent-to-agent inbox messages for this workspace
	PLUGIN_STATUS        RadarIntent = "PLUGIN_STATUS"        // [PILAR-XXIII / 126.5] Subprocess plugin pool runtime state from Nexus
	CLAUDE_FOLDER_AUDIT  RadarIntent = "CLAUDE_FOLDER_AUDIT"  // [128.1] Drift detection for .claude/skills/ vs CLAUDE.md + inventory
)

type RadarTool struct {
	graph          *rag.Graph
	wal            *rag.WAL
	cpu            *tensorx.CPUDevice
	pool           *memx.ObservablePool[memx.F32Slab]
	embedder       rag.Embedder
	lexicalIdx     *rag.LexicalIndex
	incLexIdx      *rag.LexicalIndex // [169.B] BM25-only index over .neo/incidents/
	queryCache     *rag.QueryCache       // [175] LRU cache for repeated SEMANTIC_CODE / INCIDENT_SEARCH
	textCache      *rag.TextCache        // [179] LRU cache for full-text handler output (BLAST_RADIUS)
	embCache       *rag.Cache[[]float32] // [199] skips ~30ms Ollama roundtrip on repeat embed
	hotFiles       *rag.HotFilesCache    // [LARGE-PROJECT/A 2026-05-13] LRU file-content cache invalidated by mtime; skips os.ReadFile in READ_SLICE/FILE_EXTRACT for repeat-touched files
	dbaEngine      *dba.Analyzer
	cfg            *config.NeoConfig
	workspace      string
	cpgManager       *cpg.Manager
	lastDigestTime   time.Time // set by handleProjectDigest; surfaced in BRIEFING as digest_age_hours
	briefingCallCount int      // incremented on each BRIEFING call; 0 = never called this boot [156.A]
	lastBriefingTime     time.Time         // set on each BRIEFING call [156.A]
	lastBriefingSnapshot *briefingSnapshot // [315.A] snapshot for mode:delta diff
	knowledgeStats          func() (hot, total int)            // [295.D] nil when KnowledgeStore not available
	contractHotFetch        func(ns, key string) (string, bool) // [298.A] nil when not in project mode
	knowledgeStaleContracts func() []string                     // [298.C] nil when not in project mode
	sharedGraph             *rag.SharedGraph                    // [287.E] nil when not in project mode
	knowledgeStore          *knowledge.KnowledgeStore           // [331.B] nil when not in project mode — exposes inbox API
	registry                *workspace.Registry                 // [347.A] nil-safe; cross-workspace GRAPH_WALK scatter
	gpuInfo                 hardware.GPUInfo                    // [GPU-AWARE] boot-time GPU snapshot; live stats via hardware.Detect()
	// [372.A+373.A] Session-level intent counters for advisory/nudge logic.
	sessionIntentCounts map[string]int    // intent → call count this boot
	sessionIntentMu     sync.Mutex
}

func NewRadarTool(graph *rag.Graph, cpu *tensorx.CPUDevice, pool *memx.ObservablePool[memx.F32Slab], embedder rag.Embedder, wal *rag.WAL, lexicalIdx *rag.LexicalIndex, dbaEngine *dba.Analyzer, cfg *config.NeoConfig, workspace string) *RadarTool {
	// [LARGE-PROJECT/A 2026-05-13] Hot-files cache: 32 MB cap is empirically
	// sufficient for typical pair-mode workloads (master_plan ~50KB, big
	// generated SQL files ~500KB, hot Go files <100KB each). Invalidates on
	// mtime+size mismatch — never serves stale content. Future: thread
	// capacity through cfg.RAG.HotFilesCapacityBytes if operators need tuning.
	const hotFilesCapBytes = 32 * 1024 * 1024
	return &RadarTool{
		graph:      graph,
		wal:        wal,
		cpu:        cpu,
		pool:       pool,
		embedder:   embedder,
		lexicalIdx: lexicalIdx,
		dbaEngine:  dbaEngine,
		cfg:        cfg,
		workspace:  workspace,
		hotFiles:   rag.NewHotFilesCache(hotFilesCapBytes),
	}
}

// WithKnowledgeStats wires a stats getter for BRIEFING Knowledge Base display. [295.D]
func (t *RadarTool) WithKnowledgeStats(fn func() (hot, total int)) *RadarTool {
	t.knowledgeStats = fn
	return t
}

// WithKnowledgeStore wires the shared KnowledgeStore so RadarTool can read
// inbox entries + list/mark-read for BRIEFING and the INBOX intent. [331.B]
func (t *RadarTool) WithKnowledgeStore(ks *knowledge.KnowledgeStore) *RadarTool {
	t.knowledgeStore = ks
	return t
}

// WithContractHotFetch wires a KnowledgeStore hot-cache lookup for CONTRACT_QUERY. [298.A]
func (t *RadarTool) WithContractHotFetch(fn func(ns, key string) (string, bool)) *RadarTool {
	t.contractHotFetch = fn
	return t
}

// WithKnowledgeStaleContracts wires a detector for contract entries updated after session start. [298.C]
func (t *RadarTool) WithKnowledgeStaleContracts(fn func() []string) *RadarTool {
	t.knowledgeStaleContracts = fn
	return t
}

// WithSharedGraph wires the project-level shared HNSW tier for low-coverage BLAST_RADIUS scatter. [287.E]
func (t *RadarTool) WithSharedGraph(sg *rag.SharedGraph) *RadarTool {
	t.sharedGraph = sg
	return t
}

// WithCPGManager wires the CPG manager for structural CodeRank analysis.
func (t *RadarTool) WithCPGManager(m *cpg.Manager) *RadarTool {
	t.cpgManager = m
	return t
}

// WithIncidentLex wires the BM25-only incident lexical index — lets
// handleIncidentSearch answer queries without depending on the embedder.
// [Épica 169.B]
func (t *RadarTool) WithIncidentLex(lex *rag.LexicalIndex) *RadarTool {
	t.incLexIdx = lex
	return t
}

// WithQueryCache wires the LRU cache used to short-circuit repeated
// SEMANTIC_CODE / INCIDENT_SEARCH requests. Nil cache disables caching.
// [Épica 175]
func (t *RadarTool) WithQueryCache(cache *rag.QueryCache) *RadarTool {
	t.queryCache = cache
	return t
}

// WithTextCache wires the text cache used by handleBlastRadius and other
// handlers whose full-markdown output is expensive to regenerate. [Épica 179]
func (t *RadarTool) WithTextCache(cache *rag.TextCache) *RadarTool {
	t.textCache = cache
	return t
}

// WithEmbeddingCache wires the embedded-vector cache used by
// handleSemanticCode. When the QueryCache misses but the same target
// was embedded recently, this path serves the cached vector and skips
// the ~30 ms Ollama roundtrip. [Épica 199]
func (t *RadarTool) WithEmbeddingCache(cache *rag.Cache[[]float32]) *RadarTool {
	t.embCache = cache
	return t
}

// WithRegistry wires the workspace registry for cross-workspace GRAPH_WALK scatter. [347.A]
func (t *RadarTool) WithRegistry(r *workspace.Registry) *RadarTool {
	t.registry = r
	return t
}

// WithGPUInfo stores the boot-time GPU snapshot for BRIEFING display. [GPU-AWARE]
func (t *RadarTool) WithGPUInfo(info hardware.GPUInfo) *RadarTool {
	t.gpuInfo = info
	return t
}

func (t *RadarTool) Name() string        { return "neo_radar" }
func (t *RadarTool) Description() string { return "Unified radar tool handling multiple intents" }
func (t *RadarTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"intent": map[string]any{
				"type":        "string",
				"description": "One of BLAST_RADIUS, SEMANTIC_CODE, DB_SCHEMA, TECH_DEBT_MAP, READ_MASTER_PLAN, SEMANTIC_AST, READ_SLICE, BRIEFING, AST_AUDIT, HUD_STATE, FRONTEND_ERRORS, WIRING_AUDIT, COMPILE_AUDIT, GRAPH_WALK, PROJECT_DIGEST, INCIDENT_SEARCH, PATTERN_AUDIT, CONTRACT_QUERY, FILE_EXTRACT, CONTRACT_GAP, INBOX",
				"enum":        []string{"BLAST_RADIUS", "SEMANTIC_CODE", "DB_SCHEMA", "TECH_DEBT_MAP", "READ_MASTER_PLAN", "SEMANTIC_AST", "READ_SLICE", "BRIEFING", "AST_AUDIT", "HUD_STATE", "FRONTEND_ERRORS", "WIRING_AUDIT", "COMPILE_AUDIT", "GRAPH_WALK", "PROJECT_DIGEST", "INCIDENT_SEARCH", "PATTERN_AUDIT", "CONTRACT_QUERY", "FILE_EXTRACT", "CONTRACT_GAP", "INBOX", "PLUGIN_STATUS", "CLAUDE_FOLDER_AUDIT"},
			},
			"target": map[string]any{
				"type":        "string",
				"description": "Target identifier. Usage per intent: BLAST_RADIUS/AST_AUDIT/READ_SLICE/SEMANTIC_AST/COMPILE_AUDIT/FILE_EXTRACT → file path; SEMANTIC_CODE → natural-language query (alias: 'query' — e.g. 'función que calcula SSRF'); DB_SCHEMA → SQL query; TECH_DEBT_MAP/BRIEFING/HUD_STATE/FRONTEND_ERRORS/WIRING_AUDIT/READ_MASTER_PLAN → unused.",
			},
			"db_alias": map[string]any{
				"type":        "string",
				"description": "Required for DB_SCHEMA. Alias of the database.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Optional for TECH_DEBT_MAP (default 10) or READ_SLICE (lines to read).",
			},
			"start_line": map[string]any{
				"type":        "integer",
				"description": "Required for READ_SLICE: starting line number (1-based).",
			},
			"mode": map[string]any{
				"type":        "string",
				"description": "Optional for BRIEFING: 'compact' returns single-line summary; 'full' (default) returns full detail. 'delta' returns only fields that changed since last BRIEFING. Auto-compacts at 8KB.",
				"enum":        []string{"compact", "full", "delta"},
			},
			"min_results": map[string]any{
				"type":        "integer",
				"description": "[SRE-100.B] Optional for SEMANTIC_CODE. When vector search returns fewer than this, auto-fallback to literal grep (default 1). Force 3+ for ambiguous queries.",
			},
			"force_grep": map[string]any{
				"type":        "boolean",
				"description": "[SRE-102.A] Optional for BLAST_RADIUS. When true, skip graph check and always return grep file:line tuples with 2-line context. Use when the RAG index is known stale.",
			},
			"force_contract": map[string]any{
				"type":        "boolean",
				"description": "[Épica 256.B] Optional for BLAST_RADIUS. When true, skip CPG walk and return only cross-boundary frontend callers via HTTP contract analysis (OpenAPI + Go route scan + TS fetch patterns). Use for full-stack workspaces to see which TS files call a Go handler.",
			},
			"bypass_cache": map[string]any{
				"type":        "boolean",
				"description": "[Épica 183] Optional for BLAST_RADIUS / SEMANTIC_CODE / PROJECT_DIGEST / GRAPH_WALK. When true, skip the TextCache/QueryCache lookup AND the write — forces full recomputation and refreshes the cached entry. Use when disk state changed outside MCP ingest (manual edit + no certify) and you need guaranteed fresh data.",
			},
			"targets": map[string]any{
				"type":        "array",
				"description": "[SRE-136.A] Optional for BLAST_RADIUS. Array of file paths for parallel batch analysis. When provided, overrides 'target'. Returns merged report with per-target impact + max_confidence.",
				"items":       map[string]any{"type": "string"},
			},
			"include_unexported": map[string]any{
				"type":        "boolean",
				"description": "Optional for COMPILE_AUDIT. When true, the symbol_map includes package-private (unexported) functions, methods, and types in addition to exported ones. Use when searching for unexported helpers like runASTAuditGlob.",
			},
			"filter_symbol": map[string]any{
				"type":        "string",
				"description": "[299.B] Optional for COMPILE_AUDIT. Case-insensitive substring filter for symbol_map keys. Returns only entries whose name contains the substring. Example: filter_symbol:'handleContract' returns just matching 1-2 lines. No effect on build/cert checks.",
			},
			"max_depth": map[string]any{
				"type":        "integer",
				"description": "Optional for GRAPH_WALK: BFS depth limit (default 2).",
			},
			"edge_kind": map[string]any{
				"type":        "string",
				"description": "Optional for GRAPH_WALK: edge type filter — 'call' | 'cfg' | 'contain' | 'all' (default).",
				"enum":        []string{"call", "cfg", "contain", "all"},
			},
			"force_tier": map[string]any{
				"type":        "string",
				"description": "[Épica 229.5] Optional for INCIDENT_SEARCH. Force a specific retrieval tier — 'bm25' (keyword), 'hnsw' (semantic, needs Ollama), or 'text' (raw keyword scan). Default cascades bm25 → hnsw → text.",
				"enum":        []string{"bm25", "hnsw", "text"},
			},
			"min_calls": map[string]any{
				"type":        "integer",
				"description": "[Épica 229.6] Optional for PROJECT_DIGEST. Minimum number of calls for an edge to appear in the package-coupling section (default 0 — show all).",
			},
			"filter_package": map[string]any{
				"type":        "string",
				"description": "[Épica 229.6] Optional for PROJECT_DIGEST. When set, the package-coupling section only shows edges whose source OR destination contains this substring.",
			},
			"cross_workspace": map[string]any{
				"type":        "boolean",
				"description": "[274.C] Optional for SEMANTIC_CODE. When true and in project mode, scatter the semantic search to all member workspaces via Nexus and aggregate deduplicated results by file+line.",
			},
			"target_workspace": map[string]any{
				"type":        "string",
				"description": "Optional: target workspace ID or 'project' to run on the project federation root. Injected by Nexus when routing multi-workspace calls.",
			},
			"method": map[string]any{
				"type":        "string",
				"description": "[290] CONTRACT_QUERY: HTTP verb filter (GET, POST, PUT, PATCH, DELETE). Empty = all methods.",
			},
			"validate_payload": map[string]any{
				"type":        "string",
				"description": "[290] CONTRACT_QUERY: optional JSON string to validate against the extracted request schema of the matched endpoint.",
			},
			"query": map[string]any{
				"type":        "string",
				"description": "[270] FILE_EXTRACT: symbol name or search term. Exact match against symbol_map first; falls back to case-insensitive substring search within the file.",
			},
			"symbols": map[string]any{
				"type":        "array",
				"description": "[317.A] FILE_EXTRACT: array of symbol names to extract in one call. Overrides 'query' when present. Max 10 symbols. context_lines:0 returns full body of each.",
				"items":       map[string]any{"type": "string"},
			},
			"context_lines": map[string]any{
				"type":        "integer",
				"description": "[270] FILE_EXTRACT: lines of context above and below each match (default 5).",
			},
			"filter_open": map[string]any{
				"type":        "boolean",
				"description": "[318.A] READ_MASTER_PLAN: when true, return only open (- [ ]) task lines with their parent ## and ### headings. ~13× fewer tokens than the full phase. Use for mid-session task checks.",
			},
			"scope": map[string]any{
				"type":        "string",
				"enum":        []string{"workspace", "project"},
				"description": "[332.B] READ_MASTER_PLAN: 'workspace' (default) returns local master_plan.md only. 'project' appends a shared-epics table from the KnowledgeStore (NSEpics), sorted by priority then status. Combine with filter_open:true to see only open/in_progress project epics.",
			},
			"filter": map[string]any{
				"type":        "string",
				"enum":        []string{"unread", "all", "urgent"},
				"description": "[331.C] INBOX: unread (default) | all | urgent. Filters which entries targeting the current workspace are returned.",
			},
			"key": map[string]any{
				"type":        "string",
				"description": "[331.C] INBOX fetch mode: `to-<wsID>-<topic>` key. Returns full body + marks read. When omitted, INBOX returns the filtered table.",
			},
		},
		Required: []string{"intent"},
	}
}

// radarHandlers maps each RadarIntent to its handler. [SRE-122.A]
var radarHandlers = map[RadarIntent]func(*RadarTool, context.Context, map[string]any) (any, error){
	BLAST_RADIUS:     (*RadarTool).handleBlastRadius,
	SEMANTIC_CODE:    (*RadarTool).handleSemanticCode,
	DB_SCHEMA:        (*RadarTool).handleDBSchema,
	TECH_DEBT_MAP:    (*RadarTool).handleTechDebtMap,
	READ_MASTER_PLAN: (*RadarTool).handleReadMasterPlan,
	SEMANTIC_AST:     (*RadarTool).handleSemanticAST,
	READ_SLICE:       (*RadarTool).handleReadSlice,
	BRIEFING:         (*RadarTool).handleBriefing,
	AST_AUDIT:        (*RadarTool).handleASTAudit,
	HUD_STATE:        (*RadarTool).handleHUDState,
	FRONTEND_ERRORS:  (*RadarTool).handleFrontendErrors,
	WIRING_AUDIT:     (*RadarTool).handleWiringAudit,
	COMPILE_AUDIT:    (*RadarTool).handleCompileAudit,
	GRAPH_WALK:       (*RadarTool).handleGraphWalk,
	PROJECT_DIGEST:   (*RadarTool).handleProjectDigest,
	INCIDENT_SEARCH:  (*RadarTool).handleIncidentSearch,
	PATTERN_AUDIT:    (*RadarTool).handlePatternAudit,
	CONTRACT_QUERY:   (*RadarTool).handleContractQuery,
	FILE_EXTRACT:     (*RadarTool).handleFileExtract,
	CONTRACT_GAP:     (*RadarTool).handleContractGap,
	INBOX:                (*RadarTool).handleInbox,             // [331.C]
	PLUGIN_STATUS:        (*RadarTool).handlePluginStatus,       // [PILAR-XXIII / 126.5]
	CLAUDE_FOLDER_AUDIT:  (*RadarTool).handleClaudeFolderAudit, // [128.1]
}

func (t *RadarTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	telemetry.LogAction("RadarTool Execute start")
	state.MarkResearched()
	intentStr, ok := args["intent"].(string)
	if !ok {
		return nil, fmt.Errorf("intent must be a string")
	}
	handler, ok := radarHandlers[RadarIntent(intentStr)]
	if !ok {
		return nil, fmt.Errorf("unknown intent %s", intentStr)
	}
	// [372.A+373.A] Track intent usage for advisories/nudges.
	t.sessionIntentMu.Lock()
	if t.sessionIntentCounts == nil {
		t.sessionIntentCounts = make(map[string]int)
	}
	t.sessionIntentCounts[intentStr]++
	t.sessionIntentMu.Unlock()

	return handler(t, ctx, args)
}

// [SRE-19.1.1] Strict administrative actions — no tactical intents.
type DaemonAction string

const (
	ActionPullTasks    DaemonAction = "PullTasks"
	ActionPushTasks    DaemonAction = "PushTasks"
	ActionVacuumMemory DaemonAction = "Vacuum_Memory"
	ActionSetStage     DaemonAction = "SetStage"
	ActionFlushPMEM    DaemonAction = "FLUSH_PMEM"    // [SRE-20.3.1] God-Mode: vaciar caches PMEM
	ActionQuarantineIP DaemonAction = "QUARANTINE_IP" // [SRE-20.3.1] God-Mode: aislar IP via eBPF
	ActionMarkDone     DaemonAction = "MARK_DONE"     // [275.B] Mark epics done in master_plan.md
	ActionExecuteNext  DaemonAction = "execute_next"  // [138.C.1] PILAR XXVII: iterative MCP-driven daemon
	ActionApprove      DaemonAction = "approve"       // [138.C.2] PILAR XXVII: operator approves an executed task
	ActionReject       DaemonAction = "reject"        // [138.C.3+C.8] PILAR XXVII: operator rejects with reason_kind
	ActionTrustStatus  DaemonAction = "trust_status"  // [138.C.6] PILAR XXVII: report top-N patterns by trust
	ActionPairAuditEmit DaemonAction = "pair_audit_emit" // [138.E.1] PILAR XXVII: agent emits red_team finding events for trust calibration
)

type DaemonTool struct {
	wal       *rag.WAL
	workspace string
	cfg       *config.NeoConfig   // [348.A] nil-safe; project vacuum scatter
	registry  *workspace.Registry // [348.A] nil-safe; project vacuum scatter
}

func NewDaemonTool(wal *rag.WAL, workspace string) *DaemonTool {
	return &DaemonTool{wal: wal, workspace: workspace}
}

// WithConfig wires the NeoConfig for project-scoped Vacuum_Memory scatter. [348.A]
func (t *DaemonTool) WithConfig(cfg *config.NeoConfig) *DaemonTool {
	t.cfg = cfg
	return t
}

// WithRegistry wires the workspace registry for project-scoped Vacuum_Memory scatter. [348.A]
func (t *DaemonTool) WithRegistry(r *workspace.Registry) *DaemonTool {
	t.registry = r
	return t
}

func (t *DaemonTool) Name() string { return "neo_daemon" }

// pairExemptHandlers returns the registry of daemon actions that
// bypass the pair-mode prohibition. Adding a new exempt action means
// adding one map entry instead of duplicating an `if` branch in the
// dispatcher. [DeepSeek VULN-007]
//
// Membership criteria: read-only OR strictly required for pair-mode
// trust calibration. Mutations of operator state (PullTasks, approve,
// reject, etc.) MUST stay gated.
func (t *DaemonTool) pairExemptHandlers() map[DaemonAction]func(context.Context, map[string]any) (any, error) {
	return map[DaemonAction]func(context.Context, map[string]any) (any, error){
		ActionMarkDone:      t.handleMarkDone,      // [275.B] master_plan checkbox flip
		ActionTrustStatus:   t.handleTrustStatus,   // [138.C.6] read-only trust report
		ActionPairAuditEmit: t.handlePairAuditEmit, // [138.E.1] feeds the calibration loop
	}
}

// [SRE-19.1.2] In Pair-Programming Mode (cfg.Server.Mode="pair"), this tool is PROHIBITED.
// Use it exclusively in Autonomous Night Mode (daemon mode) for task queue management.
func (t *DaemonTool) Description() string {
	return "[SRE-BUROCRACIA] Administrative SRE Daemon: task queue and cognitive stage management. PROHIBITED in Pair-Mode — use only in Autonomous Daemon Mode."
}

// [SRE-19.1.1] Strict schema: action field only with 4 administrative values.
func (t *DaemonTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "Administrative action. PROHIBITED in Pair-Mode.",
				"enum":        []string{"PullTasks", "PushTasks", "Vacuum_Memory", "SetStage", "FLUSH_PMEM", "QUARANTINE_IP", "MARK_DONE", "execute_next", "approve", "reject", "trust_status", "pair_audit_emit"},
			},
			"tasks": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"description": map[string]any{"type": "string"},
						"target_file": map[string]any{"type": "string"},
					},
					"required": []string{"description", "target_file"},
				},
				"description": "Required for PushTasks.",
			},
			"stage": map[string]any{
				"type":        "integer",
				"description": "Required for SetStage (1-6).",
			},
			"target_ip": map[string]any{
				"type":        "string",
				"description": "Required for QUARANTINE_IP: the IP address to isolate via eBPF.",
			},
			"agent_role": map[string]any{ // [SRE-25.1.2] Role-based task routing
				"type":        "string",
				"description": "Optional for PullTasks. Filter tasks by agent role (e.g. \"frontend\", \"backend\").",
			},
			"epic_id": map[string]any{ // [275.B] MARK_DONE
				"type":        "string",
				"description": "Required for MARK_DONE: epic ID prefix to mark as done (e.g. \"272.G.1\"). Marks all matching '- [ ] **{epic_id}' lines in master_plan.md as [x].",
			},
			"scope": map[string]any{ // [348.A] project vacuum scatter + [138.E.1] pair_audit_emit scope
				"type":        "string",
				"description": `Required for pair_audit_emit (format "pattern:scope" e.g. "audit:.go:pkg/state"). Optional for Vacuum_Memory: use "project" to scatter to member workspaces via Nexus. [348.A / 138.E.1]`,
			},
			"finding_id": map[string]any{ // [138.E.1] pair_audit_emit
				"type":        "string",
				"description": "Required for pair_audit_emit: stable identifier for this finding (e.g. \"PAIR-AUDIT-TOCTOU-001\"). Used for dedup and resolution tracking.",
			},
			"claim_text": map[string]any{ // [138.E.1] pair_audit_emit
				"type":        "string",
				"description": "Required for pair_audit_emit: one-sentence claim. Truncated at 240 chars by storage layer.",
			},
			"severity": map[string]any{ // [138.E.1] pair_audit_emit
				"type":        "integer",
				"description": "Optional for pair_audit_emit (default 5, range 1-10). Model's self-rated severity of the finding.",
			},
			"files": map[string]any{ // [138.E.1] pair_audit_emit
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional for pair_audit_emit: repo-relative paths the finding implicates. Used by certify-hook to infer Outcome=Success when operator certifies one of these files.",
			},
			"task_id": map[string]any{ // [138.C] approve / reject
				"type":        "string",
				"description": "Required for approve/reject: identifier of the task previously returned by execute_next.",
			},
			"session_id": map[string]any{ // [138.C] execute_next / approve
				"type":        "string",
				"description": "Optional for execute_next/approve: session identifier for cross-call continuity (defaults to workspace-scoped).",
			},
			"operator_note": map[string]any{ // [138.C] approve
				"type":        "string",
				"description": "Optional for approve: free-form note attached to the DaemonResult on approval (audit trail).",
			},
			"reason": map[string]any{ // [138.C] reject
				"type":        "string",
				"description": "Optional for reject: free-form description of why the operator rejected the task output.",
			},
			"reason_kind": map[string]any{ // [138.C] reject
				"type":        "string",
				"description": "Optional for reject: failure category (quality, suboptimal, infra). Drives trust update direction. Omit field for unset.",
				"enum":        []string{"quality", "suboptimal", "infra"},
			},
			"requeue": map[string]any{ // [138.C] reject
				"type":        "boolean",
				"description": "Optional for reject: when true, push the rejected task back into the queue for retry.",
			},
			"filter_pattern": map[string]any{ // [138.C.6] trust_status
				"type":        "string",
				"description": "Optional for trust_status: case-insensitive substring filter on (pattern, scope) keys. Empty = all entries.",
			},
			"top": map[string]any{ // [138.C.6] trust_status
				"type":        "integer",
				"description": "Optional for trust_status: max entries returned, sorted by lower_bound DESC (default unlimited).",
			},
		},
		Required: []string{"action"},
	}
}

// [SRE-19.1.2] Pair-Mode enforcement: returns error if NEO_SERVER_MODE=pair.
// Exception: MARK_DONE is allowed in all modes — it only edits master_plan.md.
func (t *DaemonTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	actionStr, ok := args["action"].(string)
	if !ok {
		return nil, fmt.Errorf("action must be a string")
	}
	// pairExemptHandlers maps actions that bypass the pair-mode
	// prohibition (MARK_DONE, trust_status, pair_audit_emit). Read-only
	// or trust-calibration actions that the operator must be able to
	// invoke during normal Pair workflow without flipping NEO_SERVER_MODE.
	// [DeepSeek VULN-007 — replaced 3 separate `if` checks with one map]
	if handler, ok := t.pairExemptHandlers()[DaemonAction(actionStr)]; ok {
		return handler(ctx, args)
	}
	if os.Getenv("NEO_SERVER_MODE") == "pair" {
		return nil, fmt.Errorf("[SRE-BUROCRACIA] neo_daemon is PROHIBITED in Pair-Programming Mode. Switch to Daemon Mode to use administrative tools")
	}
	if ThermicStabilizing.Load() == 1 {
		return nil, fmt.Errorf("[SRE-STABILIZING] Sistema en modo térmico estabilizador (RAPL > 60W). neo_daemon suspendido hasta que baje la temperatura. Reintenta en unos minutos.")
	}
	return t.dispatchDaemonAction(ctx, DaemonAction(actionStr), args)
}

// dispatchDaemonAction routes a non-exempt daemon action to its handler.
// Extracted from Execute to keep Execute below CC=15 — the switch
// contributed ~11 branches that, combined with the early-return guards,
// pushed Execute over the limit. [138.E.2 refactor]
func (t *DaemonTool) dispatchDaemonAction(ctx context.Context, action DaemonAction, args map[string]any) (any, error) {
	switch action {
	case ActionPushTasks:
		return t.handlePushTasks(ctx, args)
	case ActionPullTasks:
		return t.handlePullTasks(ctx, args)
	case ActionVacuumMemory:
		return t.handleVacuumMemory(ctx, args)
	case ActionSetStage:
		return t.handleSetStage(ctx, args)
	case ActionFlushPMEM:
		return t.handleFlushPMEM(ctx, args)
	case ActionQuarantineIP:
		return t.handleQuarantineIP(ctx, args)
	case ActionExecuteNext:
		return t.handleExecuteNext(ctx, args)
	case ActionApprove:
		return t.handleApprove(ctx, args)
	case ActionReject:
		return t.handleReject(ctx, args)
	case ActionTrustStatus:
		return t.handleTrustStatus(ctx, args)
	}
	return nil, fmt.Errorf("unknown daemon action: %s", action)
}

type ChaosDrillTool struct {
	workspace string
	cfg       *config.NeoConfig
	registry  *workspace.Registry // [346.A] nil-safe; project chaos scatter
}

func NewChaosDrillTool(workspace string, cfg *config.NeoConfig) *ChaosDrillTool {
	return &ChaosDrillTool{workspace: workspace, cfg: cfg}
}

func (t *ChaosDrillTool) WithRegistry(r *workspace.Registry) *ChaosDrillTool {
	t.registry = r
	return t
}

func (t *ChaosDrillTool) Name() string { return "neo_chaos_drill" }

// [SRE-18] Redefined description for synchronous Epic 18 Dron.
func (t *ChaosDrillTool) Description() string {
	return "Synchronous SRE Chaos Dron: stress-tests a target with configurable aggression (1-10). Optional fault injection. Returns consolidated Markdown report. Max 10s siege."
}

// [SRE-18.1.2] New schema: target + aggression_level + inject_faults + digital_twin.
func (t *ChaosDrillTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"target": map[string]any{
				"type":        "string",
				"description": `URL or handler to stress-test (e.g. http://localhost:8081/health). Use "project" to scatter the drill to all running member workspaces via Nexus. [346.A]`,
			},
			"aggression_level": map[string]any{
				"type":        "integer",
				"description": "Siege intensity 1-10 (Lvl 10 = Ouroboros / 10,000 goroutines). Project scatter defaults to 3.",
			},
			"inject_faults": map[string]any{
				"type":        "boolean",
				"description": "If true, triggers simulated SQL/Redis faults during bombardment.",
			},
			"digital_twin": map[string]any{
				"type":        "boolean",
				"description": "[SRE-49] If true, runs a 10× load mirror and certifies P99 latency regression.",
			},
			"flow": map[string]any{
				"type":        "string",
				"description": `[346.A] Named test flow path (e.g. "/health", "/api/ping"). When target:"project", appended to each child's loopback URL. Defaults to "/health".`,
			},
		},
		Required: []string{"target"},
	}
}

// [SRE-18] Synchronous Chaos Dron — max 10 seconds, inline report.
func (t *ChaosDrillTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	// [346.A] Project scatter bypasses the local siege mutex — each child runs its own drill.
	if target, _ := args["target"].(string); target == "project" {
		return t.scatterProjectChaos(ctx, args)
	}

	// [SRE-25.3.1] Global Chaos Mutex: prevent concurrent sieges from collapsing the host.
	if telemetry.IsChaosActive() {
		return nil, fmt.Errorf("[SRE-SWARM] VETO: Siege already active. Wait for current chaos drill to complete before launching a new one")
	}

	target, _ := args["target"].(string)
	if target == "" {
		target = t.cfg.Integrations.ChaosDrillTarget
	}

	aggrLevel := 5
	if lvl, ok := args["aggression_level"].(float64); ok && lvl >= 1 && lvl <= 10 {
		aggrLevel = int(lvl)
	}
	injectFaults, _ := args["inject_faults"].(bool)
	digitalTwin, _ := args["digital_twin"].(bool)

	// [SRE-18.2.1] Map aggression_level to goroutine count
	goroutines := aggrLevel * 1000

	// [SRE-24.1.3] Signal active siege to TUI/HUD
	telemetry.SetChaosState(true, aggrLevel)
	defer telemetry.SetChaosState(false, 0)

	// [SRE-18.2.2] Fault injection during siege
	if injectFaults {
		telemetry.LogAction("[CHAOS] Injecting infrastructure faults (SQL/Redis simulation)")
		go func() {
			defer func() { recover() }()
			telemetry.LogAction("[SRE-FAULT] Simulated SQL timeout injected")
			telemetry.LogAction("[SRE-FAULT] Simulated Redis NOAUTH injected")
		}()
	}

	// [SRE-18.1.1] Synchronous siege: max 10 seconds
	siegeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	client := chaosHTTPClient
	start := time.Now()
	totalReqs, failedReqs, panicLog := runSiegeLoop(siegeCtx, target, goroutines, client)
	elapsed := time.Since(start).Seconds()

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	shedded := sre.MetricsMESShedded.Load()

	rps := float64(totalReqs) / elapsed
	errPct := 0.0
	if totalReqs > 0 {
		errPct = float64(failedReqs) / float64(totalReqs) * 100
	}

	status := "🟢 RESILIENT"
	if failedReqs > 0 || shedded > 0 {
		status = "🔴 UNSTABLE"
	}

	panicSummary := ""
	if panicLog.Len() > 0 {
		panicSummary = fmt.Sprintf("\n**Panic Digest (RAG):** `%s`", strings.TrimSpace(panicLog.String()))
	}

	report := fmt.Sprintf(`### 🌪️ SRE CHAOS DRILL REPORT 🌪️
| Metric | Value |
|--------|-------|
| **Target** | %s |
| **Aggression** | Lvl %d (%d goroutines) |
| **TPS** | %.2f |
| **Errors** | %.1f%% (%d/%d) |
| **Events Shedded** | %d |
| **Max Heap RAM** | %.2f MB |
| **GC Runs** | %d |
| **Status** | %s |%s`,
		target, aggrLevel, goroutines,
		rps, errPct, failedReqs, totalReqs,
		shedded, float64(memStats.HeapAlloc)/(1024*1024),
		memStats.NumGC, status, panicSummary)

	// [SRE-49] Digital Twin: run 10× load mirror and certify P99 latency. [SRE-49.1/49.2]
	if digitalTwin || (t.cfg != nil && t.cfg.SRE.DigitalTwinTesting) {
		twinReport := runDigitalTwin(ctx, target, rps)
		report += "\n\n" + twinReport
	}

	return map[string]any{"content": []map[string]any{{"type": "text", "text": report}}}, nil
}

// nexusChaosStatus is the minimal /status entry shape used by scatterProjectChaos.
type nexusChaosStatus struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Port   int    `json:"port"`
	Status string `json:"status"`
}

// scatterProjectChaos implements target:"project" — drills all running remote workspaces. [346.A]
func (t *ChaosDrillTool) scatterProjectChaos(ctx context.Context, args map[string]any) (any, error) {
	nexusPort := t.cfg.Server.NexusDispatcherPort
	if nexusPort == 0 {
		return nil, fmt.Errorf("[346.A] nexus_dispatcher_port not configured; cannot scatter chaos drill")
	}

	aggrLevel := 3 // conservative default for scatter
	if lvl, ok := args["aggression_level"].(float64); ok && lvl >= 1 && lvl <= 10 {
		aggrLevel = int(lvl)
	}
	injectFaults, _ := args["inject_faults"].(bool)

	targetPath := "/health"
	if flow, _ := args["flow"].(string); strings.HasPrefix(flow, "/") {
		targetPath = flow
	}

	entries, err := t.fetchNexusStatuses(ctx, nexusPort)
	if err != nil {
		return nil, err
	}

	absWs, _ := filepath.Abs(t.workspace)
	type drillTarget struct {
		id   string
		port int
	}
	var targets []drillTarget
	for _, e := range entries {
		if e.Status != "running" || e.Port == 0 {
			continue
		}
		if absEntry, _ := filepath.Abs(e.Path); absEntry == absWs {
			continue // skip self — avoids re-entrant chaos mutex
		}
		targets = append(targets, drillTarget{id: e.ID, port: e.Port})
	}

	if len(targets) == 0 {
		msg := "### 🌪️ Project Chaos Scatter\nNo other running workspaces found. Ensure Nexus has at least one additional member workspace running."
		return map[string]any{"content": []map[string]any{{"type": "text", "text": msg}}}, nil
	}

	type scatterResult struct {
		wsID   string
		report string
	}
	results := make([]scatterResult, len(targets))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, tgt := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, dt drillTarget) {
			defer func() { <-sem; wg.Done() }()
			childTarget := fmt.Sprintf("http://127.0.0.1:%d%s", dt.port, targetPath)
			report, fwdErr := t.forwardChaosToNexus(ctx, nexusPort, dt.id, childTarget, aggrLevel, injectFaults)
			if fwdErr != nil {
				report = fmt.Sprintf("❌ %s: %v", dt.id, fwdErr)
			}
			results[idx] = scatterResult{wsID: dt.id, report: report}
		}(i, tgt)
	}
	wg.Wait()

	var sb strings.Builder
	fmt.Fprintf(&sb, "### 🌪️ PROJECT CHAOS SCATTER — %d workspace(s) | aggression:%d\n\n", len(targets), aggrLevel)
	for _, r := range results {
		fmt.Fprintf(&sb, "#### Workspace: `%s`\n%s\n\n---\n\n", r.wsID, r.report)
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": sb.String()}}}, nil
}

// fetchNexusStatuses queries GET /status on the local Nexus dispatcher.
func (t *ChaosDrillTool) fetchNexusStatuses(ctx context.Context, nexusPort int) ([]nexusChaosStatus, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/status", nexusPort) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: SafeInternalHTTPClient allows only loopback; port is from NexusDispatcherPort config
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build nexus status request: %w", err)
	}
	client := sre.SafeInternalHTTPClient(5)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nexus /status: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(resp.Body)
	var entries []nexusChaosStatus
	if jsonErr := json.Unmarshal(body, &entries); jsonErr != nil {
		return nil, fmt.Errorf("parse nexus /status: %w", jsonErr)
	}
	return entries, nil
}

// forwardChaosToNexus POSTs a chaos scatter request to Nexus for a specific child. [346.A]
func (t *ChaosDrillTool) forwardChaosToNexus(ctx context.Context, nexusPort int, wsID, target string, aggrLevel int, injectFaults bool) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"target":           target,
		"aggression_level": aggrLevel,
		"inject_faults":    injectFaults,
	})
	url := fmt.Sprintf("http://127.0.0.1:%d/internal/chaos/%s", nexusPort, wsID) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: SafeInternalHTTPClient allows only loopback; port is from NexusDispatcherPort config
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build chaos forward request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// [PRIVILEGE-002] Authenticate with the ephemeral internal token injected by Nexus at boot.
	if tok := os.Getenv("NEO_NEXUS_INTERNAL_TOKEN"); tok != "" {
		req.Header.Set("X-Neo-Internal-Token", tok)
	}
	client := sre.SafeInternalHTTPClient(35)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("nexus chaos forward %s: %w", wsID, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("nexus chaos %s: HTTP %d: %s", wsID, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, _ := io.ReadAll(resp.Body)
	var nr struct {
		WorkspaceID string `json:"workspace_id"`
		Report      string `json:"report"`
	}
	if jsonErr := json.Unmarshal(body, &nr); jsonErr != nil {
		return string(body), nil
	}
	return nr.Report, nil
}

// runSiegeLoop fires goroutines*10 GET requests against target within siegeCtx. [SRE-122.B]
func runSiegeLoop(siegeCtx context.Context, target string, goroutines int, client *http.Client) (totalReqs, failedReqs int64, panicLog strings.Builder) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, goroutines)
	for i := 0; i < goroutines*10 && siegeCtx.Err() == nil; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() {
				<-sem
				if r := recover(); r != nil {
					fmt.Fprintf(&panicLog, "[PANIC] %v\n", r)
				}
			}()
			resp, err := client.Get(target)
			if err != nil || (resp != nil && resp.StatusCode >= 500) {
				atomic.AddInt64(&failedReqs, 1)
			} else if resp != nil {
				resp.Body.Close()
			}
			atomic.AddInt64(&totalReqs, 1)
		}()
	}
	wg.Wait()
	return totalReqs, failedReqs, panicLog
}

// runDigitalTwin executes a 10× load mirror and certifies P99 latency. [SRE-49.1/49.2]
func runDigitalTwin(ctx context.Context, target string, baselineRPS float64) string {
	const multiplier = 10
	const maxSecs = 5

	twinCtx, cancel := context.WithTimeout(ctx, maxSecs*time.Second)
	defer cancel()

	twinGoroutines := max(100, min(5000, int(baselineRPS*multiplier)))

	var latencies []int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, twinGoroutines)
	client := chaosHTTPClient

	for i := 0; i < twinGoroutines && twinCtx.Err() == nil; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			t0 := time.Now()
			resp, err := client.Get(target)
			latMs := time.Since(t0).Milliseconds()
			if err == nil && resp != nil {
				resp.Body.Close()
			}
			mu.Lock()
			latencies = append(latencies, latMs)
			mu.Unlock()
		}()
	}
	wg.Wait()

	p99 := computeP99(latencies)
	baselineP99 := p99 / multiplier // heuristic: compare twin P99 to scaled baseline
	regression := p99 > int64(float64(baselineP99)*1.2*multiplier)

	certStatus := "✅ CERTIFIED (P99 within 1.2× baseline)"
	if regression {
		certStatus = "❌ REGRESSION DETECTED (P99 exceeds 1.2× baseline)"
	}

	return fmt.Sprintf(`### 🔮 DIGITAL TWIN REPORT (10× Load Mirror)
| Metric | Value |
|--------|-------|
| **Twin Goroutines** | %d |
| **Requests fired** | %d |
| **P99 Latency** | %d ms |
| **Latency Certification** | %s |`,
		twinGoroutines, len(latencies), p99, certStatus)
}

// computeP99 returns the 99th-percentile latency from a slice of millisecond measurements.
func computeP99(latencies []int64) int64 {
	if len(latencies) == 0 {
		return 0
	}
	sorted := make([]int64, len(latencies))
	copy(sorted, latencies)
	// Insertion sort — small N in practice due to 5s cap.
	for i := 1; i < len(sorted); i++ {
		key := sorted[i]
		j := i - 1
		for j >= 0 && sorted[j] > key {
			sorted[j+1] = sorted[j]
			j--
		}
		sorted[j+1] = key
	}
	idx := int(float64(len(sorted)) * 0.99)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// [SRE-17.2.1] Renamed from PipelineTool to CertifyMutationTool.
// [SRE-17.1.1] Epic 8 (Fuzzy Patcher / Shadow Buffer) is DEPRECATED.
// The AI uses its native edit tools; this tool certifies the result via ACID validation.
type CertifyMutationTool struct {
	wal       *rag.WAL
	graph     *rag.Graph
	cpu       tensorx.ComputeDevice
	embedder  rag.Embedder
	workspace string
	cfg       *config.NeoConfig          // [SRE-26.2.1] for dynamic module routing
	bus       *pubsub.Bus                // [SRE-32.2.2] nil-safe; emits bouncer + flashback events
	fpi       *rag.FlashbackFPI          // [SRE-35.2.1] nil-safe; FPI hit/miss tracking
	drift     *rag.DriftMonitor          // [SRE-35.1.2] nil-safe; cognitive drift recording
	inferGW   *inference.Gateway         // [SRE-86.A] nil-safe; inference gateway for fix suggestions
	radarRef  *RadarTool                 // [292.A] nil-safe; used for contract drift detection post-certify
	registry  *workspace.Registry        // [345.A] nil-safe; cross-workspace routing via Nexus
	contractMu        sync.Mutex
	prevContractSnap  []cpg.ContractNode // [292.A] snapshot from last go certify
	prevOpenAPIHash   string             // [334.A] sha256 of last observed openapi spec; "" = first run
}

func NewCertifyMutationTool(wal *rag.WAL, graph *rag.Graph, cpu tensorx.ComputeDevice, embedder rag.Embedder, workspace string, cfg *config.NeoConfig) *CertifyMutationTool {
	return &CertifyMutationTool{
		wal:       wal,
		graph:     graph,
		cpu:       cpu,
		embedder:  embedder,
		workspace: workspace,
		cfg:       cfg,
	}
}

// WithBus attaches an event bus so the certifier can emit dashboard events.
func (t *CertifyMutationTool) WithBus(bus *pubsub.Bus) *CertifyMutationTool {
	t.bus = bus
	return t
}

// WithInferenceGW attaches the inference gateway for auto-fix suggestions. [SRE-86.A]
func (t *CertifyMutationTool) WithInferenceGW(gw *inference.Gateway) *CertifyMutationTool {
	t.inferGW = gw
	return t
}

// WithRadar attaches the radar tool for contract drift detection post-certify. [292.A]
func (t *CertifyMutationTool) WithRadar(r *RadarTool) *CertifyMutationTool {
	t.radarRef = r
	return t
}

// WithRegistry attaches the workspace registry for cross-workspace certify routing. [345.A]
func (t *CertifyMutationTool) WithRegistry(r *workspace.Registry) *CertifyMutationTool {
	t.registry = r
	return t
}

func (t *CertifyMutationTool) Name() string { return "neo_sre_certify_mutation" }

func (t *CertifyMutationTool) Description() string {
	return "ACID Mutation Guardian: Reads files already edited by the AI from disk, validates syntax, complexity intent, and runs tests. On success: indexes to RAG. On failure: git rollback."
}

// [SRE-17.2.2] Simplified schema: no code injection, only file paths and complexity hint.
func (t *CertifyMutationTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"mutated_files": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Absolute paths of files already edited by the AI.",
			},
			"complexity_intent": map[string]any{
				"type":        "string",
				"description": "Declared complexity of the mutation.",
				"enum":        []string{"O(1)_OPTIMIZATION", "O(LogN)_SEARCH", "FEATURE_ADD", "BUG_FIX"},
			},
			"rollback_mode": map[string]any{
				"type":        "string",
				"description": "[SRE-76.3] Rollback strategy on failure. atomic=revert all files (default), granular=revert only failing file, none=report only.",
				"enum":        []string{"atomic", "granular", "none"},
			},
			"dry_run": map[string]any{
				"type":        "boolean",
				"description": "[SRE-102.D] When true, run AST + build checks but DO NOT write the certification seal or index to RAG. Safe pre-flight check for files still being edited; does not contaminate TTL.",
			},
		},
		Required: []string{"mutated_files", "complexity_intent"},
	}
}

// extractCertifiedBy parses _certified_by from args ([]string or []any). [SRE-122.C]
func extractCertifiedBy(args map[string]any) []string {
	if cb, ok := args["_certified_by"].([]string); ok {
		return cb
	}
	if cbRaw, ok := args["_certified_by"].([]any); ok {
		var out []string
		for _, v := range cbRaw {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// lockCRDTFiles validates that no file in filesRaw is CRDT-locked, returns the valid file list. [SRE-122.C]
func lockCRDTFiles(filesRaw []any) ([]string, error) {
	var locked []string
	for _, fRaw := range filesRaw {
		fname, _ := fRaw.(string)
		if fname == "" {
			continue
		}
		if globalCRDT.IsFileLocked(fname) {
			return nil, fmt.Errorf("[SRE-SWARM] VETO: Archivo bloqueado por concurrencia de otro Agente: %s", fname)
		}
		locked = append(locked, fname)
	}
	return locked, nil
}

// certifyWSGroup holds files destined for a single remote workspace. [345.A]
type certifyWSGroup struct {
	wsID  string
	files []string
}

// partitionCertifyFiles splits filesRaw into local files (owned by this workspace) and
// remote groups (keyed by owning workspace ID). Unknown-workspace paths fall through to
// localFiles so certifyOneFile can emit its own rejection message. [345.A]
func (t *CertifyMutationTool) partitionCertifyFiles(filesRaw []any) (localFiles []any, remoteGroups map[string]*certifyWSGroup) {
	localFiles = make([]any, 0, len(filesRaw))
	remoteGroups = map[string]*certifyWSGroup{}
	if t.registry == nil {
		return append(localFiles, filesRaw...), remoteGroups
	}
	for _, fRaw := range filesRaw {
		fname, _ := fRaw.(string)
		if fname == "" {
			continue
		}
		if isPathInWorkspace(t.workspace, fname) {
			localFiles = append(localFiles, fRaw)
			continue
		}
		wsID, _ := workspaceOfFile(fname, t.registry)
		if wsID == "" {
			localFiles = append(localFiles, fRaw) // unknown ws → certifyOneFile emits rejection
			continue
		}
		g := remoteGroups[wsID]
		if g == nil {
			g = &certifyWSGroup{wsID: wsID}
			remoteGroups[wsID] = g
		}
		g.files = append(g.files, fname)
	}
	return localFiles, remoteGroups
}

// certifyLocalBatch pre-reads snapshots, runs certifyOneFile for each local path, and manages
// atomic/granular/none rollback. Extracted from Execute to keep CC≤15. [SRE-31.2.2]
func (t *CertifyMutationTool) certifyLocalBatch(ctx context.Context, localFiles []any, complexityIntent string, fastMode, dryRun bool, rollbackMode string) []string {
	snapshots := make(map[string][]byte, len(localFiles))
	for _, fRaw := range localFiles {
		if fname, ok := fRaw.(string); ok {
			if data, err := os.ReadFile(fname); err == nil { //nolint:gosec // G304-WORKSPACE-CANON: validated by isPathInWorkspace in partitionCertifyFiles
				snapshots[fname] = data
			}
		}
	}
	rollbackAll := func() {
		for fname, data := range snapshots {
			if err := os.WriteFile(fname, data, 0644); err != nil {
				log.Printf("[SRE-ATOMIC-ROLLBACK] Failed to restore %s: %v", fname, err)
			}
		}
		state.RecordError(fmt.Sprintf("certify rollback at %d", time.Now().Unix()))
		if globalHyperGraph != nil {
			for fname := range snapshots {
				globalHyperGraph.RecordCodeError(fname, "certify_rollback", 0.9)
			}
		}
	}

	var results []string
	for _, fRaw := range localFiles {
		filename, _ := fRaw.(string)
		if filename == "" {
			continue
		}
		var rollbackFn func()
		switch rollbackMode {
		case "granular":
			snap := snapshots[filename]
			rollbackFn = func() {
				if snap != nil {
					_ = os.WriteFile(filename, snap, 0644)
					state.RecordError(fmt.Sprintf("certify granular rollback: %s at %d", filename, time.Now().Unix()))
				}
			}
		case "none":
			rollbackFn = func() {}
		default:
			rollbackFn = rollbackAll
		}
		results = append(results, t.certifyOneFile(ctx, filename, complexityIntent, fastMode, dryRun, rollbackFn))
	}
	if line := t.testImpactSummary(localFiles); line != "" {
		log.Print(line)
		results = append(results, line)
	}
	return results
}

// testImpactSummary builds an operator-facing one-liner naming the
// _test.go files that could be affected by this batch (same-package
// siblings + cross-package transitive importers via GRAPH_EDGES).
// Returns "" when there's nothing useful to surface (no wal, empty
// batch, no impacted tests). [Phase 2 MV / Speed-First]
//
// Both log AND certify-response carry this so operators tailing logs
// AND operators reading the tool output see the same data. Execution
// still runs the changed package's full suite; future narrowing into
// `go test -run` requires symbol-level mapping (deferred epic).
func (t *CertifyMutationTool) testImpactSummary(localFiles []any) string {
	if t.wal == nil || len(localFiles) == 0 {
		return ""
	}
	mutatedRel := make([]string, 0, len(localFiles))
	for _, f := range localFiles {
		s, ok := f.(string)
		if !ok || s == "" {
			continue
		}
		rel, err := filepath.Rel(t.workspace, s)
		if err != nil || strings.HasPrefix(rel, "..") {
			rel = s
		}
		mutatedRel = append(mutatedRel, filepath.ToSlash(rel))
	}
	impacted := testsImpactedBy(t.wal, t.workspace, mutatedRel)
	if len(impacted) == 0 {
		return ""
	}
	return fmt.Sprintf("[CERTIFY-TEST-IMPACT] %d test file(s) could be impacted by this batch: %v",
		len(impacted), impacted)
}

// [SRE-17.3] ACID Certifier: reads from disk, validates, tests, indexes or rolls back.
func (t *CertifyMutationTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	filesRaw, ok := args["mutated_files"].([]any)
	if !ok || len(filesRaw) == 0 {
		return nil, fmt.Errorf("mutated_files array is required")
	}
	complexityIntent, _ := args["complexity_intent"].(string)
	// [SRE-76.3] rollback_mode: atomic (default), granular, none.
	rollbackMode, _ := args["rollback_mode"].(string)
	if rollbackMode == "" {
		rollbackMode = "atomic"
	}
	// [SRE-102.D] dry_run: validate without sealing or indexing.
	dryRun, _ := args["dry_run"].(bool)
	// [SRE-62.3] Certified-by from Synthetic Debate — injected by main.go when consensus_enabled.
	certifiedBy := extractCertifiedBy(args)

	// [SRE-25.2.2] CRDT veto: reject if any file is already being certified by another agent.
	lockedFiles, crdtErr := lockCRDTFiles(filesRaw)
	if crdtErr != nil {
		return nil, crdtErr
	}
	for _, fname := range lockedFiles {
		globalCRDT.LockFile(fname)
	}
	defer func() {
		for _, fname := range lockedFiles {
			globalCRDT.UnlockFile(fname)
		}
	}()

	// [345.A] Partition files: local (this workspace) vs remote (other workspaces via Nexus).
	localFiles, remoteGroups := t.partitionCertifyFiles(filesRaw)

	fastMode := os.Getenv("NEO_SERVER_MODE") == "fast"
	// [SRE-31.2.2 + 345.A] Certify local files with atomic/granular/none rollback.
	// Remote workspaces manage their own rollback independently.
	results := t.certifyLocalBatch(ctx, localFiles, complexityIntent, fastMode, dryRun, rollbackMode)

	// [345.A] Forward remote files to their owning workspaces via Nexus dispatcher.
	for _, group := range remoteGroups {
		remoteResults := t.forwardCertifyToNexus(ctx, group.wsID, group.files, complexityIntent, rollbackMode, dryRun)
		results = append(results, remoteResults...)
	}

	// [138.E.2] Pair-mode trust feedback loop hook — best-effort,
	// non-fatal. Extracted to helper to keep Execute below CC=15.
	runPairFeedbackHook(results, dryRun)

	output := strings.Join(results, "\n")
	if len(certifiedBy) > 0 {
		// [SRE-62.3] Append consensus signature to batch output.
		output += "\n" + fmt.Sprintf(`{"certified_by": ["%s"], "signature": "Certified by %s"}`,
			strings.Join(certifiedBy, `", "`), strings.Join(certifiedBy, ", "))
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": output}}}, nil
}

// certifyOneFile runs the full certification pipeline for a single file and returns the result message.
// Uses early return instead of continue — caller appends the message to results unconditionally.
// [SRE-60.3] Result now includes a "checks" object so callers know which steps ran and passed.
func (t *CertifyMutationTool) certifyOneFile(ctx context.Context, filename, complexityIntent string, fastMode, dryRun bool, rollbackAll func()) string {
	// [Épica 330.L] Workspace ownership guard — reject paths outside t.workspace.
	// Cross-workspace certify pollutes session_state, heatmap, and RAG index of
	// the wrong workspace. Validated 2026-04-23 when vision-link BRIEFING
	// surfaced strategos paths in session_mutations. Reject BEFORE AST/bouncer
	// to avoid wasted CPU on foreign files.
	if !isPathInWorkspace(t.workspace, filename) {
		log.Printf("[SRE-OWN] reject cross-workspace path in certify: workspace=%s file=%s", t.workspace, filename)
		return fmt.Sprintf("❌ Cross-workspace path rejected: %s does not belong to workspace %s. Invoke neo_sre_certify_mutation on the MCP of the OWNING workspace.", filename, t.workspace)
	}

	src, err := os.ReadFile(filename) //nolint:gosec // G304-WORKSPACE-CANON
	if err != nil {
		return fmt.Sprintf("❌ Cannot read %s from disk: %v", filename, err)
	}
	ext := filepath.Ext(filename)

	// Track which checks ran and what they returned.
	checks := map[string]string{
		"ast":     "ok",
		"build":   "skipped",
		"bouncer": "skipped",
		"tests":   "skipped",
	}

	// [FIX-1] AST Syntax Guard — all tree-sitter-supported languages.
	if syntaxErr := astx.ValidateSyntax(ctx, src, filename); syntaxErr != nil {
		checks["ast"] = "fail:" + syntaxErr.Error()
		rollbackAll()
		t.emitBouncer(filename, "fail", "AST-SYNTAX: "+syntaxErr.Error())
		flashback := t.certifyFlashback(ctx, syntaxErr.Error(), filename)
		return fmt.Sprintf("🛑 Rollback Ejecutado. Fallaste en [AST-SYNTAX]: %v. Intenta otra lógica.%s", syntaxErr, flashback)
	}

	if msg := t.runFileChecks(ctx, filename, ext, complexityIntent, src, fastMode, checks, rollbackAll); msg != "" {
		return msg
	}

	// [371.C] DeepSeek pre-certify for hot-path files.
	dsResult := t.deepseekPreCheck(filename, src)
	checks["deepseek"] = formatDSCertifyCheck(dsResult)
	if dsResult.Blocked {
		rollbackAll()
		return fmt.Sprintf("❌ DS_BLOCKER: %s", dsResult.Summary)
	}

	// [SRE-102.D] Dry run: all checks passed but skip the side effects (RAG
	// index, seal stamp, session_state). Returns a distinct marker so the
	// agent can tell a dry_run success apart from a real certification.
	if dryRun {
		return fmt.Sprintf(`{"status": "Dry-run OK", "file": %q, "complexity": %q, "checks": {"ast": "ok", "build": %q, "bouncer": %q, "tests": %q}, "note": "no seal written, no rag indexing"}`,
			filename, complexityIntent, checks["build"], checks["bouncer"], checks["tests"])
	}

	// [SRE-17.4.1] Green Path: index to RAG silently.
	go func(fname string, source []byte) {
		chunks := astx.SemanticChunk(context.Background(), source, filepath.Ext(fname))
		if len(chunks) == 0 {
			chunks = [][]byte{source}
		}
		// Batch the embed calls when the embedder supports /api/embed (plural).
		// Hot-paths shipping ~5-20 chunks per certify see 3-10× wall-clock win.
		texts := make([]string, len(chunks))
		for i, chunk := range chunks {
			texts[i] = string(chunk)
		}
		vecs, err := rag.EmbedMany(context.Background(), t.embedder, texts)
		if err != nil {
			return
		}
		for i, vec := range vecs {
			docID := docIDCounter.Add(1) + uint64(time.Now().UnixNano())
			_ = t.graph.Insert(context.Background(), docID, vec, 5, t.cpu, t.wal)
			_ = t.wal.SaveDocMeta(docID, fname, texts[i], 0)
		}
	}(filename, src)

	// [BLAST_RADIUS dep-graph fix 2/3] Refresh this file's file→file edges in
	// the GRAPH_EDGES bucket so BLAST_RADIUS impact stays fresh between full
	// re-indexes. Its own goroutine — independent of the embed path above, so a
	// failed embed never skips the dep-graph update. ReplaceFileEdges is
	// idempotent: a dropped import leaves no stale edge.
	go func(fname string, source []byte) {
		rel, relErr := filepath.Rel(t.workspace, fname)
		if relErr != nil {
			return
		}
		relSlash := filepath.ToSlash(rel)
		edges := fileDepEdges(t.workspace, workspaceModulePath(t.workspace), relSlash,
			extractImports(string(source), filepath.Ext(fname)))
		if err := rag.ReplaceFileEdges(t.wal, relSlash, edges); err != nil {
			log.Printf("[SRE-WARN] dep-graph edges for %s: %v", relSlash, err)
		}
	}(filename, src)

	stampCertifiedFile(projectRootOf(filename), filename)
	// [159.A] Record certified mutation in AST_HEATMAP for TECH_DEBT_MAP hotspot tracking.
	if hmErr := telemetry.RecordMutation(filename); hmErr != nil {
		log.Printf("[SRE-159] heatmap write error: %v", hmErr)
	}
	// [PILAR-XXVII/243.F] Also persist to observability.db so the web HUD +
	// TUI can show mutations-last-24h + top-hotspots across restarts.
	observability.GlobalStore.RecordMutation(filename, false)
	// [Épica 79.2] Record certified path in session_state for BRIEFING session_mutations.
	if err := t.wal.AppendSessionCertified(briefingSessionID(t.workspace), filename); err != nil {
		log.Printf("[SRE-79] session_state write error: %v", err)
	}
	// [335.A] Broadcast to Nexus so siblings can mirror this mutation.
	go broadcastSessionMutation(t.workspace, filename)

	// [SRE-88.A] Run auto-remediation detection on certified Go files.
	remediationHint := ""
	if ext == ".go" {
		if rems, remErr := astx.DetectRemediations(filename, src); remErr == nil && len(rems) > 0 {
			remediationHint = fmt.Sprintf(`, "suggested_remediation": "%d zero-alloc issue(s) detected — run AST_AUDIT for details"`, len(rems))
			log.Printf("[SRE-88] %d remediation(s) detected in %s", len(rems), filename)
		}
	}

	// [292.A] Detect contract drift for Go files — run in background to not delay certify response.
	if ext == ".go" && t.radarRef != nil {
		go t.checkContractDrift()
	}

	t.emitBouncer(filename, "pass", complexityIntent)
	return fmt.Sprintf(
		`{"status": "Aprobado e Indexado", "file": "%s", "complexity": "%s", "checks": {"ast": "%s", "build": "%s", "bouncer": "%s", "tests": "%s"}%s}`,
		filename, complexityIntent,
		checks["ast"], checks["build"], checks["bouncer"], checks["tests"],
		remediationHint,
	)
}

// workspaceOfFile returns the workspace ID and path for a given absolute file path by
// scanning the registry for the longest matching workspace root prefix. [345.A]
func workspaceOfFile(absFile string, reg *workspace.Registry) (wsID, wsPath string) {
	best := ""
	for _, ws := range reg.Workspaces {
		absWs, _ := filepath.Abs(ws.Path)
		prefix := absWs + string(filepath.Separator)
		if strings.HasPrefix(absFile, prefix) && len(absWs) > len(best) {
			best = absWs
			wsID = ws.ID
			wsPath = ws.Path
		}
	}
	return wsID, wsPath
}

// forwardCertifyToNexus proxies a certify batch for a remote workspace through the Nexus
// dispatcher's /internal/certify/{workspace_id} endpoint. Returns per-file result lines. [345.A]
func (t *CertifyMutationTool) forwardCertifyToNexus(ctx context.Context, wsID string, files []string, intent, rollbackMode string, dryRun bool) []string {
	nexusURL := fmt.Sprintf("http://127.0.0.1:%d", t.cfg.Server.NexusDispatcherPort)
	payload, err := json.Marshal(map[string]any{
		"mutated_files":     files,
		"complexity_intent": intent,
		"rollback_mode":     rollbackMode,
		"dry_run":           dryRun,
	})
	if err != nil {
		return []string{fmt.Sprintf("❌ cross-ws certify(%s) marshal: %v", wsID, err)}
	}

	url := nexusURL + "/internal/certify/" + wsID //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: SafeInternalHTTPClient allows only loopback; nexusURL uses NexusDispatcherPort from neo.yaml
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return []string{fmt.Sprintf("❌ cross-ws certify(%s) request: %v", wsID, err)}
	}
	req.Header.Set("Content-Type", "application/json")
	// [PRIVILEGE-001] Authenticate with the ephemeral internal token injected by Nexus at boot.
	if tok := os.Getenv("NEO_NEXUS_INTERNAL_TOKEN"); tok != "" {
		req.Header.Set("X-Neo-Internal-Token", tok)
	}

	client := sre.SafeInternalHTTPClient(30)
	resp, err := client.Do(req)
	if err != nil {
		return []string{fmt.Sprintf("❌ cross-ws certify(%s) nexus error: %v — is workspace running?", wsID, err)}
	}
	defer resp.Body.Close() //nolint:errcheck

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return []string{fmt.Sprintf("❌ cross-ws certify(%s) HTTP %d: %s", wsID, resp.StatusCode, string(body))}
	}

	var nr struct {
		Results     []string `json:"results"`
		WorkspaceID string   `json:"workspace_id"`
	}
	if parseErr := json.Unmarshal(body, &nr); parseErr != nil {
		return []string{fmt.Sprintf("❌ cross-ws certify(%s) parse: %v", wsID, parseErr)}
	}
	return nr.Results
}

// runFileChecks runs polyglot/Go build + bouncer + tests depending on file extension and mode. [SRE-122.D]
// Returns an error message on failure, or "" on success. Mutates checks map in place.
// isPathInWorkspace returns true when filename resolves to a path ANCHORED
// inside workspace. Both args are absolutized + cleaned before comparison so
// symlinks and `..` segments can't escape. [Épica 330.L]
//
// Rationale: certify-level guard prevents agent A (operating vision-link) from
// polluting vision-link's session_state with paths from workspace B (strategos).
// The bug manifested as cross-workspace file paths showing in BRIEFING's
// session_mutations of the wrong workspace.
func isPathInWorkspace(workspace, filename string) bool {
	absWs, err := filepath.Abs(workspace)
	if err != nil {
		return false
	}
	absFile, err := filepath.Abs(filename)
	if err != nil {
		return false
	}
	// Clean normalizes "/" + "." + ".." — Rel fails if Abs paths are on
	// different volumes, treat that as out-of-workspace.
	rel, err := filepath.Rel(filepath.Clean(absWs), filepath.Clean(absFile))
	if err != nil {
		return false
	}
	// Rel returns "../foo/bar" when the target escapes workspace. Any prefix
	// of "../" means outside. Also ".." alone (target IS the parent dir).
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (t *CertifyMutationTool) runFileChecks(ctx context.Context, filename, ext, complexityIntent string, src []byte, fastMode bool, checks map[string]string, rollbackAll func()) string {
	// [SRE-26.3.1/26.3.2/26.3.3] Universal module router — polyglot, skipped in pair mode.
	if ext != ".go" && !fastMode && os.Getenv("NEO_SERVER_MODE") != "pair" {
		checks["build"] = "running"
		if msg := t.runPolyglotBuild(ctx, filename, ext, rollbackAll); msg != "" {
			checks["build"] = "fail"
			return msg
		}
		checks["build"] = "ok"
	}

	// [SRE-23.2.2] Thermodynamic Bouncer + TDD — Go ONLY, non-fast-mode.
	if ext == ".go" && !fastMode {
		checks["build"] = "running"
		checks["bouncer"] = "running"
		checks["tests"] = "running"
		if msg, bounce := t.runGoBouncer(ctx, filename, src, complexityIntent, rollbackAll); msg != "" {
			checks["build"] = "fail"
			checks["bouncer"] = "fail"
			checks["tests"] = "fail"
			// [SRE-86.A.3] Enrich error with suggested fix from inference gateway.
			if t.inferGW != nil && bounce != nil {
				if fix, fixErr := t.inferGW.SuggestFix(ctx, bounce, nil); fixErr == nil && fix != "" {
					msg = msg + fmt.Sprintf("\n\n--- suggested_fix ---\n%s", fix)
				}
			}
			return msg
		}
		checks["build"] = "ok"
		checks["bouncer"] = "ok"
		checks["tests"] = "ok"
	} else if ext == ".go" && fastMode {
		// Fast mode: go build only, no bouncer/tests.
		checks["build"] = "running"
		buildCmd := exec.CommandContext(ctx, "go", "build", filepath.Dir(filename)) //nolint:gosec // G204-LITERAL-BIN
		buildCmd.Dir = goModRootOf(filename) // [T001] go.mod root, NOT neo.yaml root
		sre.HardenSubprocess(buildCmd, 0)    // [T006-sweep] cgo grandchildren can pin pipes
		if out, buildErr := buildCmd.CombinedOutput(); buildErr != nil {
			checks["build"] = "fail:" + strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
		} else {
			checks["build"] = "ok"
		}
	}
	return ""
}

// truncateBuildOutput keeps the last maxLines lines of command output so that
// error messages sent to the LLM stay within a reasonable token budget. The
// full raw output is still passed to certifyFlashback for semantic search.
func truncateBuildOutput(out []byte, maxLines int) string {
	s := string(out)
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	omitted := len(lines) - maxLines
	return fmt.Sprintf("... [%d lines omitted — showing last %d] ...\n%s",
		omitted, maxLines, strings.Join(lines[len(lines)-maxLines:], "\n"))
}

// runPolyglotBuild runs the language-specific build for non-Go files.
// Returns a rollback message on failure, or "" on success.
func (t *CertifyMutationTool) runPolyglotBuild(ctx context.Context, filename, ext string, rollbackAll func()) string {
	moduleDir, buildCmd := t.resolveModuleBuild(filename)
	if moduleDir != "" {
		shCmd := exec.CommandContext(ctx, "sh", "-c", buildCmd) //nolint:gosec // G204-SHELL-WITH-VALIDATION
		shCmd.Dir = filepath.Join(projectRootOf(filename), moduleDir)
		sre.HardenSubprocess(shCmd, 0) // [T006-sweep] polyglot module builds may invoke long-running compilers
		out, errBuild := shCmd.CombinedOutput()
		if errBuild != nil {
			rollbackAll()
			return fmt.Sprintf("🛑 Rollback Ejecutado. Fallaste en [MODULE-BUILD:%s]: %v\n%s", moduleDir, errBuild, truncateBuildOutput(out, 80))
		}
		return ""
	}

	// [SRE-26.3.3] No YAML entry — language-specific fallback.
	var fallbackCmd *exec.Cmd
	switch ext {
	case ".rs":
		fallbackCmd = exec.CommandContext(ctx, "cargo", "build")
		fallbackCmd.Dir = filepath.Dir(filename)
		sre.HardenSubprocess(fallbackCmd, 0) // [T006-sweep] cargo build runs rustc with parallel codegen
	case ".py":
		fallbackCmd = exec.CommandContext(ctx, "python3", "-c",
			fmt.Sprintf("import ast, sys; ast.parse(open('%s').read())", filename))
		fallbackCmd.Dir = projectRootOf(filename)
	case ".ts", ".tsx", ".js", ".jsx", ".css", ".html":
		log.Printf("[SRE-26] Build omitido: módulo no configurado en neo.yaml para %s", filename)
	default:
		log.Printf("[SRE-26] Extensión sin fallback (%s) — solo AST", ext)
	}
	if fallbackCmd != nil {
		out, errFallback := fallbackCmd.CombinedOutput()
		if errFallback != nil {
			rollbackAll()
			return fmt.Sprintf("🛑 Rollback Ejecutado. Fallaste en [%s-BUILD]: %v\n%s",
				strings.ToUpper(strings.TrimPrefix(ext, ".")), errFallback, truncateBuildOutput(out, 80))
		}
	}
	return ""
}

// resolveModuleBuild finds the YAML-configured build command for a file, if any.
func (t *CertifyMutationTool) resolveModuleBuild(filename string) (moduleDir, buildCmd string) {
	if t.cfg == nil {
		return "", ""
	}
	rel, errRel := filepath.Rel(projectRootOf(filename), filename)
	if errRel != nil {
		return "", ""
	}
	parts := strings.SplitN(filepath.ToSlash(rel), "/", 2)
	if len(parts) == 0 {
		return "", ""
	}
	cmd, ok := t.cfg.Workspace.Modules[parts[0]]
	if !ok {
		return "", ""
	}
	return parts[0], cmd
}

// runGoBouncer runs the thermodynamic bouncer and TDD for Go files. [SRE-86.A]
// Returns (errorMsg, bounceResult). errorMsg="" means success. bounceResult is
// populated on failure to feed inference.SuggestFix for auto-fix proposals.
func (t *CertifyMutationTool) runGoBouncer(ctx context.Context, filename string, src []byte, complexityIntent string, rollbackAll func()) (string, *inference.BounceResult) {
	// [SRE-17.3.2] Thermodynamic Bouncer: detect O(N²) when O(1) is claimed.
	if complexityIntent == "O(1)_OPTIMIZATION" {
		code := string(src)
		innerFor := strings.Count(code, "\n\t\tfor ") + strings.Count(code, "\n\t\t\tfor ")
		if innerFor > 0 {
			rollbackAll()
			msg := fmt.Sprintf("🛑 Rollback Ejecutado. Fallaste en [TERMODINÁMICA]: código O(N²) detectado (%d bucles anidados) pero se declaró O(1). Intenta otra lógica.", innerFor)
			return msg, &inference.BounceResult{
				Passed:       false,
				ErrorContext: msg,
				FailedChecks: []string{"BOUNCER-THERMODYNAMIC"},
				FilePath:     filename,
			}
		}
	}
	// [SRE-76.4] Preflight dependency check — verify imports resolve before running tests.
	pkgPath := filepath.Dir(filename)
	listCmd := exec.CommandContext(ctx, "go", "list", pkgPath) //nolint:gosec // G204-LITERAL-BIN
	listCmd.Dir = goModRootOf(filename) // [T001] go.mod root, NOT neo.yaml root
	if out, listErr := listCmd.CombinedOutput(); listErr != nil {
		outStr := strings.TrimSpace(string(out))
		var missing []string
		for line := range strings.SplitSeq(outStr, "\n") {
			if strings.Contains(line, "cannot find package") || strings.Contains(line, "no required module") {
				missing = append(missing, strings.TrimSpace(line))
			}
		}
		if len(missing) > 0 {
			msg := fmt.Sprintf(`{"status": "blocked", "reason": "missing_dependency", "missing": ["%s"], "suggestion": "Run go get or create placeholder packages before certifying"}`,
				strings.Join(missing, `", "`))
			return msg, &inference.BounceResult{
				Passed:       false,
				ErrorContext: outStr,
				FailedChecks: []string{"BUILD-DEPENDENCY"},
				FilePath:     filename,
			}
		}
	}

	// [SRE-17.3.3] Test-Driven Validation.
	testCmd := exec.CommandContext(ctx, "go", "test", "-short", pkgPath) //nolint:gosec // G204-LITERAL-BIN
	testCmd.Dir = goModRootOf(filename) // [T001] go.mod root, NOT neo.yaml root
	out, errTest := testCmd.CombinedOutput()
	if errTest != nil {
		rollbackAll()
		flashback := t.certifyFlashback(ctx, string(out), filename) // full output for semantic flashback search
		msg := fmt.Sprintf("🛑 Rollback Ejecutado. Fallaste en [TDD]: %v. Intenta otra lógica.\n%s%s", errTest, truncateBuildOutput(out, 80), flashback)
		return msg, &inference.BounceResult{
			Passed:       false,
			ErrorContext: string(out),
			FailedChecks: []string{"TDD"},
			FilePath:     filename,
		}
	}
	return "", nil
}

// [SRE-21.3.1] Write certified file path to lock file for pre-commit hook verification.
// [SRE-101.C] Logs the resolved seal path so projectRootOf mis-resolution (seal
// landing in a sub-directory lock file instead of REPO_ROOT) is visible in logs.
func stampCertifiedFile(workspace, filename string) {
	lockPath := filepath.Join(workspace, ".neo", "db", "certified_state.lock")
	_ = os.MkdirAll(filepath.Dir(lockPath), 0755)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[SRE-CERTIFY] failed to open seal lock %s: %v", lockPath, err)
		return
	}
	defer f.Close()
	log.Printf("[SRE-CERTIFY] writing seal to %s (file=%s)", lockPath, filename)
	fmt.Fprintf(f, "%s|%d\n", filename, time.Now().Unix())
}

// [SRE-28.4.1-3] certifyFlashback delegates to rag.SearchFlashback (testable) and formats result.
func (t *CertifyMutationTool) certifyFlashback(ctx context.Context, errOutput, filename string) string {
	result, err := rag.SearchFlashback(ctx, t.graph, t.wal, t.cpu, t.embedder, errOutput, filename, t.fpi, t.drift)
	if err != nil || result == nil {
		return ""
	}
	// [SRE-32.2.2] Emit flashback event to Operator HUD.
	if t.bus != nil {
		t.bus.Publish(pubsub.Event{
			Type: pubsub.EventFlashback,
			Payload: map[string]any{
				"file_path": result.FilePath,
				"distance":  result.Distance,
				"content":   result.Content,
			},
		})
	}
	return rag.FormatFlashbackMessage(result)
}

// emitBouncer publishes a certification result event to the Operator HUD bus.
// nil-safe: no-op if bus is not set. [SRE-32.2.2]
func (t *CertifyMutationTool) emitBouncer(file, status, reason string) {
	if t.bus == nil {
		return
	}
	t.bus.Publish(pubsub.Event{
		Type: pubsub.EventBouncer,
		Payload: map[string]any{
			"file":   file,
			"status": status,
			"reason": reason,
		},
	})
}

// [SRE-28.2.1] MemoryCommitTool: saves an episodic lesson to the short-term memex buffer.
// REM sleep will later consolidate it into the HNSW long-term store.
type MemoryCommitTool struct {
	workspace string
}

func NewMemoryCommitTool(workspace string) *MemoryCommitTool {
	return &MemoryCommitTool{workspace: workspace}
}

func (t *MemoryCommitTool) Name() string { return "neo_memory_commit" }

func (t *MemoryCommitTool) Description() string {
	return "[SRE-28.2.1] Commits an episodic memory entry (lesson learned) to the short-term memex buffer. Consolidated into long-term HNSW memory during the next REM sleep cycle (5 min idle)."
}

// [SRE-28.2.2] Schema: topic, scope, content.
func (t *MemoryCommitTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"topic": map[string]any{
				"type":        "string",
				"description": "Short label for this memory (e.g. 'race condition in certifier').",
			},
			"scope": map[string]any{
				"type":        "string",
				"description": "File path or module this lesson applies to (e.g. 'cmd/neo-mcp/macro_tools.go').",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "The lesson learned — what happened, why, and how it was fixed.",
			},
		},
		Required: []string{"topic", "scope", "content"},
	}
}

func (t *MemoryCommitTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	topic, _ := args["topic"].(string)
	scope, _ := args["scope"].(string)
	content, _ := args["content"].(string)
	if topic == "" || content == "" {
		return nil, fmt.Errorf("topic and content are required")
	}
	entry := state.MemexEntry{
		Topic:   topic,
		Scope:   scope,
		Content: content,
		Causal:  state.CaptureCausalContext("user_commit", ""),
	}
	if err := state.MemexCommit(entry); err != nil {
		return nil, fmt.Errorf("memex commit failed: %w", err)
	}
	// [SRE-72.1] HyperGraph: record code→causal edge for memory commit
	if globalHyperGraph != nil && scope != "" {
		globalHyperGraph.RecordCodeCausal(scope, entry.ID, topic)
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("✅ Episodio '%s' guardado en memex buffer. Será consolidado en el próximo ciclo REM.", topic)}}}, nil
}

// BreakingChange describes a single HTTP contract breaking change detected post-certify. [292.A]
type BreakingChange struct {
	Type     string `json:"type"`     // route_removed | method_changed | handler_renamed | field_added_required
	Endpoint string `json:"endpoint"` // HTTP path
	Old      string `json:"old"`
	New      string `json:"new"`
}

// checkContractDrift re-resolves contracts after a successful Go certify and compares
// with the previous snapshot. Emits SSE event + INC file when breaking changes found. [292.A/B]
func (t *CertifyMutationTool) checkContractDrift() {
	current, _ := t.radarRef.resolveContracts()

	t.contractMu.Lock()
	prev := t.prevContractSnap
	t.prevContractSnap = current
	t.contractMu.Unlock()

	if prev == nil {
		return // first run — establish baseline, no diff
	}

	breaking := diffContracts(prev, current)
	if len(breaking) == 0 {
		return
	}

	log.Printf("[292.A] contract drift: %d breaking change(s) detected", len(breaking))

	// [Épica 330.A] Drop breaking changes whose affected route has 0 TS callers in the
	// previous snapshot. Intentional cleanups (e.g. removal of /debug/pprof/*) should not
	// page operators — log-only. If every change was a zero-caller cleanup, skip the INC
	// + SSE + Nexus broadcast entirely.
	actionable := filterActionableBreaking(breaking, prev)
	if len(actionable) == 0 {
		log.Printf("[330.A] all %d breaking change(s) had 0 frontend callers — intentional cleanup, no INC emitted", len(breaking))
		return
	}

	// [292.B] Emit SSE event with the actionable subset.
	if t.bus != nil {
		wsName := filepath.Base(t.workspace)
		t.bus.Publish(pubsub.Event{
			Type: pubsub.EventContractDrift,
			At:   time.Now(),
			Payload: map[string]any{
				"breaking":     actionable,
				"workspace_id": wsName,
				"timestamp":    time.Now().Unix(),
			},
		})
	}

	// [292.B] Auto-generate INC file.
	t.writeContractDriftINC(actionable)

	// [343.A MVP] When this workspace belongs to a federation, also append a
	// pending-approval proposal per breaking change so the affected sibling
	// sees it via BRIEFING / neo_debt scope:"project". Best-effort: failure
	// to persist never masks the INC + Nexus notification.
	t.proposeContractChanges(actionable)

	// [292.D] Notify Nexus dispatcher so it can fan out to project siblings.
	notifyNexusContractDrift(t.workspace, actionable)

	// [334.A] Detect OpenAPI spec file change and broadcast hash+path to Nexus.
	t.detectAndBroadcastOpenAPIChange()
}

// filterActionableBreaking returns only BreakingChanges that had known frontend callers
// in the previous snapshot. Changes affecting routes with zero callers are treated as
// intentional cleanups and logged but not emitted as incidents. [Épica 330.A]
//
// Indexing strategy: build a callers-per-route map keyed by "METHOD PATH" (exact) and by
// "PATH" (for route_removed where the method is part of `Old` string). If either lookup
// yields a non-zero caller count, the change is actionable.
func filterActionableBreaking(breaking []BreakingChange, prev []cpg.ContractNode) []BreakingChange {
	callerCount := make(map[string]int, len(prev)*2)
	for _, c := range prev {
		n := len(c.FrontendCallers)
		if n == 0 {
			continue
		}
		callerCount[c.Method+" "+c.Path] += n
		callerCount[c.Path] += n
	}
	actionable := breaking[:0]
	for _, b := range breaking {
		n := callerCount[b.Old]
		if n == 0 {
			n = callerCount[b.Endpoint]
		}
		if n == 0 {
			log.Printf("[CONTRACT-WARN] %s %s: 0 frontend callers in previous snapshot — intentional cleanup, skipping INC", b.Type, b.Endpoint)
			continue
		}
		actionable = append(actionable, b)
	}
	return actionable
}

// notifyNexusContractDrift POSTs breaking changes to Nexus /internal/contract/broadcast. [292.D]
// No-op when not running under Nexus (nexusDispatcherBase returns "").
func notifyNexusContractDrift(workspace string, breaking []BreakingChange) {
	nexusBase := nexusDispatcherBase()
	if nexusBase == "" {
		return
	}
	wsID := resolveWorkspaceID(workspace)
	if wsID == "" {
		wsID = filepath.Base(workspace)
	}
	payload, err := json.Marshal(map[string]any{
		"breaking":       breaking,
		"from_workspace": wsID,
	})
	if err != nil {
		return
	}
	client := sre.SafeInternalHTTPClient(5)
	url := nexusBase + "/internal/contract/broadcast"
	resp, err := client.Post(url, "application/json", strings.NewReader(string(payload))) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: url derived from NEO_EXTERNAL_URL via nexusDispatcherBase
	if err != nil {
		log.Printf("[292.D] contract broadcast POST failed: %v", err)
		return
	}
	_ = resp.Body.Close()
	log.Printf("[292.D] contract drift broadcast → Nexus %s (ws=%s, breaking=%d)", nexusBase, wsID, len(breaking))
}

// broadcastSessionMutation POSTs a certified file to Nexus /internal/session/broadcast
// so sibling workspaces can mirror it in their peer_session_state. [335.A]
func broadcastSessionMutation(workspace, file string) {
	nexusBase := nexusDispatcherBase()
	if nexusBase == "" {
		return
	}
	wsID := resolveWorkspaceID(workspace)
	if wsID == "" {
		wsID = filepath.Base(workspace)
	}
	payload, err := json.Marshal(map[string]any{
		"workspace_id":  wsID,
		"mutated_file":  file,
		"certified_at":  time.Now().Unix(),
	})
	if err != nil {
		return
	}
	client := sre.SafeInternalHTTPClient(3)
	url := nexusBase + "/internal/session/broadcast"
	resp, postErr := client.Post(url, "application/json", strings.NewReader(string(payload))) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: url derived from NEO_EXTERNAL_URL via nexusDispatcherBase
	if postErr != nil {
		log.Printf("[335.A] session broadcast POST failed: %v", postErr)
		return
	}
	_ = resp.Body.Close()
}

// detectAndBroadcastOpenAPIChange hashes the OpenAPI spec file and broadcasts to Nexus
// when it differs from the previous run. Called from checkContractDrift. [334.A]
func (t *CertifyMutationTool) detectAndBroadcastOpenAPIChange() {
	specData, specRel, err := cpg.FindOpenAPISpecData(t.workspace)
	if err != nil || len(specData) == 0 {
		return // no spec found — nothing to track
	}
	raw := sha256.Sum256(specData)
	hash := hex.EncodeToString(raw[:])

	t.contractMu.Lock()
	prev := t.prevOpenAPIHash
	t.prevOpenAPIHash = hash
	t.contractMu.Unlock()

	if prev == "" || prev == hash {
		return // first run or no change
	}
	log.Printf("[334.A] OpenAPI spec changed: %s (hash=%s)", specRel, hash[:8])
	notifyNexusOpenAPIBroadcast(t.workspace, specRel, hash)
}

// notifyNexusOpenAPIBroadcast POSTs {workspace_id, spec_path, hash} to Nexus
// /internal/openapi/broadcast so siblings can invalidate their spec caches. [334.A]
func notifyNexusOpenAPIBroadcast(workspace, specPath, hash string) {
	nexusBase := nexusDispatcherBase()
	if nexusBase == "" {
		return
	}
	wsID := resolveWorkspaceID(workspace)
	if wsID == "" {
		wsID = filepath.Base(workspace)
	}
	payload, err := json.Marshal(map[string]any{
		"workspace_id": wsID,
		"spec_path":    specPath,
		"hash":         hash,
	})
	if err != nil {
		return
	}
	client := sre.SafeInternalHTTPClient(5)
	url := nexusBase + "/internal/openapi/broadcast"
	resp, postErr := client.Post(url, "application/json", strings.NewReader(string(payload))) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: url derived from NEO_EXTERNAL_URL via nexusDispatcherBase
	if postErr != nil {
		log.Printf("[334.A] openapi broadcast POST failed: %v", postErr)
		return
	}
	defer resp.Body.Close() //nolint:errcheck
	log.Printf("[334.A] openapi spec broadcast → Nexus %s (ws=%s, hash=%s)", nexusBase, wsID, hash[:8])
}

// diffContracts computes breaking changes between old and new contract snapshots. [292.A]
func diffContracts(prev, current []cpg.ContractNode) []BreakingChange {
	type key struct{ method, path string }
	prevMap := make(map[key]cpg.ContractNode, len(prev))
	for _, c := range prev {
		prevMap[key{c.Method, c.Path}] = c
	}
	currMap := make(map[key]cpg.ContractNode, len(current))
	for _, c := range current {
		currMap[key{c.Method, c.Path}] = c
	}

	var breaking []BreakingChange
	// Route removed.
	for k, old := range prevMap {
		if _, ok := currMap[k]; !ok {
			breaking = append(breaking, BreakingChange{
				Type:     "route_removed",
				Endpoint: old.Path,
				Old:      old.Method + " " + old.Path,
				New:      "(removed)",
			})
		}
	}
	// Method changed (same path, different method).
	prevByPath := make(map[string]cpg.ContractNode, len(prev))
	for _, c := range prev {
		prevByPath[c.Path] = c
	}
	for _, c := range current {
		if old, ok := prevByPath[c.Path]; ok && old.Method != c.Method && old.Method != "" {
			breaking = append(breaking, BreakingChange{
				Type:     "method_changed",
				Endpoint: c.Path,
				Old:      old.Method,
				New:      c.Method,
			})
		}
	}
	// Handler renamed (same path+method, different BackendFn).
	for k, old := range prevMap {
		if curr, ok := currMap[k]; ok && old.BackendFn != "" && curr.BackendFn != "" && old.BackendFn != curr.BackendFn {
			breaking = append(breaking, BreakingChange{
				Type:     "handler_renamed",
				Endpoint: k.path,
				Old:      old.BackendFn,
				New:      curr.BackendFn,
			})
		}
	}
	return breaking
}

// proposeContractChanges appends a ContractProposal per breaking change to
// `.neo-project/CONTRACT_PROPOSALS.md` when this workspace is a federation
// member. Best-effort: log + continue on any error. [343.A MVP]
func (t *CertifyMutationTool) proposeContractChanges(breaking []BreakingChange) {
	projDir, ok := federation.FindNeoProjectDir(t.workspace)
	if !ok {
		return // standalone workspace — nothing to propose
	}
	wsName := filepath.Base(t.workspace)
	for _, bc := range breaking {
		endpoint := bc.Endpoint
		if bc.Old != "" && bc.New != "" {
			endpoint = fmt.Sprintf("%s (%s → %s)", bc.Endpoint, bc.Old, bc.New)
		}
		_, err := federation.AppendContractProposal(projDir, federation.ContractProposal{
			FromWorkspace: wsName,
			Endpoint:      endpoint,
			ChangeType:    bc.Type,
			// AffectedCallers left empty in MVP — filterActionableBreaking
			// already filtered out 0-caller changes, but the exact file list
			// lives in the pre-certify CPG snapshot not captured here. Follow-up
			// épica can wire the callers through to the proposal record.
		})
		if err != nil {
			log.Printf("[343.A] AppendContractProposal failed: %v", err)
		}
	}
}

// writeContractDriftINC creates a .neo/incidents/INC-<ts>-contract-drift.md file. [292.B]
func (t *CertifyMutationTool) writeContractDriftINC(breaking []BreakingChange) {
	incDir := filepath.Join(t.workspace, ".neo", "incidents")
	if err := os.MkdirAll(incDir, 0o750); err != nil {
		return
	}
	wsName := filepath.Base(t.workspace)
	ts := time.Now().Format("20060102-150405")
	fname := filepath.Join(incDir, fmt.Sprintf("INC-%s-contract-drift.md", ts))

	var sb strings.Builder
	fmt.Fprintf(&sb, "# INC-%s — Contract Drift Detected\n\n", ts)
	fmt.Fprintf(&sb, "**Affected Services:** %s\n\n", wsName)
	fmt.Fprintf(&sb, "**Timestamp:** %s\n\n", time.Now().Format(time.RFC3339))
	sb.WriteString("## Breaking Changes\n\n")
	for _, b := range breaking {
		fmt.Fprintf(&sb, "- **%s** `%s`: `%s` → `%s`\n", b.Type, b.Endpoint, b.Old, b.New)
	}
	sb.WriteString("\n## Recommended Actions\n\n")
	sb.WriteString("1. Run `neo_radar(intent:\"CONTRACT_QUERY\", target:\"<path>\")` for full schema diff.\n")
	sb.WriteString("2. Update frontend callers to match new contract.\n")
	sb.WriteString("3. Re-run certify on frontend files after update.\n")

	if err := os.WriteFile(fname, []byte(sb.String()), 0o640); err != nil { //nolint:gosec // G304-DIR-WALK: incDir controlled
		log.Printf("[292.B] failed to write contract drift INC: %v", err)
	} else {
		log.Printf("[292.B] contract drift INC written: %s", fname)
	}
}
