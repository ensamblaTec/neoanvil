// Package nexus — plugin_pool.go
// [PILAR-XXIII/123.3] Subprocess plugin spawner. Mirrors ProcessPool patterns
// for workspace children but tracks plugins by name (not workspace ID).
//
// Plugins are MCP servers spawned as separate OS processes per ADR-005:
// docs/adr/ADR-005-plugin-architecture.md. stdin/stdout become the JSON-RPC
// channel for the MCP handshake (driven by Épica 123.4 — tool aggregation).
// stderr is redirected to a per-plugin log file under logsDir.
//
// Stop() sends SIGTERM. The 5s grace window + SIGKILL escalation belongs to
// Épica 123.5 (gracious shutdown, mirroring Épica 229.1 for workspace children).
package nexus

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/plugin"
)

// VaultLookup resolves a vault entry name to its plaintext value. Returns
// (value, true) on hit. PluginPool calls this for each name in
// PluginSpec.EnvFromVault when building the subprocess environment.
//
// Implementations live elsewhere: pkg/auth.Credentials backed by keystore
// (PILAR XXXIII), or os.LookupEnv as a development fallback. Pass nil to
// disable vault resolution entirely.
type VaultLookup func(name string) (string, bool)

// PluginStatus enumerates lifecycle states.
type PluginStatus string

const (
	PluginStatusStarting PluginStatus = "starting"
	PluginStatusRunning  PluginStatus = "running"
	PluginStatusStopped  PluginStatus = "stopped"
	PluginStatusErrored  PluginStatus = "errored"
)

// PluginProcess holds runtime state of one spawned plugin.
type PluginProcess struct {
	Spec      *plugin.PluginSpec
	Cmd       *exec.Cmd
	Stdin     io.WriteCloser
	Stdout    io.ReadCloser
	Status    PluginStatus
	PID       int
	StartedAt time.Time
	LastErr   error

	done       chan struct{}
	stderrFile *os.File // owned by monitor goroutine; closed after Wait()
}

// Done returns a channel closed when the plugin has exited and the monitor
// goroutine cleaned up. Useful for tests and shutdown paths.
func (pp *PluginProcess) Done() <-chan struct{} { return pp.done }

// PluginPool manages a set of subprocess MCP plugins.
type PluginPool struct {
	mu      sync.RWMutex
	active  map[string]*PluginProcess
	vault   VaultLookup
	logsDir string
}

// NewPluginPool returns an empty pool. logsDir is the directory where each
// plugin's stderr is appended (one file per plugin name). vault may be nil
// (treated as a no-op resolver).
func NewPluginPool(vault VaultLookup, logsDir string) *PluginPool {
	if vault == nil {
		vault = func(string) (string, bool) { return "", false }
	}
	return &PluginPool{
		active:  make(map[string]*PluginProcess),
		vault:   vault,
		logsDir: logsDir,
	}
}

