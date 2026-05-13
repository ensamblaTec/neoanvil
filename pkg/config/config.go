package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type ServerConfig struct {
	LogLevel        string `yaml:"log_level"`
	Host            string `yaml:"host"`              // Interfaz de red para SSE/HUD
	Port            int    `yaml:"port"`              // HUD Port (neo-hud frontend)
	SandboxPort     int    `yaml:"sandbox_port"`      // Sandbox ingestion server (mTLS)
	TacticalPort    int    `yaml:"tactical_port"`     // Sandbox tactical aux server (local-only sync)
	DiagnosticsPort int    `yaml:"diagnostics_port"`  // pprof/diagnósticos (6060)
	SREListenerPort int    `yaml:"sre_listener_port"` // Listener de incidentes externos (8082)
	SSEPort         int    `yaml:"sse_port"`          // Puerto del transporte SSE MCP
	SSEPath         string `yaml:"sse_path"`          // Ruta GET del endpoint SSE
	SSEMessagePath  string `yaml:"sse_message_path"`  // Ruta POST del endpoint de mensajes
	Mode            string   `yaml:"mode"`              // "pair", "fast", "daemon", "idle", "gameday"
	Tailscale       bool     `yaml:"tailscale"`         // Interruptor SRE para WireGuard P2P
	GossipPeers     []string `yaml:"gossip_peers"`      // [SRE-27.1.1] Pool de IPs Tailscale para Gossip P2P
	GossipPort      int      `yaml:"gossip_port"`       // [SRE-27.1.1] Puerto TCP del listener Gossip (default 8086)
	DashboardPort          int      `yaml:"dashboard_port"`           // [SRE-32.1.2] Operator HUD web dashboard (default 8087)
	NexusDispatcherPort    int      `yaml:"nexus_dispatcher_port"`    // Puerto del Nexus dispatcher (default 9000); usado por neo-mcp para scatter cross-workspace
	// [PILAR-XXIII] Tool call observability: timeout for any tools/call dispatch. 0 = disabled.
	ToolTimeoutSeconds int `yaml:"tool_timeout_seconds"` // default 120s — cancels hung handlers so the client never waits forever
}

// IntegrationsConfig contiene endpoints de servicios externos e internos.
// Zero-Hardcoding: todos los URLs de integración vienen de neo.yaml.
type IntegrationsConfig struct {
	PLCEndpoint      string `yaml:"plc_endpoint"`       // Endpoint del PLC para e-stop
	ERPEndpoint      string `yaml:"erp_endpoint"`       // Endpoint ERP para sincronización
	ChaosDrillTarget string `yaml:"chaos_drill_target"` // Target por defecto del chaos drill
	HUDBaseURL       string `yaml:"hud_base_url"`       // URL base del HUD web (neo-hud)
	SandboxBaseURL   string `yaml:"sandbox_base_url"`   // URL base del servidor táctico interno
}

type WorkspaceConfig struct {
	IgnoreDirs        []string          `yaml:"ignore_dirs"`
	AllowedExtensions []string          `yaml:"allowed_extensions"`
	MaxFileSizeMB     int               `yaml:"max_file_size_mb"`
	Modules           map[string]string `yaml:"modules"`       // [SRE-26.1.1] subdir -> build command
	DominantLang      string            `yaml:"dominant_lang"` // go|python|rust|typescript; overridable by ProjectConfig
	// CanonicalID is the cross-machine stable identifier of this workspace.
	// When set, it overrides the auto-resolution in pkg/brain/identity.go
	// (git remote walk-up → project_name → path-hash fallback). Useful when
	// the auto-resolved value is wrong (forks, monorepo subtrees, mirrors).
	// Format: free-form string; convention is `<host>/<owner>/<repo>` or
	// `project:<name>:<basename>` to match the resolver's output. [135.A.1]
	CanonicalID string `yaml:"canonical_id,omitempty"`

	// Scope declares the workspace's primary work domain. Reserved values:
	// "fullstack" (default — load all rules + skills, current behavior),
	// "backend" (Go MCP code / pkg/cmd-only — frontend rules irrelevant),
	// "frontend" (web/, UI only — DB doctrine + gosec irrelevant),
	// "infra" (Nexus/PKI/SCADA — code-quality + DB skills not central),
	// "db" (migrations + dba — code-quality lite, but DB doctrine essential).
	//
	// Today this field is INFORMATIONAL — surfaced in BRIEFING for context,
	// used by future scope-aware rule injection (ADR-015) that will let
	// SessionStart hook selectively load only the relevant skills/docs.
	// See [CONFIG-FIELD-BACKFILL-RULE] in directives. [358.A]
	Scope string `yaml:"scope,omitempty"`
}

type DatabaseConfig struct {
	Name         string `yaml:"name"`
	Driver       string `yaml:"driver"`
	DSN          string `yaml:"dsn"`
	MaxOpenConns int    `yaml:"max_open_conns"` // 0 = use default (2)
}

type AIConfig struct {
	Provider            string   `yaml:"provider"`
	BaseURL             string   `yaml:"base_url"`
	EmbedBaseURL        string   `yaml:"embed_base_url"`         // Ollama dedicado para embeddings. Vacío = usa base_url.
	EmbeddingURLs       []string `yaml:"embedding_urls"`          // [303.E] Round-robin pool of Ollama embedding instances. Overrides EmbedBaseURL when non-empty.
	EmbeddingModel      string   `yaml:"embedding_model"`
	ContextWindow       int      `yaml:"context_window"`
	EmbedTimeoutSeconds int      `yaml:"embed_timeout_seconds"`
	// LocalModel is the default model name passed to neo_local_llm (ADR-013)
	// when args["model"] is not provided. Empty falls back to the tool's
	// internal default ("qwen2.5-coder:7b"), which fits any 8 GB+ GPU and
	// 16 GB+ system RAM. Operators with ≥64 GB system RAM can set this to
	// "qwen2.5-coder:32b" for higher quality at ~30 s/audit.
	LocalModel string `yaml:"local_model"`
}

type RAGConfig struct {
	DBPath             string `yaml:"db_path"`
	ChunkSize          int    `yaml:"chunk_size"`
	Overlap            int    `yaml:"overlap"`
	BatchSize          int    `yaml:"batch_size"`
	IngestionWorkers   int    `yaml:"ingestion_workers"`
	// OllamaConcurrency caps simultaneous embedder.Embed() calls during bulk
	// indexation (ingestion workers). Prevents HTTP 500 when workers > Ollama's
	// internal queue. Rule of thumb: OLLAMA_NUM_PARALLEL (default 4 in Ollama).
	OllamaConcurrency int `yaml:"ollama_concurrency"`

	// [SRE-97.D] EmbedConcurrency caps concurrent Embed() calls during search
	// (BLAST_RADIUS, SEMANTIC_CODE) — separate from ingestion. Search hits one
	// call per query, indexing bursts N workers simultaneously. Default: 2.
	EmbedConcurrency int `yaml:"embed_concurrency"`

	// [SRE-35] Max HNSW vectors tracked per workspace before capacity warning fires.
	MaxNodesPerWorkspace int `yaml:"max_nodes_per_workspace"`
	// [SRE-35] Fraction [0-1] of MaxNodesPerWorkspace that triggers EventMemoryCapacity (default 0.80).
	WorkspaceCapacityWarnPct float32 `yaml:"workspace_capacity_warn_pct"`
	// [SRE-35] DriftMonitor threshold: EventCognitiveDrift fires when rolling avg distance exceeds this (default 0.45).
	DriftThreshold float32 `yaml:"drift_threshold"`
	// [SRE-36] GC pressure threshold: EventGCPressure fires when NumGC/file exceeds this during ingestion (default 5).
	GCPressureThreshold float64 `yaml:"gc_pressure_threshold"`
	// [SRE-36] Arena pool miss-rate threshold: EventArenaThresh fires when missRate exceeds this (default 0.20).
	ArenaMissRateThreshold float64 `yaml:"arena_miss_rate_threshold"`
	// [PILAR-XXV/170.A] Vector quantization mode for HNSW Graph.Vectors.
	// "float32" (default): full precision, 3 KB per 768-d vector.
	// "int8": symmetric quantization with per-vector scale factor, 768 B
	// per vector (4× less RAM), ~0.5-1% recall loss on semantic search,
	// ~3× faster dot product (int8 arithmetic + compiler auto-vec). Opt-in
	// until the corpus grows past ~100k nodes where RAM begins to matter.
	VectorQuant string `yaml:"vector_quant"`
	// [PILAR-XXV/175] LRU cache capacity for repeated semantic queries.
	// 0 disables the cache. Default 256 covers typical SRE sessions where
	// the same target is queried 5-10× during a debugging loop. At 54 ns
	// per hit vs ~6 µs for a fresh HNSW walk, even a 20% hit rate pays
	// for the 256-entry map (roughly 20 KB RAM).
	QueryCacheCapacity int `yaml:"query_cache_capacity"`
	// [PILAR-XXV/199] LRU cache capacity for embedded query vectors.
	// When a SEMANTIC_CODE query misses the QueryCache but its vector is
	// already embedded, skip the ~30 ms Ollama roundtrip. Each entry is
	// 768 × 4 B ≈ 3 KB; default 128 = 384 KB RAM. 0 disables.
	EmbeddingCacheCapacity int `yaml:"embedding_cache_capacity"`
	// [273.A] MaxEmbedChars truncates input text before sending to Ollama embeddings API.
	// Default nomic-embed-text has a 2048-token context window (~4000 chars safe bound for code).
	// The local nomic-embed-text-8k variant (Modelfile: num_ctx 8192) supports ~24000 chars —
	// set max_embed_chars: 24000 in neo.yaml when using that variant. 0 = no truncation (unsafe).
	MaxEmbedChars int `yaml:"max_embed_chars"`
	// [367.C] HNSW query batching coalescer — amortizes per-query overhead (LockOSThread,
	// SIMD dispatch init, pool fetch) for burst workloads. Default disabled.
	HNSWBatchEnabled  bool `yaml:"hnsw_batch_enabled"`   // default false; enable when burst throughput > 50 qps sustained
	HNSWBatchWindowMS int  `yaml:"hnsw_batch_window_ms"` // coalescing window in ms; 0 = 2ms default
	HNSWBatchMaxSize  int  `yaml:"hnsw_batch_max_size"`  // flush immediately when batch reaches this size; 0 = 32 default
	// [ÉPICA 149 / PILAR XXIX] Fast-boot snapshot of the in-memory HNSW
	// graph. Mirrors PILAR XXXII CPG fast-boot. When the snapshot is
	// fresh, neo-mcp boot reads back the binary blob (~few seconds for a
	// 3 GB graph) instead of cold-rebuilding from the bbolt WAL (~6 min
	// for 3.3 GB). Stale guard via (WAL file size, bbolt Tx.ID) in header.
	HNSWPersistPath            string `yaml:"hnsw_persist_path"`            // default ".neo/db/hnsw.bin"; relative paths joined with workspace
	HNSWPersistIntervalMinutes int    `yaml:"hnsw_persist_interval_minutes"` // default 30; 0 = disable periodic save (only SIGTERM hook saves)
}

