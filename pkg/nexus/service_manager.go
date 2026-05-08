package nexus

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// ServiceStatus describes the runtime state of a managed system service.
type ServiceStatus string

const (
	ServiceStopped   ServiceStatus = "stopped"
	ServiceStarting  ServiceStatus = "starting"
	ServiceHealthy   ServiceStatus = "healthy"
	ServiceUnhealthy ServiceStatus = "unhealthy"
)

// ManagedService tracks a single Ollama instance managed by Nexus.
type ManagedService struct {
	Name      string
	Cfg       OllamaServiceConfig
	Status    ServiceStatus
	PID       int
	cmd       *exec.Cmd
	logFile   *os.File
	restarts  int
	restartTS []time.Time
	mu        sync.Mutex
}

// ServiceInfo is a read-only snapshot returned by ServiceManager.List().
type ServiceInfo struct {
	Name    string        `json:"name"`
	Status  ServiceStatus `json:"status"`
	Port    int           `json:"port"`
	PID     int           `json:"pid"`
	Enabled bool          `json:"enabled"`
}

// ServiceManager manages system-level services (Ollama instances) as dependencies
// of the Nexus child pool. Services are started before children and watched by the
// same cadence as WatchDog, reusing Nexus watchdog config (failure_threshold, etc.).
type ServiceManager struct {
	cfg      *NexusConfig
	services map[string]*ManagedService
	mu       sync.RWMutex
}

// NewServiceManager builds a ServiceManager from the Nexus config.
// Only services with Enabled: true are tracked; disabled services are skipped
// entirely so there is zero overhead when the feature is not configured.
func NewServiceManager(cfg *NexusConfig) *ServiceManager {
	sm := &ServiceManager{
		cfg:      cfg,
		services: make(map[string]*ManagedService),
	}
	if cfg.Nexus.Services.OllamaLLM.Enabled {
		sm.services["ollama_llm"] = &ManagedService{
			Name:   "ollama_llm",
			Cfg:    cfg.Nexus.Services.OllamaLLM,
			Status: ServiceStopped,
		}
	}
	if cfg.Nexus.Services.OllamaEmbed.Enabled {
		sm.services["ollama_embed"] = &ManagedService{
			Name:   "ollama_embed",
			Cfg:    cfg.Nexus.Services.OllamaEmbed,
			Status: ServiceStopped,
		}
	}
	return sm
}

// EnsureAll starts all enabled services in parallel and waits for them to become
// healthy (or their health_timeout_seconds to expire) before returning.
// Returns immediately when no services are enabled (the default).
// Non-fatal: a partial failure is logged and children are started anyway —
// embeddings degrade gracefully via circuit breaker + grep fallback.
func (sm *ServiceManager) EnsureAll(ctx context.Context) error {
	sm.mu.RLock()
	svcs := make([]*ManagedService, 0, len(sm.services))
	for _, svc := range sm.services {
		svcs = append(svcs, svc)
	}
	sm.mu.RUnlock()

	if len(svcs) == 0 {
		return nil
	}

	errCh := make(chan error, len(svcs))
	for _, svc := range svcs {
		go func() { errCh <- sm.ensureService(ctx, svc) }()
	}

	var errs []string
	for range svcs {
		if err := <-errCh; err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// ensureService checks if the service is already healthy (externally started or
// previously started by Nexus), starts it if not, then waits for health.
// Also schedules ensure_models in a background goroutine so model pulls never
// block the boot sequence regardless of model size.
func (sm *ServiceManager) ensureService(ctx context.Context, svc *ManagedService) error {
	if sm.isHealthy(svc) {
		svc.mu.Lock()
		svc.Status = ServiceHealthy
		svc.mu.Unlock()
		log.Printf("[NEXUS-EVENT] service_already_healthy name=%s port=%d", svc.Name, svc.Cfg.Port)
		go sm.ensureModels(ctx, svc)
		return nil
	}

	svc.mu.Lock()
	svc.Status = ServiceStarting
	svc.mu.Unlock()
	log.Printf("[NEXUS-EVENT] service_starting name=%s port=%d", svc.Name, svc.Cfg.Port)

	if err := sm.startOllama(svc); err != nil {
		svc.mu.Lock()
		svc.Status = ServiceUnhealthy
		svc.mu.Unlock()
		return fmt.Errorf("start %s: %w", svc.Name, err)
	}

	timeout := time.Duration(svc.Cfg.HealthTimeoutSeconds) * time.Second
	deadline := time.Now().Add(timeout)
	// [SRE-103.A] Ollama health target is loopback server-controlled.
	client := sre.SafeInternalHTTPClient(2)
	bindAddr := svc.Cfg.BindAddr
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}
	healthURL := fmt.Sprintf("http://%s:%d%s", bindAddr, svc.Cfg.Port, svc.Cfg.HealthPath)

	backoff := 500 * time.Millisecond
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		resp, err := client.Get(healthURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				svc.mu.Lock()
				svc.Status = ServiceHealthy
				svc.mu.Unlock()
				log.Printf("[NEXUS-EVENT] service_healthy name=%s port=%d", svc.Name, svc.Cfg.Port)
				go sm.ensureModels(ctx, svc)
				return nil
			}
		}
		time.Sleep(backoff)
		if backoff < 4*time.Second {
			backoff *= 2
		}
	}

	svc.mu.Lock()
	svc.Status = ServiceUnhealthy
	svc.mu.Unlock()
	log.Printf("[NEXUS-WARN] service_health_timeout name=%s timeout=%s", svc.Name, timeout)
	return fmt.Errorf("%s unhealthy after %s", svc.Name, timeout)
}