// Start spawns the plugin described by spec. Errors if the plugin is already
// running or the binary cannot be launched. The returned PluginProcess
// exposes Stdin/Stdout for the MCP handshake (driven by Épica 123.4).
func (pp *PluginPool) Start(spec *plugin.PluginSpec) (*PluginProcess, error) {
	if spec == nil {
		return nil, errors.New("nil PluginSpec")
	}
	pp.mu.Lock()
	defer pp.mu.Unlock()

	if _, exists := pp.active[spec.Name]; exists {
		return nil, fmt.Errorf("plugin %q already running", spec.Name)
	}

	// [147.H] Canonicalize binary path — prevents path-traversal via symlinks or
	// relative segments in plugin specs. EvalSymlinks resolves the real path so
	// the actual binary that will execute is unambiguous to the operator.
	canonBinary, err := filepath.EvalSymlinks(spec.Binary)
	if err != nil {
		// Fall back to filepath.Abs if the binary doesn't exist yet (e.g. pre-install).
		canonBinary, err = filepath.Abs(spec.Binary)
		if err != nil {
			return nil, fmt.Errorf("plugin %q binary path %q unresolvable: %w", spec.Name, spec.Binary, err)
		}
	}
	canonBinary = filepath.Clean(canonBinary)

	env, missing := pp.buildEnv(spec)
	cmd := exec.Command(canonBinary, spec.Args...) //nolint:gosec // G204-LITERAL-BIN: canonBinary is EvalSymlinks+Clean resolved from operator-controlled config; not user input.
	cmd.Env = env
	applyParentDeathSignal(cmd) // Linux: SIGKILL on parent death. Darwin: own pgrp.

	stdin, stdout, err := pluginStdioPipes(cmd)
	if err != nil {
		return nil, fmt.Errorf("pipes %s: %w", spec.Name, err)
	}

	logFile, err := pp.openLogFile(spec.Name)
	if err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("open log %s: %w", spec.Name, err)
	}
	// Tee stderr to the per-plugin log file AND a prefix-tagged stream
	// to Nexus's own stderr so operators can `tail -F` a single source
	// for cross-plugin diagnostics. Note: with io.MultiWriter the Go
	// runtime spawns an internal goroutine to copy from the child's
	// stderr pipe — logFile must stay open until cmd.Wait() drains it.
	prefix := newPrefixWriter(fmt.Sprintf("[plugin:%s] ", spec.Name), os.Stderr)
	cmd.Stderr = io.MultiWriter(logFile, prefix)

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		_ = logFile.Close()
		return nil, fmt.Errorf("start %s: %w", spec.Name, err)
	}

	proc := newPluginProcess(spec, cmd, stdin, stdout, missing)
	proc.stderrFile = logFile // monitor closes after Wait
	pp.active[spec.Name] = proc

	go pp.monitor(proc)
	return proc, nil
}

// pluginStdioPipes wires stdin/stdout pipes for a not-yet-started cmd.
func pluginStdioPipes(cmd *exec.Cmd) (io.WriteCloser, io.ReadCloser, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	return stdin, stdout, nil
}

// newPluginProcess assembles the runtime record. Missing vault entries are
// surfaced via LastErr — non-fatal warning, plugin may handle absence itself.
func newPluginProcess(spec *plugin.PluginSpec, cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser, missing []string) *PluginProcess {
	proc := &PluginProcess{
		Spec:      spec,
		Cmd:       cmd,
		Stdin:     stdin,
		Stdout:    stdout,
		Status:    PluginStatusRunning,
		PID:       cmd.Process.Pid,
		StartedAt: time.Now(),
		done:      make(chan struct{}),
	}
	if len(missing) > 0 {
		proc.LastErr = fmt.Errorf("missing vault entries: %v", missing)
	}
	return proc
}

// monitor blocks on cmd.Wait() and cleans up state when the process exits.
// Status becomes Errored on non-nil wait error (covers signal exit too) and
// Stopped on clean exit. Closes the per-plugin stderr log file after
// Wait() drains the io.MultiWriter copy goroutine.
func (pp *PluginPool) monitor(proc *PluginProcess) {
	waitErr := proc.Cmd.Wait()

	if proc.stderrFile != nil {
		_ = proc.stderrFile.Close()
	}

	pp.mu.Lock()
	delete(pp.active, proc.Spec.Name)
	proc.LastErr = waitErr
	if waitErr != nil {
		proc.Status = PluginStatusErrored
	} else {
		proc.Status = PluginStatusStopped
	}
	pp.mu.Unlock()

	close(proc.done)
}

// DefaultStopGrace is the grace window between SIGTERM and SIGKILL. Mirrors
// Épica 229.1 for workspace children. Override per-call via StopGracefully
// when a different timing is required (e.g. fast tests use 0 or 100ms).
const DefaultStopGrace = 5 * time.Second

// Stop terminates the named plugin. Sends SIGTERM, waits DefaultStopGrace
// for clean exit, then escalates to SIGKILL if the process still runs.
// Blocks until the process has actually exited (or the grace expired).
func (pp *PluginPool) Stop(name string) error {
	return pp.StopGracefully(name, DefaultStopGrace)
}