type CognitiveConfig struct {
	Strictness  float32 `yaml:"strictness"`
	ArenaSize   int     `yaml:"arena_size"`
	XAIEnabled  bool    `yaml:"xai_enabled"`
	AutoApprove bool    `yaml:"auto_approve"`
}

// CPGConfig controls the Code Property Graph builder and runtime (PILAR XX).
type CPGConfig struct {
	// PageRankIters is the number of PageRank iterations for CodeRank (default 50).
	PageRankIters int `yaml:"page_rank_iters"`
	// PageRankDamping is the damping factor for CodeRank (default 0.85).
	PageRankDamping float64 `yaml:"page_rank_damping"`
	// ActivationAlpha is the energy decay per hop in Spreading Activation (default 0.5).
	ActivationAlpha float64 `yaml:"activation_alpha"`
	// MaxHeapMB is the OOM guard threshold — CPG is discarded if heap exceeds this (default 50).
	MaxHeapMB int `yaml:"max_heap_mb"`
	// PersistPath is the file path for the serialized CPG snapshot (default .neo/db/cpg.bin).
	PersistPath string `yaml:"persist_path"` // [PILAR-XXXII]
	// PersistIntervalMinutes controls how often the CPG is auto-saved (0 = disabled, default 15).
	PersistIntervalMinutes int `yaml:"persist_interval_minutes"` // [PILAR-XXXII]
	// PackagePath is the SSA analysis entry point passed to cpg.Manager.Start (default "./cmd/neo-mcp"). [Épica 314.A]
	PackagePath string `yaml:"package_path"`
	// PackageDir is the filesystem directory for SSA analysis relative to workspace root (default "cmd/neo-mcp"). [Épica 314.A]
	PackageDir string `yaml:"package_dir"`
}

type PKIConfig struct {
	CACertPath     string `yaml:"ca_cert_path"`
	ServerCertPath string `yaml:"server_cert_path"`
	ServerKeyPath  string `yaml:"server_key_path"`
}

type SREConfig struct {
	StrictMode             bool     `yaml:"strict_mode"`
	CusumTarget            float64  `yaml:"cusum_target"`
	CusumThreshold         float64  `yaml:"cusum_threshold"`
	RingBufferSize         int      `yaml:"ring_buffer_size"`
	AutoVacuumInterval     string   `yaml:"auto_vacuum_interval"`
	GamedayBaselineRPS     int      `yaml:"gameday_baseline_rps"`
	TrustedLocalPorts      []int    `yaml:"trusted_local_ports"`     // Localhost ports that bypass SSRF guard
	SafeCommands           []string `yaml:"safe_commands"`           // [SRE-34.3.1] Watchdog auto-approve whitelist
	UnsupervisedMaxCycles  int      `yaml:"unsupervised_max_cycles"` // [SRE-34.3.2] Max auto-approve cycles before reverting
	KineticMonitoring      bool     `yaml:"kinetic_monitoring"`      // [SRE-44] Enable hardware bio-feedback monitoring (default true)
	KineticThreshold       float64  `yaml:"kinetic_threshold"`       // [SRE-44] Max allowed power deviation fraction before alert (default 0.15 = 15%)
	DigitalTwinTesting     bool     `yaml:"digital_twin_testing"`    // [SRE-49] Enable digital twin load mirroring in chaos drill (default false)
	ConsensusEnabled            bool     `yaml:"consensus_enabled"`             // [SRE-48] Enable multi-agent consensus voting (default false)
	ConsensusQuorum             float64  `yaml:"consensus_quorum"`              // [SRE-48] Fraction of models that must agree to proceed (default 0.66)
	ContextCompressThresholdKB  int      `yaml:"context_compress_threshold_kb"` // [SRE-58.1] Proactive compress suggestion threshold in KB (default 600)
	OracleAlertThreshold        float64  `yaml:"oracle_alert_threshold"`        // [SRE-61] OracleEngine: emit alert when FailProb24h ≥ this (default 0.75)
	OracleHeapLimitMB           float64  `yaml:"oracle_heap_limit_mb"`          // [SRE-61] OracleEngine: heap saturation ceiling in MB (default 512)
	OraclePowerLimitW           float64  `yaml:"oracle_power_limit_w"`          // [SRE-61] OracleEngine: thermal saturation ceiling in Watts (default 80)
	CertifyTTLMinutes           int      `yaml:"certify_ttl_minutes"`           // [SRE-76.2] Seal TTL for pre-commit hook. 0 = mode default (pair:15, fast:5)
	SessionStateTTLHours        int      `yaml:"session_state_ttl_hours"`       // [SRE-108.B] Vacuum_Memory purges session_state entries older than this (default 24)
	TokenBudgetSessionWarn      int      `yaml:"token_budget_session_warn"`     // [312.A] emit EventSuggestCompress when session output tokens exceed this (default 50000)
	TokenBudgetSessionHard      int      `yaml:"token_budget_session_hard"`     // [312.A] add ⚠️ SESSION_BUDGET_EXCEEDED hint to BRIEFING (default 100000)
	INCArchiveDays              int      `yaml:"inc_archive_days"`              // [Épica 330.C] Move .neo/incidents/INC-*.md older than N days to .neo/incidents/archive/ at boot. 0 = disabled. Default 30.
	RuntimeMemoryLimitMB        int      `yaml:"runtime_memory_limit_mb"`       // [Épica 365.A] Soft cap for Go runtime heap via debug.SetMemoryLimit. 0 = uncapped (default). Recommended: 4096 (4GB) for production. Not to be confused with cpg.max_heap_mb (per-subsystem).
	InboxQuotaPerSenderPerHour  int      `yaml:"inbox_quota_per_sender_per_hour"` // [Épica 331.A] Max inbox messages from a single sender workspace per rolling hour. 0 = default 30.
	PeerSessionMirrorCap        int      `yaml:"peer_session_mirror_cap"`         // [Épica 335.A] Max certified mutations stored per peer workspace in peer_session_state. 0 = default 50.
	PGOCaptureIntervalMinutes   int      `yaml:"pgo_capture_interval_minutes"`    // [PILAR LXIX / 364.C] Continuous pprof capture for PGO refresh. 0 = disabled (default), recommended 60. Writes to .neo/pgo/profile-<unix>.pgo; rotates files older than 24h.
	DebtAuditLog                bool     `yaml:"debt_audit_log"`                  // [361.A] Append JSON resolution records to .neo/db/debt_resolution_log.jsonl on every resolve. Default false.
	CPUAffinityEnabled          bool     `yaml:"cpu_affinity_enabled"`            // [367.A] Pin HNSW search goroutines to dedicated CPU cores via SchedSetaffinity (Linux only). Default false.
	CPUAffinityCores            []int    `yaml:"cpu_affinity_cores"`              // [367.A] Ordered list of CPU core IDs for round-robin assignment. Empty = [0,1,2,3] when cpu_affinity_enabled is true.
	TaskOrphanTimeoutMin        int      `yaml:"task_orphan_timeout_min"`         // [362.A] Minutes before an in_progress delegate task is classified as orphaned. 0 = default 60.
	BriefingHTTPTimeoutMs       int      `yaml:"briefing_http_timeout_ms"`        // [MCPI-46] Per-call timeout for parallel BRIEFING HTTP gathering (presence, plugins, nexus-debt, ollama). 0 = default 500ms.
	DaemonMaxRetries            int      `yaml:"daemon_max_retries"`              // [132.A] Max orphan-recovery retries before a task is marked failed_permanent. 0 = default 3.
	DaemonCheckpointTTLHours    int      `yaml:"daemon_checkpoint_ttl_hours"`     // [132.A] Stale task checkpoint TTL in hours for cross-session recovery. 0 = default 24.
	DaemonTokenBudgetPerTask    int      `yaml:"daemon_token_budget_per_task"`    // [132.B] Max tokens a single task may consume before it is skipped. 0 = default 20000.
	DaemonTokenBudgetSession    int      `yaml:"daemon_token_budget_session"`     // [132.B] Session-wide token cap; emits EventDaemonBudgetWarning at 90%. 0 = default 200000.
	DaemonAutoRecertify         bool     `yaml:"daemon_auto_recertify_before_commit"` // [132.D] Re-certify stale seals (<60s TTL remaining) before each git commit. Default true.
	ThermalPressureCheck        bool     `yaml:"thermal_pressure_check"`              // [132.E] Use sre.ThermalPressure() in homeostasis loop (darwin: sysctl/powermetrics; linux: /sys/class/thermal). Default true.
	DaemonBackendMode           string   `yaml:"daemon_backend_mode"`                 // [132.F] "auto"|"deepseek"|"claude" — task backend routing policy. Default "auto".
	MaxDirectiveChars           int      `yaml:"max_directive_chars"`                 // [374.A] Max chars per directive text. 0 = default 500.
	MaxDirectives               int      `yaml:"max_directives"`                      // [374.B] Max total active directives. 0 = default 60.
	DeepseekPreCertify          string   `yaml:"deepseek_pre_certify"`                // [371.A] "auto"|"manual"|"off". auto=invoke DS before AST on hot-path. manual=advisory only. off=silent. Default "manual".
	DeepseekHotPaths            []string `yaml:"deepseek_hot_paths"`                  // [371.B] Glob patterns for hot-path files that trigger DS pre-certify. Default: crypto, storage, auth, nexus core.
	DeepseekBlockSeverity       int      `yaml:"deepseek_block_severity"`             // [371.D] DS findings >= this SEV fail certification. 0 = default 9.
	ReadSliceAdvisoryOff        bool     `yaml:"read_slice_advisory_off"`             // [372.C] Disable FILE_EXTRACT advisory on READ_SLICE results. Default false (advisory enabled).
	ToolNudgesOff               bool     `yaml:"tool_nudges_off"`                     // [373.D] Disable underutilized tool suggestions in BRIEFING full mode. Default false (nudges enabled).
}

