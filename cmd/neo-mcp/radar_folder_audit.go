package main

// radar_folder_audit.go — CLAUDE_FOLDER_AUDIT intent: drift detection for
// .claude/skills/ vs CLAUDE.md references and path globs. [128.1]

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// handleClaudeFolderAudit returns a Markdown table auditing the .claude/skills/
// directory against CLAUDE.md references and docs/claude-folder-inventory.md. [128.1]
func (t *RadarTool) handleClaudeFolderAudit(_ context.Context, _ map[string]any) (any, error) {
	rows, err := auditClaudeFolder(t.workspace)
	if err != nil {
		return nil, err
	}
	return mcpText(renderFolderAuditTable(rows)), nil
}

// folderAuditRow holds one skill's audit state. [128.1]
type folderAuditRow struct {
	skillName   string
	exists      bool // SKILL.md file present
	inCLAUDE    bool // referenced in CLAUDE.md
	inInventory bool // referenced in docs/claude-folder-inventory.md
	pathsValid  bool // all paths: globs match ≥1 file (true when no paths: field)
	brokenXrefs []string
}

// reMarkdownLink matches [text](path) where path does not start with http.
var reMarkdownLink = regexp.MustCompile(`\[([^\]]+)\]\(([^)#]+)\)`)

// auditClaudeFolder performs the full audit and returns one row per skill. [128.1]
func auditClaudeFolder(workspace string) ([]folderAuditRow, error) {
	skillGlob := filepath.Join(workspace, ".claude", "skills", "*", "SKILL.md")
	skillFiles, _ := filepath.Glob(skillGlob)

	claudeMD := readFileBytes(filepath.Join(workspace, "CLAUDE.md"))
	inventoryMD := readFileBytes(filepath.Join(workspace, "docs", "claude-folder-inventory.md"))

	var rows []folderAuditRow
	for _, sf := range skillFiles {
		skillDir := filepath.Dir(sf)
		name := filepath.Base(skillDir)
		body, _ := os.ReadFile(sf) //nolint:gosec // G304-DIR-WALK: path from filepath.Glob within workspace

		row := folderAuditRow{
			skillName:   name,
			exists:      true,
			inCLAUDE:    claudeMD != nil && strings.Contains(string(claudeMD), name),
			inInventory: inventoryMD != nil && strings.Contains(string(inventoryMD), name),
			pathsValid:  true,
		}

		// Verify paths: globs from frontmatter — prefer YAML list, fall back to
		// inline value. pathsValid stays true when no paths: field is present.
		globs := collectYAMLListItems(string(body), "paths")
		if len(globs) == 0 {
			// Try inline value: "paths: pkg/**/*.go"
			fm := parseFrontmatterKV(extractFrontmatter(string(body)))
			if v, ok := fm["paths"]; ok && v != "" {
				globs = []string{v}
			}
		}
		for _, g := range globs {
			g = strings.TrimSpace(g)
			if g == "" {
				continue
			}
			absGlob := g
			if !filepath.IsAbs(g) {
				absGlob = filepath.Join(workspace, g)
			}
			matches, _ := filepath.Glob(absGlob)
			if len(matches) == 0 {
				row.pathsValid = false
				break
			}
		}

		// Verify See also: / markdown cross-refs in body.
		for _, m := range reMarkdownLink.FindAllStringSubmatch(string(body), -1) {
			ref := m[2]
			if strings.HasPrefix(ref, "http") {
				continue
			}
			// Resolve relative to the skill dir.
			abs := ref
			if !filepath.IsAbs(ref) {
				abs = filepath.Join(skillDir, ref)
			}
			if _, statErr := os.Stat(abs); statErr != nil {
				row.brokenXrefs = append(row.brokenXrefs, ref)
			}
		}

		rows = append(rows, row)
	}

	return rows, nil
}

// renderFolderAuditTable formats the audit results as a Markdown table. [128.1]
func renderFolderAuditTable(rows []folderAuditRow) string {
	if len(rows) == 0 {
		return "## CLAUDE_FOLDER_AUDIT\n\nNo skills found in `.claude/skills/`.\n"
	}
	var sb strings.Builder
	sb.WriteString("## CLAUDE_FOLDER_AUDIT\n\n")
	sb.WriteString("| skill_name | exists | in_CLAUDE.md | in_inventory | paths_valid | broken_xrefs |\n")
	sb.WriteString("|------------|--------|--------------|--------------|-------------|---------------|\n")
	issues := 0
	for _, r := range rows {
		xrefs := "—"
		if len(r.brokenXrefs) > 0 {
			xrefs = strings.Join(r.brokenXrefs, ", ")
			issues++
		}
		pathMark := boolMark(r.pathsValid)
		if !r.pathsValid {
			issues++
		}
		inCLAUDE := boolMark(r.inCLAUDE)
		if !r.inCLAUDE {
			issues++
		}
		fmt.Fprintf(&sb, "| `%s` | %s | %s | %s | %s | %s |\n",
			r.skillName,
			boolMark(r.exists),
			inCLAUDE,
			boolMark(r.inInventory),
			pathMark,
			xrefs,
		)
	}
	if issues == 0 {
		sb.WriteString("\n✅ No drift detected.\n")
	} else {
		fmt.Fprintf(&sb, "\n⚠️  %d issue(s) found — review rows marked ✗.\n", issues)
	}
	return sb.String()
}

func boolMark(v bool) string {
	if v {
		return "✓"
	}
	return "✗"
}

// extractFrontmatter returns the raw text between the first pair of --- delimiters. [128.1/128.2]
func extractFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---") {
		return ""
	}
	rest := content[3:]
	if len(rest) > 0 && rest[0] == '\n' {
		rest = rest[1:]
	}
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// collectYAMLListItems scans content for YAML list items under a given key. [128.1]
// Handles the common case:
//
//	paths:
//	  - "glob/pattern"
func collectYAMLListItems(content, key string) []string {
	lines := strings.Split(content, "\n")
	var items []string
	inBlock := false
	for _, line := range lines {
		if strings.TrimSpace(line) == key+":" {
			inBlock = true
			continue
		}
		if inBlock {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "- ") {
				val := strings.Trim(strings.TrimPrefix(trimmed, "- "), `"'`)
				items = append(items, val)
			} else if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				break // block ended
			}
		}
	}
	return items
}

// readFileBytes reads a file and returns its content, or nil on error. [128.1]
func readFileBytes(path string) []byte {
	data, err := os.ReadFile(path) //nolint:gosec // G304-WORKSPACE-CANON: path via filepath.Join(workspace,...)
	if err != nil {
		return nil
	}
	return data
}