// startOllama executes `ollama serve` with the configured environment.
// Stdout/stderr go to ~/.neo/logs/nexus-service-<name>.log.
// The returned process is tracked in svc.cmd so StopAll can send SIGINT to it.
func (sm *ServiceManager) startOllama(svc *ManagedService) error {
	ollamaPath, err := exec.LookPath("ollama")
	if err != nil {
		return fmt.Errorf("ollama not found in PATH: %w", err)
	}

	cmd := exec.Command(ollamaPath, "serve")

	bindAddr := svc.Cfg.BindAddr
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}
	env := os.Environ()
	env = append(env, fmt.Sprintf("OLLAMA_HOST=%s:%d", bindAddr, svc.Cfg.Port))
	for k, v := range svc.Cfg.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	cmd.Env = env

	// Redirect output to a dedicated log file — never mix with nexus stderr.
	var logFile *os.File
	logDir := sm.cfg.Nexus.Logs.Dir
	if logDir != "" {
		if mkErr := os.MkdirAll(logDir, 0o755); mkErr == nil {
			logPath := fmt.Sprintf("%s/nexus-service-%s.log", logDir, svc.Name)
			rotateIfLarge(logPath, sm.cfg.Nexus.Logs.RotateMB, sm.cfg.Nexus.Logs.KeepFiles)
			if f, fErr := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); fErr == nil {
				logFile = f
				cmd.Stdout = f
				cmd.Stderr = f
			}
		}
	}
	if logFile == nil {
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
	}

	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return fmt.Errorf("exec ollama serve: %w", err)
	}

	svc.mu.Lock()
	svc.cmd = cmd
	svc.logFile = logFile
	svc.PID = cmd.Process.Pid
	svc.mu.Unlock()

	log.Printf("[NEXUS-EVENT] service_started name=%s pid=%d port=%d", svc.Name, cmd.Process.Pid, svc.Cfg.Port)

	// Monitor exit in background — marks unhealthy so watchdog can restart.
	go func() {
		_ = cmd.Wait()
		svc.mu.Lock()
		svc.Status = ServiceUnhealthy
		svc.PID = 0
		if svc.logFile != nil {
			_ = svc.logFile.Close()
			svc.logFile = nil
		}
		svc.mu.Unlock()
		log.Printf("[NEXUS-EVENT] service_exited name=%s", svc.Name)
	}()

	return nil
}

