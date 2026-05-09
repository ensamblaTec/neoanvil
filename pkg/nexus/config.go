// Package nexus — config.go
// [SRE-80.A.2] Dispatcher-level configuration loader.
//
// NexusConfig is intentionally SEPARATE from pkg/config (which serves neo-mcp
// workspace-level configuration). Keeping the loaders independent guarantees
// that the global dispatcher (~/.neo/nexus.yaml) never conflicts with a
// per-project neo.yaml.
//
// Resolution order:
//  1. $NEO_NEXUS_CONFIG env var (absolute path)
//  2. ~/.neo/nexus.yaml
//  3. built-in defaults (defaultNexusConfig)
package nexus

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// OllamaServiceConfig describes a single Ollama instance managed by Nexus.
// When Enabled is true, Nexus starts and monitors this instance as a system-level
// dependency before spawning any neo-mcp children.
type OllamaServiceConfig struct {
	Enabled              bool              `yaml:"enabled"`
	Port                 int               `yaml:"port"`
	BindAddr             string            `yaml:"bind_addr"`
	Env                  map[string]string `yaml:"env"`
	HealthPath           string            `yaml:"health_path"`
	EnsureModels         []string          `yaml:"ensure_models"`
	HealthTimeoutSeconds int               `yaml:"health_timeout_seconds"`
	PullTimeoutSeconds   int               `yaml:"pull_timeout_seconds"`
}

// ServicesConfig holds the system-level services that Nexus manages.
// Services are started before children and monitored by the same watchdog cadence.
type ServicesConfig struct {
	OllamaLLM   OllamaServiceConfig `yaml:"ollama_llm"`
	OllamaEmbed OllamaServiceConfig `yaml:"ollama_embed"`
}

// NexusConfig mirrors nexus.yaml. All fields are top-level under `nexus:`.
type NexusConfig struct {
	Nexus NexusSection `yaml:"nexus"`
}

// NexusSection holds the dispatcher configuration.
type NexusSection struct {
	DispatcherPort int    `yaml:"dispatcher_port"`
	BindAddr       string `yaml:"bind_addr"`
	BinPath        string `yaml:"bin_path"`
	PortRangeBase      int `yaml:"port_range_base"`
	PortRangeSize      int `yaml:"port_range_size"`
	MockPortRangeBase  int `yaml:"mock_port_range_base"`  // [291.B] Base port for ephemeral mock servers.
	MockPortRangeSize  int `yaml:"mock_port_range_size"`  // [291.B] Number of ports in mock range.
	RegistryFile   string `yaml:"registry_file"`
	PortsFile      string `yaml:"ports_file"`

	// ManagedWorkspaces is an explicit allowlist of workspace IDs or names that
	// Nexus should start as SSE children. Empty = all workspaces in the registry.
	// Use this to exclude stdio-only projects from the SSE process pool.
	ManagedWorkspaces []string `yaml:"managed_workspaces"`

	Logs      LogsConfig      `yaml:"logs"`
	Child     ChildConfig     `yaml:"child"`
	Watchdog  WatchdogConfig  `yaml:"watchdog"`
	API       APIConfig       `yaml:"api"`
	Telemetry TelemetryConfig `yaml:"telemetry"`

	// [SRE-85.A] DashboardPort — Operator HUD now served by Nexus (headless worker).
	DashboardPort int `yaml:"dashboard_port"`

	// Services manages system-level dependencies (Ollama instances) that Nexus
	// starts and monitors before spawning children. All disabled by default (opt-in).
	// When enabled, OLLAMA_EMBED_HOST is auto-injected into child.extra_env so all
	// workspaces share the dedicated embedding instance without touching neo.yaml.
	Services ServicesConfig `yaml:"services"`

	// Shadow mirrors /mcp/message traffic to a secondary target for canary testing.
	Shadow NexusShadowConfig `yaml:"shadow"`

	// Debt is the Nexus-level debt registry config (PILAR LXVI / 351.A).
	// When Enabled:true, verify_boot timeouts, watchdog trips, and service
	// manager failures are appended to ~/.neo/nexus_debt.md with dedup.
	Debt DebtConfig `yaml:"debt"`

	// Plugins is the subprocess MCP plugin pool config (PILAR XXIII / 123.6).
	// When Enabled:true, Nexus loads the plugin manifest from
	// $NEO_PLUGINS_CONFIG → ~/.neo/plugins.yaml and spawns each enabled
	// PluginSpec via PluginPool. Default disabled (opt-in).
	Plugins PluginsConfig `yaml:"plugins"`

	// SSE session limits (PILAR XXVIII 145.B — DoS defense).
	// MaxSSESessions caps total concurrent SSE connections across all clients.
	// MaxSSESessionsPerIP caps connections from a single remote IP.
	// SSEIdleTimeoutSeconds closes sessions that receive no messages for the
	// given duration; 0 disables the idle check.
	MaxSSESessions        int `yaml:"max_sse_sessions"`
	MaxSSESessionsPerIP   int `yaml:"max_sse_sessions_per_ip"`
	SSEIdleTimeoutSeconds int `yaml:"sse_idle_timeout_seconds"`
}