// InferenceConfig controls the 4-level inference router (LOCAL/OLLAMA/HYBRID/CLOUD).
// [SRE-34.2.1] Zero-Hardcoding: all model names and budget from neo.yaml.
type InferenceConfig struct {
	CloudTokenBudgetDaily int     `yaml:"cloud_token_budget_daily"` // [SRE-34] Hard daily token cap for CLOUD tier
	CloudModel            string  `yaml:"cloud_model"`              // Model identifier for CLOUD escalation
	OllamaModel           string  `yaml:"ollama_model"`             // Local model for OLLAMA/HYBRID tiers
	OllamaBaseURL         string  `yaml:"ollama_base_url"`          // Defaults to ai.base_url if empty
	ConfidenceThreshold   float32 `yaml:"confidence_threshold"`  // Below this → escalate to next level (default 0.7)
	MaxLocalAttempts      int     `yaml:"max_local_attempts"`    // Retries on LOCAL/OLLAMA before escalating (default 3)
	OfflineMode           bool    `yaml:"offline_mode"`          // If true, never escalates to CLOUD tier (default false)
	DebtFile              string  `yaml:"debt_file"`             // Path to technical debt backlog markdown (default "technical_debt_backlog.md")
	Mode                  string  `yaml:"mode"`                  // Routing preference: "local", "hybrid", "cloud" (default "hybrid")
	SurrenderAfter        int     `yaml:"surrender_after"`       // [SRE-51] OLLAMA failures before graceful surrender to debt file (default 3)
	MaxAutoFixAttempts    int     `yaml:"max_auto_fix_attempts"` // [SRE-86.B] Auto-fix retry count (0=disabled, >0=auto-retry with inference fix)
	// [PILAR-XXVII/243.E] Cost per million tokens for each known model.
	// Used by observability.Store to convert token counts into USD. Local
	// Ollama models should map to {0,0}. Cloud models (claude-*, gpt-*,
	// gemini-*) should carry vendor pricing. Keys are matched verbatim —
	// the caller is responsible for passing the exact model identifier.
	CostTable map[string]CostEntry `yaml:"cost_table,omitempty"`

	// [PILAR-XXVII/245.Q] Agent-prefix → default model mapping. When the
	// MCP initialize handshake reports clientInfo.name@version, we rarely
	// know the underlying LLM (Claude Code can run Opus / Sonnet / Haiku
	// depending on the user's plan). Operators map the stable prefix of
	// the agent to a representative model so cost accounting is non-zero
	// without per-version maintenance. Matched longest-prefix-first.
	// Example:
	//   agent_model_map:
	//     claude-code: claude-opus-4-7
	//     gemini-cli:  gemini-1.5-pro
	AgentModelMap map[string]string `yaml:"agent_model_map,omitempty"`
}

// CostEntry expresses vendor pricing per model in USD-per-million-tokens.
type CostEntry struct {
	InputPerMTok  float64 `yaml:"input_per_mtok"`
	OutputPerMTok float64 `yaml:"output_per_mtok"`
}

// SentinelConfig governs the policy engine (Épica 40) and dreaming (Épica 41). [SRE-40/41]
type SentinelConfig struct {
	HeapThresholdMB           int     `yaml:"heap_threshold_mb"`            // Deny intensive actions above this (default 500)
	GoroutineExplosionLimit   int     `yaml:"goroutine_explosion_limit"`    // Max goroutines before guard triggers (default 10000)
	ColdStartGraceSec         int     `yaml:"cold_start_grace_sec"`         // No auto-approve during first N seconds (default 30)
	AuditLogMaxSize           int     `yaml:"audit_log_max_size"`           // Max audit entries before pruning (default 1000)
	DreamCycleCount           int     `yaml:"dream_cycle_count"`            // Dreams per REM sleep (default 3)
	ImmunityConfidenceInit    float64 `yaml:"immunity_confidence_init"`     // Initial confidence for new immune entries (default 0.6)
	ImmunityActivationMin     float64 `yaml:"immunity_activation_min"`      // Min confidence to activate immunity (default 0.5)
}

// DefaultSentinelConfig returns a SentinelConfig with safe production defaults.
// Use when a SentinelConfig is needed outside of a full neo.yaml load (e.g. Nexus plugin policy engine).
func DefaultSentinelConfig() SentinelConfig {
	return SentinelConfig{
		HeapThresholdMB:         500,
		GoroutineExplosionLimit: 10000,
		ColdStartGraceSec:       30,
		AuditLogMaxSize:         1000,
		DreamCycleCount:         3,
		ImmunityConfidenceInit:  0.6,
		ImmunityActivationMin:   0.5,
	}
}

// KineticConfig controls hardware bio-feedback (Épica 44). [SRE-44]
type KineticConfig struct {
	SpectralBins          int     `yaml:"spectral_bins"`            // DFT frequency bins (default 8)
	AnomalyThresholdSigma float64 `yaml:"anomaly_threshold_sigma"`  // Sigma threshold for anomaly detection (default 2.0)
	GCPauseThresholdUs    int     `yaml:"gc_pause_threshold_us"`    // GC pause threshold in microseconds (default 10000)
	HeapCriticalSigma     float64 `yaml:"heap_critical_sigma"`      // Sigma for critical heap action (default 5.0)
	HeapWarningSigma      float64 `yaml:"heap_warning_sigma"`       // Sigma for warning heap action (default 3.0)
}

// ColdstoreConfig controls the OLAP cold storage engine (Épica 38). [SRE-38]
type ColdstoreConfig struct {
	MaxOpenConns      int `yaml:"max_open_conns"`      // SQLite max open connections (default 3)
	MaxIdleConns      int `yaml:"max_idle_conns"`      // SQLite max idle connections (default 2)
	DefaultQueryLimit int `yaml:"default_query_limit"` // Default LIMIT for analytics queries (default 50)
}

// HyperGraphConfig controls the multidimensional relationship engine (Épica 42). [SRE-42]
type HyperGraphConfig struct {
	MaxImpactDepth   int     `yaml:"max_impact_depth"`    // Max BFS hops for butterfly analysis (default 5)
	RiskDecayFactor  float64 `yaml:"risk_decay_factor"`   // Risk decay per hop (default 0.7)
	MinRiskThreshold float64 `yaml:"min_risk_threshold"`  // Cutoff for risk propagation (default 0.01)
}


// GovernanceConfig controls autonomous governance, quarantine, and ghost-mode policies.
type GovernanceConfig struct {
	AutoYesSafeCommands bool   `yaml:"auto_yes_safe_commands"` // Auto-approve commands matching safe_commands whitelist (default false)
	QuarantineEnforced  bool   `yaml:"quarantine_enforced"`    // Enforce IP quarantine via eBPF on QUARANTINE_IP signal (default true)
	GhostModeMaxCycles  int    `yaml:"ghost_mode_max_cycles"`  // Max autonomous cycles before forced checkpoint (default 50)
	GhostMode           bool   `yaml:"ghost_mode"`             // [SRE-50] Enable full unsupervised ghost mode (default false)
	ConstitutionPath    string `yaml:"constitution_path"`      // [SRE-40] Path to Rego/policy file (default ".neo/constitution.rego")
}

// [PILAR-XXVIII/248.D] ShadowConfig removed here — the live shadow
// traffic mirroring lives in pkg/nexus.NexusConfig.Shadow (nexus.yaml).
// The neo.yaml shadow: block never had runtime consumers.

// [SRE-94] Federated Dream Synthesis config.
type FederationConfig struct {
	DreamSchedule      string  `yaml:"dream_schedule"` // cron expression
	DedupThreshold     float64 `yaml:"dedup_threshold"`
	ManifestBucket     string  `yaml:"manifest_bucket"`
	MaxVectorsPerNode  int     `yaml:"max_vectors_per_node"`
	HarvestTimeoutSec  int     `yaml:"harvest_timeout_sec"`
}

// [SRE-95] Voice of the Leviathan config.
type LLMConfig struct {
	OllamaURL     string            `yaml:"ollama_url"`
	Model         string            `yaml:"model"`
	MaxTokens     int               `yaml:"max_tokens"`
	Temperature   float64           `yaml:"temperature"`
	LlamaCppPath  string            `yaml:"llamacpp_path"`
	GGUFPath      string            `yaml:"gguf_path"`
	Aliases       map[string]string `yaml:"aliases"`
}

// AuthConfig carries runtime identity loaded from ~/.neo/credentials.json.
// Never persisted to neo.yaml (yaml:"-"). [PILAR-XXXIII]
type AuthConfig struct {
	TenantID string
}

// HardwareConfig controls GPU detection and adaptive parameter tuning.
// [GPU-AWARE] gpu_available:"auto" probes nvidia-smi at boot; "true"/"false"
// force-override detection (useful in containers where the tool may be absent).
type HardwareConfig struct {
	// GPUAvailable controls whether GPU is assumed present.
	// "auto" (default): probe nvidia-smi at boot.
	// "true"/"false": explicit override.
	GPUAvailable string `yaml:"gpu_available"`
	// When GPU is detected these override the base RAG/inference values.
	// Zero/empty = keep base values unchanged.
	GPUOllamaModel      string `yaml:"gpu_ollama_model"`      // overrides inference.ollama_model when GPU present
	GPUEmbedConcurrency int    `yaml:"gpu_embed_concurrency"` // overrides rag.embed_concurrency (default 8)
	GPUBatchSize        int    `yaml:"gpu_batch_size"`        // overrides rag.batch_size (default 400)
}

// DeepSeekConfig holds configuration for the DeepSeek plugin (PILAR XXIV).
type DeepSeekConfig struct {
	CacheTTLSeconds        int   `yaml:"cache_ttl_seconds"`             // [131.C] Block1 structural cache TTL. 0 = default 3600s.
	MaxBlock1Chars         int   `yaml:"max_block1_chars"`              // [131.C] Max chars for Block1 static context. 0 = default 80000.
	RateLimitTPM           int64 `yaml:"rate_limit_tokens_per_minute"`  // [131.B] Token-bucket refill rate. 0 = default 60000 TPM.
	Burst                  int64 `yaml:"burst"`                         // [131.B] Token-bucket burst capacity. 0 = default 10000.
	ContextPressureTokens  int   `yaml:"context_pressure_tokens"`         // [131.E] Auto-distill threshold. 0 = default 30000.
	SessionTTLMinutes      int   `yaml:"session_ttl_minutes"`             // [131.E] Thread idle TTL. 0 = default 15min.
	SessionReaperIntervalM int   `yaml:"session_reaper_interval_minutes"` // [131.E] Reaper goroutine interval. 0 = default 5min.
	ChunkSizeTokens        int   `yaml:"chunk_size_tokens"`               // [131.F] LineChunker window size. 0 = default 2000.
	MaxTokensPerSession    int64 `yaml:"max_tokens_per_session"`          // [131.J] Session billing circuit breaker. 0 = default 500000.

	// Default model + thinking parameters for the plugin's calls. Per-call
	// overrides via the tool args still take precedence. Empty values fall
	// through to client.go defaults / server-side per-model defaults.
	// [PILAR-XXIV / Phase 4 audit fix — 2026-05-01]
	DefaultModel           string `yaml:"default_model"`            // "deepseek-v4-flash" (default) | "deepseek-v4-pro"
	DefaultThinkingType    string `yaml:"default_thinking_type"`    // "enabled" | "disabled" (empty = server default)
	DefaultReasoningEffort string `yaml:"default_reasoning_effort"` // "high" | "max" (empty = server default = "high")
}

