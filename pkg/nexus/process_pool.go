package nexus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/singleflight"
	"github.com/ensamblatec/neoanvil/pkg/sre"
	"github.com/ensamblatec/neoanvil/pkg/workspace"
)

// prefixWriter prepends a short label to each newline-terminated line and
// forwards the result to an underlying writer. Used to stream child process
// logs to the nexus terminal with a workspace-ID prefix so operators can
// distinguish output from multiple children at a glance.
type prefixWriter struct {
	prefix []byte
	out    io.Writer
	buf    []byte
}

func newPrefixWriter(label string, out io.Writer) *prefixWriter {
	return &prefixWriter{prefix: []byte(label), out: out}
}

func (pw *prefixWriter) Write(p []byte) (int, error) {
	pw.buf = append(pw.buf, p...)
	for {
		idx := bytes.IndexByte(pw.buf, '\n')
		if idx < 0 {
			break
		}
		line := append(pw.prefix, pw.buf[:idx+1]...)
		if _, err := pw.out.Write(line); err != nil {
			return 0, err
		}
		pw.buf = pw.buf[idx+1:]
	}
	return len(p), nil
}

// isTerminal reports whether the given file is connected to a terminal.
// Used to decide whether to mirror child logs to the nexus console.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// reservePort binds bind:port and keeps the listener open, eliminating the
// TOCTOU window between "port is free" and "child starts". The caller is
// responsible for closing the returned listener after cmd.Start() (parent
// copy); the child holds its own dup via ExtraFiles. [145.J]
func reservePort(bind string, port int) (*net.TCPListener, error) {
	addr := fmt.Sprintf("%s:%d", bind, port)
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	return net.ListenTCP("tcp", tcpAddr)
}

