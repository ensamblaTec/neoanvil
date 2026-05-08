package cpg

// tsconfig.go — parse compilerOptions.paths from tsconfig.json for alias resolution.
// Used by both blast_radius_lang.go (PILAR XXIX) and bridge.go (PILAR XXX).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ReadTSConfigPaths parses tsconfig.json at workspace root and returns alias→src
// mappings (e.g. "@/" → "src/"). Defaults to {"@/": "src/", "~/": "src/"} when
// the file is absent or unparseable. [Épica 257.A]
func ReadTSConfigPaths(workspace string) map[string]string {
	result := map[string]string{"@/": "src/", "~/": "src/"}
	data, err := os.ReadFile(filepath.Join(workspace, "tsconfig.json")) //nolint:gosec // G304-WORKSPACE-CANON: workspace pinned at boot
	if err != nil {
		return result
	}
	var cfg struct {
		CompilerOptions struct {
			Paths map[string][]string `json:"paths"`
		} `json:"compilerOptions"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return result
	}
	for alias, targets := range cfg.CompilerOptions.Paths {
		if len(targets) == 0 {
			continue
		}
		key := strings.TrimSuffix(alias, "*")
		val := strings.TrimSuffix(targets[0], "*")
		result[key] = val
	}
	return result
}