type NeoConfig struct {
	SRE          SREConfig          `yaml:"sre"`

	Server       ServerConfig       `yaml:"server"`
	Workspace    WorkspaceConfig    `yaml:"workspace"`
	AI           AIConfig           `yaml:"ai"`
	RAG          RAGConfig          `yaml:"rag"`
	Cognitive    CognitiveConfig    `yaml:"cognitive"`
	PKI          PKIConfig          `yaml:"pki"`
	Integrations IntegrationsConfig `yaml:"integrations"`
	Inference    InferenceConfig    `yaml:"inference"`   // [SRE-34.2.1] 4-level inference router
	Sentinel     SentinelConfig     `yaml:"sentinel"`    // [SRE-40/41] Policy engine + dreaming
	Kinetic      KineticConfig      `yaml:"kinetic"`     // [SRE-44] Hardware bio-feedback
	Coldstore    ColdstoreConfig    `yaml:"coldstore"`   // [SRE-38] OLAP cold storage
	HyperGraph   HyperGraphConfig   `yaml:"hypergraph"`  // [SRE-42] Multidimensional relations
	Governance   GovernanceConfig   `yaml:"governance"`  // Autonomous governance policies
	Federation   FederationConfig   `yaml:"federation"`  // [SRE-94] Federated dream synthesis
	LLM          LLMConfig          `yaml:"llm"`         // [SRE-95] Local LLM for offline ops
	CPG          CPGConfig          `yaml:"cpg"`         // [PILAR-XX] Code Property Graph builder
	Hardware     HardwareConfig     `yaml:"hardware"`    // [GPU-AWARE] GPU detection + adaptive params
	DeepSeek     DeepSeekConfig     `yaml:"deepseek"`    // [PILAR-XXIV] DeepSeek plugin config
	Databases    []DatabaseConfig   `yaml:"databases"`
	Auth         AuthConfig         `yaml:"-"`           // [PILAR-XXXIII] populated at runtime from credentials.json; not persisted
	Project      *ProjectConfig     `yaml:"-"`           // [PILAR-XXXI] populated at runtime from .neo-project/neo.yaml; not persisted
	Org          *OrgConfig         `yaml:"-"`           // [PILAR-LXVII / 354.A] populated at runtime from .neo-org/neo.yaml walk-up; not persisted
}

func defaultNeoConfig() *NeoConfig {
	return &NeoConfig{
		Server: ServerConfig{
			LogLevel:        "info",
			Host:            "127.0.0.1",
			Port:            8080,
			SandboxPort:     8081,
			TacticalPort:    8084,
			DiagnosticsPort: 6060,
			SREListenerPort: 8082,
			SSEPort:         8085,
			SSEPath:         "/mcp/sse",
			SSEMessagePath:  "/mcp/message",
			Mode:            "pair",
			Tailscale:       false,
			GossipPeers:     []string{},
			GossipPort:          8086,
			DashboardPort:       8087,
			NexusDispatcherPort: 9000,
			ToolTimeoutSeconds:  120,
		},
		Integrations: IntegrationsConfig{
			PLCEndpoint:      "http://localhost:9091/plc/stop",
			ERPEndpoint:      "http://localhost:9090/erp/ingest",
			ChaosDrillTarget: "http://127.0.0.1:8084/health",
			HUDBaseURL:       "http://localhost:8080",
			SandboxBaseURL:   "http://127.0.0.1:8084",
		},
		Workspace: WorkspaceConfig{
			IgnoreDirs: []string{
				"node_modules", "vendor", ".git", "dist", ".neo",
				// Python virtual environments and package caches — never index these
				"lib", "site-packages", "__pycache__", "venv", ".venv", "env",
			},
			AllowedExtensions: []string{".go", ".ts", ".js", ".py", ".md", ".rs", ".html", ".css", ".yaml"},
			MaxFileSizeMB:     5,
			Modules:           map[string]string{"web": "npm run build"}, // [SRE-26.1.2]
			// [358.A] Scope is intentionally NOT defaulted here — backfill in
			// applyWorkspaceDefaults handles it. Setting default in struct literal
			// would prevent backfill from triggering needsSave=true, so the
			// write-back to neo.yaml would never fire and the field would never
			// be persisted to disk for operators to see. See ADR-015 + bug audit
			// 2026-05-13 post-rebuild validation.
		},
		AI: AIConfig{
			Provider:            "ollama",
			BaseURL:             "http://localhost:11434",
			EmbeddingModel:      "nomic-embed-text",
			ContextWindow:       8192,
			EmbedTimeoutSeconds: 8,
		},
		RAG: RAGConfig{
			DBPath:                   ".neo/db/hnsw.db",
			ChunkSize:                3000,
			Overlap:                  500,
			BatchSize:                100,
			IngestionWorkers:         4,
			OllamaConcurrency:        3,
			EmbedConcurrency:         2,
			MaxNodesPerWorkspace:     50000,
			WorkspaceCapacityWarnPct: 0.80,
			DriftThreshold:           0.45,
			GCPressureThreshold:      5.0,
			ArenaMissRateThreshold:   0.20,
			VectorQuant:              "float32",
			QueryCacheCapacity:       256,
			EmbeddingCacheCapacity:   128,
			MaxEmbedChars:            4000,
			// [ÉPICA 149] HNSW fast-boot snapshot defaults — see comment at field decl.
			HNSWPersistPath:            ".neo/db/hnsw.bin",
			HNSWPersistIntervalMinutes: 30,
		},
		Cognitive: CognitiveConfig{
			Strictness:  0.75,
			ArenaSize:   100000,
			XAIEnabled:  true,
			AutoApprove: false,
		},
		PKI: PKIConfig{
			CACertPath:     ".neo/pki/ca.crt",
			ServerCertPath: ".neo/pki/server.crt",
			ServerKeyPath:  ".neo/pki/server.key",
		},
		SRE: SREConfig{
			StrictMode:            true,
			CusumTarget:           0.70,
			CusumThreshold:        0.15,
			RingBufferSize:        150,
			AutoVacuumInterval:    "5m",
			GamedayBaselineRPS:    5000,
			TrustedLocalPorts:     []int{11434, 11435, 8085, 8080, 8081, 6060},
			SafeCommands:          defaultSafeCommands(),
			UnsupervisedMaxCycles: 10,
			KineticMonitoring:     true,
			KineticThreshold:      0.15,
			DigitalTwinTesting:    false,
			ConsensusEnabled:           false,
			ConsensusQuorum:            0.66,
			ContextCompressThresholdKB: 600,
			OracleAlertThreshold:       0.75,
			OracleHeapLimitMB:          512.0,
			OraclePowerLimitW:          80.0,
			CertifyTTLMinutes:          0, // 0 = mode default (pair:15, fast:5)
			SessionStateTTLHours:       24,
			TokenBudgetSessionWarn:     50000,
			TokenBudgetSessionHard:     100000,
			INCArchiveDays:             30,
			TaskOrphanTimeoutMin:       60,  // [362.A] 60 minutes default orphan window
			BriefingHTTPTimeoutMs:      500, // [MCPI-46] 500ms per parallel HTTP call
			DaemonAutoRecertify:        true, // [132.D] enabled by default
			ThermalPressureCheck:       true, // [132.E] enabled by default
		},
		Inference: InferenceConfig{
			CloudTokenBudgetDaily: 50000,
			CloudModel:            "claude-sonnet-4-6",
			OllamaModel:           "qwen2:0.5b",
			OllamaBaseURL:         "", // falls back to ai.base_url
			ConfidenceThreshold:   0.70,
			MaxLocalAttempts:      3,
			OfflineMode:           false,
			DebtFile:              "technical_debt_backlog.md",
			Mode:                  "hybrid",
			SurrenderAfter:        3,
			MaxAutoFixAttempts:    0, // [SRE-86.B] disabled by default; set >0 to enable inference auto-fix
			CostTable:             defaultCostTable(),
			AgentModelMap:         defaultAgentModelMap(),
		},
		Sentinel: SentinelConfig{
			HeapThresholdMB:         500,
			GoroutineExplosionLimit: 10000,
			ColdStartGraceSec:       30,
			AuditLogMaxSize:         1000,
			DreamCycleCount:         3,
			ImmunityConfidenceInit:  0.6,
			ImmunityActivationMin:   0.5,
		},
		Kinetic: KineticConfig{
			SpectralBins:          8,
			AnomalyThresholdSigma: 2.0,
			GCPauseThresholdUs:    10000,
			HeapCriticalSigma:     5.0,
			HeapWarningSigma:      3.0,
		},
		Coldstore: ColdstoreConfig{
			MaxOpenConns:      3,
			MaxIdleConns:      2,
			DefaultQueryLimit: 50,
		},
		HyperGraph: HyperGraphConfig{
			MaxImpactDepth:   5,
			RiskDecayFactor:  0.7,
			MinRiskThreshold: 0.01,
		},
		Governance: GovernanceConfig{
			AutoYesSafeCommands: false,
			QuarantineEnforced:  true,
			GhostModeMaxCycles:  50,
			GhostMode:           false,
			ConstitutionPath:    ".neo/constitution.rego",
		},
		Federation: FederationConfig{
			DreamSchedule:     "0 3 * * *",
			DedupThreshold:    0.92,
			ManifestBucket:    "dream_manifests",
			MaxVectorsPerNode: 500,
			HarvestTimeoutSec: 300,
		},
		LLM: LLMConfig{
			OllamaURL:   "http://localhost:11434",
			Model:       "llama3.2:3b",
			MaxTokens:   2048,
			Temperature: 0.3,
			Aliases: map[string]string{
				"status": `neo_radar(intent: "BRIEFING", mode: "compact")`,
				"chaos":  `neo_chaos_drill(target: "$1", aggression_level: 5)`,
			},
		},
		DeepSeek: DeepSeekConfig{
			CacheTTLSeconds:        3600,  // [131.C] Block1 cache TTL 1h
			MaxBlock1Chars:         80000, // [131.C] ~20K tokens of static context
			RateLimitTPM:           60000, // [131.B] DeepSeek default API limit
			Burst:                  10000, // [131.B] burst capacity
			ContextPressureTokens:  30000, // [131.E] auto-distill at 30K tokens
			SessionTTLMinutes:      15,    // [131.E] 15min idle TTL
			SessionReaperIntervalM: 5,     // [131.E] reaper every 5min
			ChunkSizeTokens:        2000,  // [131.F] line chunker window size
			// [Phase 4 audit fix] Canonical model + thinking defaults. Empty thinking
			// fields use the server's per-model default (which today is enabled/high
			// for v4-flash and v4-pro). Operator explicit override goes via neo.yaml.
			DefaultModel:           "deepseek-v4-flash",
			DefaultThinkingType:    "",
			DefaultReasoningEffort: "",
		},
		Databases: []DatabaseConfig{},
	}
}

