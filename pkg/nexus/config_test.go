package nexus

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadNexusConfig_NoFile returns defaults when no yaml exists. [SRE-80.D.1]
func TestLoadNexusConfig_NoFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NEO_NEXUS_CONFIG", filepath.Join(dir, "does-not-exist.yaml"))

	cfg, err := LoadNexusConfig()
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if cfg.Nexus.DispatcherPort != 9000 {
		t.Errorf("default DispatcherPort want 9000, got %d", cfg.Nexus.DispatcherPort)
	}
	if cfg.Nexus.BindAddr != "127.0.0.1" {
		t.Errorf("default BindAddr want 127.0.0.1, got %q", cfg.Nexus.BindAddr)
	}
	if cfg.Nexus.Child.StdinMode != "devnull" {
		t.Errorf("default StdinMode want devnull, got %q", cfg.Nexus.Child.StdinMode)
	}
	if !cfg.Nexus.Watchdog.Enabled {
		t.Error("default Watchdog.Enabled want true")
	}
}

// TestLoadNexusConfig_FromEnvPath loads yaml when NEO_NEXUS_CONFIG points
// to an existing file.
func TestLoadNexusConfig_FromEnvPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus.yaml")
	yaml := `nexus:
  dispatcher_port: 9500
  bind_addr: "0.0.0.0"
  port_range_base: 9200
  port_range_size: 50
  child:
    stdin_mode: "inherit"
    startup_timeout_seconds: 30
  watchdog:
    enabled: false
    failure_threshold: 10
  api:
    auth_token: "secret123"
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEO_NEXUS_CONFIG", path)

	cfg, err := LoadNexusConfig()
	if err != nil {
		t.Fatalf("LoadNexusConfig: %v", err)
	}
	if cfg.Nexus.DispatcherPort != 9500 {
		t.Errorf("DispatcherPort want 9500, got %d", cfg.Nexus.DispatcherPort)
	}
	if cfg.Nexus.BindAddr != "0.0.0.0" {
		t.Errorf("BindAddr want 0.0.0.0, got %q", cfg.Nexus.BindAddr)
	}
	if cfg.Nexus.PortRangeBase != 9200 {
		t.Errorf("PortRangeBase want 9200, got %d", cfg.Nexus.PortRangeBase)
	}
	if cfg.Nexus.Child.StdinMode != "inherit" {
		t.Errorf("StdinMode want inherit, got %q", cfg.Nexus.Child.StdinMode)
	}
	if cfg.Nexus.Child.StartupTimeoutSeconds != 30 {
		t.Errorf("StartupTimeoutSeconds want 30, got %d", cfg.Nexus.Child.StartupTimeoutSeconds)
	}
	if cfg.Nexus.Watchdog.Enabled {
		t.Error("Watchdog.Enabled want false")
	}
	if cfg.Nexus.Watchdog.FailureThreshold != 10 {
		t.Errorf("FailureThreshold want 10, got %d", cfg.Nexus.Watchdog.FailureThreshold)
	}
	if cfg.Nexus.API.AuthToken != "secret123" {
		t.Errorf("AuthToken want secret123, got %q", cfg.Nexus.API.AuthToken)
	}
}

// TestLoadNexusConfig_Backfill fills missing fields with defaults when a
// partial yaml is provided.
func TestLoadNexusConfig_Backfill(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus.yaml")
	// Minimal yaml: only dispatcher_port set, everything else must be defaulted.
	yaml := `nexus:
  dispatcher_port: 9999
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEO_NEXUS_CONFIG", path)

	cfg, err := LoadNexusConfig()
	if err != nil {
		t.Fatalf("LoadNexusConfig: %v", err)
	}
	if cfg.Nexus.DispatcherPort != 9999 {
		t.Errorf("DispatcherPort want 9999, got %d", cfg.Nexus.DispatcherPort)
	}
	if cfg.Nexus.BindAddr != "127.0.0.1" {
		t.Errorf("BindAddr want default 127.0.0.1, got %q", cfg.Nexus.BindAddr)
	}
	if cfg.Nexus.PortRangeBase != 9100 {
		t.Errorf("PortRangeBase want default 9100, got %d", cfg.Nexus.PortRangeBase)
	}
	if cfg.Nexus.Child.StdinMode != "devnull" {
		t.Errorf("StdinMode want default devnull, got %q", cfg.Nexus.Child.StdinMode)
	}
	if cfg.Nexus.Watchdog.CheckIntervalSeconds != 10 {
		t.Errorf("CheckIntervalSeconds want default 10, got %d", cfg.Nexus.Watchdog.CheckIntervalSeconds)
	}
	if cfg.Nexus.Watchdog.MaxRestartsPerHour != 5 {
		t.Errorf("MaxRestartsPerHour want default 5, got %d", cfg.Nexus.Watchdog.MaxRestartsPerHour)
	}
}

