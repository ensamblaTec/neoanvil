// Package plugin defines the manifest schema and loader for subprocess
// MCP plugins. See ADR-005 for the architecture decision: plugins are
// separate MCP servers spawned by Nexus, declared in ~/.neo/plugins.yaml.
//
// Resolution order for the manifest path:
//  1. $NEO_PLUGINS_CONFIG env var (absolute path)
//  2. ~/.neo/plugins.yaml
//  3. built-in empty manifest (no plugins)
package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// nameRegex restricts plugin Name and NamespacePrefix to a safe alphabet:
// lowercase letters, digits, dash, underscore. Prevents path-traversal-style
// values (../, slashes, dots) from polluting tool routing or manifest joins.
var nameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// CurrentManifestVersion is the schema version this build understands.
// Incremented when a backwards-incompatible change is introduced. Loader
// rejects any value greater than this (forward-incompatible) but accepts 0
// (treated as "unversioned, assume current").
const CurrentManifestVersion = 1

// TierWorkspace, TierProject, TierNexus enumerate the valid scopes a plugin
// may declare. The tier governs lifecycle and visibility — a workspace-tier
// plugin is spawned per-workspace, a nexus-tier plugin is a singleton.
const (
	TierWorkspace = "workspace"
	TierProject   = "project"
	TierNexus     = "nexus"
)

// Manifest is the top-level schema serialized to plugins.yaml.
type Manifest struct {
	ManifestVersion int           `yaml:"manifest_version"`
	Plugins         []*PluginSpec `yaml:"plugins"`
}

// PluginSpec describes a single subprocess plugin registered with Nexus.
type PluginSpec struct {
	Name              string   `yaml:"name"`
	Description       string   `yaml:"description,omitempty"`
	Binary            string   `yaml:"binary"`
	Args              []string `yaml:"args,omitempty"`
	EnvFromVault      []string `yaml:"env_from_vault,omitempty"`
	Tier              string   `yaml:"tier"`
	NamespacePrefix   string   `yaml:"namespace_prefix,omitempty"`
	Enabled           bool     `yaml:"enabled"`
	MaxMemoryMB       int      `yaml:"max_memory_mb,omitempty"`       // [P-QUOTA] RSS limit; 0 = unlimited. Enforced via cgroup on Linux, watchdog on macOS.
	MaxCPUPercent     int      `yaml:"max_cpu_percent,omitempty"`     // [P-QUOTA] CPU throttle 1-100; 0 = unlimited.
	AllowedWorkspaces []string `yaml:"allowed_workspaces,omitempty"` // [P-WSACL 147.C] Default-deny: empty = NO workspace may call this plugin. Use ["*"] to allow all, or list specific workspace IDs.
	// AutoRestartOnZombie enables automatic respawn when the health monitor
	// detects the plugin has been in a zombie state (tools_registered=[] or
	// consecutive poll errors) for longer than the zombie threshold (60s).
	// Defaults to true via applyDefaults (nil == not-set → true). Set
	// `auto_restart_on_zombie: false` in plugins.yaml to opt out. [ÉPICA 152.D]
	AutoRestartOnZombie *bool `yaml:"auto_restart_on_zombie,omitempty"`
}

// WantsAutoRestart returns true when zombie auto-restart is enabled for this
// spec (default true; false only when operator explicitly set
// `auto_restart_on_zombie: false` in plugins.yaml). Safe to call on nil Spec.
func (s *PluginSpec) WantsAutoRestart() bool {
	if s == nil || s.AutoRestartOnZombie == nil {
		return true
	}
	return *s.AutoRestartOnZombie
}

// DefaultManifestPath returns ~/.neo/plugins.yaml (or "" if the home dir
// cannot be resolved).
func DefaultManifestPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".neo", "plugins.yaml")
}

// resolveManifestPath returns the path that LoadManifest will read, or "" if
// no manifest file exists.
func resolveManifestPath() string {
	if env := strings.TrimSpace(os.Getenv("NEO_PLUGINS_CONFIG")); env != "" {
		return env
	}
	return DefaultManifestPath()
}

// LoadManifest reads, parses, and validates the plugin manifest. When no
// manifest file is present the returned Manifest is empty (no plugins).
//
// Refuses to load when the file's mode permits group/other access — the
// manifest declares which binaries Nexus will spawn at boot, so a less
// restrictive mode opens an arbitrary-binary execution vector if any
// other local process can write to the file. SSH-style strict mode check.
func LoadManifest() (*Manifest, error) {
	path := resolveManifestPath()
	if path == "" {
		return &Manifest{ManifestVersion: CurrentManifestVersion}, nil
	}

	if err := checkManifestPermissions(path); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path) //nolint:gosec // G304-WORKSPACE-CANON: path is $NEO_PLUGINS_CONFIG or ~/.neo/plugins.yaml, both pinned at boot. Permissions verified above.
	if err != nil {
		if os.IsNotExist(err) {
			return &Manifest{ManifestVersion: CurrentManifestVersion}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	m := &Manifest{}
	if err := yaml.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	m.applyDefaults()
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return m, nil
}

// checkManifestPermissions returns nil when path is missing or has a
// strict mode (no group/other access bits set: 0o077 mask is zero). Any
// other mode returns a fail-closed error with the actual mode in octal
// and a remediation hint. Caller treats os.IsNotExist as benign upstream.
func checkManifestPermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf(
			"plugins manifest %s has too-permissive mode %#o — fix with `chmod 0600 %s` "+
				"(group/other must have NO access; this file declares which binaries Nexus will spawn)",
			path, mode, path)
	}
	return nil
}