// applyServerDefaults backfills Server, Integrations, SRE.AutoVacuumInterval and Storage.Engine fields.
func applyServerDefaults(cfg *NeoConfig, ns *bool) {
	if cfg.Server.Host == "" {
		cfg.Server.Host = "0.0.0.0"
		*ns = true
	}
	if cfg.Server.SandboxPort == 0 {
		cfg.Server.SandboxPort = 8081
		*ns = true
	}
	if cfg.Server.SSEPort == 0 {
		cfg.Server.SSEPort = 8085
		*ns = true
	}
	if cfg.Server.SSEPath == "" {
		cfg.Server.SSEPath = "/mcp/sse"
		*ns = true
	}
	if cfg.Server.SSEMessagePath == "" {
		cfg.Server.SSEMessagePath = "/mcp/message"
		*ns = true
	}
	if cfg.Server.TacticalPort == 0 {
		cfg.Server.TacticalPort = 8084
		*ns = true
	}
	if cfg.Server.DiagnosticsPort == 0 {
		cfg.Server.DiagnosticsPort = 6060
		*ns = true
	}
	if cfg.Server.SREListenerPort == 0 {
		cfg.Server.SREListenerPort = 8082
		*ns = true
	}
	if cfg.Server.Mode == "" {
		cfg.Server.Mode = "pair"
		*ns = true
	}
	// [SRE-27.1.1] Gossip P2P backfill
	if cfg.Server.GossipPort == 0 {
		cfg.Server.GossipPort = 8086
		*ns = true
	}
	// [SRE-32.1.2] Dashboard HUD port backfill
	if cfg.Server.DashboardPort == 0 {
		cfg.Server.DashboardPort = 8087
		*ns = true
	}
	if cfg.Server.NexusDispatcherPort == 0 {
		cfg.Server.NexusDispatcherPort = 9000
		*ns = true
	}
	// [PILAR-XXIII] Tool call timeout backfill
	if cfg.Server.ToolTimeoutSeconds == 0 {
		cfg.Server.ToolTimeoutSeconds = 120
		*ns = true
	}
}

// applyIntegrationDefaults backfills Integrations endpoints, SRE scheduler, and Storage engine.
func applyIntegrationDefaults(cfg *NeoConfig, ns *bool) {
	if cfg.Integrations.PLCEndpoint == "" {
		cfg.Integrations.PLCEndpoint = "http://localhost:9091/plc/stop"
		*ns = true
	}
	if cfg.Integrations.ERPEndpoint == "" {
		cfg.Integrations.ERPEndpoint = "http://localhost:9090/erp/ingest"
		*ns = true
	}
	if cfg.Integrations.ChaosDrillTarget == "" {
		cfg.Integrations.ChaosDrillTarget = "http://127.0.0.1:8084/health"
		*ns = true
	}
	if cfg.Integrations.HUDBaseURL == "" {
		cfg.Integrations.HUDBaseURL = "http://localhost:8080"
		*ns = true
	}
	if cfg.Integrations.SandboxBaseURL == "" {
		cfg.Integrations.SandboxBaseURL = "http://127.0.0.1:8084"
		*ns = true
	}
	if cfg.SRE.AutoVacuumInterval == "" {
		cfg.SRE.AutoVacuumInterval = "5m"
		*ns = true
	}
}

// scaledCacheCapacity returns an auto-scaled RAG cache capacity proportional to
// MaxNodesPerWorkspace. capacity = maxNodes / divisor, floored at minCap. For
// small workspaces (≤50k nodes default) returns minCap; for 1M+ nodes scales
// linearly (e.g. 1M/500 = 2000 query-cache entries). Operators can override by
// setting an explicit query_cache_capacity / embedding_cache_capacity in neo.yaml.
//
// Heuristic chosen empirically: query divisor=500 (0.2% of max_nodes), embed
// divisor=1000 (0.1%) — balances RAM footprint against hit-rate degradation
// observed when capacity << index size (e.g. strategos at 765k nodes with
// cap=256 query / 128 embed produced ~0% hit ratio).
func scaledCacheCapacity(maxNodes, divisor, minCap int) int {
	scaled := maxNodes / divisor
	if scaled < minCap {
		return minCap
	}
	return scaled
}

// applyRAGDefaults backfills RAG observability and concurrency thresholds.
func applyRAGDefaults(cfg *NeoConfig, ns *bool) {
	// Backfill OllamaConcurrency for configs that predate this field.
	if cfg.RAG.OllamaConcurrency == 0 {
		cfg.RAG.OllamaConcurrency = 3
		*ns = true
	}
	// [SRE-97.D] Backfill EmbedConcurrency (search path — separate from indexation).
	if cfg.RAG.EmbedConcurrency == 0 {
		cfg.RAG.EmbedConcurrency = 2
		*ns = true
	}
	// [SRE-35/36] Backfill observability thresholds (Zero-Hardcoding).
	if cfg.RAG.MaxNodesPerWorkspace == 0 {
		cfg.RAG.MaxNodesPerWorkspace = 50000
		*ns = true
	}
	if cfg.RAG.WorkspaceCapacityWarnPct == 0 {
		cfg.RAG.WorkspaceCapacityWarnPct = 0.80
		*ns = true
	}
	if cfg.RAG.DriftThreshold == 0 {
		cfg.RAG.DriftThreshold = 0.45
		*ns = true
	}
	if cfg.RAG.GCPressureThreshold == 0 {
		cfg.RAG.GCPressureThreshold = 5.0
		*ns = true
	}
	if cfg.RAG.ArenaMissRateThreshold == 0 {
		cfg.RAG.ArenaMissRateThreshold = 0.20
		*ns = true
	}
	// [PILAR-XXV/170.A] Vector quantization backfill — default is float32 to
	// preserve full precision and avoid silent schema changes for existing
	// deployments. Set to "int8" in neo.yaml to opt into 4× RAM reduction.
	if cfg.RAG.VectorQuant == "" {
		cfg.RAG.VectorQuant = "float32"
		*ns = true
	}
	// [PILAR-XXV/175 + LARGE-PROJECT] Query cache capacity backfill —
	// auto-scales with MaxNodesPerWorkspace via scaledCacheCapacity.
	// 50k nodes → 256 (min), 600k nodes → 1200, 1M nodes → 2000.
	// Set explicit value in neo.yaml to override (any non-zero value sticks).
	// Set to negative in neo.yaml to disable the cache entirely — but bear in
	// mind 0 triggers backfill, so use 1 for "minimal cache" or omit + clear it
	// out-of-band if disabling is the goal.
	if cfg.RAG.QueryCacheCapacity == 0 {
		cfg.RAG.QueryCacheCapacity = scaledCacheCapacity(cfg.RAG.MaxNodesPerWorkspace, 500, 256)
		*ns = true
	}
	// [PILAR-XXV/199 + LARGE-PROJECT] Embedding cache capacity backfill —
	// auto-scales: max_nodes/1000, floor 128. 1M nodes → 1024 entries.
	if cfg.RAG.EmbeddingCacheCapacity == 0 {
		cfg.RAG.EmbeddingCacheCapacity = scaledCacheCapacity(cfg.RAG.MaxNodesPerWorkspace, 1000, 128)
		*ns = true
	}
	// [273.A] Max embed chars backfill — default 4000 (conservative for nomic-embed-text 2048-token default).
	// Workspaces using nomic-embed-text-8k (num_ctx 8192 Modelfile, ~24000 chars) must set max_embed_chars in neo.yaml.
	if cfg.RAG.MaxEmbedChars == 0 {
		cfg.RAG.MaxEmbedChars = 4000
		*ns = true
	}
	// [ÉPICA 149] HNSW fast-boot backfill. Path is auto-defaulted when
	// missing; interval value 0 is preserved (it's an operator opt-out
	// for the periodic save — SIGTERM still saves). New workspaces get
	// the 30-minute default via defaultNeoConfig() + write-back.
	if cfg.RAG.HNSWPersistPath == "" {
		cfg.RAG.HNSWPersistPath = ".neo/db/hnsw.bin"
		*ns = true
	}
}

// applyHardwareDefaults backfills GPU detection mode and GPU-adaptive override defaults.
func applyHardwareDefaults(cfg *NeoConfig, ns *bool) {
	if cfg.Hardware.GPUAvailable == "" {
		cfg.Hardware.GPUAvailable = "auto"
		*ns = true
	}
	if cfg.Hardware.GPUEmbedConcurrency == 0 {
		cfg.Hardware.GPUEmbedConcurrency = 8
		*ns = true
	}
	if cfg.Hardware.GPUBatchSize == 0 {
		cfg.Hardware.GPUBatchSize = 400
		*ns = true
	}
}

// applySREDefaults backfills SRE governance, safety, and oracle thresholds.
func applySREDefaults(cfg *NeoConfig, ns *bool) {
	// [SRE-29.1.2] AST Audit trusted_local_ports backfill
	if len(cfg.SRE.TrustedLocalPorts) == 0 {
		cfg.SRE.TrustedLocalPorts = []int{11434, 11435, 8085, 8080, 8081, 6060}
		*ns = true
	}
	// [SRE-34.3.1] Safe-command whitelist backfill
	if len(cfg.SRE.SafeCommands) == 0 {
		cfg.SRE.SafeCommands = defaultSafeCommands()
		*ns = true
	}
	// [SRE-34.3.2] Unsupervised max cycles backfill
	if cfg.SRE.UnsupervisedMaxCycles == 0 {
		cfg.SRE.UnsupervisedMaxCycles = 10
		*ns = true
	}
	// SRE kinetic + consensus backfill
	if cfg.SRE.KineticThreshold == 0 {
		cfg.SRE.KineticThreshold = 0.15
		*ns = true
	}
	if cfg.SRE.ConsensusQuorum == 0 {
		cfg.SRE.ConsensusQuorum = 0.66
		*ns = true
	}
	if cfg.SRE.ContextCompressThresholdKB == 0 {
		cfg.SRE.ContextCompressThresholdKB = 600
		*ns = true
	}
	applySREOracleAndBudgetDefaults(cfg, ns)
}

func applySREOracleThresholds(cfg *NeoConfig, ns *bool) {
	// [SRE-61] OracleEngine thresholds — zero means not set in yaml.
	if cfg.SRE.OracleAlertThreshold == 0 {
		cfg.SRE.OracleAlertThreshold = 0.75
		*ns = true
	}
	if cfg.SRE.OracleHeapLimitMB == 0 {
		cfg.SRE.OracleHeapLimitMB = 512.0
		*ns = true
	}
	if cfg.SRE.OraclePowerLimitW == 0 {
		cfg.SRE.OraclePowerLimitW = 80.0
		*ns = true
	}
}

func applySRESessionAndTokenDefaults(cfg *NeoConfig, ns *bool) {
	// [SRE-108.B] session_state TTL backfill (default 24h purge in Vacuum_Memory)
	if cfg.SRE.SessionStateTTLHours == 0 {
		cfg.SRE.SessionStateTTLHours = 24
		*ns = true
	}
	// [312.A] Token budget ceiling backfill
	if cfg.SRE.TokenBudgetSessionWarn == 0 {
		cfg.SRE.TokenBudgetSessionWarn = 50000
		*ns = true
	}
	if cfg.SRE.TokenBudgetSessionHard == 0 {
		cfg.SRE.TokenBudgetSessionHard = 100000
		*ns = true
	}
	// [331.A] InboxQuotaPerSenderPerHour: 0 → 30 (caps runaway sender).
	if cfg.SRE.InboxQuotaPerSenderPerHour == 0 {
		cfg.SRE.InboxQuotaPerSenderPerHour = 30
		*ns = true
	}
}