// isHealthy pings the service health endpoint synchronously.
func (sm *ServiceManager) isHealthy(svc *ManagedService) bool {
	bindAddr := svc.Cfg.BindAddr
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}
	// [SRE-103.A] Ollama health target is loopback server-controlled.
	client := sre.SafeInternalHTTPClient(2)
	url := fmt.Sprintf("http://%s:%d%s", bindAddr, svc.Cfg.Port, svc.Cfg.HealthPath)
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ensureModels pulls any models in EnsureModels that are not already present.
// Runs in a background goroutine — never blocks boot regardless of model size.
// Uses the Ollama REST API (/api/pull) directly so it works even when the
// ollama CLI is not in PATH on the PATH visible to Nexus.
func (sm *ServiceManager) ensureModels(ctx context.Context, svc *ManagedService) {
	if len(svc.Cfg.EnsureModels) == 0 {
		return
	}
	bindAddr := svc.Cfg.BindAddr
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}
	baseURL := fmt.Sprintf("http://%s:%d", bindAddr, svc.Cfg.Port)
	present := sm.listModels(baseURL)

	pullTimeout := time.Duration(svc.Cfg.PullTimeoutSeconds) * time.Second
	if pullTimeout <= 0 {
		pullTimeout = 5 * time.Minute
	}

	for _, model := range svc.Cfg.EnsureModels {
		if _, ok := present[model]; ok {
			log.Printf("[NEXUS] service %s model already present: %s", svc.Name, model)
			continue
		}
		log.Printf("[NEXUS-EVENT] service_pulling_model name=%s model=%s", svc.Name, model)
		pullCtx, cancel := context.WithTimeout(ctx, pullTimeout)
		if err := sm.pullModel(pullCtx, baseURL, model); err != nil {
			log.Printf("[NEXUS-WARN] service %s pull %s failed: %v", svc.Name, model, err)
		} else {
			log.Printf("[NEXUS-EVENT] service_model_ready name=%s model=%s", svc.Name, model)
		}
		cancel()
	}
}

// listModels fetches /api/tags and returns a set of present model names.
// Indexes by full name AND short name (without :tag suffix) for flexible matching.
func (sm *ServiceManager) listModels(baseURL string) map[string]struct{} {
	// [SRE-103.A] Ollama loopback.
	client := sre.SafeInternalHTTPClient(5)
	resp, err := client.Get(baseURL + "/api/tags")
	if err != nil {
		return map[string]struct{}{}
	}
	defer resp.Body.Close()

	var result struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return map[string]struct{}{}
	}

	out := make(map[string]struct{}, len(result.Models)*2)
	for _, m := range result.Models {
		out[m.Name] = struct{}{}
		if short, _, ok := strings.Cut(m.Name, ":"); ok {
			out[short] = struct{}{}
		}
	}
	return out
}

// pullModel streams an Ollama model pull via /api/pull (NDJSON).
// Logs status lines so operators can follow large model downloads in nexus logs.
func (sm *ServiceManager) pullModel(ctx context.Context, baseURL, model string) error {
	body := fmt.Sprintf(`{"name":%q}`, model)
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/api/pull", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// [SRE-103.A] Ollama model pull — loopback; pull can take long for 19GB
	// models so use a larger timeout (10 min).
	client := sre.SafeInternalHTTPClient(600)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("pull %s: %w", model, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pull %s: HTTP %d", model, resp.StatusCode)
	}

	var line struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	dec := json.NewDecoder(resp.Body)
	for dec.More() {
		line.Status = ""
		line.Error = ""
		if err := dec.Decode(&line); err != nil {
			break
		}
		if line.Status != "" {
			log.Printf("[NEXUS] pull %s: %s", model, line.Status)
		}
	}
	if line.Error != "" {
		return fmt.Errorf("pull %s: %s", model, line.Error)
	}
	return nil
}

// WatchServices monitors all managed services via HTTP health checks and restarts
// unhealthy ones using the same policy as WatchDog (auto_restart, max_restarts_per_hour).
// Runs as a goroutine alongside pool.WatchDog in cmd/neo-nexus/main.go.
func (sm *ServiceManager) WatchServices(ctx context.Context) {
	if len(sm.services) == 0 {
		return
	}
	interval := time.Duration(sm.cfg.Nexus.Watchdog.CheckIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sm.watchTick(ctx)
		}
	}
}

