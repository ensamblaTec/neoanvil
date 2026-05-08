package config

// org.go — Org-tier config: one level above project. [PILAR LXVII / 354.A]
//
// Hierarchy: workspace → project → org. An org aggregates multiple
// `.neo-project/`s that share directives, cross-project debt, knowledge, and
// a single LLM policy. `.neo-org/neo.yaml` lives in a parent directory above
// the projects and is discovered by walk-up (up to 10 levels, deeper than
// project's 5 so an org can span multiple project trees).

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// OrgConfig describes the contract for a cross-project federation.
//
// Typical layout on disk:
//
//	~/develop/.neo-org/
//	├── neo.yaml              (OrgConfig)
//	├── DIRECTIVES.md         (rules applied to every project)
//	├── DEBT.md               (cross-project debt)
//	├── knowledge/            (markdown sync dir, mirror of org.db)
//	└── db/org.db             (BoltDB, owned by coordinator_project)
//
// [354.A]
type OrgConfig struct {
	OrgName string `yaml:"org_name"`
	// Projects is the list of absolute paths pointing to each project root
	// (the parent of `.neo-project/`). Order matters for coordinator failover
	// — see CoordinatorProject + 354.B amendment.
	Projects []string `yaml:"projects"`
	// KnowledgePath overrides the default `.neo-org/knowledge/` sync dir.
	// Leave empty to use the default.
	KnowledgePath string `yaml:"knowledge_path,omitempty"`
	// SharedMemoryPath overrides the default `.neo-org/db/org.db`.
	SharedMemoryPath string `yaml:"shared_memory_path,omitempty"`
	// CoordinatorProject names which project opens org.db in read-write mode.
	// All other projects open it read-only. Match is by basename OR absolute
	// path. Leave empty for legacy first-project-wins behavior (not recommended
	// in production — race-prone when multiple workspaces boot simultaneously).
	CoordinatorProject string `yaml:"coordinator_project,omitempty"`
	// CoordinatorTakeoverSeconds is the grace window before a non-coordinator
	// project takes over the RW lock when the nominated coordinator has no
	// heartbeats in its workspaces. Default 120s (see 354.B amendment).
	CoordinatorTakeoverSeconds int `yaml:"coordinator_takeover_seconds,omitempty"`
	// DirectivesPath overrides the default `.neo-org/DIRECTIVES.md`.
	DirectivesPath string `yaml:"directives_path,omitempty"`
	// DebtPath overrides the default `.neo-org/DEBT.md`.
	DebtPath string `yaml:"debt_path,omitempty"`
	// Writers is the allowlist of workspace IDs (or basenames) permitted to
	// perform write operations to the org tier (neo_memory store/learn/drop,
	// neo_debt record/resolve scope:"org"). When the list is empty (default)
	// the coordinator project alone may write — matching prior behavior.
	// Non-listed workspaces receive "org: workspace X not in writers allowlist".
	// [361.A]
	Writers []string `yaml:"writers,omitempty"`
	// LLMOverrides, if set, reverse-precedences onto every project merge
	// (project LLMOverrides still win when both are set). Same semantics as
	// ProjectConfig.LLMOverrides. [354.A]
	LLMOverrides *LLMOverrides `yaml:"llm_overrides,omitempty"`
}

const orgConfigFile = "neo.yaml"
const orgConfigDir = ".neo-org"

// maxOrgWalkUp is the ceiling on directory levels walked when searching for
// `.neo-org/`. Deeper than projects (5) because an org can live a couple of
// parents above multiple project trees.
const maxOrgWalkUp = 10

// LoadOrgConfig walks up from startDir for `.neo-org/neo.yaml`. Returns
// (nil, nil) when no org config is found — callers treat this as
// workspace-only or project-only mode. [354.A]
func LoadOrgConfig(startDir string) (*OrgConfig, error) {
	dir := startDir
	for range maxOrgWalkUp {
		candidate := filepath.Join(dir, orgConfigDir, orgConfigFile)
		data, err := os.ReadFile(candidate) //nolint:gosec // G304-WORKSPACE-CANON: walk-up from startDir
		if err == nil {
			var oc OrgConfig
			if yerr := yaml.Unmarshal(data, &oc); yerr != nil {
				return nil, yerr
			}
			if oc.CoordinatorTakeoverSeconds == 0 {
				oc.CoordinatorTakeoverSeconds = 120
			}
			return &oc, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, nil // reached root without finding
		}
		dir = parent
	}
	return nil, nil
}

// WriteOrgConfig persists an OrgConfig to `<root>/.neo-org/neo.yaml`. Creates
// the directory on demand. [354.A]
func WriteOrgConfig(root string, oc *OrgConfig) error {
	if oc == nil {
		return nil
	}
	dir := filepath.Join(root, orgConfigDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	data, err := yaml.Marshal(oc)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, orgConfigFile), data, 0o640)
}

// FindNeoOrgDir returns the absolute path to the discovered `.neo-org/`
// directory, walking up from startDir. Mirrors the semantics of
// federation.FindNeoProjectDir. [354.A]
func FindNeoOrgDir(startDir string) (string, bool) {
	dir := startDir
	for range maxOrgWalkUp {
		candidate := filepath.Join(dir, orgConfigDir)
		if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
	return "", false
}

// IsCoordinatorProject reports whether projectRoot is designated as the org
// coordinator. Match rules mirror isCoordinatorWorkspace at project tier:
// exact absolute path OR basename match. When CoordinatorProject is empty,
// every project behaves as coordinator (legacy mode — race-prone). [354.A]
func IsCoordinatorProject(projectRoot string, org *OrgConfig) bool {
	if org == nil || org.CoordinatorProject == "" {
		return true
	}
	c := org.CoordinatorProject
	return projectRoot == c ||
		filepath.Base(projectRoot) == c ||
		filepath.Base(projectRoot) == filepath.Base(c)
}