func applySREArchiveAndLifecycleDefaults(cfg *NeoConfig, ns *bool) {
	// [330.C] INC auto-archive: 0 on fresh load means "not set" → default 30 days.
	if cfg.SRE.INCArchiveDays == 0 {
		cfg.SRE.INCArchiveDays = 30
		*ns = true
	}
	// [365.A] RuntimeMemoryLimitMB: intentionally NOT backfilled — 0 = opt-in by operator.
	// [362.A] TaskOrphanTimeoutMin: 0 → default 60 minutes
	if cfg.SRE.TaskOrphanTimeoutMin == 0 {
		cfg.SRE.TaskOrphanTimeoutMin = 60
		*ns = true
	}
	// [MCPI-46] BriefingHTTPTimeoutMs: 0 → default 500ms
	if cfg.SRE.BriefingHTTPTimeoutMs == 0 {
		cfg.SRE.BriefingHTTPTimeoutMs = 500
		*ns = true
	}
}

func applySREDaemonDefaults(cfg *NeoConfig, ns *bool) {
	// [132.A] DaemonMaxRetries: 0 → default 3
	if cfg.SRE.DaemonMaxRetries == 0 {
		cfg.SRE.DaemonMaxRetries = 3
		*ns = true
	}
	// [132.A] DaemonCheckpointTTLHours: 0 → default 24
	if cfg.SRE.DaemonCheckpointTTLHours == 0 {
		cfg.SRE.DaemonCheckpointTTLHours = 24
		*ns = true
	}
	// [132.B] DaemonTokenBudgetPerTask: 0 → default 20000
	if cfg.SRE.DaemonTokenBudgetPerTask == 0 {
		cfg.SRE.DaemonTokenBudgetPerTask = 20000
		*ns = true
	}
	// [132.B] DaemonTokenBudgetSession: 0 → default 200000
	if cfg.SRE.DaemonTokenBudgetSession == 0 {
		cfg.SRE.DaemonTokenBudgetSession = 200000
		*ns = true
	}
	// [132.F] DaemonBackendMode: "" → default "auto"
	if cfg.SRE.DaemonBackendMode == "" {
		cfg.SRE.DaemonBackendMode = "auto"
		*ns = true
	}
	// [374.A] MaxDirectiveChars: 0 → default 500
	if cfg.SRE.MaxDirectiveChars == 0 {
		cfg.SRE.MaxDirectiveChars = 500
		*ns = true
	}
	// [374.B] MaxDirectives: 0 → default 60
	if cfg.SRE.MaxDirectives == 0 {
		cfg.SRE.MaxDirectives = 60
		*ns = true
	}
	// [371.A] DeepseekPreCertify: empty → default "manual"
	if cfg.SRE.DeepseekPreCertify == "" {
		cfg.SRE.DeepseekPreCertify = "manual"
		*ns = true
	}
	// [371.B] DeepseekHotPaths: empty → sensible defaults
	if len(cfg.SRE.DeepseekHotPaths) == 0 {
		cfg.SRE.DeepseekHotPaths = []string{
			"pkg/brain/crypto.go",
			"pkg/brain/storage/*.go",
			"pkg/auth/*.go",
			"pkg/nexus/process_pool.go",
			"cmd/neo-nexus/main.go",
			"cmd/neo-nexus/sse.go",
			"cmd/neo-nexus/plugin_routing.go",
		}
		*ns = true
	}
	// [371.D] DeepseekBlockSeverity: 0 → default 9
	if cfg.SRE.DeepseekBlockSeverity == 0 {
		cfg.SRE.DeepseekBlockSeverity = 9
		*ns = true
	}
}

// applySREOracleAndBudgetDefaults backfills oracle thresholds, session budgets,
// and task lifecycle limits extracted from applySREDefaults to keep CC≤15.
func applySREOracleAndBudgetDefaults(cfg *NeoConfig, ns *bool) {
	applySREOracleThresholds(cfg, ns)
	applySRESessionAndTokenDefaults(cfg, ns)
	applySREArchiveAndLifecycleDefaults(cfg, ns)
	applySREDaemonDefaults(cfg, ns)
}

// applyDeepSeekDefaults backfills DeepSeek plugin config fields. [PILAR XXIV / 131.B-C]
func applyDeepSeekDefaults(cfg *NeoConfig, ns *bool) {
	if cfg.DeepSeek.CacheTTLSeconds == 0 {
		cfg.DeepSeek.CacheTTLSeconds = 3600
		*ns = true
	}
	if cfg.DeepSeek.MaxBlock1Chars == 0 {
		cfg.DeepSeek.MaxBlock1Chars = 80000
		*ns = true
	}
	if cfg.DeepSeek.RateLimitTPM == 0 {
		cfg.DeepSeek.RateLimitTPM = 60000
		*ns = true
	}
	if cfg.DeepSeek.Burst == 0 {
		cfg.DeepSeek.Burst = 10000
		*ns = true
	}
	if cfg.DeepSeek.ContextPressureTokens == 0 {
		cfg.DeepSeek.ContextPressureTokens = 30000
		*ns = true
	}
	if cfg.DeepSeek.SessionTTLMinutes == 0 {
		cfg.DeepSeek.SessionTTLMinutes = 15
		*ns = true
	}
	if cfg.DeepSeek.SessionReaperIntervalM == 0 {
		cfg.DeepSeek.SessionReaperIntervalM = 5
		*ns = true
	}
	if cfg.DeepSeek.ChunkSizeTokens == 0 {
		cfg.DeepSeek.ChunkSizeTokens = 2000
		*ns = true
	}
	if cfg.DeepSeek.MaxTokensPerSession == 0 {
		cfg.DeepSeek.MaxTokensPerSession = 500000
		*ns = true
	}
}

// applyAIDefaults backfills AI provider, URLs and model fields.
func applyAIDefaults(cfg *NeoConfig, ns *bool) {
	if cfg.AI.EmbedBaseURL == "" {
		cfg.AI.EmbedBaseURL = cfg.AI.BaseURL
		*ns = true
	}
	if cfg.AI.EmbedTimeoutSeconds == 0 {
		cfg.AI.EmbedTimeoutSeconds = 8
		*ns = true
	}
	if cfg.AI.Provider == "" {
		cfg.AI.Provider = "ollama"
		*ns = true
	}
	if cfg.AI.BaseURL == "" {
		cfg.AI.BaseURL = "http://localhost:11434"
		*ns = true
	}
	if cfg.AI.EmbeddingModel == "" {
		cfg.AI.EmbeddingModel = "nomic-embed-text"
		*ns = true
	}
	if cfg.AI.ContextWindow == 0 {
		cfg.AI.ContextWindow = 8192
		*ns = true
	}
	// [ADR-013] Backfill LocalModel so the config-watcher round-trip
	// preserves the field — without this, an empty cfg.AI.LocalModel
	// at boot would marshal back as `local_model: ""` and stick to
	// disk, hiding the operator's intent.
	if cfg.AI.LocalModel == "" {
		cfg.AI.LocalModel = "qwen2.5-coder:7b"
		*ns = true
	}
}

// applyInferenceDefaults backfills Inference routing and model selection fields.
func applyInferenceDefaults(cfg *NeoConfig, ns *bool) {
	// [SRE-34.2.1] Inference config backfill
	if cfg.Inference.CloudTokenBudgetDaily == 0 {
		cfg.Inference.CloudTokenBudgetDaily = 50000
		*ns = true
	}
	if cfg.Inference.CloudModel == "" {
		cfg.Inference.CloudModel = "claude-sonnet-4-6"
		*ns = true
	}
	if cfg.Inference.OllamaModel == "" {
		cfg.Inference.OllamaModel = "qwen2:0.5b"
		*ns = true
	}
	if cfg.Inference.ConfidenceThreshold == 0 {
		cfg.Inference.ConfidenceThreshold = 0.70
		*ns = true
	}
	// Inference extended fields backfill
	if cfg.Inference.MaxLocalAttempts == 0 {
		cfg.Inference.MaxLocalAttempts = 3
		*ns = true
	}
	if cfg.Inference.DebtFile == "" {
		cfg.Inference.DebtFile = "technical_debt_backlog.md"
		*ns = true
	}
	// Inference extended backfill
	if cfg.Inference.Mode == "" {
		cfg.Inference.Mode = "hybrid"
		*ns = true
	}
	if cfg.Inference.SurrenderAfter == 0 {
		cfg.Inference.SurrenderAfter = 3
		*ns = true
	}
	// [PILAR-XXVII/243.E] Cost table — always provide sane defaults so the
	// observability Store can compute USD without operator intervention.
	// Users can override per-model in neo.yaml.
	if cfg.Inference.CostTable == nil {
		cfg.Inference.CostTable = defaultCostTable()
		*ns = true
	}
	if cfg.Inference.AgentModelMap == nil {
		cfg.Inference.AgentModelMap = defaultAgentModelMap()
		*ns = true
	}
}

// defaultAgentModelMap returns the canonical agent-prefix → model map
// used when neo.yaml does not supply one. The identifiers on the left
// are the stable prefix of the MCP clientInfo.name handshake; the
// values are the pricing model used by CostForModel.
// [PILAR-XXVII/245.Q]
func defaultAgentModelMap() map[string]string {
	return map[string]string{
		"claude-code":    "claude-opus-4-7",   // Claude Code CLI — defaults to Opus tier; operators override when on Sonnet plan
		"claude-desktop": "claude-sonnet-4-6", // Claude Desktop — usually Sonnet
		"gemini-cli":     "gemini-1.5-pro",    // Gemini CLI
		"mcp-inspector":  "gemini-1.5-flash",  // Cheapest safe guess for the official inspector
	}
}

// defaultCostTable returns the canonical cost map used when neo.yaml
// does not supply one. Local Ollama models are 0/0; cloud pricing is in
// USD-per-million-tokens (public vendor rates as of 2026-04).
// [PILAR-XXVII/243.E]
func defaultCostTable() map[string]CostEntry {
	return map[string]CostEntry{
		// Anthropic Claude family — Opus / Sonnet / Haiku 4.x.
		"claude-opus-4-7":      {InputPerMTok: 15.0, OutputPerMTok: 75.0},
		"claude-sonnet-4-6":    {InputPerMTok: 3.0, OutputPerMTok: 15.0},
		"claude-haiku-4-5":     {InputPerMTok: 0.80, OutputPerMTok: 4.0},
		// Google Gemini family — Pro / Flash / Nano.
		"gemini-1.5-pro":   {InputPerMTok: 1.25, OutputPerMTok: 5.0},
		"gemini-1.5-flash": {InputPerMTok: 0.075, OutputPerMTok: 0.30},
		// OpenAI GPT family — reference rates.
		"gpt-4o":      {InputPerMTok: 2.50, OutputPerMTok: 10.0},
		"gpt-4o-mini": {InputPerMTok: 0.15, OutputPerMTok: 0.60},
		// Local Ollama models — zero cost, listed so the lookup succeeds.
		"qwen2:0.5b":       {InputPerMTok: 0, OutputPerMTok: 0},
		"qwen2.5-coder:7b": {InputPerMTok: 0, OutputPerMTok: 0},
		"llama3:8b":        {InputPerMTok: 0, OutputPerMTok: 0},
	}
}

// CostForModel returns the cost entry for a model name, falling back to
// a zero entry when unknown so the caller never crashes on a novel model.
func (c InferenceConfig) CostForModel(model string) CostEntry {
	if e, ok := c.CostTable[model]; ok {
		return e
	}
	return CostEntry{}
}

