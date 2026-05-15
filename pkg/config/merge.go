package config

// merge.go — 3-tier config deep merge: workspace > project > global.
// Merge rules: scalar fields — workspace wins over project wins over global.
// Slice fields (IgnoreDirs) — append(global, project, workspace) deduplicated.
// LLM fields (ai.provider, ai.embedding_model, inference.mode) — REVERSE
// precedence when project.llm_overrides is set (project wins over workspace)
// so a federation can pin a shared model across all members. [350.A]
// PILAR XXXI, épica 258.B.

import "log"

// MergeConfigs applies a 3-tier deep merge. workspace is highest priority.
// project and global may be nil (no-op for that tier). [Épica 258.B]
func MergeConfigs(workspace NeoConfig, project *ProjectConfig, global *NeoConfig) NeoConfig {
	return MergeConfigsWithOrg(workspace, project, nil, global)
}

// MergeConfigsWithOrg is the 4-tier variant: workspace > project > org > global.
// For LLM fields the precedence reverses — project LLMOverrides beat workspace,
// and org LLMOverrides fill the gap when project didn't override. [354.A]
func MergeConfigsWithOrg(workspace NeoConfig, project *ProjectConfig, org *OrgConfig, global *NeoConfig) NeoConfig {
	result := workspace

	if global != nil {
		result = applyGlobalDefaults(result, *global)
	}
	if org != nil {
		result = applyOrgOverrides(result, org)
	}
	if project != nil {
		result = applyProjectOverrides(result, project)
	}

	return result
}

// applyOrgOverrides applies org-scoped settings. Only LLMOverrides have reverse
// precedence (org fills gap when project LLMOverrides unset). The rest of the
// OrgConfig fields (DirectivesPath, DebtPath, KnowledgePath, etc.) are stored
// on the result via dst.Org for downstream consumers to query. [354.A]
func applyOrgOverrides(dst NeoConfig, org *OrgConfig) NeoConfig {
	if org.LLMOverrides != nil {
		o := org.LLMOverrides
		if o.EmbeddingModel != "" && dst.AI.EmbeddingModel == "" {
			log.Printf("[CONFIG] llm embedding_model set by org: → %s", o.EmbeddingModel)
			dst.AI.EmbeddingModel = o.EmbeddingModel
		}
		if o.Provider != "" && dst.AI.Provider == "" {
			log.Printf("[CONFIG] llm provider set by org: → %s", o.Provider)
			dst.AI.Provider = o.Provider
		}
		if o.InferenceMode != "" && dst.Inference.Mode == "" {
			log.Printf("[CONFIG] inference.mode set by org: → %s", o.InferenceMode)
			dst.Inference.Mode = o.InferenceMode
		}
	}
	dst.Org = org
	return dst
}

// applyGlobalDefaults fills zero-value fields in dst from global.
// Workspace values always win — only fill when dst field is the zero value.
func applyGlobalDefaults(dst, global NeoConfig) NeoConfig {
	// IgnoreDirs: prepend global dirs (lowest priority), workspace appended last.
	if len(global.Workspace.IgnoreDirs) > 0 {
		dst.Workspace.IgnoreDirs = dedupStrings(append(global.Workspace.IgnoreDirs, dst.Workspace.IgnoreDirs...))
	}
	return dst
}

// applyProjectOverrides applies project-scoped settings to dst.
// DominantLang from project is a DEFAULT for workspaces that don't set
// their own — workspace explicit wins. (Inverted 2026-05-15 from "project
// beats workspace" which broke strategosia-frontend: project="go" forced
// IndexCoverage to count .go files in a TypeScript workspace, producing a
// permanent RAG 0% false alarm. The polyglot case — strategos backend in
// Go + strategosia frontend in TS under one project — needs each workspace
// to keep its own lang.) IgnoreDirsAdd is appended additively. LLMOverrides
// still force a model set across the whole federation (reverse precedence
// vs the rest of the config — see [350.A]).
func applyProjectOverrides(dst NeoConfig, project *ProjectConfig) NeoConfig {
	if project.DominantLang != "" && dst.Workspace.DominantLang == "" {
		dst.Workspace.DominantLang = project.DominantLang
	}
	if len(project.IgnoreDirsAdd) > 0 {
		dst.Workspace.IgnoreDirs = dedupStrings(append(dst.Workspace.IgnoreDirs, project.IgnoreDirsAdd...))
	}
	dst = applyLLMOverrides(dst, project)
	dst.Project = project
	return dst
}

// applyLLMOverrides stamps LLM-scope fields from project over the workspace
// ones when project.llm_overrides is set. Each overridden field logs a
// [CONFIG] line so the operator sees the effective value at boot. [350.A]
func applyLLMOverrides(dst NeoConfig, project *ProjectConfig) NeoConfig {
	o := project.LLMOverrides
	if o == nil {
		return dst
	}
	if o.EmbeddingModel != "" && dst.AI.EmbeddingModel != o.EmbeddingModel {
		log.Printf("[CONFIG] llm embedding_model overridden by project: %s → %s",
			dst.AI.EmbeddingModel, o.EmbeddingModel)
		dst.AI.EmbeddingModel = o.EmbeddingModel
	}
	if o.Provider != "" && dst.AI.Provider != o.Provider {
		log.Printf("[CONFIG] llm provider overridden by project: %s → %s",
			dst.AI.Provider, o.Provider)
		dst.AI.Provider = o.Provider
	}
	if o.InferenceMode != "" && dst.Inference.Mode != o.InferenceMode {
		log.Printf("[CONFIG] inference.mode overridden by project: %s → %s",
			dst.Inference.Mode, o.InferenceMode)
		dst.Inference.Mode = o.InferenceMode
	}
	return dst
}

// dedupStrings returns a slice with duplicate strings removed, preserving order.
func dedupStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}