// applyDefaults backfills omitted fields with conventional defaults.
func (m *Manifest) applyDefaults() {
	if m.ManifestVersion == 0 {
		m.ManifestVersion = CurrentManifestVersion
	}
	for _, p := range m.Plugins {
		if p == nil {
			continue
		}
		if p.Tier == "" {
			p.Tier = TierNexus
		}
		if p.NamespacePrefix == "" {
			p.NamespacePrefix = p.Name
		}
		// [152.D] Default true — nil means "not set in YAML" (opt-in to restart).
		// Operator writes `auto_restart_on_zombie: false` to disable per plugin.
		if p.AutoRestartOnZombie == nil {
			v := true
			p.AutoRestartOnZombie = &v
		}
	}
}

// Validate enforces schema invariants: known version, unique names, valid
// tiers, non-empty required fields, safe identifier alphabet.
func (m *Manifest) Validate() error {
	if m.ManifestVersion < 0 {
		return fmt.Errorf("manifest_version=%d cannot be negative", m.ManifestVersion)
	}
	if m.ManifestVersion > CurrentManifestVersion {
		return fmt.Errorf("manifest_version=%d is newer than this build supports (%d)", m.ManifestVersion, CurrentManifestVersion)
	}
	seen := make(map[string]struct{}, len(m.Plugins))
	for i, p := range m.Plugins {
		if p == nil {
			return fmt.Errorf("plugins[%d] is nil", i)
		}
		if err := validatePlugin(i, p); err != nil {
			return err
		}
		if _, dup := seen[p.Name]; dup {
			return fmt.Errorf("plugins[%d]: duplicate name %q", i, p.Name)
		}
		seen[p.Name] = struct{}{}
	}
	return nil
}

func validatePlugin(idx int, p *PluginSpec) error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("plugins[%d]: name is required", idx)
	}
	if !nameRegex.MatchString(p.Name) {
		return fmt.Errorf("plugins[%d]: name %q must match %s (lowercase, digits, dash, underscore)", idx, p.Name, nameRegex)
	}
	if strings.TrimSpace(p.Binary) == "" {
		return fmt.Errorf("plugins[%d] (%s): binary is required", idx, p.Name)
	}
	switch p.Tier {
	case TierWorkspace, TierProject, TierNexus:
	default:
		return fmt.Errorf("plugins[%d] (%s): invalid tier %q (want workspace|project|nexus)", idx, p.Name, p.Tier)
	}
	if p.NamespacePrefix != "" && !nameRegex.MatchString(p.NamespacePrefix) {
		return fmt.Errorf("plugins[%d] (%s): namespace_prefix %q must match %s", idx, p.Name, p.NamespacePrefix, nameRegex)
	}
	return validatePluginResources(idx, p)
}

// validatePluginResources checks resource quotas, vault entries, and workspace
// allowlist — fields added after the initial schema. Kept separate to stay
// under CC≤15 in validatePlugin.
func validatePluginResources(idx int, p *PluginSpec) error {
	for j, env := range p.EnvFromVault {
		if strings.TrimSpace(env) == "" {
			return fmt.Errorf("plugins[%d] (%s): env_from_vault[%d] is empty", idx, p.Name, j)
		}
	}
	if p.MaxMemoryMB < 0 {
		return fmt.Errorf("plugins[%d] (%s): max_memory_mb must be >= 0", idx, p.Name)
	}
	if p.MaxCPUPercent < 0 || p.MaxCPUPercent > 100 {
		return fmt.Errorf("plugins[%d] (%s): max_cpu_percent must be 0-100", idx, p.Name)
	}
	for j, ws := range p.AllowedWorkspaces {
		if strings.TrimSpace(ws) == "" {
			return fmt.Errorf("plugins[%d] (%s): allowed_workspaces[%d] is empty", idx, p.Name, j)
		}
		if ws == "*" {
			continue // [147.C] explicit wildcard — allow all workspaces
		}
		if !nameRegex.MatchString(ws) {
			return fmt.Errorf("plugins[%d] (%s): allowed_workspaces[%d] %q must match %s or be \"*\"", idx, p.Name, j, ws, nameRegex)
		}
	}
	return nil
}

// EnabledPlugins returns the subset of plugins that opted in via Enabled:true.
// Convenience for callers (Nexus pool, BRIEFING enumeration). Nil-safe.
func (m *Manifest) EnabledPlugins() []*PluginSpec {
	if m == nil {
		return nil
	}
	out := make([]*PluginSpec, 0, len(m.Plugins))
	for _, p := range m.Plugins {
		if p != nil && p.Enabled {
			out = append(out, p)
		}
	}
	return out
}