// ResolveAgentModel maps a raw agent identifier (e.g. "claude-code@2.1.114")
// to a pricing model. Uses AgentModelMap with longest-prefix-first match,
// so a config entry for "claude-code" catches every claude-code@<version>
// without per-version maintenance. Falls back to the raw identifier when
// no prefix matches — downstream CostForModel then returns a zero entry
// and the HUD still shows tokens, just cost=$0.
// [PILAR-XXVII/245.Q]
func (c InferenceConfig) ResolveAgentModel(agent string) string {
	if agent == "" {
		return ""
	}
	var best string
	for prefix := range c.AgentModelMap {
		if !strings.HasPrefix(agent, prefix) {
			continue
		}
		if len(prefix) > len(best) {
			best = prefix
		}
	}
	if best != "" {
		return c.AgentModelMap[best]
	}
	return agent
}

// UsageCost computes USD cost for a (model, inTokens, outTokens) tuple.
func (c InferenceConfig) UsageCost(model string, inTokens, outTokens int) float64 {
	e := c.CostForModel(model)
	return (float64(inTokens)*e.InputPerMTok + float64(outTokens)*e.OutputPerMTok) / 1_000_000.0
}

// applyWorkspaceDefaults backfills Cognitive, Storage and Workspace fields.
func applyWorkspaceDefaults(cfg *NeoConfig, ns *bool) {
	// Cognitive config backfill — ArenaSize=0 breaks MCTS; Strictness=0 disables bouncer
	if cfg.Cognitive.ArenaSize == 0 {
		cfg.Cognitive.ArenaSize = 100000
		*ns = true
	}
	if cfg.Cognitive.Strictness == 0 {
		cfg.Cognitive.Strictness = 0.75
		*ns = true
	}
	// [SRE-26.1.1] Module router backfill — ensure map is never nil
	if cfg.Workspace.Modules == nil {
		cfg.Workspace.Modules = map[string]string{"web": "npm run build"}
		*ns = true
	}
	// Workspace safety backfill — MaxFileSizeMB=0 means no limit (OOM risk)
	if cfg.Workspace.MaxFileSizeMB == 0 {
		cfg.Workspace.MaxFileSizeMB = 5
		*ns = true
	}
	// [358.A] Scope backfill — empty defaults to "fullstack" (current behavior).
	// See ADR-015 + [CONFIG-FIELD-BACKFILL-RULE] in synced directives.
	if cfg.Workspace.Scope == "" {
		cfg.Workspace.Scope = "fullstack"
		*ns = true
	}
	// AllowedExtensions=nil means nothing gets indexed into RAG
	if len(cfg.Workspace.AllowedExtensions) == 0 {
		cfg.Workspace.AllowedExtensions = []string{".go", ".ts", ".js", ".py", ".md", ".rs", ".html", ".css", ".yaml"}
		*ns = true
	}
	// Merge mandatory ignore dirs — preserve user entries, add missing defaults
	{
		required := []string{"node_modules", "vendor", ".git", "dist", ".neo",
			"lib", "site-packages", "__pycache__", "venv", ".venv", "env"}
		existing := make(map[string]bool, len(cfg.Workspace.IgnoreDirs))
		for _, d := range cfg.Workspace.IgnoreDirs {
			existing[d] = true
		}
		for _, d := range required {
			if !existing[d] {
				cfg.Workspace.IgnoreDirs = append(cfg.Workspace.IgnoreDirs, d)
				*ns = true
			}
		}
	}
}

// applyMonitoringDefaults backfills Sentinel and Kinetic thresholds.
func applyMonitoringDefaults(cfg *NeoConfig, ns *bool) {
	// [SRE-40/41] Sentinel config backfill
	if cfg.Sentinel.HeapThresholdMB == 0 {
		cfg.Sentinel.HeapThresholdMB = 500
		*ns = true
	}
	if cfg.Sentinel.GoroutineExplosionLimit == 0 {
		cfg.Sentinel.GoroutineExplosionLimit = 10000
		*ns = true
	}
	if cfg.Sentinel.ColdStartGraceSec == 0 {
		cfg.Sentinel.ColdStartGraceSec = 30
		*ns = true
	}
	if cfg.Sentinel.AuditLogMaxSize == 0 {
		cfg.Sentinel.AuditLogMaxSize = 1000
		*ns = true
	}
	if cfg.Sentinel.DreamCycleCount == 0 {
		cfg.Sentinel.DreamCycleCount = 3
		*ns = true
	}
	if cfg.Sentinel.ImmunityConfidenceInit == 0 {
		cfg.Sentinel.ImmunityConfidenceInit = 0.6
		*ns = true
	}
	if cfg.Sentinel.ImmunityActivationMin == 0 {
		cfg.Sentinel.ImmunityActivationMin = 0.5
		*ns = true
	}
	// [SRE-44] Kinetic config backfill
	if cfg.Kinetic.SpectralBins == 0 {
		cfg.Kinetic.SpectralBins = 8
		*ns = true
	}
	if cfg.Kinetic.AnomalyThresholdSigma == 0 {
		cfg.Kinetic.AnomalyThresholdSigma = 2.0
		*ns = true
	}
	if cfg.Kinetic.GCPauseThresholdUs == 0 {
		cfg.Kinetic.GCPauseThresholdUs = 10000
		*ns = true
	}
	if cfg.Kinetic.HeapCriticalSigma == 0 {
		cfg.Kinetic.HeapCriticalSigma = 5.0
		*ns = true
	}
	if cfg.Kinetic.HeapWarningSigma == 0 {
		cfg.Kinetic.HeapWarningSigma = 3.0
		*ns = true
	}
}

// applyDataDefaults backfills Coldstore, HyperGraph, Causal and Governance fields.
func applyDataDefaults(cfg *NeoConfig, ns *bool) {
	// [SRE-38] Coldstore config backfill
	if cfg.Coldstore.MaxOpenConns == 0 {
		cfg.Coldstore.MaxOpenConns = 3
		*ns = true
	}
	if cfg.Coldstore.MaxIdleConns == 0 {
		cfg.Coldstore.MaxIdleConns = 2
		*ns = true
	}
	if cfg.Coldstore.DefaultQueryLimit == 0 {
		cfg.Coldstore.DefaultQueryLimit = 50
		*ns = true
	}
	// [SRE-42] HyperGraph config backfill
	if cfg.HyperGraph.MaxImpactDepth == 0 {
		cfg.HyperGraph.MaxImpactDepth = 5
		*ns = true
	}
	if cfg.HyperGraph.RiskDecayFactor == 0 {
		cfg.HyperGraph.RiskDecayFactor = 0.7
		*ns = true
	}
	if cfg.HyperGraph.MinRiskThreshold == 0 {
		cfg.HyperGraph.MinRiskThreshold = 0.01
		*ns = true
	}
	// Governance config backfill
	if cfg.Governance.GhostModeMaxCycles == 0 {
		cfg.Governance.GhostModeMaxCycles = 50
		*ns = true
	}
	if cfg.Governance.ConstitutionPath == "" {
		cfg.Governance.ConstitutionPath = ".neo/constitution.rego"
		*ns = true
	}
}

// [PILAR-XXVIII/248.B+D] applyShadowDarwinDefaults removed — Shadow
// (neo.yaml) and Darwin config were dropped. Shadow in nexus.yaml
// lives in pkg/nexus, not here.

// applyFederationLLMDefaults backfills Federation dream synthesis and LLM integration fields.
func applyFederationLLMDefaults(cfg *NeoConfig, ns *bool) {
	// [SRE-94] Federation config backfill
	if cfg.Federation.DreamSchedule == "" {
		cfg.Federation.DreamSchedule = "0 3 * * *"
		*ns = true
	}
	if cfg.Federation.DedupThreshold == 0 {
		cfg.Federation.DedupThreshold = 0.92
		*ns = true
	}
	if cfg.Federation.ManifestBucket == "" {
		cfg.Federation.ManifestBucket = "dream_manifests"
		*ns = true
	}
	if cfg.Federation.MaxVectorsPerNode == 0 {
		cfg.Federation.MaxVectorsPerNode = 500
		*ns = true
	}
	if cfg.Federation.HarvestTimeoutSec == 0 {
		cfg.Federation.HarvestTimeoutSec = 300
		*ns = true
	}
	// [SRE-95] LLM config backfill
	if cfg.LLM.OllamaURL == "" {
		cfg.LLM.OllamaURL = "http://localhost:11434"
		*ns = true
	}
	if cfg.LLM.Model == "" {
		cfg.LLM.Model = "llama3.2:3b"
		*ns = true
	}
	if cfg.LLM.MaxTokens == 0 {
		cfg.LLM.MaxTokens = 2048
		*ns = true
	}
	if cfg.LLM.Temperature == 0 {
		cfg.LLM.Temperature = 0.3
		*ns = true
	}
	if cfg.LLM.Aliases == nil {
		cfg.LLM.Aliases = map[string]string{
			"status": `neo_radar(intent: "BRIEFING", mode: "compact")`,
			"chaos":  `neo_chaos_drill(target: "$1", aggression_level: 5)`,
		}
		*ns = true
	}
}

// discoverCPGEntryPoint walks `<workspaceDir>/cmd/*/main.go` and returns the
// first match in sort order as (pkgPattern, pkgDir). Empty strings when no
// entrypoint is found or workspaceDir is unreachable. Config-explicit values
// always win over this discovery. [330.G]
func discoverCPGEntryPoint(workspaceDir string) (string, string) {
	if workspaceDir == "" {
		return "", ""
	}
	cmdDir := filepath.Join(workspaceDir, "cmd")
	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		return "", ""
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(cmdDir, name, "main.go")); err == nil {
			rel := filepath.ToSlash(filepath.Join("cmd", name))
			return "./" + rel, rel
		}
	}
	return "", ""
}

// applyCPGDefaults backfills Code Property Graph builder and runtime fields (PILAR XX).
// workspaceDir is used to auto-discover the SSA entry point when not configured. [330.G]
func applyCPGDefaults(cfg *NeoConfig, ns *bool, workspaceDir string) {
	if cfg.CPG.PageRankIters == 0 {
		cfg.CPG.PageRankIters = 50
		*ns = true
	}
	if cfg.CPG.PageRankDamping == 0 {
		cfg.CPG.PageRankDamping = 0.85
		*ns = true
	}
	if cfg.CPG.ActivationAlpha == 0 {
		cfg.CPG.ActivationAlpha = 0.5
		*ns = true
	}
	if cfg.CPG.MaxHeapMB == 0 {
		// [Épica 229.4b] Raised from 50 → 512. Real baseline heap for a
		// fully-warmed neo-mcp (HNSW loaded + all caches primed) sits at
		// ~200 MB; 50 guaranteed the OOM guard would fire on every boot
		// and suppress CPG serving. 512 leaves headroom for ingest bursts
		// without normalising to "always-tripped".
		cfg.CPG.MaxHeapMB = 512
		*ns = true
	}
	if cfg.CPG.PersistPath == "" {
		cfg.CPG.PersistPath = ".neo/db/cpg.bin"
		*ns = true
	}
	if cfg.CPG.PersistIntervalMinutes == 0 {
		cfg.CPG.PersistIntervalMinutes = 15
		*ns = true
	}
	if cfg.CPG.PackagePath == "" || cfg.CPG.PackageDir == "" {
		discoveredPath, discoveredDir := discoverCPGEntryPoint(workspaceDir)
		if cfg.CPG.PackagePath == "" {
			if discoveredPath != "" {
				cfg.CPG.PackagePath = discoveredPath
			} else {
				cfg.CPG.PackagePath = "./cmd/neo-mcp"
			}
			*ns = true
		}
		if cfg.CPG.PackageDir == "" {
			if discoveredDir != "" {
				cfg.CPG.PackageDir = discoveredDir
			} else {
				cfg.CPG.PackageDir = "cmd/neo-mcp"
			}
			*ns = true
		}
	}
}