// PluginsConfig controls the subprocess plugin pool. Currently a single
// flag; future fields (manifest_path override, default stop grace per-pool,
// global aggregator timeout) will land here without breaking compatibility.
type PluginsConfig struct {
	Enabled bool `yaml:"enabled"`
}

// NexusShadowConfig controls shadow traffic mirroring at the Nexus dispatcher level.
type NexusShadowConfig struct {
	Enabled         bool    `yaml:"enabled"`
	TargetURL       string  `yaml:"target_url"`
	SampleRate      float64 `yaml:"sample_rate"`
	TimeoutMs       int     `yaml:"timeout_ms"`
	UnsafeMethods   bool    `yaml:"unsafe_methods"`
	DiffThresholdMs int     `yaml:"diff_threshold_ms"`
	BufferSize      int     `yaml:"buffer_size"`
}

// LogsConfig controls child-process log redirection.
type LogsConfig struct {
	Dir       string `yaml:"dir"`
	Mode      string `yaml:"mode"` // "file" | "inherit"
	RotateMB  int    `yaml:"rotate_mb"`
	KeepFiles int    `yaml:"keep_files"`
}

// ChildConfig controls how neo-mcp children are spawned.
type ChildConfig struct {
	StdinMode             string            `yaml:"stdin_mode"` // "devnull" | "inherit"
	StartupTimeoutSeconds int               `yaml:"startup_timeout_seconds"`
	BootGraceSeconds      int               `yaml:"boot_grace_seconds"`
	ExtraEnv              map[string]string `yaml:"extra_env"`
	// [ÉPICA 150] Lifecycle policy for managed children:
	//   "eager" (default, legacy) — StartAll spawns every child at boot.
	//   "lazy"                    — StartAll registers each as StatusCold;
	//                               first /api/v1/workspaces/wake/<id> (or
	//                               EnsureRunning() in code) spawns on demand.
	// Multi-workspace operators with N>1 children pay N×~30s boot today
	// even when only 1 is in use; lazy mode amortizes that to per-use.
	Lifecycle string `yaml:"lifecycle"`
	// LazyBootTimeoutSeconds bounds how long EnsureRunning waits for a
	// cold→running transition. Default 600 (10 min) — accommodates the
	// occasional cold-cache HNSW WAL load on a multi-GB graph. Lower
	// values mean operators see "wake timeout" earlier; higher values
	// reduce false negatives when the WAL is genuinely slow.
	LazyBootTimeoutSeconds int `yaml:"lazy_boot_timeout_seconds"`
	// [ÉPICA 150.C] Idle reaper threshold. When > 0, a background
	// goroutine in ProcessPool ticks every IdleReaperTickSeconds and
	// SIGTERMs any workspace whose last_tool_call_unix is older than
	// IdleSeconds. Activity = MCP tool calls only (NOT SSE liveness —
	// a misbehaving client holding an idle SSE forever shouldn't
	// keep a workspace warm).
	//
	// Default 0 (disabled) preserves legacy behavior. Operators who
	// want lazy + idle-reaping must opt in explicitly to avoid
	// surprise SIGTERMs in workspaces they expect to stay warm.
	IdleSeconds int `yaml:"idle_seconds"`
	// IdleReaperTickSeconds bounds how often the reaper checks for
	// candidates. Default 60 — balances reaping latency vs check
	// overhead.
	IdleReaperTickSeconds int `yaml:"idle_reaper_tick_seconds"`
	// LazyPrewarmSeconds, when > 0, schedules a background pre-warm for
	// cold lazy workspaces N seconds after RegisterCold. Cancelled if the
	// workspace reaches StatusRunning before the timer fires. [ÉPICA 150.M]
	LazyPrewarmSeconds int `yaml:"lazy_prewarm_seconds"`
}

