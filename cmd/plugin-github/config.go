// cmd/plugin-github/config.go — multi-tenant config (Area 2.1.A) +
// connection pool (2.1.C) for plugin-github. Mirror of the plugin-jira
// shape so future shared infra (multi-tenant audit, vault rotation)
// migrates cleanly.
//
// Loaded from $HOME/.neo/plugins/github.json. Falls back to the
// legacy single-tenant GITHUB_TOKEN env path when the file is
// absent — operators upgrade incrementally.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/github"
)

// PluginConfig is the multi-tenant manifest. Mirrors the Jira shape
// so the audit + connectivity helpers can run on either plugin via
// the same skeleton.
type PluginConfig struct {
	Version       int                  `json:"version"`
	ActiveProject string               `json:"active_project"`
	APIKeys       map[string]*APIKey   `json:"api_keys"`
	Projects      map[string]*Project  `json:"projects"`
}

// APIKey describes one PAT-backed connection. Auth.Token / TokenRef
// is the secret payload; one of Token (inline, dev only) or TokenRef
// (env / vault key) must be set.
type APIKey struct {
	BaseURL    string         `json:"base_url"`
	Auth       AuthConfig     `json:"auth"`
	RateLimit  RateLimitConfig `json:"rate_limit"`
}

type AuthConfig struct {
	Type     string `json:"type"`      // "PAT"
	Token    string `json:"token"`     // inline (dev)
	TokenRef string `json:"token_ref"` // env:GITHUB_TOKEN | vault:foo
}

// RateLimitConfig matches the GitHub PAT contract: 5000 req/hr global
// per token. Concurrency caps in-flight calls.
type RateLimitConfig struct {
	MaxRequestsPerHour int  `json:"max_requests_per_hour"`
	Concurrency        int  `json:"concurrency"`
	RetryOn429         bool `json:"retry_on_429"`
}

// Project binds a repo to one of the registered api_keys + records
// project-level metadata (jira_ticket_regex feeds cross_ref).
type Project struct {
	APIKeyRef       string `json:"api_key"`
	Owner           string `json:"owner"`
	Repo            string `json:"repo"`
	DefaultBranch   string `json:"default_branch"`
	JiraTicketRegex string `json:"jira_ticket_regex"`
}

// defaultConfigPath is where loadPluginConfig looks first.
const defaultGithubConfigPath = "${HOME}/.neo/plugins/github.json"

// loadGithubPluginConfig reads, validates, and returns the manifest.
// Returns os.ErrNotExist when the file is absent — caller falls
// back to the legacy single-tenant path.
func loadGithubPluginConfig(path string) (*PluginConfig, error) {
	resolved := os.ExpandEnv(path)
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, err
	}
	var cfg PluginConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", resolved, err)
	}
	if cfg.Version != 2 {
		return nil, fmt.Errorf("unsupported config version %d (want 2)", cfg.Version)
	}
	if len(cfg.APIKeys) == 0 {
		return nil, errors.New("at least one api_key is required")
	}
	for name, key := range cfg.APIKeys {
		if key.Auth.Type != "PAT" {
			return nil, fmt.Errorf("api_key %q: only Type=\"PAT\" is supported", name)
		}
		if key.Auth.Token == "" && key.Auth.TokenRef == "" {
			return nil, fmt.Errorf("api_key %q: token or token_ref required", name)
		}
		if key.RateLimit.MaxRequestsPerHour == 0 {
			key.RateLimit.MaxRequestsPerHour = 5000
		}
		if key.RateLimit.Concurrency == 0 {
			key.RateLimit.Concurrency = 10
		}
	}
	for projName, proj := range cfg.Projects {
		if proj.APIKeyRef == "" {
			return nil, fmt.Errorf("project %q: api_key ref required", projName)
		}
		if _, ok := cfg.APIKeys[proj.APIKeyRef]; !ok {
			return nil, fmt.Errorf("project %q references unknown api_key %q", projName, proj.APIKeyRef)
		}
		if proj.Owner == "" || proj.Repo == "" {
			return nil, fmt.Errorf("project %q: owner+repo required", projName)
		}
	}
	if cfg.ActiveProject != "" {
		if _, ok := cfg.Projects[cfg.ActiveProject]; !ok {
			return nil, fmt.Errorf("active_project %q not in projects map", cfg.ActiveProject)
		}
	}
	return &cfg, nil
}

