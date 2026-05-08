package config

// project.go — 3-tier config hierarchy: workspace > project > global.
// PILAR XXXI, épica 258.

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ProjectConfig carries project-level settings that apply to all member workspaces.
// Only fields that make sense at project scope are included — ports, CPG, and
// inference settings stay per-workspace. [Épica 258.A]
type ProjectConfig struct {
	ProjectName      string   `yaml:"project_name"`
	MemberWorkspaces []string `yaml:"member_workspaces"` // paths or workspace IDs
	IgnoreDirsAdd    []string `yaml:"ignore_dirs_add"`   // appended to workspace ignore_dirs
	DominantLang     string   `yaml:"dominant_lang"`     // overrides workspace dominant_lang if set
	// KnowledgePath overrides the default .neo-project/knowledge/ sync directory. [296.A]
	// Leave empty to use the default. Useful when the .md files should live in a custom
	// docs/ subdirectory that is already tracked by git.
	KnowledgePath    string   `yaml:"knowledge_path,omitempty"`
	// SharedMemoryPath overrides the default .neo-project/db/shared.db path for the
	// shared HNSW vector tier accessible by all member workspaces. [287.A]
	SharedMemoryPath string `yaml:"shared_memory_path,omitempty"`
	// CoordinatorWorkspace designates the workspace that opens shared.db in
	// read-write mode. All other member workspaces open read-only.
	// Value is matched against the workspace path (exact or basename). [314.B]
	// Leave empty for legacy behavior (first workspace to boot wins the write lock).
	CoordinatorWorkspace string `yaml:"coordinator_workspace,omitempty"`
	// TokenBudgetDaily is the maximum tokens (input+output) the project may consume
	// per calendar day. When set, neo_tool_stats scope:project shows utilization and
	// a budget_exceeded flag when the limit is breached. [339.A]
	TokenBudgetDaily int `yaml:"token_budget_daily,omitempty"`
	// SharedRAGEnabled activates cross-workspace semantic search via SharedGraph. When
	// true and cross_workspace:true is requested, SEMANTIC_CODE augments its response
	// with hits from the project-level shared HNSW tier (mirrors BLAST_RADIUS shared
	// hits). Requires the coordinator workspace to have ingested content — REM sleep
	// auto-merges periodically, but a fresh project shows no hits until the coord's
	// first REM cycle. Default: false (opt-in). [330.I]
	SharedRAGEnabled bool `yaml:"shared_rag_enabled,omitempty"`
	// LLMOverrides forces a specific LLM/embedding configuration across all member
	// workspaces. Unlike other project fields, LLM settings have REVERSE precedence
	// — project wins over workspace — so a project can guarantee consistency on
	// pulled models (ej. qwen2.5-coder:32b shared in a Mac 96GB). When omitted,
	// each workspace uses its own neo.yaml AI/Inference config. [350.A]
	LLMOverrides *LLMOverrides `yaml:"llm_overrides,omitempty"`
}

// LLMOverrides bundles the project-scoped LLM forcing. All fields optional —
// zero values mean "do not override". [350.A]
type LLMOverrides struct {
	EmbeddingModel string `yaml:"embedding_model,omitempty"` // maps to ai.embedding_model
	Provider       string `yaml:"provider,omitempty"`        // maps to ai.provider
	InferenceMode  string `yaml:"inference_mode,omitempty"`  // maps to inference.mode (local|hybrid|cloud)
}

const projectConfigFile = "neo.yaml"
const projectConfigDir = ".neo-project"
const maxProjectWalkUp = 5

// LoadProjectConfig walks up from startDir looking for a .neo-project/neo.yaml file.
// Returns nil, nil when no project config is found — callers treat this as no project. [Épica 258.A]
func LoadProjectConfig(startDir string) (*ProjectConfig, error) {
	dir := startDir
	for range maxProjectWalkUp {
		candidate := filepath.Join(dir, projectConfigDir, projectConfigFile)
		data, err := os.ReadFile(candidate) //nolint:gosec // G304-WORKSPACE-CANON: config path derived from cwd walk-up
		if err == nil {
			var pc ProjectConfig
			if yamlErr := yaml.Unmarshal(data, &pc); yamlErr != nil {
				return nil, yamlErr
			}
			return &pc, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // reached filesystem root
		}
		dir = parent
	}
	return nil, nil
}

// WriteProjectConfig writes a ProjectConfig to .neo-project/neo.yaml under dir.
func WriteProjectConfig(dir string, pc *ProjectConfig) error {
	configDir := filepath.Join(dir, projectConfigDir)
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(pc)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(configDir, projectConfigFile), data, 0o644) //nolint:gosec // G306: project config is not sensitive
}