// WatchdogConfig controls health monitoring of children.
type WatchdogConfig struct {
	Enabled              bool   `yaml:"enabled"`
	HealthEndpoint       string `yaml:"health_endpoint"`
	CheckIntervalSeconds int    `yaml:"check_interval_seconds"`
	FailureThreshold     int    `yaml:"failure_threshold"`
	AutoRestart          bool   `yaml:"auto_restart"`
	MaxRestartsPerHour   int    `yaml:"max_restarts_per_hour"`
}

// APIConfig controls the REST management surface.
type APIConfig struct {
	Enabled   bool   `yaml:"enabled"`
	AuthToken string `yaml:"auth_token"`
}

// TelemetryConfig controls observability outputs.
type TelemetryConfig struct {
	PrometheusEnabled bool `yaml:"prometheus_enabled"`
	JSONLogs          bool `yaml:"json_logs"`
}

// defaultNexusConfig returns the hard-coded fallback used when no yaml exists.
// Every literal in the dispatcher MUST live here so pkg/nexus and
// cmd/neo-nexus stay free of magic numbers.
func defaultNexusConfig() *NexusConfig {
	home, _ := os.UserHomeDir()
	return &NexusConfig{
		Nexus: NexusSection{
			DispatcherPort: 9000,
			BindAddr:       "127.0.0.1",
			BinPath:        "",
			PortRangeBase:     9100,
			PortRangeSize:     200,
			MockPortRangeBase: 34800, // [291.B]
			MockPortRangeSize: 100,
			RegistryFile:   filepath.Join(home, ".neo", "workspaces.json"),
			PortsFile:      filepath.Join(home, ".neo", "nexus_ports.json"),
			Logs: LogsConfig{
				Dir:       filepath.Join(home, ".neo", "logs"),
				Mode:      "file",
				RotateMB:  50,
				KeepFiles: 5,
			},
			Child: ChildConfig{
				StdinMode:             "devnull",
				StartupTimeoutSeconds: 15,
				BootGraceSeconds:      3,
				ExtraEnv:              map[string]string{},
				Lifecycle:             "eager", // [ÉPICA 150] preserve legacy behavior by default
				LazyBootTimeoutSeconds: 600,    // [ÉPICA 150] 10 min bound for lazy spawn wait
				IdleSeconds:           0,       // [ÉPICA 150.C] disabled by default — opt-in via nexus.yaml
				IdleReaperTickSeconds: 60,      // [ÉPICA 150.C] check candidates every minute
			},
			Watchdog: WatchdogConfig{
				Enabled:              true,
				HealthEndpoint:       "/health",
				CheckIntervalSeconds: 10,
				FailureThreshold:     3,
				AutoRestart:          true,
				MaxRestartsPerHour:   5,
			},
			API: APIConfig{
				Enabled:   true,
				AuthToken: "",
			},
			Telemetry: TelemetryConfig{
				PrometheusEnabled: false,
				JSONLogs:          false,
			},
			DashboardPort:     8087,
			ManagedWorkspaces: []string{},
			Services: ServicesConfig{
				OllamaLLM: OllamaServiceConfig{
					Enabled:              false,
					Port:                 11434,
					BindAddr:             "127.0.0.1",
					HealthPath:           "/api/tags",
					Env:                  map[string]string{},
					EnsureModels:         []string{},
					HealthTimeoutSeconds: 30,
					PullTimeoutSeconds:   300,
				},
				OllamaEmbed: OllamaServiceConfig{
					Enabled:              false,
					Port:                 11435,
					BindAddr:             "127.0.0.1",
					HealthPath:           "/api/tags",
					Env:                  map[string]string{},
					EnsureModels:         []string{},
					HealthTimeoutSeconds: 30,
					PullTimeoutSeconds:   300,
				},
			},
			Debt: DebtConfig{
				Enabled:            false, // opt-in; set true in nexus.yaml to activate PILAR LXVI
				File:               "~/.neo/nexus_debt.md",
				MaxResolvedDays:    30,
				DedupWindowMinutes: 15,
				BoltDBMirror:       false,
			},
			Plugins: PluginsConfig{
				Enabled: false, // opt-in; set true to activate PILAR XXIII plugin pool
			},
			MaxSSESessions:        1000,
			MaxSSESessionsPerIP:   10,
			SSEIdleTimeoutSeconds: 300,
		},
	}
}

