package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultNeoConfig(t *testing.T) {
	cfg := defaultNeoConfig()

	if cfg.Server.Port == 0 {
		t.Error("default server port should not be 0")
	}
	if cfg.Server.Mode != "pair" {
		t.Errorf("default mode should be pair, got %s", cfg.Server.Mode)
	}
	if cfg.RAG.ChunkSize == 0 {
		t.Error("default chunk size should not be 0")
	}
	if cfg.RAG.OllamaConcurrency == 0 {
		t.Error("default ollama concurrency should not be 0")
	}
	if cfg.Sentinel.HeapThresholdMB == 0 {
		t.Error("default sentinel heap threshold should not be 0")
	}
	if cfg.Kinetic.AnomalyThresholdSigma == 0 {
		t.Error("default kinetic anomaly threshold should not be 0")
	}
	if cfg.Coldstore.MaxOpenConns == 0 {
		t.Error("default coldstore max open conns should not be 0")
	}
	if cfg.HyperGraph.MaxImpactDepth == 0 {
		t.Error("default hypergraph max impact depth should not be 0")
	}
}

func TestLoadConfigCreatesFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "neo.yaml")

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Server.Mode != "pair" {
		t.Errorf("expected pair mode, got %s", cfg.Server.Mode)
	}

	// File should have been created
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		t.Error("neo.yaml should have been created")
	}
}

func TestLoadConfigBackfill(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "neo.yaml")

	// Write minimal config
	minimal := []byte("server:\n  mode: fast\n")
	if err := os.WriteFile(cfgPath, minimal, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Server.Mode != "fast" {
		t.Errorf("expected fast mode from file, got %s", cfg.Server.Mode)
	}
	// Backfilled fields should have defaults
	if cfg.Server.GossipPort == 0 {
		t.Error("gossip port should have been backfilled")
	}
	if cfg.Sentinel.HeapThresholdMB == 0 {
		t.Error("sentinel heap threshold should have been backfilled")
	}
	if cfg.Kinetic.AnomalyThresholdSigma == 0 {
		t.Error("kinetic anomaly threshold should have been backfilled")
	}
}

// TestLoadDotEnv verifies that loadDotEnv sets env vars from a file
// and that shell env vars take priority over .env values.
func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")

	content := "TEST_NEO_FOO=from_file\nTEST_NEO_BAR=bar_value\n# comment\n\nINVALID_LINE\n"
	if err := os.WriteFile(envPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	// Pre-set FOO in shell — loadDotEnv must NOT overwrite it.
	t.Setenv("TEST_NEO_FOO", "from_shell")
	// Ensure BAR is not set before the call.
	os.Unsetenv("TEST_NEO_BAR")

	loadDotEnv(envPath)

	if got := os.Getenv("TEST_NEO_FOO"); got != "from_shell" {
		t.Errorf("shell env should win: got %q, want %q", got, "from_shell")
	}
	if got := os.Getenv("TEST_NEO_BAR"); got != "bar_value" {
		t.Errorf("loadDotEnv should set BAR: got %q, want %q", got, "bar_value")
	}
}

// TestLoadDotEnvMissing verifies that a missing .env is silently ignored.
func TestLoadDotEnvMissing(t *testing.T) {
	// Should not panic or return error.
	loadDotEnv("/tmp/neo_nonexistent_env_file_12345")
}

// TestLoadConfigEnvExpansion verifies that ${VAR} in neo.yaml is expanded
// using the environment (including values loaded from .neo/.env).
func TestLoadConfigEnvExpansion(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "neo.yaml")

	// Create .neo/.env adjacent to neo.yaml (loader looks at dir(.neo/.env))
	dotEnvDir := filepath.Join(dir, ".neo")
	if err := os.MkdirAll(dotEnvDir, 0700); err != nil {
		t.Fatal(err)
	}
	dotEnvPath := filepath.Join(dotEnvDir, ".env")
	if err := os.WriteFile(dotEnvPath, []byte("TEST_NEO_DSN=postgres://user:pass@localhost/db\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// neo.yaml references ${TEST_NEO_DSN}
	yaml := "server:\n  mode: fast\ndatabases:\n  - name: testdb\n    driver: postgres\n    dsn: \"${TEST_NEO_DSN}\"\n    max_open_conns: 1\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}

	// Ensure env var is not set before LoadConfig so it comes from .env.
	os.Unsetenv("TEST_NEO_DSN")

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Databases) == 0 {
		t.Fatal("expected at least one database entry")
	}
	if cfg.Databases[0].DSN != "postgres://user:pass@localhost/db" {
		t.Errorf("DSN not expanded: got %q", cfg.Databases[0].DSN)
	}
}