func LoadConfig(path string) (*NeoConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := defaultNeoConfig()
			// Try to copy neo.yaml.example if it exists in the same directory
			dir := filepath.Dir(path)
			examplePath := filepath.Join(dir, "neo.yaml.example")
			if exampleData, exErr := os.ReadFile(examplePath); exErr == nil {
				_ = os.WriteFile(path, exampleData, 0644)
				// Re-parse the example into cfg to pick up any values
				_ = yaml.Unmarshal(exampleData, cfg)
			} else {
				// No example found — write struct defaults
				out, marshalErr := yaml.Marshal(cfg)
				if marshalErr == nil {
					_ = os.WriteFile(path, out, 0644)
				}
			}
			// [330.G] CPG defaults need workspace-aware discovery even on first
			// boot — defaultNeoConfig() can't do it without the dir.
			unused := false
			applyCPGDefaults(cfg, &unused, dir)
			// [Bug-2/3 fix] Apply env overrides on the no-yaml path too.
			// Without this, a Nexus-spawned child whose workspace lacks a
			// neo.yaml binds to the default :8085 (NEO_PORT ignored) and
			// hits relative-URL Ollama (OLLAMA_HOST ignored) because the
			// env-override block at line 1484+ was only reached on the
			// yaml-present path. Extracted to applyChildEnvOverrides so
			// both branches stay in sync.
			applyChildEnvOverrides(cfg)
			return cfg, nil
		}
		return nil, err
	}

	// Load .neo/.env from workspace dir (secrets never van en el repo).
	// Shell env vars take priority — loadDotEnv never overwrites them.
	loadDotEnv(filepath.Join(filepath.Dir(path), ".neo", ".env"))

	// Expand ${VAR} references in the YAML template.
	// rawTemplate preserves the original (with ${VAR} intact) for safe write-back.
	rawTemplate := data
	hasEnvVars := bytes.Contains(data, []byte("${"))
	if hasEnvVars {
		data = []byte(os.ExpandEnv(string(data)))
	}

	cfg := defaultNeoConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	// Backfill missing fields with sane defaults
	needsSave := false
	applyServerDefaults(cfg, &needsSave)
	applyIntegrationDefaults(cfg, &needsSave)
	applyRAGDefaults(cfg, &needsSave)
	applySREDefaults(cfg, &needsSave)
	applyAIDefaults(cfg, &needsSave)
	applyInferenceDefaults(cfg, &needsSave)
	applyWorkspaceDefaults(cfg, &needsSave)
	applyMonitoringDefaults(cfg, &needsSave)
	applyDataDefaults(cfg, &needsSave)
	applyFederationLLMDefaults(cfg, &needsSave)
	applyCPGDefaults(cfg, &needsSave, filepath.Dir(path))
	applyHardwareDefaults(cfg, &needsSave)
	applyDeepSeekDefaults(cfg, &needsSave)

	if needsSave {
		if hasEnvVars {
			// Write back the original template — never expand secrets to disk.
			_ = os.WriteFile(path, rawTemplate, 0644)
		} else {
			if out, marshalErr := yaml.Marshal(cfg); marshalErr == nil {
				_ = os.WriteFile(path, out, 0644)
			}
		}
	}

	// Env overrides — extracted to applyChildEnvOverrides so the
	// no-yaml early-return path can call it too. [Bug-2/3 fix]
	applyChildEnvOverrides(cfg)

	// Auto-sync .neo/.env.example with any ${VAR} references found in the YAML template.
	dotEnvDir := filepath.Join(filepath.Dir(path), ".neo")
	syncDotEnvExample(dotEnvDir, rawTemplate)

	// [PILAR-XXXI] 3-tier config merge — extracted to keep LoadConfig CC≤15.
	cfg = applyProjectConfig(cfg, filepath.Dir(path))

	return cfg, nil
}

// applyChildEnvOverrides applies the runtime env-var overrides that
// Nexus injects into spawned children + that compose injects into the
// neoanvil container. Idempotent + side-effect-free on disk (overrides
// never write back to neo.yaml — keep secrets out of the file).
//
// Env vars honoured (all optional; unset = leave config as-is):
//
//	NEO_PORT          → cfg.Server.{SSE,Diagnostics,SREListener,Dashboard}Port
//	OLLAMA_HOST       → cfg.AI.BaseURL
//	OLLAMA_EMBED_HOST → cfg.AI.EmbedBaseURL
//
// [NEXUS/SRE-85 + Area 1.1.C + Bug-2/3 fix]
//
// Port layout per child (NEO_PORT = base):
//
//	base+0   → SSE/MCP transport (/mcp/message, /health, dashboard APIs)
//	base+100 → Diagnostics/pprof
//	base+200 → SRE Listener (external incidents)
//	DashboardPort → disabled (HUD served by Nexus since Épica 85)
func applyChildEnvOverrides(cfg *NeoConfig) {
	if neoPort := os.Getenv("NEO_PORT"); neoPort != "" {
		if p, convErr := strconv.Atoi(neoPort); convErr == nil && p > 0 {
			cfg.Server.SSEPort = p
			cfg.Server.DiagnosticsPort = p + 100
			cfg.Server.SREListenerPort = p + 200
			cfg.Server.DashboardPort = 0
		}
	}
	if embedHost := os.Getenv("OLLAMA_EMBED_HOST"); embedHost != "" {
		cfg.AI.EmbedBaseURL = embedHost
	}
	if llmHost := os.Getenv("OLLAMA_HOST"); llmHost != "" {
		cfg.AI.BaseURL = llmHost
	}
}

// applyProjectConfig loads .neo-project/neo.yaml walking up from workspaceDir and
// merges project overrides into cfg. Also loads `.neo-org/neo.yaml` (walk-up to
// 10 levels) when present — org fills LLM gaps left by project. No-op when
// neither config is found. [258.C / 355.A]
func applyProjectConfig(cfg *NeoConfig, workspaceDir string) *NeoConfig {
	merged := *cfg
	// Org tier (outermost): discover first so project can reference org later.
	if oc, err := LoadOrgConfig(workspaceDir); err == nil && oc != nil {
		merged = applyOrgOverrides(merged, oc)
	}
	pc, pcErr := LoadProjectConfig(workspaceDir)
	if pcErr != nil || pc == nil {
		return &merged
	}
	merged = applyProjectOverrides(merged, pc)
	return &merged
}

// syncDotEnvExample scans yamlTemplate for ${VAR_NAME} references and writes/updates
// .neo/.env.example so new variables are always reflected in the committed template.
// Existing entries in .env.example are preserved; only missing ones are appended.
// The file is only written when the set of variables changes, keeping git diffs minimal.
// Comment lines (starting with #) are excluded from the scan to avoid false positives.
// parseTemplateVars extracts unique, sorted ${VAR_NAME} references from a YAML template,
// ignoring comment lines to avoid false positives.
func parseTemplateVars(yamlTemplate []byte) []string {
	var nonCommentLines []string
	for line := range strings.SplitSeq(string(yamlTemplate), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			nonCommentLines = append(nonCommentLines, line)
		}
	}
	re := regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)\}`)
	matches := re.FindAllSubmatch([]byte(strings.Join(nonCommentLines, "\n")), -1)
	seen := make(map[string]struct{}, len(matches))
	var vars []string
	for _, m := range matches {
		name := string(m[1])
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			vars = append(vars, name)
		}
	}
	sort.Strings(vars)
	return vars
}

// readExistingEnvKeys reads KEY=VALUE lines from an .env.example file, returning
// the set of known keys and all lines (for append-only updates).
func readExistingEnvKeys(path string) (map[string]struct{}, []string) {
	existing := make(map[string]struct{})
	var lines []string
	if data, err := os.ReadFile(path); err == nil {
		for line := range strings.SplitSeq(string(data), "\n") {
			lines = append(lines, line)
			k, _, ok := strings.Cut(strings.TrimSpace(line), "=")
			if ok && !strings.HasPrefix(k, "#") {
				existing[strings.TrimSpace(k)] = struct{}{}
			}
		}
	}
	return existing, lines
}

func syncDotEnvExample(dotEnvDir string, yamlTemplate []byte) {
	vars := parseTemplateVars(yamlTemplate)
	if len(vars) == 0 {
		return
	}

	examplePath := filepath.Join(dotEnvDir, ".env.example")
	existing, existingLines := readExistingEnvKeys(examplePath)

	// Determine which vars are new.
	var newVars []string
	for _, v := range vars {
		if _, ok := existing[v]; !ok {
			newVars = append(newVars, v)
		}
	}
	if len(newVars) == 0 {
		return // Nothing to add.
	}

	// Build the updated file: existing content + new entries.
	if err := os.MkdirAll(dotEnvDir, 0700); err != nil {
		return
	}
	var sb strings.Builder
	if len(existingLines) > 0 {
		sb.WriteString(strings.Join(existingLines, "\n"))
		if !strings.HasSuffix(sb.String(), "\n") {
			sb.WriteByte('\n')
		}
	} else {
		sb.WriteString("# NeoAnvil workspace secrets — copia a .neo/.env y rellena los valores.\n")
		sb.WriteString("# .neo/.env está en .gitignore — nunca lo commitees.\n\n")
	}
	for _, v := range newVars {
		fmt.Fprintf(&sb, "%s=\n", v)
	}
	_ = os.WriteFile(examplePath, []byte(sb.String()), 0644)
}

// loadDotEnv reads KEY=VALUE pairs from path and calls os.Setenv for each.
// Shell environment takes priority: existing vars are never overwritten.
// The file is optional — a missing .env is silently ignored.
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" {
			continue
		}
		// Shell env wins — only set if not already present.
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

// defaultSafeCommands returns the Watchdog whitelist for [SRE-34.3.1].
// Commands whose prefix matches any entry here are auto-approvable in UNSUPERVISED mode.
func defaultSafeCommands() []string {
	return []string{
		"go test", "go build", "go vet", "go fmt", "go generate",
		"ls", "cat", "grep", "find", "head", "tail", "wc",
		"npm test", "npm run build", "npm ci", "npm run lint",
		"git status", "git log", "git diff", "git show",
		"cargo test", "cargo build", "cargo check",
		"python3 -m pytest", "python3 -m py_compile",
		"make test", "make build",
	}
}