// LoadNexusConfig resolves and parses the dispatcher configuration.
// Resolution: $NEO_NEXUS_CONFIG → ~/.neo/nexus.yaml → built-in defaults.
// Missing file is NOT an error; defaults are returned with a nil error.
func LoadNexusConfig() (*NexusConfig, error) {
	cfg := defaultNexusConfig()

	path := resolveConfigPath()
	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	cfg.applyBackfill()
	cfg.expandPaths()
	cfg.applyEnvOverrides()
	return cfg, nil
}

// applyEnvOverrides applies post-load env-var overrides for fields that
// must change between native and Docker deployments WITHOUT persisting
// to nexus.yaml. Symmetric with pkg/config.LoadConfig's OLLAMA_HOST /
// OLLAMA_EMBED_HOST treatment. [Area 1.1.C]
//
// Recognised vars:
//   NEO_BIND_ADDR                       → nexus.bind_addr
//                                         (typical Docker value: 0.0.0.0)
//   NEO_NEXUS_OLLAMA_LIFECYCLE=disabled → forces both ollama services
//                                         enabled:false. In Docker mode,
//                                         compose owns Ollama; Nexus
//                                         must NOT spawn its own.
func (c *NexusConfig) applyEnvOverrides() {
	if bind := os.Getenv("NEO_BIND_ADDR"); bind != "" {
		c.Nexus.BindAddr = bind
	}
	if os.Getenv("NEO_NEXUS_OLLAMA_LIFECYCLE") == "disabled" {
		c.Nexus.Services.OllamaLLM.Enabled = false
		c.Nexus.Services.OllamaEmbed.Enabled = false
	}
}

// resolveConfigPath returns the first existing config path in resolution order.
func resolveConfigPath() string {
	if envPath := os.Getenv("NEO_NEXUS_CONFIG"); envPath != "" {
		return envPath
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".neo", "nexus.yaml")
}

// applyBackfill fills zero-valued fields with defaults so a partial yaml
// doesn't leave critical ports at 0.
func (c *NexusConfig) applyBackfill() {
	d := defaultNexusConfig().Nexus
	n := &c.Nexus
	applyNexusTransportDefaults(n, d)
	applyNexusLogsChildDefaults(n, d)
	applyNexusWatchdogDefaults(n, d)
	applyNexusServiceDefaults(n, d)
}

func applyNexusTransportDefaults(n *NexusSection, d NexusSection) {
	if n.DispatcherPort == 0 {
		n.DispatcherPort = d.DispatcherPort
	}
	if n.BindAddr == "" {
		n.BindAddr = d.BindAddr
	}
	if n.PortRangeBase == 0 {
		n.PortRangeBase = d.PortRangeBase
	}
	if n.PortRangeSize == 0 {
		n.PortRangeSize = d.PortRangeSize
	}
	if n.MockPortRangeBase == 0 {
		n.MockPortRangeBase = d.MockPortRangeBase
	}
	if n.MockPortRangeSize == 0 {
		n.MockPortRangeSize = d.MockPortRangeSize
	}
	if n.RegistryFile == "" {
		n.RegistryFile = d.RegistryFile
	}
	if n.PortsFile == "" {
		n.PortsFile = d.PortsFile
	}
	if n.DashboardPort == 0 {
		n.DashboardPort = d.DashboardPort
	}
	if n.MaxSSESessions == 0 {
		n.MaxSSESessions = d.MaxSSESessions
	}
	if n.MaxSSESessionsPerIP == 0 {
		n.MaxSSESessionsPerIP = d.MaxSSESessionsPerIP
	}
	if n.SSEIdleTimeoutSeconds == 0 {
		n.SSEIdleTimeoutSeconds = d.SSEIdleTimeoutSeconds
	}
}