// StopGracefully is Stop with a caller-supplied grace window. A grace of 0
// sends SIGTERM and returns immediately without waiting (useful for tests
// that want to drive the lifecycle explicitly).
func (pp *PluginPool) StopGracefully(name string, grace time.Duration) error {
	pp.mu.RLock()
	proc, ok := pp.active[name]
	pp.mu.RUnlock()
	if !ok {
		return fmt.Errorf("plugin %q not running", name)
	}
	if proc.Cmd.Process == nil {
		return fmt.Errorf("plugin %q has no OS process", name)
	}
	// Signal the entire process group, not just the leader pid, so any
	// sub-processes the plugin spawned (e.g. sh -c, helpers) terminate
	// too — otherwise grandchildren keep the inherited stderr pipe open
	// and block cmd.Wait()'s MultiWriter copy goroutine. Requires
	// Setpgid:true applied at spawn (applyParentDeathSignal does this).
	if err := terminateProcessGroup(proc.Cmd, syscall.SIGTERM); err != nil {
		return fmt.Errorf("sigterm %s: %w", name, err)
	}
	if grace <= 0 {
		return nil
	}
	select {
	case <-proc.done:
		return nil
	case <-time.After(grace):
		if err := terminateProcessGroup(proc.Cmd, syscall.SIGKILL); err != nil {
			return fmt.Errorf("sigkill %s after %s grace: %w", name, grace, err)
		}
		<-proc.done
		return nil
	}
}

// StopAll terminates every active plugin in parallel using DefaultStopGrace.
// Returns the first error encountered. All goroutines are joined before
// returning.
func (pp *PluginPool) StopAll() error {
	return pp.StopAllGracefully(DefaultStopGrace)
}

// StopAllGracefully is StopAll with a caller-supplied grace window. Plugins
// are signalled concurrently — wall-clock cost is ≈ slowest plugin's grace,
// not sum-of-all-graces.
func (pp *PluginPool) StopAllGracefully(grace time.Duration) error {
	pp.mu.RLock()
	names := make([]string, 0, len(pp.active))
	for name := range pp.active {
		names = append(names, name)
	}
	pp.mu.RUnlock()

	if len(names) == 0 {
		return nil
	}

	errCh := make(chan error, len(names))
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			if err := pp.StopGracefully(name, grace); err != nil {
				errCh <- err
			}
		}(n)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

// List returns a snapshot of active plugin processes. Order is undefined.
func (pp *PluginPool) List() []*PluginProcess {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	out := make([]*PluginProcess, 0, len(pp.active))
	for _, proc := range pp.active {
		out = append(out, proc)
	}
	return out
}

// Get returns the named plugin's process, or (nil, false).
func (pp *PluginPool) Get(name string) (*PluginProcess, bool) {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	proc, ok := pp.active[name]
	return proc, ok
}

// buildEnv composes the subprocess environment: parent env + vault-resolved
// secrets. Returns (env, missing) where missing lists vault entries declared
// in EnvFromVault that could not be resolved.
func (pp *PluginPool) buildEnv(spec *plugin.PluginSpec) ([]string, []string) {
	env := os.Environ()
	var missing []string
	for _, name := range spec.EnvFromVault {
		val, ok := pp.vault(name)
		if !ok {
			missing = append(missing, name)
			continue
		}
		env = append(env, fmt.Sprintf("%s=%s", name, val))
	}
	return env, missing
}

// openLogFile opens (or creates) the per-plugin stderr log in append mode.
// File mode 0600 — secrets in env may leak to stderr, restrict to owner.
func (pp *PluginPool) openLogFile(name string) (*os.File, error) {
	if pp.logsDir == "" {
		return nil, errors.New("PluginPool.logsDir is empty")
	}
	if err := os.MkdirAll(pp.logsDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir logs: %w", err)
	}
	path := filepath.Join(pp.logsDir, fmt.Sprintf("plugin-%s.log", name))
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G304-WORKSPACE-CANON: path is logsDir+"plugin-"+validated-name+".log"; name has been regex-validated by manifest loader, no traversal possible.
}