// isPortFreeOn tries to bind the given bind:port pair and immediately releases
// it. Still used by resolvePortCollision's eviction retry loop where we don't
// yet want to hold the listener. [SRE-80.C.1]
func isPortFreeOn(bind string, port int) bool {
	addr := fmt.Sprintf("%s:%d", bind, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// ProcessStatus describes the runtime state of a workspace child process. [SRE-68.1.2]
type ProcessStatus string

const (
	StatusStopped    ProcessStatus = "stopped"
	StatusStarting   ProcessStatus = "starting"
	StatusRunning    ProcessStatus = "running"
	StatusError      ProcessStatus = "error"
	StatusUnhealthy  ProcessStatus = "unhealthy"
	StatusQuarantined ProcessStatus = "quarantined"
	// [ÉPICA 150] Lazy-mode: registered but not yet spawned. First MCP/SSE
	// request triggers EnsureRunning → singleflight-coalesced spawn →
	// transitions cold → starting → running. On lazy-boot timeout the
	// state reverts to cold (NOT error) so the next request retries
	// cleanly without watchdog intervention.
	StatusCold ProcessStatus = "cold"
	// [ÉPICA 150.L] Stopping is the transient state between "running"
	// and "cold" (lazy lifecycle) or "stopped" (eager). Used by the
	// idle reaper (150.C) and by Stop() to expose mid-shutdown state
	// to /status consumers. /wake during stopping waits for the
	// transition to complete before triggering a fresh spawn — without
	// this gate, the new spawn races the old child for the same port.
	StatusStopping ProcessStatus = "stopping"
)

// WorkspaceProcess tracks a single neo-mcp child process. [SRE-68.1.2]
//
// Restart-rate state (failures, restartTS, restarts) used to live on this
// struct directly; audit S9-5 (PILAR XXVIII 143.C, 2026-05-02) showed that
// pp.Start() replaces the *WorkspaceProcess pointer in pp.processes[id] on
// every restart, dropping the previous counters. An adversary that crashes
// the child loop bypassed the per-hour rate limit because the watchdog
// always saw zero recent restarts on the fresh struct. Restart-rate state
// is now in pp.restartState[wsID] *wsRestartTracker (keyed by workspace ID,
// outlives process replacements). The exported Restarts field below is
// kept as a JSON-visible mirror for /status consumers and is updated by
// maybeRestart from the tracker.
type WorkspaceProcess struct {
	Entry     workspace.WorkspaceEntry `json:"entry"`
	Port      int                      `json:"port"`
	PID       int                      `json:"pid"`
	Status    ProcessStatus            `json:"status"`
	StartedAt time.Time                `json:"started_at"`
	LastPing  time.Time                `json:"last_ping"`
	Restarts  int                      `json:"restarts"` // mirror of pp.restartState[id].restarts; do NOT mutate directly — use pp.tickRestart()
	cmd       *exec.Cmd
	logFile   *os.File // open log file handle (nil if inherit mode)
	adopted   bool     // true when the process was not spawned by this pool (adoption path)

	// [Épica 248.A] Activity counters — written atomically by RecordToolCall in the proxy
	// hot-path, read atomically by List/GetProcess. Exported so cmd/neo-nexus can read copies.
	LastToolCallUnix int64 `json:"-"` // unix timestamp of last proxied MCP tool call (0=never)
	ToolCallCount    int64 `json:"-"` // cumulative MCP tool calls proxied through Nexus

	// [Épica 283.B] Project membership — set at Start() via TopologyIndex.
	ProjectID string `json:"project_id,omitempty"`
	// [285.E] Last time this workspace received a cross-workspace memex sync.
	LastMemexSyncUnix int64 `json:"last_memex_sync_unix,omitempty"`
	// [ÉPICA 148.E] Boot progress propagated from child's /boot_progress
	// endpoint while Status==StatusStarting. Updated by verifyBoot poll
	// loop on each iteration. Empty / 0 when child has finished or boot
	// progress endpoint is unreachable. Allows operators (and BRIEFING
	// peers section) to disambiguate "child still loading vs hung".
	BootPhase string  `json:"boot_phase,omitempty"`
	BootPct   float64 `json:"boot_pct,omitempty"`
	// [ÉPICA 150] Lifecycle from cfg.Nexus.Child.Lifecycle: "eager" (boot
	// at StartAll) or "lazy" (register cold, spawn on first request). Per-
	// process so future config reload can mix policies if needed.
	Lifecycle string `json:"lifecycle,omitempty"`
}

// wsRestartTracker holds restart-rate state per workspace ID, OUTLIVING
// individual *WorkspaceProcess records. Audit fix S9-5 (PILAR XXVIII 143.C,
// 2026-05-02): pre-fix the rate state lived on the process instance and
// pp.Start() dropped it on each restart, allowing unlimited restarts/hour.
// All access is serialised through pp.mu (same mutex as pp.processes — keeps
// invariants between the process map and the tracker map atomic).
type wsRestartTracker struct {
	failures  int         // consecutive health-check failures since last recovery
	restartTS []time.Time // recent restart timestamps (rolling 1-hour window for rate limit)
	restarts  int         // monotonic restart count (mirrored to WorkspaceProcess.Restarts for JSON)
}

// ProcessPool manages a set of neo-mcp child processes. [SRE-68.1.2]
type ProcessPool struct {
	mu              sync.RWMutex
	processes       map[string]*WorkspaceProcess // workspaceID → process
	restartState    map[string]*wsRestartTracker // workspaceID → restart-rate state (PILAR XXVIII 143.C, 2026-05-02). Survives process replacements via Start.
	allocator       *PortAllocator
	binPath         string       // path to neo-mcp binary
	cfg             *NexusConfig // [SRE-80.A.3] dispatcher config (nil → legacy defaults)
	ctx             context.Context
	cancel          context.CancelFunc
	projectActivity sync.Map           // projectID → *ProjectActivityCounters [283.C]
	prevIdleState   map[string]bool    // wsID → was-idle-last-tick [285.A]
	// OnIdleTransition is called (in a new goroutine) when a workspace crosses the
	// 300s idle threshold for the first time (edge trigger, not level trigger). [285.A]
	OnIdleTransition func(wsID string)

	// Debt is the optional Nexus-level debt registry. When non-nil, verifyBoot
	// timeouts (and future watchdog/service_manager sites) emit persistent
	// entries to ~/.neo/nexus_debt.md via DebtRegistry.AppendDebt. [PILAR LXVI / 351.A]
	Debt *DebtRegistry

	// [ÉPICA 150] Coalesces concurrent EnsureRunning calls so N waiters
	// for the same cold workspace share 1 spawn. Standard library
	// singleflight: subsequent Do(key, fn) calls during fn's run wait
	// for fn's outcome; new calls after fn returns trigger fresh fn.
	spawnFlight singleflight.Group
	// [ÉPICA 150.K / DS audit fix #3] Tracks in-flight Start invocations
	// so SIGHUP reload (or other administrative actions that delete
	// process records) waits for spawn goroutines to finish before
	// removing the entry. Without this, the spawned monitorChild +
	// verifyBoot goroutines hold pointers to a record that may be
	// removed mid-flight, triggering nil-deref or status-write to
	// freed memory.
	spawnsInFlight sync.WaitGroup

	// InternalToken is a random hex token generated at Nexus boot and injected
	// into every child process as NEO_NEXUS_INTERNAL_TOKEN. Children include it
	// as X-Neo-Internal-Token on /internal/certify/* and /internal/chaos/* calls
	// so those endpoints cannot be invoked by arbitrary local processes. [PRIVILEGE-001/002]
	InternalToken string

	// prewarmTimers maps workspaceID → *time.Timer for pending lazy pre-warm
	// timers scheduled by RegisterCold (ÉPICA 150.M). Cancelled when the
	// workspace reaches StatusRunning.
	prewarmTimers sync.Map
}

// NewProcessPool creates a pool that manages neo-mcp child processes. [SRE-68.1.2]
// Legacy constructor used by tests — falls back to built-in defaults.
func NewProcessPool(allocator *PortAllocator, binPath string) *ProcessPool {
	return NewProcessPoolWithConfig(allocator, binPath, defaultNexusConfig())
}

// NewProcessPoolWithConfig creates a pool using an explicit dispatcher config.
// [SRE-80.A.3] Added so cmd/neo-nexus can inject ~/.neo/nexus.yaml settings.
// [351.A] When cfg.Nexus.Debt.Enabled, the pool auto-opens a DebtRegistry so
// verifyBoot timeouts persist to ~/.neo/nexus_debt.md without further wiring.
func NewProcessPoolWithConfig(allocator *PortAllocator, binPath string, cfg *NexusConfig) *ProcessPool {
	// Resolve symlinks at construction time so a post-boot symlink replacement
	// cannot redirect child spawns to an adversary-controlled binary.
	if !filepath.IsAbs(binPath) {
		if abs, err := filepath.Abs(binPath); err == nil {
			binPath = abs
		} else {
			log.Printf("[NEXUS-POOL] WARNING: binPath %q is not absolute and cannot be resolved: %v", binPath, err)
		}
	}
	if resolved, err := filepath.EvalSymlinks(binPath); err == nil {
		binPath = resolved
	} else {
		log.Printf("[NEXUS-POOL] WARNING: binPath %q EvalSymlinks failed (binary may not exist yet): %v", binPath, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	pp := &ProcessPool{
		processes:     make(map[string]*WorkspaceProcess),
		restartState:  make(map[string]*wsRestartTracker), // PILAR XXVIII 143.C — survives Start() replacements
		prevIdleState: make(map[string]bool),
		allocator:     allocator,
		binPath:       binPath,
		cfg:           cfg,
		ctx:           ctx,
		cancel:        cancel,
	}
	if cfg != nil && cfg.Nexus.Debt.Enabled {
		if reg, err := OpenDebtRegistry(cfg.Nexus.Debt); err != nil {
			log.Printf("[NEXUS-DEBT] open failed (disabled): %v", err)
		} else {
			pp.Debt = reg
			log.Printf("[NEXUS-DEBT] registry opened at %s (dedup=%dm, archive=%dd)", cfg.Nexus.Debt.File, cfg.Nexus.Debt.DedupWindowMinutes, cfg.Nexus.Debt.MaxResolvedDays)
		}
	}
	return pp
}

// openChildStdin resolves the stdin file descriptor for a child process
// according to cfg.Child.StdinMode. [SRE-80.B.1]
//
// The original bug: neo-mcp inspects os.Stdin at boot to decide between
// stdio+SSE dual transport and SSE-only. If the child inherits Nexus's
// stdin-as-pipe, it takes the stdio branch and blocks forever in
// json.NewDecoder(os.Stdin).Decode(). Redirecting to /dev/null forces
// the char-device branch (SSE-only) which is what Nexus wants.
func (pp *ProcessPool) openChildStdin() (io.ReadCloser, error) {
	mode := pp.cfg.Nexus.Child.StdinMode
	if mode == "inherit" {
		return nil, nil // nil → inherit parent stdin (debug only)
	}
	return os.Open(os.DevNull)
}

// openChildLog resolves stdout/stderr writers for a child process. [SRE-80.B.2]
// Mode "file" writes to cfg.Logs.Dir/nexus-<id>.log (O_APPEND).
//   - When the nexus process is connected to a terminal, child output is also
//     mirrored to os.Stderr with a "[workspace-id] " prefix so operators see
//     child logs in the same terminal window (docker-compose style).
// Mode "inherit" returns nil — child inherits parent's stdout/stderr (debug).
//
// Returns: (logFile for closing, writer for cmd.Stdout/cmd.Stderr, error).
func (pp *ProcessPool) openChildLog(workspaceID string) (*os.File, io.Writer, error) {
	logs := pp.cfg.Nexus.Logs
	if logs.Mode == "inherit" {
		return nil, nil, nil
	}
	if err := os.MkdirAll(logs.Dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("mkdir %s: %w", logs.Dir, err)
	}
	path := filepath.Join(logs.Dir, fmt.Sprintf("nexus-%s.log", workspaceID))
	rotateIfLarge(path, logs.RotateMB, logs.KeepFiles)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}

	// Mirror child output to the nexus terminal when it is interactive.
	if isTerminal(os.Stderr) {
		label := fmt.Sprintf("[%s] ", workspaceID)
		w := io.MultiWriter(f, newPrefixWriter(label, os.Stderr))
		return f, w, nil
	}
	return f, f, nil
}

// rotateIfLarge rotates a log file when its size exceeds maxMB. Keeps
// the last keep-1 rotated copies (.1, .2, ...). Silent on error — log
// rotation must never block process startup. [SRE-80.B.2]
func rotateIfLarge(path string, maxMB, keep int) {
	if maxMB <= 0 || keep <= 0 {
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() < int64(maxMB)*1024*1024 {
		return
	}
	for i := keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", path, i), fmt.Sprintf("%s.%d", path, i+1))
	}
	_ = os.Rename(path, path+".1")
}

// buildChildEnv constructs the environment variable slice for a child process.
// [Épica 302.E.1] Extracted from Start() to reduce CC.
func (pp *ProcessPool) buildChildEnv(entry workspace.WorkspaceEntry, port int) []string {
	env := append(os.Environ(),
		fmt.Sprintf("NEO_PORT=%d", port),
		fmt.Sprintf("NEO_WORKSPACE_ID=%s", entry.ID),
		fmt.Sprintf("NEO_EXTERNAL_URL=http://%s:%d/workspaces/%s", pp.cfg.Nexus.BindAddr, pp.cfg.Nexus.DispatcherPort, entry.ID),
		"NEO_NEXUS_CHILD=1", // [SRE-81.B.4] signals neo-mcp to skip global registry.Save()
	)
	// [PRIVILEGE-001/002] Inject the internal auth token so children can authenticate
	// /internal/certify/* and /internal/chaos/* calls back to Nexus.
	if pp.InternalToken != "" {
		env = append(env, fmt.Sprintf("NEO_NEXUS_INTERNAL_TOKEN=%s", pp.InternalToken))
	}
	// [145.K] Only inject ExtraEnv keys that begin with "NEO_" or "OLLAMA_".
	// Arbitrary operator-supplied keys could override reserved env vars
	// (e.g. PATH, LD_PRELOAD, HOME) and enable code-execution via crafted
	// nexus.yaml entries. Allowlisted prefixes cover all documented use cases.
	for k, v := range pp.cfg.Nexus.Child.ExtraEnv {
		if strings.HasPrefix(k, "NEO_") || strings.HasPrefix(k, "OLLAMA_") {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		} else {
			log.Printf("[NEXUS-POOL] WARNING: extra_env key %q rejected (not in NEO_*/OLLAMA_* allowlist)", k)
		}
	}
	return env
}

// spawnChild forks a new neo-mcp child process and returns its exec.Cmd
// handle and the log file that must be closed on exit.
// reservedLn, when non-nil, is the pre-reserved TCP listener for this port
// ([145.J] TOCTOU fix): its fd is passed to the child via ExtraFiles[0]
// (→ fd 3) and NEO_INHERITED_LISTENER_FD=3 signals neo-mcp to Accept on it.
// [Épica 302.E.1] Extracted from Start() to reduce CC.
func (pp *ProcessPool) spawnChild(entry workspace.WorkspaceEntry, port int, reservedLn *net.TCPListener) (*exec.Cmd, *os.File, error) {
	cmd := exec.CommandContext(pp.ctx, pp.binPath, entry.Path)
	env := pp.buildChildEnv(entry, port)

	if reservedLn != nil {
		// [145.J] Duplicate the listener fd so the child can inherit it.
		// File() creates a new os.File with CLOEXEC cleared — required for
		// inheritance across exec. The original reservedLn stays open until
		// the caller's defer closes it; the dup is closed after cmd.Start().
		lnFile, ferr := reservedLn.File()
		if ferr != nil {
			return nil, nil, fmt.Errorf("inherit listener fd: %w", ferr)
		}
		defer lnFile.Close() // parent dup closed after cmd.Start()
		cmd.ExtraFiles = []*os.File{lnFile}
		env = append(env, "NEO_INHERITED_LISTENER_FD=3")
	}

	cmd.Env = env

	// [SRE-80.B.1] stdin fix — /dev/null forces neo-mcp into SSE-only mode.
	stdin, err := pp.openChildStdin()
	if err != nil {
		return nil, nil, fmt.Errorf("open child stdin: %w", err)
	}
	if stdin != nil {
		cmd.Stdin = stdin
	}

	// [SRE-80.B.2] stdout/stderr → per-workspace log file (or inherit).
	logFile, logWriter, err := pp.openChildLog(entry.ID)
	if err != nil {
		if stdin != nil {
			_ = stdin.Close()
		}
		return nil, nil, fmt.Errorf("open child log: %w", err)
	}
	if logWriter != nil {
		cmd.Stdout = logWriter
		cmd.Stderr = logWriter
	} else {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}

	if err := cmd.Start(); err != nil {
		if stdin != nil {
			_ = stdin.Close()
		}
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, nil, fmt.Errorf("failed to start neo-mcp for %s: %w", entry.Path, err)
	}
	return cmd, logFile, nil
}

// Start launches a neo-mcp child process for a workspace. [SRE-68.1.2]
// [SRE-80.B.1] stdin redirected per cfg.Child.StdinMode.
// [SRE-80.B.2] stdout/stderr redirected per cfg.Logs.Mode.
// [SRE-80.B.3] Boot verified by HTTP health check, not blind sleep.
//
// [ÉPICA 150 / DS audit fix #2] Idempotent against concurrent callers:
// when a previous Start is in flight (status==Starting) OR the child is
// already running, Start returns nil without spawning a duplicate.
// Without this guard, EnsureRunning + manual /api/v1/workspaces/start
// could race two verifyBoot goroutines on the same proc → status churn.
func (pp *ProcessPool) Start(entry workspace.WorkspaceEntry) error {
	pp.mu.Lock()
	if proc, ok := pp.processes[entry.ID]; ok {
		if proc.Status == StatusRunning || proc.Status == StatusStarting {
			pp.mu.Unlock()
			return nil
		}
	}
	pp.mu.Unlock()

	port, err := pp.allocator.Allocate(entry.ID, entry.Path)
	if err != nil {
		return fmt.Errorf("port allocation failed for %s: %w", entry.ID, err)
	}
	// [DS audit fix #1] Defer the allocator release; suppress the release
	// once we successfully store the proc record. Without this guard,
	// every spawnChild failure leaks the port.
	releasePort := true
	defer func() {
		if releasePort {
			pp.allocator.Release(entry.Path)
		}
	}()

	// [145.J] Reserve the port NOW and hold the listener open across the
	// fork+exec window — eliminates the TOCTOU gap between isPortFreeOn and
	// cmd.Start() where an external process could steal the port and cause
	// EADDRINUSE in the child. The fd is passed to the child via ExtraFiles
	// so it can Accept on it directly (see NEO_INHERITED_LISTENER_FD in neo-mcp).
	reservedLn, err := reservePort(pp.cfg.Nexus.BindAddr, port)
	if err != nil {
		// Port in use — adopt healthy holder or evict (same logic as before).
		adopted, cerr := pp.resolvePortCollision(entry, port)
		if cerr != nil {
			return cerr
		}
		if adopted {
			releasePort = false // adopted process owns the port now
			return nil
		}
		// Port was freed by eviction — re-reserve to close the gap.
		reservedLn, err = reservePort(pp.cfg.Nexus.BindAddr, port)
		if err != nil {
			return fmt.Errorf("port %d still in use after eviction: %w", port, err)
		}
	}
	defer reservedLn.Close() // parent releases its copy after cmd.Start()

	pp.mu.Lock()
	cmd, logFile, err := pp.spawnChild(entry, port, reservedLn)
	if err != nil {
		pp.processes[entry.ID] = &WorkspaceProcess{
			Entry:  entry,
			Port:   port,
			Status: StatusError,
		}
		pp.mu.Unlock()
		// releasePort stays true — port leak prevention triggers in defer.
		return err
	}

	proc := &WorkspaceProcess{
		Entry:     entry,
		Port:      port,
		PID:       cmd.Process.Pid,
		Status:    StatusStarting,
		StartedAt: time.Now(),
		cmd:       cmd,
		logFile:   logFile,
		Lifecycle: pp.cfg.Nexus.Child.Lifecycle, // [ÉPICA 150]
	}
	pp.processes[entry.ID] = proc
	releasePort = false // proc owns the port now
	pp.mu.Unlock()
	log.Printf("[NEXUS-EVENT] child_started id=%s port=%d pid=%d", entry.ID, port, cmd.Process.Pid)

	// [ÉPICA 150.K] Increment in-flight counter BEFORE spawning the
	// monitor goroutine. WaitInFlightSpawns blocks reload paths until
	// the count reaches 0 — guarantees no goroutine holds a stale
	// pointer to a removed process record.
	pp.spawnsInFlight.Add(1)
	go pp.monitorChild(proc)
	return nil
}

// WaitInFlightSpawns blocks until every Start that's currently
// monitoring a child has finished. Reload paths call this BEFORE
// removing process records to ensure no monitor goroutine holds a
// dangling pointer. [ÉPICA 150.K / DS audit fix #3]
//
// Bounded by ctx so callers can fall back to a timeout if a misbehaving
// child never exits (e.g. operator forgot to /stop and SIGHUP arrived).
func (pp *ProcessPool) WaitInFlightSpawns(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		pp.spawnsInFlight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// monitorChild waits for the process to exit and updates bookkeeping.
// Runs in its own goroutine. [SRE-80.B.3] also triggers health-verified boot.
//
// [ÉPICA 150.K] Decrements pp.spawnsInFlight on exit so reload paths
// blocked on WaitInFlightSpawns can proceed. The defer guarantees
// decrement even if proc.cmd.Wait panics (defensive — Wait shouldn't
// panic, but the WaitGroup counter must never leak).
func (pp *ProcessPool) monitorChild(proc *WorkspaceProcess) {
	defer pp.spawnsInFlight.Done()
	go pp.verifyBoot(proc)

	_ = proc.cmd.Wait()

	pp.mu.Lock()
	// [ÉPICA 150.C] If the reaper transitioned us to Stopping, end at
	// Cold (lazy lifecycle wants demand-respawn). Otherwise crash exit
	// → Stopped (eager lifecycle expects manual /start).
	if proc.Status == StatusStopping {
		proc.Status = StatusCold
	} else {
		proc.Status = StatusStopped
	}
	proc.PID = 0
	if proc.logFile != nil {
		_ = proc.logFile.Close()
		proc.logFile = nil
	}
	pp.mu.Unlock()
	log.Printf("[NEXUS-EVENT] child_stopped id=%s port=%d final=%s", proc.Entry.ID, proc.Port, proc.Status)
}

// verifyBoot polls the child's /health endpoint until it responds or
// startup_timeout_seconds elapses. [SRE-80.B.3] Replaces the old blind-sleep.
//
// [ÉPICA 148.E] On each poll, also fetches /boot_progress (best-effort,
// no fatal on miss) so /status JSON exposes BootPhase + BootPct while
// the child is still loading. Lets operators (and BRIEFING peers
// section) disambiguate "alive, loading 67%" from "hung".
// healthProbeHost translates a bind-address into a destination address
// for health probes. "0.0.0.0" / "::" / "" all mean "bind any" — they
// are not valid destinations, so we redirect to 127.0.0.1 (loopback)
// since the child IS listening on it (binding any interface includes
// loopback). [Bug-5 fix — Docker NEO_BIND_ADDR=0.0.0.0 case]
func healthProbeHost(bindAddr string) string {
	switch bindAddr {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	}
	return bindAddr
}

func (pp *ProcessPool) verifyBoot(proc *WorkspaceProcess) {
	grace := time.Duration(pp.cfg.Nexus.Child.BootGraceSeconds) * time.Second
	timeout := time.Duration(pp.cfg.Nexus.Child.StartupTimeoutSeconds) * time.Second
	deadline := time.Now().Add(timeout)

	time.Sleep(grace)

	// [SRE-103.B] Child health target is loopback server-controlled.
	// We force the host to 127.0.0.1 here regardless of bind_addr —
	// the child binds 0.0.0.0 in Docker mode (NEO_BIND_ADDR=0.0.0.0),
	// but you can't dial to 0.0.0.0 from a client; it's a "bind-any"
	// address, not a destination. The child binding all interfaces
	// includes loopback, so the health probe stays correct in both
	// native (127.0.0.1) and Docker (0.0.0.0 listen, 127.0.0.1 probe)
	// modes. [Bug-5 fix]
	client := sre.SafeInternalHTTPClient(2)
	healthHost := healthProbeHost(pp.cfg.Nexus.BindAddr)
	healthURL := fmt.Sprintf("http://%s:%d%s",
		healthHost,
		proc.Port,
		pp.cfg.Nexus.Watchdog.HealthEndpoint,
	)
	progressURL := fmt.Sprintf("http://%s:%d/boot_progress",
		healthHost, proc.Port)

	backoff := 500 * time.Millisecond
	for time.Now().Before(deadline) {
		// Side-poll the boot progress endpoint. Best-effort; a 404 (older
		// neo-mcp without the endpoint) or transient error just leaves
		// BootPhase/BootPct unchanged. We do this BEFORE the health check
		// so the operator's first /status read sees boot progress even on
		// the very first iteration.
		pollChildBootProgress(client, progressURL, proc, &pp.mu)

		resp, err := client.Get(healthURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				pp.mu.Lock()
				if proc.Status == StatusStarting {
					proc.Status = StatusRunning
					proc.LastPing = time.Now()
					// Boot complete — clear progress so /status doesn't
					// show stale "hnsw_load 67%" forever.
					proc.BootPhase = ""
					proc.BootPct = 0
				}
				pp.mu.Unlock()
				// Cancel any pending pre-warm timer — workspace is already up. [ÉPICA 150.M]
				if t, ok := pp.prewarmTimers.LoadAndDelete(proc.Entry.ID); ok {
					t.(*time.Timer).Stop()
				}
				log.Printf("[NEXUS-EVENT] child_healthy id=%s port=%d", proc.Entry.ID, proc.Port)
				return
			}
		}
		time.Sleep(backoff)
		if backoff < 4*time.Second {
			backoff *= 2
		}
	}

	pp.mu.Lock()
	proc.Status = StatusError
	pid := proc.PID
	cmd := proc.cmd
	pp.mu.Unlock()
	log.Printf("[NEXUS-EVENT] child_boot_timeout id=%s port=%d timeout=%s",
		proc.Entry.ID, proc.Port, timeout)

	// [351.A] Persist as Nexus-level debt so affected workspace sees it at
	// next BRIEFING. Best-effort: failure to write debt never masks the boot
	// timeout itself.
	if pp.Debt != nil {
		_, derr := pp.Debt.AppendDebt(NexusDebtEvent{
			Priority:           "P0",
			Title:              fmt.Sprintf("%s verifyBoot timeout after %s — likely BoltDB lock held by zombie", proc.Entry.ID, timeout),
			AffectedWorkspaces: []string{proc.Entry.ID},
			Source:             "verify_boot",
			Recommended:        fmt.Sprintf("lsof +D %s/.neo/db/ | kill the non-Nexus PID | POST /api/v1/workspaces/start/%s", proc.Entry.Path, proc.Entry.ID),
		})
		if derr != nil {
			log.Printf("[NEXUS-DEBT] append failed: %v", derr)
		}
	}

	// Kill the orphan process so it releases BoltDB locks immediately.
	// Without this, the process survives with status=error and holds
	// hnsw.db/brain.db, blocking the next boot cycle. [ÉPICA 271.B, 302.A]
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		time.Sleep(2 * time.Second)
		_ = cmd.Process.Kill()
		log.Printf("[NEXUS] verifyBoot timeout: killed orphan pid=%d", cmd.Process.Pid)
		log.Printf("[NEXUS-EVENT] child_orphan_killed id=%s pid=%d", proc.Entry.ID, cmd.Process.Pid)
	} else if pid > 0 {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
			time.Sleep(2 * time.Second)
			_ = p.Kill()
			log.Printf("[NEXUS] verifyBoot timeout: killed orphan pid=%d", pid)
			log.Printf("[NEXUS-EVENT] child_orphan_killed id=%s pid=%d", proc.Entry.ID, pid)
		}
	}
}

// pollChildBootProgress fetches the child's /boot_progress endpoint and
// updates proc.BootPhase + proc.BootPct under mu. Best-effort: any
// network error, non-200 status, or unexpected JSON shape just leaves
// the existing values unchanged (older neo-mcp without the endpoint
// returns 404). Uses a tight 1s timeout so a slow child doesn't extend
// the verifyBoot iteration cadence. [ÉPICA 148.E]
func pollChildBootProgress(client *http.Client, url string, proc *WorkspaceProcess, mu *sync.RWMutex) {
	// Use a context-bound request so we can cap latency separately from
	// the client's overall timeout (which is shared with /health).
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var payload struct {
		Phase   string  `json:"phase"`
		HNSWPct float64 `json:"hnsw_pct"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return
	}
	mu.Lock()
	proc.BootPhase = payload.Phase
	proc.BootPct = payload.HNSWPct
	mu.Unlock()
}

// Stop gracefully shuts down the child process for a workspace. [SRE-68.1.2]
// [SRE-80.C.2] SIGTERM → wait up to startup_timeout → SIGKILL.
// Handles both spawned (cmd != nil) and adopted (cmd == nil, adopted == true) processes.
func (pp *ProcessPool) Stop(workspaceID string) error {
	pp.mu.Lock()
	proc, ok := pp.processes[workspaceID]
	pp.mu.Unlock()

	if !ok {
		return fmt.Errorf("workspace %s not running", workspaceID)
	}

	graceful := time.Duration(pp.cfg.Nexus.Child.StartupTimeoutSeconds) * time.Second

	// Adopted process: we hold no cmd handle, signal by PID directly.
	if proc.adopted {
		if proc.PID > 0 {
			p, err := os.FindProcess(proc.PID)
			if err == nil {
				if serr := p.Signal(os.Interrupt); serr != nil {
					_ = p.Kill()
				} else {
					// Give the adopted child time to exit cleanly.
					done := make(chan struct{})
					go func() {
						for {
							if e := p.Signal(syscall.Signal(0)); e != nil {
								break
							}
							time.Sleep(200 * time.Millisecond)
						}
						close(done)
					}()
					select {
					case <-done:
						log.Printf("[NEXUS] Workspace %s (adopted) stopped cleanly", workspaceID)
					case <-time.After(graceful):
						_ = p.Kill()
						log.Printf("[NEXUS] Workspace %s (adopted) force-killed after %s timeout", workspaceID, graceful)
					}
				}
			}
		}
		pp.mu.Lock()
		proc.Status = StatusStopped
		proc.PID = 0
		pp.mu.Unlock()
		return nil
	}

	if proc.cmd == nil || proc.cmd.Process == nil {
		return fmt.Errorf("workspace %s not running", workspaceID)
	}

	if err := proc.cmd.Process.Signal(os.Interrupt); err != nil {
		return fmt.Errorf("failed to signal process %d: %w", proc.PID, err)
	}

	done := make(chan struct{})
	go func() {
		_ = proc.cmd.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Printf("[NEXUS] Workspace %s stopped cleanly", workspaceID)
	case <-time.After(graceful):
		_ = proc.cmd.Process.Kill()
		log.Printf("[NEXUS] Workspace %s force-killed after %s timeout", workspaceID, graceful)
	}

	pp.mu.Lock()
	proc.Status = StatusStopped
	proc.PID = 0
	if proc.logFile != nil {
		_ = proc.logFile.Close()
		proc.logFile = nil
	}
	pp.mu.Unlock()
	return nil
}

// Health checks if a workspace's child process is responsive. [SRE-68.1.2]
func (pp *ProcessPool) Health(workspaceID string) bool {
	pp.mu.RLock()
	proc, ok := pp.processes[workspaceID]
	pp.mu.RUnlock()

	if !ok || proc.Status != StatusRunning {
		return false
	}

	// [SRE-103.B] Child health target is loopback server-controlled.
	client := sre.SafeInternalHTTPClient(2)
	url := fmt.Sprintf("http://%s:%d%s",
		healthProbeHost(pp.cfg.Nexus.BindAddr),
		proc.Port,
		pp.cfg.Nexus.Watchdog.HealthEndpoint,
	)
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()

	pp.mu.Lock()
	proc.LastPing = time.Now()
	pp.mu.Unlock()
	return resp.StatusCode == http.StatusOK
}

// StartAll launches processes for all workspaces in the registry. [SRE-68.1.3]
func (pp *ProcessPool) StartAll(entries []workspace.WorkspaceEntry) {
	// [ÉPICA 150] Lifecycle-aware boot: when cfg.Nexus.Child.Lifecycle ==
	// "lazy", register each workspace as StatusCold without spawning. The
	// first MCP/SSE request (or explicit /api/v1/workspaces/wake/<id>)
	// triggers EnsureRunning which singleflight-coalesces concurrent waits.
	if pp.cfg != nil && pp.cfg.Nexus.Child.Lifecycle == "lazy" {
		for _, entry := range entries {
			pp.RegisterCold(entry)
		}
		log.Printf("[NEXUS] lazy lifecycle: registered %d workspace(s) as cold (spawn on first request)", len(entries))
		return
	}
	for _, entry := range entries {
		if err := pp.Start(entry); err != nil {
			log.Printf("[NEXUS] Failed to start workspace %s: %v", entry.ID, err)
		}
	}
}

// RegisterCold registers a workspace in the pool with status=cold WITHOUT
// spawning the child. Used by StartAll under lazy lifecycle and exposed
// for tests. Idempotent: re-registering a known workspace is a no-op.
// [ÉPICA 150]
func (pp *ProcessPool) RegisterCold(entry workspace.WorkspaceEntry) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	if _, exists := pp.processes[entry.ID]; exists {
		return
	}
	pp.processes[entry.ID] = &WorkspaceProcess{
		Entry:     entry,
		Status:    StatusCold,
		Lifecycle: "lazy",
	}
	// Schedule background pre-warm if configured. [ÉPICA 150.M]
	if pp.cfg != nil && pp.cfg.Nexus.Child.LazyPrewarmSeconds > 0 {
		delay := time.Duration(pp.cfg.Nexus.Child.LazyPrewarmSeconds) * time.Second
		wsID := entry.ID
		t := time.AfterFunc(delay, func() {
			log.Printf("[NEXUS-EVENT] prewarm_start id=%s", wsID)
			if err := pp.EnsureRunning(wsID); err != nil {
				log.Printf("[NEXUS-EVENT] prewarm_fail id=%s err=%v", wsID, err)
			}
		})
		pp.prewarmTimers.Store(wsID, t)
	}
}

// waitForStopped blocks until the named workspace transitions out of
// StatusStopping (to Cold/Stopped/Error). Returns nil on success,
// error on timeout. [ÉPICA 150.L / DS audit fix #4]
//
// Used by EnsureRunning to avoid the wake-during-stopping race where
// the new spawn binds the port still held by the dying child. Polls
// every 100ms — typical SIGTERM grace is sub-second on healthy
// children, so we converge quickly.
//
// Reads proc.Status under the lock — the previous version released
// the RLock then dereferenced proc.Status, which is a plain string
// field (not atomic). -race flagged the dirty read.
func (pp *ProcessPool) waitForStopped(workspaceID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pp.mu.RLock()
		proc := pp.processes[workspaceID]
		var status ProcessStatus
		if proc != nil {
			status = proc.Status
		}
		pp.mu.RUnlock()
		if proc == nil || status != StatusStopping {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("waitForStopped: timeout after %v", timeout)
}

// IdleReaper runs as a background goroutine, periodically SIGTERMing
// workspaces whose last_tool_call_unix is older than IdleSeconds. The
// transition is graceful: status flips to StatusStopping, monitorChild
// observes the SIGTERM via cmd.Wait and finalizes to StatusCold.
//
// [ÉPICA 150.C / DS audit fix #5 mitigation]
// Activity = MCP tool calls only (LastToolCallUnix). SSE liveness
// doesn't count — a misbehaving client holding an idle SSE forever
// shouldn't keep a workspace warm against the operator's intent.
//
// Disabled when cfg.Child.IdleSeconds == 0 (default). Cancels cleanly
// on ctx.Done.
func (pp *ProcessPool) IdleReaper(ctx context.Context) {
	if pp.cfg == nil || pp.cfg.Nexus.Child.IdleSeconds <= 0 {
		log.Printf("[NEXUS-IDLE] reaper disabled (idle_seconds=0)")
		return
	}
	tickSec := pp.cfg.Nexus.Child.IdleReaperTickSeconds
	if tickSec <= 0 {
		tickSec = 60
	}
	idleSec := int64(pp.cfg.Nexus.Child.IdleSeconds)
	log.Printf("[NEXUS-IDLE] reaper enabled idle_seconds=%d tick_seconds=%d", idleSec, tickSec)
	ticker := time.NewTicker(time.Duration(tickSec) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pp.idleReaperTick(idleSec)
		}
	}
}

// idleReaperTick scans the process map for idle running children and
// SIGTERMs them. Held under mu.RLock to read the snapshot, then
// mu.Lock briefly to flip status to Stopping before signaling. The
// actual SIGTERM happens outside both locks — process.Signal is
// independent of our internal state.
func (pp *ProcessPool) idleReaperTick(idleSec int64) {
	now := time.Now().Unix()
	type candidate struct {
		id  string
		pid int
	}
	var victims []candidate
	pp.mu.RLock()
	for id, proc := range pp.processes {
		if proc.Status != StatusRunning {
			continue
		}
		// Activity counter from RecordToolCall; 0 = never. We treat 0
		// as "use StartedAt as the floor" so workspaces booted but
		// never used STILL get reaped after idle_seconds passes.
		last := atomic.LoadInt64(&proc.LastToolCallUnix)
		if last == 0 {
			last = proc.StartedAt.Unix()
		}
		if now-last >= idleSec {
			victims = append(victims, candidate{id: id, pid: proc.PID})
		}
	}
	pp.mu.RUnlock()

	for _, v := range victims {
		pp.mu.Lock()
		proc := pp.processes[v.id]
		if proc == nil || proc.Status != StatusRunning {
			pp.mu.Unlock()
			continue // raced — skip
		}
		proc.Status = StatusStopping
		pp.mu.Unlock()
		log.Printf("[NEXUS-IDLE] SIGTERM idle workspace=%s pid=%d (last_call %ds ago)",
			v.id, v.pid, now-time.Now().Unix()+idleSec)
		if v.pid > 0 {
			if proc, err := os.FindProcess(v.pid); err == nil {
				_ = proc.Signal(syscall.SIGTERM)
			}
		}
	}
}

// EnsureRunning transitions a workspace from cold (or any non-running
// state) to running, blocking until verifyBoot reports healthy or the
// configured lazy_boot_timeout elapses. Concurrent callers for the same
// workspace coalesce via singleflight.Group — only the first does the
// real work; the rest wait on the same outcome.
//
// [DS audit fix #2] Start is idempotent against concurrent invocations,
// so even if /api/v1/workspaces/start/<id> races with EnsureRunning the
// second call is a no-op once status==Starting.
//
// [DS audit fix #5] On wait timeout, status reverts to StatusCold (NOT
// Error) so the next caller's retry triggers a fresh spawn. This avoids
// the watchdog blind-spot where StatusError sits forever ungrounded.
//
// Returns nil when the child is verified healthy, or an error describing
// the failure mode (unknown workspace, spawn failure, wait timeout).
//
// [ÉPICA 150]
func (pp *ProcessPool) EnsureRunning(workspaceID string) error {
	pp.mu.RLock()
	proc, ok := pp.processes[workspaceID]
	if !ok {
		pp.mu.RUnlock()
		return fmt.Errorf("EnsureRunning: unknown workspace %q", workspaceID)
	}
	if proc.Status == StatusRunning {
		pp.mu.RUnlock()
		return nil
	}
	entry := proc.Entry
	pp.mu.RUnlock()

	_, err, _ := pp.spawnFlight.Do(workspaceID, func() (any, error) {
		// [ÉPICA 150.L / DS audit fix #4] If reaper or operator just
		// SIGTERMed this workspace, wait for the transition to
		// Cold/Stopped before respawning — otherwise port still bound
		// by graceful-shutdown child → bind fails. Bounded by a 30s
		// floor on the lazy_boot_timeout to avoid wedging on a
		// hung-shutdown child.
		if waitErr := pp.waitForStopped(workspaceID, 30*time.Second); waitErr != nil {
			return nil, fmt.Errorf("EnsureRunning: wait stopping: %w", waitErr)
		}
		// Re-check inside singleflight: another caller may have completed
		// the spawn while we were waiting on Do's lock.
		pp.mu.RLock()
		cur := pp.processes[workspaceID]
		if cur != nil && cur.Status == StatusRunning {
			pp.mu.RUnlock()
			return nil, nil
		}
		pp.mu.RUnlock()
		if startErr := pp.Start(entry); startErr != nil {
			return nil, fmt.Errorf("EnsureRunning: spawn failed for %s: %w", workspaceID, startErr)
		}
		// Wait for verifyBoot to flip Status=Running.
		timeoutSec := pp.cfg.Nexus.Child.LazyBootTimeoutSeconds
		if timeoutSec <= 0 {
			timeoutSec = 600
		}
		deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
		for time.Now().Before(deadline) {
			pp.mu.RLock()
			now := pp.processes[workspaceID]
			pp.mu.RUnlock()
			if now == nil {
				return nil, fmt.Errorf("EnsureRunning: workspace %s removed mid-spawn", workspaceID)
			}
			if now.Status == StatusRunning {
				return nil, nil
			}
			if now.Status == StatusError {
				return nil, fmt.Errorf("EnsureRunning: spawn errored for %s", workspaceID)
			}
			time.Sleep(500 * time.Millisecond)
		}
		// [DS audit fix #5] Revert to cold so the next request retries
		// instead of leaving the workspace stuck.
		pp.mu.Lock()
		if final := pp.processes[workspaceID]; final != nil {
			final.Status = StatusCold
		}
		pp.mu.Unlock()
		return nil, fmt.Errorf("EnsureRunning: wait timeout after %ds for %s", timeoutSec, workspaceID)
	})
	// Predictive topology wake: after a successful spawn, pre-warm cold lazy
	// siblings in the same project federation. [ÉPICA 150.N]
	if err == nil {
		go pp.wakeProjectSiblings(workspaceID)
	}
	return err
}

// wakeProjectSiblings wakes all cold lazy workspaces that share the same
// ProjectID as workspaceID. Called in a goroutine after a successful
// EnsureRunning so co-located workspaces are ready before the user
// switches to them. Skips workspaceID itself and non-project workspaces.
// [ÉPICA 150.N]
func (pp *ProcessPool) wakeProjectSiblings(workspaceID string) {
	pp.mu.RLock()
	src, ok := pp.processes[workspaceID]
	if !ok || src.ProjectID == "" {
		pp.mu.RUnlock()
		return
	}
	projectID := src.ProjectID
	var siblings []string
	for id, p := range pp.processes {
		if id == workspaceID {
			continue
		}
		if p.ProjectID != projectID {
			continue
		}
		if p.Status == StatusCold && p.Lifecycle == "lazy" {
			siblings = append(siblings, id)
		}
	}
	pp.mu.RUnlock()
	for _, sib := range siblings {
		go func() {
			log.Printf("[NEXUS-EVENT] topology_prewarm id=%s triggered_by=%s", sib, workspaceID)
			if err := pp.EnsureRunning(sib); err != nil {
				log.Printf("[NEXUS-EVENT] topology_prewarm_fail id=%s err=%v", sib, err)
			}
		}()
	}
}

// StopAll gracefully shuts down all child processes. [SRE-68.1.3]
func (pp *ProcessPool) StopAll() {
	pp.mu.RLock()
	ids := make([]string, 0, len(pp.processes))
	for id := range pp.processes {
		ids = append(ids, id)
	}
	pp.mu.RUnlock()

	for _, id := range ids {
		if err := pp.Stop(id); err != nil {
			log.Printf("[NEXUS] Error stopping %s: %v", id, err)
		}
	}
	pp.cancel()
}

// WatchDog monitors child processes via HTTP health checks and restarts
// unhealthy ones. [SRE-80.B.3] Replaces the old status-based stub.
//
// Per iteration:
//  1. Skip StatusStopped / StatusStarting / StatusQuarantined.
//  2. GET <bind>:<port><health_endpoint> with 2s timeout.
//  3. Success → reset failure counter, mark Running.
//  4. Failure → increment failures; if ≥ failure_threshold, mark Unhealthy
//     and schedule restart (unless rate-limited by MaxRestartsPerHour).
func (pp *ProcessPool) WatchDog(ctx context.Context) {
	if !pp.cfg.Nexus.Watchdog.Enabled {
		log.Printf("[NEXUS-WATCHDOG] disabled via cfg")
		return
	}
	interval := time.Duration(pp.cfg.Nexus.Watchdog.CheckIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pp.watchdogTick()
		}
	}
}

// checkChildHealth does a single GET /health against the child and returns
// true when the response is HTTP 200. [Épica 302.E.2]
func (pp *ProcessPool) checkChildHealth(p *WorkspaceProcess, client *http.Client) bool {
	url := fmt.Sprintf("http://%s:%d%s",
		healthProbeHost(pp.cfg.Nexus.BindAddr),
		p.Port,
		pp.cfg.Nexus.Watchdog.HealthEndpoint,
	)
	resp, err := client.Get(url)
	ok := err == nil && resp != nil && resp.StatusCode == http.StatusOK
	if resp != nil {
		resp.Body.Close()
	}
	return ok
}

// watchdogTick runs one pass of the health check loop.
func (pp *ProcessPool) watchdogTick() {
	pp.mu.RLock()
	targets := make([]*WorkspaceProcess, 0, len(pp.processes))
	for _, entry := range pp.processes {
		if entry.Status == StatusRunning || entry.Status == StatusUnhealthy {
			targets = append(targets, entry)
		}
	}
	pp.mu.RUnlock()

	// [SRE-103.B] Child health target is loopback server-controlled.
	client := sre.SafeInternalHTTPClient(2)
	for _, p := range targets {
		ok := pp.checkChildHealth(p, client)

		pp.mu.Lock()
		// Audit S9-5 fix (PILAR XXVIII 143.C): failures count lives on the
		// pool-level tracker, NOT on the WorkspaceProcess. The tracker
		// survives Start()'s pointer replacement so an attacker cannot reset
		// the failure counter by triggering a single restart and then
		// crashing the new process.
		tracker := pp.getOrCreateTracker(p.Entry.ID)
		if ok {
			if tracker.failures > 0 {
				log.Printf("[NEXUS-EVENT] child_recovered id=%s", p.Entry.ID)
			}
			tracker.failures = 0
			p.Status = StatusRunning
			p.LastPing = time.Now()
			pp.mu.Unlock()
			continue
		}
		tracker.failures++
		threshold := pp.cfg.Nexus.Watchdog.FailureThreshold
		if tracker.failures < threshold {
			pp.mu.Unlock()
			continue
		}
		p.Status = StatusUnhealthy
		log.Printf("[NEXUS-EVENT] child_unhealthy id=%s failures=%d", p.Entry.ID, tracker.failures)
		pp.mu.Unlock()

		pp.maybeRestart(p)
	}

	// [285.A] Idle edge-trigger — extracted to fireIdleTransitions. [Épica 302.E.2]
	pp.fireIdleTransitions()
}

// fireIdleTransitions fires OnIdleTransition for workspaces that just crossed
// the 300 s idle threshold (edge trigger, not level trigger). [285.A, Épica 302.E.2]
func (pp *ProcessPool) fireIdleTransitions() {
	if pp.OnIdleTransition == nil {
		return
	}
	now := time.Now().Unix()
	const idleThreshold = int64(300)
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	for wsID, entry := range pp.processes {
		if entry.Status != StatusRunning || entry.LastToolCallUnix == 0 {
			continue
		}
		idle := now - atomic.LoadInt64(&entry.LastToolCallUnix)
		isIdleNow := idle >= idleThreshold
		wasIdle := pp.prevIdleState[wsID]
		if isIdleNow && !wasIdle {
			pp.prevIdleState[wsID] = true
			cb := pp.OnIdleTransition
			id := wsID
			go cb(id)
		} else if !isIdleNow && wasIdle {
			pp.prevIdleState[wsID] = false
		}
	}
}

// maybeRestart schedules a restart if auto_restart is enabled and the
// per-hour rate limit is not exceeded. [SRE-80.B.3]
// getOrCreateTracker returns the per-workspace restart tracker, lazily
// creating it on first access. Caller MUST hold pp.mu (write lock — we
// mutate the map). Audit S9-5 (PILAR XXVIII 143.C): the tracker is keyed
// by workspace ID and outlives any individual *WorkspaceProcess pointer,
// so rate-limit state survives Start() replacing the map entry.
func (pp *ProcessPool) getOrCreateTracker(wsID string) *wsRestartTracker {
	t, ok := pp.restartState[wsID]
	if !ok {
		t = &wsRestartTracker{}
		pp.restartState[wsID] = t
	}
	return t
}

func (pp *ProcessPool) maybeRestart(proc *WorkspaceProcess) {
	if !pp.cfg.Nexus.Watchdog.AutoRestart {
		return
	}

	pp.mu.Lock()
	tracker := pp.getOrCreateTracker(proc.Entry.ID)
	now := time.Now()
	cutoff := now.Add(-1 * time.Hour)
	kept := tracker.restartTS[:0]
	for _, ts := range tracker.restartTS {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	tracker.restartTS = kept
	if len(tracker.restartTS) >= pp.cfg.Nexus.Watchdog.MaxRestartsPerHour {
		proc.Status = StatusQuarantined
		pp.mu.Unlock()
		log.Printf("[NEXUS-EVENT] child_quarantined id=%s reason=rate_limit", proc.Entry.ID)
		return
	}
	tracker.restartTS = append(tracker.restartTS, now)
	tracker.restarts++
	proc.Restarts = tracker.restarts // mirror to JSON-visible field
	entry := proc.Entry
	pp.mu.Unlock()

	savedPort := proc.Port
	if proc.cmd != nil && proc.cmd.Process != nil {
		_ = proc.cmd.Process.Kill()
	}
	// Wait for the OS to release the port before calling Start() — without
	// this, the isPortFreeOn check in Start() fires immediately after Kill()
	// while the kernel is still draining the socket, causing a restart loop.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if isPortFreeOn(pp.cfg.Nexus.BindAddr, savedPort) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Printf("[NEXUS-EVENT] child_restarting id=%s attempt=%d", entry.ID, proc.Restarts)
	if err := pp.Start(entry); err != nil {
		log.Printf("[NEXUS-EVENT] child_restart_failed id=%s err=%v", entry.ID, err)
	}
}

// resolvePortCollision handles a port-in-use conflict detected in Start().
// It first tries to adopt the holder (healthy → no disruption), then falls
// back to eviction + port-wait. Called with pp.mu held.
// Returns (true, nil) when the process was adopted — caller must return nil.
// Returns (false, nil) when the port was freed and spawning can proceed.
// Returns (false, err) when the port could not be freed. [SRE-80.C.1-ADOPT]
func (pp *ProcessPool) resolvePortCollision(entry workspace.WorkspaceEntry, port int) (adopted bool, err error) {
	if pp.tryAdoptProcess(entry, port) {
		return true, nil
	}
	evictPortHolder(port)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if isPortFreeOn(pp.cfg.Nexus.BindAddr, port) {
			return false, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Printf("[NEXUS-EVENT] port_collision id=%s port=%d", entry.ID, port)
	return false, fmt.Errorf("port %d is in use; release it or choose a different base", port)
}

// pidOnPort returns the PID of the process listening on the given TCP port,
// or 0 if none can be determined. Uses lsof(8) which is available on macOS
// and most Linux distros. [SRE-80.C.1-ADOPT]
//
//nolint:gosec // G204-LITERAL-BIN: literal binary "lsof" with validated numeric port arg
func pidOnPort(port int) int {
	out, err := exec.Command("lsof", "-ti", fmt.Sprintf(":%d", port)).Output()
	if err != nil || len(out) == 0 {
		return 0
	}
	// lsof may list multiple PIDs separated by newlines; take the first.
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	pid, _ := strconv.Atoi(strings.TrimSpace(line))
	return pid
}

// processNameForPID returns the base process name for pid.
// Linux fast path reads /proc/<pid>/comm (no subprocess). Falls back to ps(1)
// on macOS/BSD. Returns "" when the process cannot be identified.
//
//nolint:gosec // G304: path is always /proc/<int>/comm where <int> is a non-negative integer returned by lsof; cannot escape /proc/
//nolint:gosec // G204-LITERAL-BIN: literal binary "ps" with validated integer PID arg
func processNameForPID(pid int) string {
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil { //nolint:gosec // G304: /proc/<int>/comm
		return strings.TrimSpace(string(data))
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output() //nolint:gosec // G204-LITERAL-BIN
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// tryAdoptProcess checks whether there is already a healthy child listening on
// port and, if so, registers it in the pool without spawning a new process.
// Must be called with pp.mu held (it writes directly to pp.processes).
// Returns true when adoption succeeded. [SRE-80.C.1-ADOPT]
func (pp *ProcessPool) tryAdoptProcess(entry workspace.WorkspaceEntry, port int) bool {
	healthURL := fmt.Sprintf("http://%s:%d%s",
		healthProbeHost(pp.cfg.Nexus.BindAddr), port,
		pp.cfg.Nexus.Watchdog.HealthEndpoint,
	)
	client := sre.SafeInternalHTTPClient(2)
	resp, err := client.Get(healthURL)
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return false
	}
	resp.Body.Close()

	pid := pidOnPort(port)
	if pid <= 0 {
		return false
	}

	proc := &WorkspaceProcess{
		Entry:     entry,
		Port:      port,
		PID:       pid,
		Status:    StatusRunning,
		StartedAt: time.Now(),
		LastPing:  time.Now(),
		adopted:   true,
	}
	pp.processes[entry.ID] = proc
	log.Printf("[NEXUS-EVENT] child_adopted id=%s port=%d pid=%d", entry.ID, port, pid)
	go pp.monitorAdoptedChild(proc)
	return true
}

// evictPortHolder sends SIGKILL to whatever process is listening on port.
// Best-effort: logs failures, never returns an error (callers handle the
// isPortFreeOn retry loop).
// Guard: never kills the current process (self-eviction would crash Nexus).
// [SRE-80.C.1-ADOPT]
func evictPortHolder(port int) {
	pid := pidOnPort(port)
	if pid <= 0 {
		log.Printf("[NEXUS-EVENT] port_evict_skip port=%d (pid not found)", port)
		return
	}
	if pid == os.Getpid() {
		log.Printf("[NEXUS-EVENT] port_evict_skip port=%d (self)", port)
		return
	}
	// [150.O] Refuse to kill a process whose name does not contain "neo-mcp".
	// Prevents accidentally terminating unrelated services that happen to hold
	// the port (e.g. a dev HTTP server) after an unclean shutdown.
	if name := processNameForPID(pid); !strings.Contains(name, "neo-mcp") {
		log.Printf("[NEXUS-EVENT] port_evict_skip port=%d pid=%d name=%q (not neo-mcp)", port, pid, name)
		return
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = p.Kill()
	log.Printf("[NEXUS-EVENT] port_evicted port=%d pid=%d", port, pid)
}

// monitorAdoptedChild polls the process liveness every 5 s via signal(0)
// and marks the workspace stopped when the process exits.
// Runs in its own goroutine. [SRE-80.C.1-ADOPT]
func (pp *ProcessPool) monitorAdoptedChild(proc *WorkspaceProcess) {
	p, err := os.FindProcess(proc.PID)
	if err != nil {
		return
	}
	for {
		time.Sleep(5 * time.Second)
		if e := p.Signal(syscall.Signal(0)); e != nil {
			break
		}
		pp.mu.Lock()
		proc.LastPing = time.Now()
		pp.mu.Unlock()
	}
	pp.mu.Lock()
	proc.Status = StatusStopped
	proc.PID = 0
	pp.mu.Unlock()
	log.Printf("[NEXUS-EVENT] adopted_child_exited id=%s port=%d", proc.Entry.ID, proc.Port)
}

// List returns all workspace processes with their status. [SRE-68.3.1]
func (pp *ProcessPool) List() []WorkspaceProcess {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	result := make([]WorkspaceProcess, 0, len(pp.processes))
	for _, p := range pp.processes {
		cp := *p
		// [Épica 248.A] Atomic reads for activity fields — RecordToolCall writes these
		// without holding the pool lock, so a plain struct copy would be a data race.
		cp.LastToolCallUnix = atomic.LoadInt64(&p.LastToolCallUnix)
		cp.ToolCallCount = atomic.LoadInt64(&p.ToolCallCount)
		cp.LastMemexSyncUnix = atomic.LoadInt64(&p.LastMemexSyncUnix) // [285.E]
		result = append(result, cp)
	}
	return result
}

// SiblingsByProject returns all processes that belong to a given project. [292.D]
func (pp *ProcessPool) SiblingsByProject(projectID string) []WorkspaceProcess {
	if projectID == "" {
		return nil
	}
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	var result []WorkspaceProcess
	for _, p := range pp.processes {
		if p.ProjectID != projectID {
			continue
		}
		cp := *p
		cp.LastToolCallUnix = atomic.LoadInt64(&p.LastToolCallUnix)
		cp.ToolCallCount = atomic.LoadInt64(&p.ToolCallCount)
		cp.LastMemexSyncUnix = atomic.LoadInt64(&p.LastMemexSyncUnix)
		result = append(result, cp)
	}
	return result
}

// GetProcess returns the process for a workspace ID.
func (pp *ProcessPool) GetProcess(workspaceID string) (*WorkspaceProcess, bool) {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	p, ok := pp.processes[workspaceID]
	if !ok {
		return nil, false
	}
	cp := *p
	cp.LastToolCallUnix = atomic.LoadInt64(&p.LastToolCallUnix)   // [Épica 248.A]
	cp.ToolCallCount = atomic.LoadInt64(&p.ToolCallCount)         // [Épica 248.A]
	cp.LastMemexSyncUnix = atomic.LoadInt64(&p.LastMemexSyncUnix) // [285.E]
	return &cp, true
}

// RecordToolCall marks activity on a workspace — O(1), lock-free after pointer lookup.
// Also updates project-level counters when the workspace belongs to a project. [Épica 248.B, 283.C]
func (pp *ProcessPool) RecordToolCall(workspaceID string) {
	pp.mu.RLock()
	proc, ok := pp.processes[workspaceID]
	pp.mu.RUnlock()
	if !ok {
		return
	}
	now := time.Now().Unix()
	atomic.StoreInt64(&proc.LastToolCallUnix, now)
	atomic.AddInt64(&proc.ToolCallCount, 1)
	if proc.ProjectID != "" {
		pp.recordProjectActivity(proc.ProjectID, now)
	}
}

// recordProjectActivity updates project-level aggregated counters. [283.C]
func (pp *ProcessPool) recordProjectActivity(projectID string, nowUnix int64) {
	v, _ := pp.projectActivity.LoadOrStore(projectID, &ProjectActivityCounters{})
	counters := v.(*ProjectActivityCounters)
	atomic.StoreInt64(&counters.LastToolCallUnix, nowUnix)
	atomic.AddInt64(&counters.ToolCallCount, 1)
}

// GetProjectActivity returns aggregated activity counters for a project. [283.E]
func (pp *ProcessPool) GetProjectActivity(projectID string) *ProjectActivityCounters {
	v, ok := pp.projectActivity.Load(projectID)
	if !ok {
		return &ProjectActivityCounters{}
	}
	c := v.(*ProjectActivityCounters)
	return &ProjectActivityCounters{
		LastToolCallUnix: atomic.LoadInt64(&c.LastToolCallUnix),
		ToolCallCount:    atomic.LoadInt64(&c.ToolCallCount),
	}
}

// UpdateLastMemexSync records that a memex sync completed for wsID at the given unix timestamp. [285.E]
func (pp *ProcessPool) UpdateLastMemexSync(wsID string, nowUnix int64) {
	pp.mu.RLock()
	proc, ok := pp.processes[wsID]
	pp.mu.RUnlock()
	if ok {
		atomic.StoreInt64(&proc.LastMemexSyncUnix, nowUnix)
	}
}

// ApplyTopology sets ProjectID on all running processes according to the topology index. [283.B/F]
// Called after StartAll and on SIGHUP rebuild so that RecordToolCall can aggregate by project.
func (pp *ProcessPool) ApplyTopology(topo *TopologyIndex) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	for wsID, proc := range pp.processes {
		proc.ProjectID = topo.ProjectForWorkspace(wsID)
	}
}