func applyNexusLogsChildDefaults(n *NexusSection, d NexusSection) {
	if n.Logs.Dir == "" {
		n.Logs.Dir = d.Logs.Dir
	}
	if n.Logs.Mode == "" {
		n.Logs.Mode = d.Logs.Mode
	}
	if n.Logs.RotateMB == 0 {
		n.Logs.RotateMB = d.Logs.RotateMB
	}
	if n.Logs.KeepFiles == 0 {
		n.Logs.KeepFiles = d.Logs.KeepFiles
	}
	if n.Child.StdinMode == "" {
		n.Child.StdinMode = d.Child.StdinMode
	}
	if n.Child.StartupTimeoutSeconds == 0 {
		n.Child.StartupTimeoutSeconds = d.Child.StartupTimeoutSeconds
	}
	if n.Child.BootGraceSeconds == 0 {
		n.Child.BootGraceSeconds = d.Child.BootGraceSeconds
	}
	if n.Child.ExtraEnv == nil {
		n.Child.ExtraEnv = map[string]string{}
	}
	// [ÉPICA 150] Lifecycle defaults — explicit "eager" preserves legacy
	// behavior so any existing nexus.yaml without the field gets the same
	// boot-everything semantics.
	if n.Child.Lifecycle == "" {
		n.Child.Lifecycle = d.Child.Lifecycle
	}
	if n.Child.LazyBootTimeoutSeconds == 0 {
		n.Child.LazyBootTimeoutSeconds = d.Child.LazyBootTimeoutSeconds
	}
	// [ÉPICA 150.C] IdleSeconds defaults to 0 (disabled) — DON'T backfill,
	// otherwise operators who explicitly want lazy WITHOUT idle reaping
	// would get surprised. Only backfill the tick interval when the
	// reaper is enabled (IdleSeconds > 0) to avoid silent 0 → 60 surprise.
	if n.Child.IdleSeconds > 0 && n.Child.IdleReaperTickSeconds == 0 {
		n.Child.IdleReaperTickSeconds = d.Child.IdleReaperTickSeconds
	}
}

func applyNexusWatchdogDefaults(n *NexusSection, d NexusSection) {
	if n.Watchdog.HealthEndpoint == "" {
		n.Watchdog.HealthEndpoint = d.Watchdog.HealthEndpoint
	}
	if n.Watchdog.CheckIntervalSeconds == 0 {
		n.Watchdog.CheckIntervalSeconds = d.Watchdog.CheckIntervalSeconds
	}
	if n.Watchdog.FailureThreshold == 0 {
		n.Watchdog.FailureThreshold = d.Watchdog.FailureThreshold
	}
	if n.Watchdog.MaxRestartsPerHour == 0 {
		n.Watchdog.MaxRestartsPerHour = d.Watchdog.MaxRestartsPerHour
	}
}

// applyNexusServiceDefaults applies port/path defaults regardless of enabled
// state so partial YAML entries (e.g. only `enabled: true`) work correctly.
func applyNexusServiceDefaults(n *NexusSection, d NexusSection) {
	llm := &n.Services.OllamaLLM
	if llm.Port == 0 {
		llm.Port = d.Services.OllamaLLM.Port
	}
	if llm.BindAddr == "" {
		llm.BindAddr = d.Services.OllamaLLM.BindAddr
	}
	if llm.HealthPath == "" {
		llm.HealthPath = d.Services.OllamaLLM.HealthPath
	}
	if llm.HealthTimeoutSeconds == 0 {
		llm.HealthTimeoutSeconds = d.Services.OllamaLLM.HealthTimeoutSeconds
	}
	if llm.PullTimeoutSeconds == 0 {
		llm.PullTimeoutSeconds = d.Services.OllamaLLM.PullTimeoutSeconds
	}
	emb := &n.Services.OllamaEmbed
	if emb.Port == 0 {
		emb.Port = d.Services.OllamaEmbed.Port
	}
	if emb.BindAddr == "" {
		emb.BindAddr = d.Services.OllamaEmbed.BindAddr
	}
	if emb.HealthPath == "" {
		emb.HealthPath = d.Services.OllamaEmbed.HealthPath
	}
	if emb.HealthTimeoutSeconds == 0 {
		emb.HealthTimeoutSeconds = d.Services.OllamaEmbed.HealthTimeoutSeconds
	}
	if emb.PullTimeoutSeconds == 0 {
		emb.PullTimeoutSeconds = d.Services.OllamaEmbed.PullTimeoutSeconds
	}
}

// expandPaths substitutes a leading "~/" with the user home directory in
// every filesystem path field so configs stay portable between machines.
func (c *NexusConfig) expandPaths() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	expand := func(p string) string {
		if strings.HasPrefix(p, "~/") {
			return filepath.Join(home, p[2:])
		}
		return p
	}
	n := &c.Nexus
	n.RegistryFile = expand(n.RegistryFile)
	n.PortsFile = expand(n.PortsFile)
	n.Logs.Dir = expand(n.Logs.Dir)
	n.BinPath = expand(n.BinPath)
}