// TestLoadConfigEnvWriteBackPreservesTemplate verifies that when neo.yaml
// contains ${VAR} references and a backfill write-back occurs, the file
// retains the original ${VAR} template — secrets are never written to disk.
func TestLoadConfigEnvWriteBackPreservesTemplate(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "neo.yaml")

	// Set the env var directly (no .env file needed for this test).
	t.Setenv("TEST_NEO_WRITEBACK_DSN", "postgres://secret:hunter2@localhost/db")

	// Minimal config with ${VAR} — missing many fields so needsSave=true.
	original := "server:\n  mode: fast\ndatabases:\n  - name: prod\n    driver: postgres\n    dsn: \"${TEST_NEO_WRITEBACK_DSN}\"\n    max_open_conns: 1\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(cfgPath); err != nil {
		t.Fatal(err)
	}

	// Read what was written back to disk.
	written, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	writtenStr := string(written)
	if strings.Contains(writtenStr, "hunter2") {
		t.Error("secret value must not be written back to neo.yaml")
	}
	if !strings.Contains(writtenStr, "${TEST_NEO_WRITEBACK_DSN}") {
		t.Error("${VAR} template must be preserved in the written-back file")
	}
}

// TestSyncDotEnvExampleAutoGenerated verifies that LoadConfig auto-generates
// .neo/.env.example with placeholder entries for every ${VAR} found in neo.yaml.
func TestSyncDotEnvExampleAutoGenerated(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "neo.yaml")
	dotEnvDir := filepath.Join(dir, ".neo")
	examplePath := filepath.Join(dotEnvDir, ".env.example")

	t.Setenv("TEST_NEO_AUTO_DSN", "postgres://x:y@localhost/z")
	t.Setenv("TEST_NEO_AUTO_KEY", "secret")

	yaml := "server:\n  mode: fast\ndatabases:\n  - name: db\n    driver: postgres\n    dsn: \"${TEST_NEO_AUTO_DSN}\"\n    max_open_conns: 1\nai:\n  base_url: \"${TEST_NEO_AUTO_KEY}\"\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(cfgPath); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf(".env.example not generated: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "TEST_NEO_AUTO_DSN=") {
		t.Errorf(".env.example missing TEST_NEO_AUTO_DSN entry:\n%s", content)
	}
	if !strings.Contains(content, "TEST_NEO_AUTO_KEY=") {
		t.Errorf(".env.example missing TEST_NEO_AUTO_KEY entry:\n%s", content)
	}
	// Values must NOT appear in .env.example
	if strings.Contains(content, "postgres://x:y") {
		t.Error(".env.example must not contain real secret values")
	}
}

// TestSyncDotEnvExampleIdempotent verifies that re-running LoadConfig does not
// duplicate entries in .env.example if they are already present.
func TestSyncDotEnvExampleIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "neo.yaml")
	dotEnvDir := filepath.Join(dir, ".neo")
	examplePath := filepath.Join(dotEnvDir, ".env.example")

	t.Setenv("TEST_NEO_IDEM_DSN", "postgres://a:b@localhost/c")

	yaml := "server:\n  mode: fast\ndatabases:\n  - name: db\n    driver: postgres\n    dsn: \"${TEST_NEO_IDEM_DSN}\"\n    max_open_conns: 1\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}

	// Run twice.
	for i := 0; i < 2; i++ {
		if _, err := LoadConfig(cfgPath); err != nil {
			t.Fatal(err)
		}
	}

	data, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	count := strings.Count(content, "TEST_NEO_IDEM_DSN=")
	if count != 1 {
		t.Errorf("expected exactly 1 occurrence of TEST_NEO_IDEM_DSN=, got %d:\n%s", count, content)
	}
}

// TestSyncDotEnvExampleIgnoresComments verifies that ${VAR} patterns inside YAML
// comment lines are NOT added to .env.example (regression test for the comment-
// stripping fix in syncDotEnvExample).
func TestSyncDotEnvExampleIgnoresComments(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "neo.yaml")
	dotEnvDir := filepath.Join(dir, ".neo")
	examplePath := filepath.Join(dotEnvDir, ".env.example")

	t.Setenv("TEST_NEO_REAL_VAR", "realvalue")

	// YAML has ${COMMENT_VAR} only in a comment and ${TEST_NEO_REAL_VAR} in real config.
	yaml := "server:\n  mode: fast\n# comment with ${COMMENT_VAR} should be ignored\nai:\n  base_url: \"${TEST_NEO_REAL_VAR}\"\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(cfgPath); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf(".env.example not generated: %v", err)
	}
	content := string(data)
	if strings.Contains(content, "COMMENT_VAR=") {
		t.Errorf(".env.example must not contain vars from comment lines:\n%s", content)
	}
	if !strings.Contains(content, "TEST_NEO_REAL_VAR=") {
		t.Errorf(".env.example must contain TEST_NEO_REAL_VAR:\n%s", content)
	}
}