// TestLoadNexusConfig_ExpandPaths converts leading ~/ to absolute paths.
func TestLoadNexusConfig_ExpandPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus.yaml")
	yaml := `nexus:
  registry_file: "~/custom/workspaces.json"
  ports_file: "~/custom/nexus_ports.json"
  logs:
    dir: "~/custom/logs"
  bin_path: "~/custom/bin/neo-mcp"
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEO_NEXUS_CONFIG", path)

	cfg, err := LoadNexusConfig()
	if err != nil {
		t.Fatalf("LoadNexusConfig: %v", err)
	}
	home, _ := os.UserHomeDir()
	wantRegistry := filepath.Join(home, "custom", "workspaces.json")
	if cfg.Nexus.RegistryFile != wantRegistry {
		t.Errorf("RegistryFile want %q, got %q", wantRegistry, cfg.Nexus.RegistryFile)
	}
	wantPorts := filepath.Join(home, "custom", "nexus_ports.json")
	if cfg.Nexus.PortsFile != wantPorts {
		t.Errorf("PortsFile want %q, got %q", wantPorts, cfg.Nexus.PortsFile)
	}
	wantLogs := filepath.Join(home, "custom", "logs")
	if cfg.Nexus.Logs.Dir != wantLogs {
		t.Errorf("Logs.Dir want %q, got %q", wantLogs, cfg.Nexus.Logs.Dir)
	}
	wantBin := filepath.Join(home, "custom", "bin", "neo-mcp")
	if cfg.Nexus.BinPath != wantBin {
		t.Errorf("BinPath want %q, got %q", wantBin, cfg.Nexus.BinPath)
	}
}

// TestLoadNexusConfig_MalformedYaml returns an error for broken yaml.
func TestLoadNexusConfig_MalformedYaml(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus.yaml")
	if err := os.WriteFile(path, []byte("not: valid: yaml: : :"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEO_NEXUS_CONFIG", path)

	_, err := LoadNexusConfig()
	if err == nil {
		t.Error("expected parse error, got nil")
	}
}

// TestLoadNexusConfig_PluginsDisabledByDefault verifies the PILAR XXIII
// feature flag is opt-in. [123.6]
func TestLoadNexusConfig_PluginsDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NEO_NEXUS_CONFIG", filepath.Join(dir, "missing.yaml"))

	cfg, err := LoadNexusConfig()
	if err != nil {
		t.Fatalf("LoadNexusConfig: %v", err)
	}
	if cfg.Nexus.Plugins.Enabled {
		t.Error("Plugins.Enabled default want false, got true")
	}
}

func TestLoadNexusConfig_PluginsEnabledFromYaml(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus.yaml")
	body := `nexus:
  plugins:
    enabled: true
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEO_NEXUS_CONFIG", path)

	cfg, err := LoadNexusConfig()
	if err != nil {
		t.Fatalf("LoadNexusConfig: %v", err)
	}
	if !cfg.Nexus.Plugins.Enabled {
		t.Error("Plugins.Enabled want true after yaml override, got false")
	}
}
