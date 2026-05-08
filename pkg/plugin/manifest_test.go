package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "plugins.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func loadFromEnv(t *testing.T, path string) (*Manifest, error) {
	t.Helper()
	t.Setenv("NEO_PLUGINS_CONFIG", path)
	return LoadManifest()
}

func TestLoadManifest_MissingFileReturnsEmpty(t *testing.T) {
	t.Setenv("NEO_PLUGINS_CONFIG", filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("expected no error for missing file, got %v", err)
	}
	if m.ManifestVersion != CurrentManifestVersion {
		t.Errorf("manifest_version=%d want %d", m.ManifestVersion, CurrentManifestVersion)
	}
	if len(m.Plugins) != 0 {
		t.Errorf("plugins=%d want 0", len(m.Plugins))
	}
}

func TestLoadManifest_HappyPath(t *testing.T) {
	body := `manifest_version: 1
plugins:
  - name: jira
    description: Atlassian Jira integration
    binary: /usr/local/bin/neo-plugin-jira
    args: ["--verbose"]
    env_from_vault: [JIRA_TOKEN, JIRA_DOMAIN]
    tier: nexus
    namespace_prefix: jira
    enabled: true
  - name: github
    binary: /usr/local/bin/neo-plugin-github
    tier: nexus
    enabled: false
`
	path := writeManifest(t, body)
	m, err := loadFromEnv(t, path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(m.Plugins) != 2 {
		t.Fatalf("plugins=%d want 2", len(m.Plugins))
	}
	jira := m.Plugins[0]
	if jira.Name != "jira" || jira.Tier != TierNexus || jira.NamespacePrefix != "jira" {
		t.Errorf("jira spec mismatch: %+v", jira)
	}
	gh := m.Plugins[1]
	if gh.NamespacePrefix != "github" {
		t.Errorf("namespace_prefix default not applied: %q", gh.NamespacePrefix)
	}
	enabled := m.EnabledPlugins()
	if len(enabled) != 1 || enabled[0].Name != "jira" {
		t.Errorf("EnabledPlugins() = %+v, want [jira]", enabled)
	}
}

func TestLoadManifest_RejectsFutureVersion(t *testing.T) {
	body := `manifest_version: 99
plugins: []
`
	path := writeManifest(t, body)
	if _, err := loadFromEnv(t, path); err == nil {
		t.Fatal("expected error for forward-incompatible version")
	}
}

func TestLoadManifest_RejectsDuplicateName(t *testing.T) {
	body := `manifest_version: 1
plugins:
  - name: jira
    binary: /a
    tier: nexus
    enabled: true
  - name: jira
    binary: /b
    tier: nexus
    enabled: true
`
	path := writeManifest(t, body)
	if _, err := loadFromEnv(t, path); err == nil {
		t.Fatal("expected duplicate-name error")
	}
}

func TestLoadManifest_RejectsInvalidTier(t *testing.T) {
	body := `manifest_version: 1
plugins:
  - name: weird
    binary: /a
    tier: galaxy
    enabled: true
`
	path := writeManifest(t, body)
	if _, err := loadFromEnv(t, path); err == nil {
		t.Fatal("expected invalid-tier error")
	}
}

func TestLoadManifest_RequiresBinaryAndName(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing binary", "manifest_version: 1\nplugins:\n  - name: x\n    tier: nexus\n    enabled: true\n"},
		{"missing name", "manifest_version: 1\nplugins:\n  - binary: /a\n    tier: nexus\n    enabled: true\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeManifest(t, tc.body)
			if _, err := loadFromEnv(t, path); err == nil {
				t.Fatalf("expected validation error for %q", tc.name)
			}
		})
	}
}

func TestLoadManifest_RejectsNegativeVersion(t *testing.T) {
	body := `manifest_version: -1
plugins: []
`
	path := writeManifest(t, body)
	if _, err := loadFromEnv(t, path); err == nil {
		t.Fatal("expected error for negative manifest_version")
	}
}

func TestLoadManifest_RejectsUnsafeName(t *testing.T) {
	cases := []string{
		"../traversal",
		"with/slash",
		"UPPER",
		"with space",
		"<script>",
		"-leading-dash",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			body := "manifest_version: 1\nplugins:\n  - name: \"" + name + "\"\n    binary: /a\n    tier: nexus\n    enabled: true\n"
			path := writeManifest(t, body)
			if _, err := loadFromEnv(t, path); err == nil {
				t.Fatalf("expected error for unsafe name %q", name)
			}
		})
	}
}

func TestLoadManifest_RejectsUnsafeNamespacePrefix(t *testing.T) {
	body := `manifest_version: 1
plugins:
  - name: ok
    binary: /a
    tier: nexus
    namespace_prefix: "../bad"
    enabled: true
`
	path := writeManifest(t, body)
	if _, err := loadFromEnv(t, path); err == nil {
		t.Fatal("expected error for unsafe namespace_prefix")
	}
}

func TestLoadManifest_RejectsEmptyEnvFromVault(t *testing.T) {
	body := `manifest_version: 1
plugins:
  - name: ok
    binary: /a
    tier: nexus
    env_from_vault: ["GOOD", "  "]
    enabled: true
`
	path := writeManifest(t, body)
	if _, err := loadFromEnv(t, path); err == nil {
		t.Fatal("expected error for empty env_from_vault entry")
	}
}

func TestLoadManifest_RejectsWorldReadable(t *testing.T) {
	body := "manifest_version: 1\nplugins: []\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "plugins.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEO_PLUGINS_CONFIG", path)
	_, err := LoadManifest()
	if err == nil {
		t.Fatal("expected error for 0644 mode")
	}
	if !strings.Contains(err.Error(), "too-permissive") {
		t.Errorf("error should mention permissions: %v", err)
	}
}

func TestLoadManifest_RejectsGroupWritable(t *testing.T) {
	body := "manifest_version: 1\nplugins: []\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "plugins.yaml")
	if err := os.WriteFile(path, []byte(body), 0o660); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEO_PLUGINS_CONFIG", path)
	if _, err := LoadManifest(); err == nil {
		t.Error("expected error for group-writable mode 0660")
	}
}

func TestLoadManifest_AcceptsStrict0600(t *testing.T) {
	body := "manifest_version: 1\nplugins: []\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "plugins.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEO_PLUGINS_CONFIG", path)
	if _, err := LoadManifest(); err != nil {
		t.Errorf("strict 0600 should be accepted: %v", err)
	}
}

func TestEnabledPlugins_NilManifestSafe(t *testing.T) {
	var m *Manifest
	if got := m.EnabledPlugins(); got != nil {
		t.Errorf("nil manifest should return nil slice, got %v", got)
	}
}

func TestLoadManifest_DefaultsTierAndNamespacePrefix(t *testing.T) {
	body := `manifest_version: 1
plugins:
  - name: foo
    binary: /a
    enabled: true
`
	path := writeManifest(t, body)
	m, err := loadFromEnv(t, path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := m.Plugins[0]
	if p.Tier != TierNexus {
		t.Errorf("tier default got %q want %q", p.Tier, TierNexus)
	}
	if p.NamespacePrefix != "foo" {
		t.Errorf("namespace_prefix default got %q want %q", p.NamespacePrefix, "foo")
	}
}