// TestNEOPortOverride verifies that NEO_PORT env var overrides cfg.Server.SSEPort,
// allowing Nexus children to listen on their dynamically-assigned port.
func TestNEOPortOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "neo.yaml")

	yaml := "server:\n  mode: pair\n  sse_port: 8085\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("NEO_PORT", "9142")

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.SSEPort != 9142 {
		t.Errorf("NEO_PORT should override SSEPort: got %d, want 9142", cfg.Server.SSEPort)
	}
}

func TestLoadConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "neo.yaml")

	// First load creates defaults
	cfg1, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	// Second load reads them back
	cfg2, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	if cfg1.Server.Port != cfg2.Server.Port {
		t.Errorf("port mismatch after round-trip: %d vs %d", cfg1.Server.Port, cfg2.Server.Port)
	}
	if cfg1.RAG.ChunkSize != cfg2.RAG.ChunkSize {
		t.Errorf("chunk size mismatch: %d vs %d", cfg1.RAG.ChunkSize, cfg2.RAG.ChunkSize)
	}
	if cfg1.Sentinel.HeapThresholdMB != cfg2.Sentinel.HeapThresholdMB {
		t.Errorf("sentinel heap threshold mismatch: %d vs %d", cfg1.Sentinel.HeapThresholdMB, cfg2.Sentinel.HeapThresholdMB)
	}
}

// TestDiscoverCPGEntryPoint_PrefersCmdMain verifies auto-discovery picks up
// the first `cmd/<name>/main.go` when `package_path` is not configured. [330.G]
func TestDiscoverCPGEntryPoint_PrefersCmdMain(t *testing.T) {
	dir := t.TempDir()
	// cmd/alpha/main.go (second alphabetically in practice if we add zulu too)
	if err := os.MkdirAll(filepath.Join(dir, "cmd", "alpha"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmd", "alpha", "main.go"), []byte("package main\nfunc main(){}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pkgPath, pkgDir := discoverCPGEntryPoint(dir)
	if pkgPath != "./cmd/alpha" {
		t.Errorf("pkgPath = %q, want ./cmd/alpha", pkgPath)
	}
	if pkgDir != "cmd/alpha" {
		t.Errorf("pkgDir = %q, want cmd/alpha", pkgDir)
	}
}

// TestDiscoverCPGEntryPoint_DeterministicOrder verifies that with multiple
// cmd/<name> subdirs the discovery picks the lexicographically-first one
// (`alpha` before `zulu`). [330.G]
func TestDiscoverCPGEntryPoint_DeterministicOrder(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"zulu", "alpha", "mike"} {
		if err := os.MkdirAll(filepath.Join(dir, "cmd", name), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmd", name, "main.go"), []byte("package main\nfunc main(){}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pkgPath, pkgDir := discoverCPGEntryPoint(dir)
	if pkgPath != "./cmd/alpha" || pkgDir != "cmd/alpha" {
		t.Errorf("expected ./cmd/alpha (first lexicographic), got pkgPath=%q pkgDir=%q", pkgPath, pkgDir)
	}
}

// TestDiscoverCPGEntryPoint_NoCmdDir returns empty when no cmd/ directory
// exists — caller falls back to the literal default `./cmd/neo-mcp`. [330.G]
func TestDiscoverCPGEntryPoint_NoCmdDir(t *testing.T) {
	dir := t.TempDir()
	pkgPath, pkgDir := discoverCPGEntryPoint(dir)
	if pkgPath != "" || pkgDir != "" {
		t.Errorf("expected empty for missing cmd/, got pkgPath=%q pkgDir=%q", pkgPath, pkgDir)
	}
}

// TestLoadConfig_AutoDiscoversCPGEntry verifies LoadConfig applies auto-discovery
// when a brand-new workspace has cmd/<name>/main.go but no explicit config. [330.G]
func TestLoadConfig_AutoDiscoversCPGEntry(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "cmd", "myservice"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmd", "myservice", "main.go"), []byte("package main\nfunc main(){}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(filepath.Join(dir, "neo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CPG.PackagePath != "./cmd/myservice" {
		t.Errorf("cfg.CPG.PackagePath = %q, want ./cmd/myservice", cfg.CPG.PackagePath)
	}
	if cfg.CPG.PackageDir != "cmd/myservice" {
		t.Errorf("cfg.CPG.PackageDir = %q, want cmd/myservice", cfg.CPG.PackageDir)
	}
}