func (sm *ServiceManager) watchTick(ctx context.Context) {
	sm.mu.RLock()
	svcs := make([]*ManagedService, 0, len(sm.services))
	for _, svc := range sm.services {
		svcs = append(svcs, svc)
	}
	sm.mu.RUnlock()

	for _, svc := range svcs {
		svc.mu.Lock()
		status := svc.Status
		svc.mu.Unlock()

		// Skip services still in initial boot — verifyBoot owns their state.
		if status == ServiceStarting {
			continue
		}

		healthy := sm.isHealthy(svc)

		svc.mu.Lock()
		if healthy {
			if svc.Status != ServiceHealthy {
				log.Printf("[NEXUS-EVENT] service_recovered name=%s", svc.Name)
			}
			svc.Status = ServiceHealthy
			svc.mu.Unlock()
			continue
		}
		prev := svc.Status
		svc.Status = ServiceUnhealthy
		svc.mu.Unlock()

		if prev != ServiceUnhealthy {
			log.Printf("[NEXUS-EVENT] service_unhealthy name=%s", svc.Name)
		}
		sm.maybeRestartService(ctx, svc)
	}
}

func (sm *ServiceManager) maybeRestartService(ctx context.Context, svc *ManagedService) {
	if !sm.cfg.Nexus.Watchdog.AutoRestart {
		return
	}

	svc.mu.Lock()
	now := time.Now()
	cutoff := now.Add(-1 * time.Hour)
	kept := svc.restartTS[:0]
	for _, ts := range svc.restartTS {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	svc.restartTS = kept
	if len(svc.restartTS) >= sm.cfg.Nexus.Watchdog.MaxRestartsPerHour {
		svc.mu.Unlock()
		log.Printf("[NEXUS-EVENT] service_quarantined name=%s reason=rate_limit", svc.Name)
		return
	}
	svc.restartTS = append(svc.restartTS, now)
	svc.restarts++
	attempt := svc.restarts
	// Kill existing process if we started it — never kill a user-managed Ollama.
	if svc.cmd != nil && svc.cmd.Process != nil {
		_ = svc.cmd.Process.Kill()
	}
	svc.mu.Unlock()

	log.Printf("[NEXUS-EVENT] service_restarting name=%s attempt=%d", svc.Name, attempt)
	if err := sm.ensureService(ctx, svc); err != nil {
		log.Printf("[NEXUS-EVENT] service_restart_failed name=%s err=%v", svc.Name, err)
	}
}

// StopAll gracefully stops all services that Nexus started (svc.cmd != nil).
// Services that were already running when Nexus launched are left untouched.
func (sm *ServiceManager) StopAll() {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	for _, svc := range sm.services {
		svc.mu.Lock()
		cmd := svc.cmd
		name := svc.Name
		pid := svc.PID
		svc.mu.Unlock()

		if cmd == nil || cmd.Process == nil {
			continue
		}
		log.Printf("[NEXUS] stopping service %s pid=%d", name, pid)
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func(c *exec.Cmd) {
			_ = c.Wait()
			close(done)
		}(cmd)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
		svc.mu.Lock()
		svc.Status = ServiceStopped
		svc.PID = 0
		if svc.logFile != nil {
			_ = svc.logFile.Close()
			svc.logFile = nil
		}
		svc.mu.Unlock()
	}
}

// List returns status snapshots for all configured services (enabled or not).
func (sm *ServiceManager) List() []ServiceInfo {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	out := make([]ServiceInfo, 0, len(sm.services))
	for _, svc := range sm.services {
		svc.mu.Lock()
		out = append(out, ServiceInfo{
			Name:    svc.Name,
			Status:  svc.Status,
			Port:    svc.Cfg.Port,
			PID:     svc.PID,
			Enabled: svc.Cfg.Enabled,
		})
		svc.mu.Unlock()
	}
	return out
}