// resolveGithubToken extracts the actual PAT from a key — inline
// when available, env: prefix lookup otherwise. vault: prefix is
// reserved for future Brain Portable integration.
func resolveGithubToken(key *APIKey) (string, error) {
	if key.Auth.Token != "" {
		return key.Auth.Token, nil
	}
	ref := key.Auth.TokenRef
	switch {
	case strings.HasPrefix(ref, "env:"):
		val := os.Getenv(strings.TrimPrefix(ref, "env:"))
		if val == "" {
			return "", fmt.Errorf("env var %q is empty", strings.TrimPrefix(ref, "env:"))
		}
		return val, nil
	case strings.HasPrefix(ref, "vault:"):
		return "", errors.New("vault prefix not implemented yet, use env or inline token")
	default:
		return "", fmt.Errorf("token_ref %q must start with env or vault prefix", ref)
	}
}

// clientPool is the per-tenant connection cache. Lazy-create on
// first use; rebuild on SIGHUP via invalidateAll. Same shape as
// plugin-jira's pool so multi-tenant audit can plug in identically.
type clientPool struct {
	mu      sync.RWMutex
	cfg     *PluginConfig
	entries map[string]*github.Client // keyed by api_key name
}

func newClientPool(cfg *PluginConfig) *clientPool {
	return &clientPool{
		cfg:     cfg,
		entries: make(map[string]*github.Client, len(cfg.APIKeys)),
	}
}

// clientFor returns the client for a project name, building lazily.
// Concurrent callers race once; subsequent reads are RLock-cheap.
func (p *clientPool) clientFor(projectName string) (*github.Client, *Project, error) {
	if projectName == "" {
		projectName = p.cfg.ActiveProject
	}
	proj, ok := p.cfg.Projects[projectName]
	if !ok {
		return nil, nil, fmt.Errorf("project %q not configured", projectName)
	}
	p.mu.RLock()
	if c, ok := p.entries[proj.APIKeyRef]; ok {
		p.mu.RUnlock()
		return c, proj, nil
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	// Re-check under write lock.
	if c, ok := p.entries[proj.APIKeyRef]; ok {
		return c, proj, nil
	}
	key := p.cfg.APIKeys[proj.APIKeyRef]
	token, err := resolveGithubToken(key)
	if err != nil {
		return nil, proj, err
	}
	c, err := github.NewClient(github.Config{
		BaseURL: key.BaseURL,
		Token:   token,
	})
	if err != nil {
		return nil, proj, err
	}
	p.entries[proj.APIKeyRef] = c
	return c, proj, nil
}

// invalidateAll drops every cached client. Wire to SIGHUP for
// credential rotation without a plugin restart. Anchored at the
// package level via _ = (*clientPool).invalidateAll below so the
// linter doesn't drop it; the SIGHUP wire-up is the next chunk.
func (p *clientPool) invalidateAll() {
	p.mu.Lock()
	p.entries = make(map[string]*github.Client, len(p.cfg.APIKeys))
	p.mu.Unlock()
}

// Anchor invalidateAll to the package surface — same pattern used
// in cmd/neo-nexus/notify_subscriber.go to keep helpers
// link-reachable until their first real caller lands.
var _ = (*clientPool)(nil).invalidateAll

// configFileExists short-circuits the loader without surfacing an
// error to the boot path when the file just isn't there yet.
func githubConfigFileExists() bool {
	resolved := os.ExpandEnv(defaultGithubConfigPath)
	if !filepath.IsAbs(resolved) {
		return false
	}
	_, err := os.Stat(resolved)
	return err == nil
}

// auditEvent carries the multi-tenant fields that the audit ledger
// records per call. mirrored from pkg/auth.Event but localized so
// plugin-github's audit log file can be parsed independently of
// plugin-jira's.
type auditEvent struct {
	Timestamp string         `json:"ts"`
	Tenant    string         `json:"tenant"`
	Project   string         `json:"project"`
	Owner     string         `json:"owner"`
	Repo      string         `json:"repo"`
	Action    string         `json:"action"`
	Result    string         `json:"result"`
	Details   map[string]any `json:"details,omitempty"`
}

// appendAuditEvent serializes one event to ~/.neo/audit-github.log
// (operator-readable JSONL). Errors are logged but never block the
// dispatch path — audit failure must not turn into request failure.
// [Area 2.2.F]
func appendAuditEvent(path string, e auditEvent) error {
	if path == "" {
		return errors.New("audit path is empty")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304-CLI-CONSENT: path comes from operator config, not request
	if err != nil {
		return err
	}
	defer f.Close()
	if e.Timestamp == "" {
		e.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(body, '\n')); err != nil {
		return err
	}
	return nil
}

// defaultAuditLogPath returns ~/.neo/audit-github.log.
func defaultAuditLogPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "audit-github.log"
	}
	return filepath.Join(home, ".neo", "audit-github.log")
}
