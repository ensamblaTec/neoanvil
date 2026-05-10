package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/astx"
	"github.com/ensamblatec/neoanvil/pkg/kanban"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// serverBootTime is set once at process start; used to derive a stable session ID per run.
var serverBootTime = time.Now()

func briefingSessionID(workspace string) string {
	return fmt.Sprintf("%s|%d", workspace, serverBootTime.Unix())
}

func briefingBinaryAge(workspace string) string {
	binPath := filepath.Join(workspace, "bin", "neo-mcp")
	binStat, err := os.Stat(binPath)
	if err != nil {
		return ""
	}
	binMtime := binStat.ModTime()
	binAge := time.Since(binMtime)
	if binAge < 5*time.Minute {
		return ""
	}

	// Compare against last commit timestamp touching cmd/ or pkg/.
	cmd := exec.Command("git", "-C", workspace, "log", "-1", "--format=%ct", "--", "cmd/", "pkg/") //nolint:gosec // G204-LITERAL-BIN
	out, gitErr := cmd.Output()
	if gitErr == nil {
		tsStr := strings.TrimSpace(string(out))
		if tsUnix, parseErr := strconv.ParseInt(tsStr, 10, 64); parseErr == nil {
			lastCommit := time.Unix(tsUnix, 0)
			if lastCommit.After(binMtime) {
				stale := time.Since(lastCommit)
				return fmt.Sprintf(" ⚠️ binary_stale_vs_HEAD=%dm", int(stale.Minutes()))
			}
		}
	}
	return fmt.Sprintf(" | binary_age=%dm", int(binAge.Minutes()))
}

func detectMemorySchemaStale(workspace string) bool {
	binPath := filepath.Join(workspace, "bin", "neo-mcp")
	srcPath := filepath.Join(workspace, "cmd", "neo-mcp", "tool_memory.go")
	binInfo, err := os.Stat(binPath)
	if err != nil {
		return false
	}
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return false
	}
	return srcInfo.ModTime().After(binInfo.ModTime())
}

func (t *RadarTool) handleASTAudit(ctx context.Context, args map[string]any) (any, error) {
	target, _ := args["target"].(string)
	if target == "" {
		return nil, fmt.Errorf("target (file path) is required for AST_AUDIT")
	}
	// Glob pattern or directory → batch mode
	if strings.ContainsAny(target, "*?[") {
		return t.runASTAuditGlob(ctx, target)
	}
	absTarget := target
	if !filepath.IsAbs(absTarget) {
		absTarget = filepath.Join(t.workspace, absTarget)
	}
	if info, statErr := os.Stat(absTarget); statErr == nil && info.IsDir() {
		return t.runASTAuditGlob(ctx, absTarget)
	}

	ext := filepath.Ext(target)
	switch ext {
	case ".go":
		return auditGoSource(t, target)
	case ".md":
		return auditMarkdownFrontmatter(t, absTarget) // [128.2]
	case ".ts", ".tsx", ".js", ".jsx":
		result, err := auditFrontendSource(ctx, t, absTarget)
		if result != nil || err != nil {
			return result, err
		}
		return auditMultilangSource(ctx, t, absTarget, ext)
	default:
		return auditMultilangSource(ctx, t, absTarget, ext)
	}
}

func auditGoSource(t *RadarTool, target string) (any, error) {
	src, err := os.ReadFile(target) //nolint:gosec // G304-WORKSPACE-CANON
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", target, err)
	}
	findings, err := astx.AuditGoFile(target, src)
	if err != nil {
		return nil, fmt.Errorf("audit failed: %w", err)
	}
	t.registerAuditTechDebt(target, findings)
	return mcpText(astx.FormatAuditReport(findings)), nil
}

func auditFrontendSource(ctx context.Context, t *RadarTool, absTarget string) (any, error) {
	if t.cfg == nil {
		return nil, nil
	}
	linterCmd := t.resolveLinterCmd(absTarget)
	if linterCmd == "" {
		return nil, nil
	}
	lintCtx, lintCancel := context.WithTimeout(ctx, 30*time.Second)
	defer lintCancel()
	shCmd := exec.CommandContext(lintCtx, "sh", "-c", linterCmd) //nolint:gosec // G204-SHELL-WITH-VALIDATION
	shCmd.Dir = filepath.Dir(absTarget)
	sre.HardenSubprocess(shCmd, 0) // [T006-sweep] golangci-lint may parallel-spawn gocompile workers
	out, errLint := shCmd.CombinedOutput()
	text := fmt.Sprintf("## Linter output for %s\n```\n%s\n```", absTarget, string(out))
	if errLint != nil {
		text = fmt.Sprintf("⚠️  Linter exit error: %v\n%s", errLint, text)
	}
	return mcpText(text), nil
}

func auditMultilangSource(ctx context.Context, t *RadarTool, absTarget, ext string) (any, error) {
	analyzer, ok := astx.DefaultRouter.Lookup(absTarget)
	if !ok {
		return nil, fmt.Errorf("AST_AUDIT: no analyzer for %q — supported: .go, .py, .ts, .tsx, .js, .jsx, .rs", ext)
	}
	src, readErr := os.ReadFile(absTarget) //nolint:gosec // G304-WORKSPACE-CANON
	if readErr != nil {
		return nil, fmt.Errorf("cannot read %s: %w", absTarget, readErr)
	}
	auditCtx, auditCancel := context.WithTimeout(ctx, 10*time.Second)
	defer auditCancel()
	findings, auditErr := analyzer.Analyze(auditCtx, absTarget, src)
	if auditErr != nil {
		return nil, fmt.Errorf("audit failed: %w", auditErr)
	}
	t.registerAuditTechDebt(absTarget, findings)
	return mcpText(astx.FormatAuditReport(findings)), nil
}

func (t *RadarTool) resolveLinterCmd(target string) string {
	rel, errRel := filepath.Rel(t.workspace, target)
	if errRel != nil {
		return ""
	}
	parts := strings.SplitN(filepath.ToSlash(rel), "/", 2)
	if len(parts) == 0 {
		return ""
	}
	cmd, ok := t.cfg.Workspace.Modules[parts[0]]
	if !ok || !strings.Contains(cmd, "lint") {
		return ""
	}
	return cmd
}

// isBundleOrVendorDir returns true for directory names that hold machine-generated
// or third-party code that should never go through AST_AUDIT. Keeps the signal-to-noise
// ratio of the glob walker honest (AUDIT-2026-04-23: minified JS in static/assets/
// produced CC≈216 false positives).
func isBundleOrVendorDir(name string) bool {
	switch name {
	case "vendor", "node_modules", "dist", "build", ".git":
		return true
	}
	return false
}

// isMinifiedOrBundledAsset detects individual files that are machine-generated bundles:
// minified JS/CSS (`*.min.js`) or hashed Vite/Rollup artifacts (`*-[hash].{js,css}`)
// living under `static/assets/` or similar. The CC/SHADOW detectors are useless on
// these and produce noise.
func isMinifiedOrBundledAsset(path string) bool {
	base := filepath.Base(path)
	// Minified suffix pattern
	if strings.HasSuffix(base, ".min.js") || strings.HasSuffix(base, ".min.css") {
		return true
	}
	// Any JS/CSS living inside a static-assets directory is assumed to be bundled.
	if strings.Contains(path, "/static/assets/") ||
		strings.Contains(path, "/dist/") ||
		strings.Contains(path, "/build/") {
		if ext := filepath.Ext(base); ext == ".js" || ext == ".css" || ext == ".mjs" || ext == ".cjs" {
			return true
		}
	}
	return false
}

func (t *RadarTool) runASTAuditGlob(ctx context.Context, pattern string) (any, error) {
	if !filepath.IsAbs(pattern) {
		pattern = filepath.Join(t.workspace, pattern)
	}

	var files []string

	isDir := false
	if info, statErr := os.Stat(pattern); statErr == nil && info.IsDir() {
		isDir = true
	}

	if isDir || strings.Contains(pattern, "**") {
		root := pattern
		if strings.Contains(pattern, "**") {
			root = filepath.Clean(strings.SplitN(pattern, "**", 2)[0])
		}
		walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && isBundleOrVendorDir(d.Name()) {
				return filepath.SkipDir
			}
			if _, ok := astx.DefaultRouter.Lookup(path); ok && !isMinifiedOrBundledAsset(path) {
				files = append(files, path)
			}
			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("WalkDir failed: %w", walkErr)
		}
	} else {
		matches, globErr := filepath.Glob(pattern)
		if globErr != nil {
			return nil, fmt.Errorf("invalid glob pattern: %w", globErr)
		}
		for _, m := range matches {
			if _, ok := astx.DefaultRouter.Lookup(m); ok && !isMinifiedOrBundledAsset(m) {
				files = append(files, m)
			}
		}
	}

	if len(files) == 0 {
		return mcpText(fmt.Sprintf("⚠️  AST_AUDIT: no supported files matched: %s", pattern)), nil
	}
	return mcpText(t.runASTAuditBatch(ctx, files, pattern)), nil
}

func (t *RadarTool) runASTAuditBatch(ctx context.Context, files []string, label string) string {
	type entry struct {
		path     string
		findings []astx.AuditFinding
	}
	var clean, skipped int
	var issues []entry

	for _, file := range files {
		if ctx.Err() != nil {
			break
		}
		analyzer, ok := astx.DefaultRouter.Lookup(file)
		if !ok {
			skipped++
			continue
		}
		src, readErr := os.ReadFile(file) //nolint:gosec // G304-DIR-WALK
		if readErr != nil {
			skipped++
			continue
		}
		auditCtx, auditCancel := context.WithTimeout(ctx, 10*time.Second)
		findings, auditErr := analyzer.Analyze(auditCtx, file, src)
		auditCancel()
		if auditErr != nil {
			skipped++
			continue
		}
		t.registerAuditTechDebt(file, findings)
		realCount := 0
		for _, f := range findings {
			if f.Kind != "CC_SUMMARY" {
				realCount++
			}
		}
		if realCount == 0 {
			clean++
		} else {
			issues = append(issues, entry{file, findings})
		}
	}

	rel, _ := filepath.Rel(t.workspace, label)
	var sb strings.Builder
	fmt.Fprintf(&sb, "## AST_AUDIT Glob: %s — %d file(s)\n\n", rel, len(files))
	if len(issues) == 0 {
		fmt.Fprintf(&sb, "✅ AST_AUDIT: No issues found (%d clean", clean)
		if skipped > 0 {
			fmt.Fprintf(&sb, ", %d skipped", skipped)
		}
		sb.WriteString(")\n")
		return sb.String()
	}
	fmt.Fprintf(&sb, "✅ Clean: %d | ⚠️  Issues: %d", clean, len(issues))
	if skipped > 0 {
		fmt.Fprintf(&sb, " | ⏭️  Skipped: %d", skipped)
	}
	sb.WriteString("\n\n")
	for _, item := range issues {
		itemRel, _ := filepath.Rel(t.workspace, item.path)
		fmt.Fprintf(&sb, "#### %s\n", itemRel)
		sb.WriteString(astx.FormatAuditReport(item.findings))
		sb.WriteString("\n")
	}
	return sb.String()
}

// auditMarkdownFrontmatter validates SKILL.md / agent .md frontmatter. [128.2]
// Returns a report with severity/message findings compatible with Go AST findings.
func auditMarkdownFrontmatter(t *RadarTool, absPath string) (any, error) {
	data, err := os.ReadFile(absPath) //nolint:gosec // G304-WORKSPACE-CANON: path resolved via filepath.Join(workspace,...)
	if err != nil {
		return nil, fmt.Errorf("auditMarkdownFrontmatter: cannot read %s: %w", absPath, err)
	}
	findings := parseSkillFrontmatter(absPath, data)
	rel, _ := filepath.Rel(t.workspace, absPath)
	if len(findings) == 0 {
		return mcpText(fmt.Sprintf("✅ AST_AUDIT (.md): %s — frontmatter valid", rel)), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## AST_AUDIT frontmatter: %s — %d finding(s)\n\n", rel, len(findings))
	for _, f := range findings {
		fmt.Fprintf(&sb, "- **line %d** [%s] %s\n", f.line, f.severity, f.message)
	}
	return mcpText(sb.String()), nil
}

type mdFinding struct {
	line     int
	severity string
	message  string
}

// knownModelIDs is the allow-list for the model: field in skill frontmatter. [128.2]
var knownModelIDs = map[string]bool{
	"claude-sonnet-4-6":           true,
	"claude-opus-4-7":             true,
	"claude-haiku-4-5-20251001":   true,
	"claude-sonnet-4-5":           true,
	"claude-haiku-4-5":            true,
}

// deprecatedTools is the list of tools that should not appear in allowed-tools. [128.2]
var deprecatedMCPTools = []string{
	"neo_cache_stats", "neo_cache_flush", "neo_cache_resize", "neo_cache_warmup",
	"neo_cache_persist", "neo_cache_inspect", "neo_run_command", "neo_approve_command",
	"neo_kill_command", "neo_memory_commit", "neo_learn_directive", "neo_rem_sleep",
	"neo_load_snapshot", "neo_apply_patch", "neo_dependency_graph", "neo_pipeline",
	"neo_inspect_dom", "neo_inspect_matrix", "neo_inject_fault",
}

func validateNameField(name, dirName string, lineOf map[string]int) []mdFinding {
	if name != dirName {
		return []mdFinding{{
			line:     lineOf["name"],
			severity: "WARN",
			message:  fmt.Sprintf("name %q does not match directory %q", name, dirName),
		}}
	}
	return nil
}

func validateDescriptionField(desc string, lineOf map[string]int) []mdFinding {
	if desc == "" {
		return []mdFinding{{line: lineOf["description"], severity: "ERROR", message: "description is empty"}}
	}
	if len(desc) > 200 {
		return []mdFinding{{
			line:     lineOf["description"],
			severity: "WARN",
			message:  fmt.Sprintf("description length %d exceeds 200 chars", len(desc)),
		}}
	}
	return nil
}

func validateBoolField(key, raw string, lineOf map[string]int) []mdFinding {
	if raw != "true" && raw != "false" {
		return []mdFinding{{
			line:     lineOf[key],
			severity: "WARN",
			message:  fmt.Sprintf("%s must be bool, got %q", key, raw),
		}}
	}
	return nil
}

func validateModelField(model string, lineOf map[string]int) []mdFinding {
	if !knownModelIDs[model] {
		return []mdFinding{{
			line:     lineOf["model"],
			severity: "WARN",
			message:  fmt.Sprintf("unknown model %q — expected one of: claude-sonnet-4-6, claude-opus-4-7, claude-haiku-4-5-20251001", model),
		}}
	}
	return nil
}

func validateAllowedToolsField(tools string, lineOf map[string]int) []mdFinding {
	var findings []mdFinding
	for _, dep := range deprecatedMCPTools {
		if strings.Contains(tools, dep) {
			findings = append(findings, mdFinding{
				line:     lineOf["allowed-tools"],
				severity: "ERROR",
				message:  fmt.Sprintf("allowed-tools contains deprecated tool %q", dep),
			})
		}
	}
	return findings
}

// parseSkillFrontmatter extracts and validates YAML frontmatter from a .md file. [128.2]
// Returns findings sorted by line number. Fail-open: missing frontmatter = 0 findings.
func parseSkillFrontmatter(path string, data []byte) []mdFinding {
	content := string(data)
	if !strings.HasPrefix(content, "---") {
		return nil
	}
	rest := content[3:]
	if rest != "" && rest[0] == '\n' {
		rest = rest[1:]
	}
	fm, _, found := strings.Cut(rest, "\n---")
	if !found {
		return []mdFinding{{line: 1, severity: "ERROR", message: "frontmatter: missing closing ---"}}
	}

	var findings []mdFinding
	parsed := parseFrontmatterKV(fm)
	lineOf := frontmatterLineIndex(content, fm)
	dirName := filepath.Base(filepath.Dir(path))

	if name, ok := parsed["name"]; ok {
		findings = append(findings, validateNameField(name, dirName, lineOf)...)
	} else {
		findings = append(findings, mdFinding{line: 1, severity: "WARN", message: "frontmatter: missing required field 'name'"})
	}
	if desc, ok := parsed["description"]; ok {
		findings = append(findings, validateDescriptionField(desc, lineOf)...)
	} else {
		findings = append(findings, mdFinding{line: 1, severity: "WARN", message: "frontmatter: missing required field 'description'"})
	}
	if raw, ok := parsed["disable-model-invocation"]; ok {
		findings = append(findings, validateBoolField("disable-model-invocation", raw, lineOf)...)
	}
	if model, ok := parsed["model"]; ok {
		findings = append(findings, validateModelField(model, lineOf)...)
	}
	if tools, ok := parsed["allowed-tools"]; ok {
		findings = append(findings, validateAllowedToolsField(tools, lineOf)...)
	}
	return findings
}

// parseFrontmatterKV parses simple key: value lines from raw frontmatter text.
// Does not handle multi-line YAML values — sufficient for SKILL.md validation.
func parseFrontmatterKV(fm string) map[string]string {
	out := make(map[string]string)
	for line := range strings.SplitSeq(fm, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if _, exists := out[k]; !exists { // first occurrence wins (multi-line value continuation skipped)
			out[k] = v
		}
	}
	return out
}

// frontmatterLineIndex returns the 1-based line number for each frontmatter key. [128.2]
func frontmatterLineIndex(content, fm string) map[string]int {
	out := make(map[string]int)
	lineNum := 2 // frontmatter starts after first "---\n"
	for line := range strings.SplitSeq(fm, "\n") {
		if k, _, ok := strings.Cut(line, ":"); ok {
			k = strings.TrimSpace(k)
			if _, exists := out[k]; !exists {
				out[k] = lineNum
			}
		}
		lineNum++
	}
	_ = content // retained for future use
	return out
}

func (t *RadarTool) registerAuditTechDebt(file string, findings []astx.AuditFinding) {
	for _, f := range findings {
		if f.Kind == "COMPLEXITY" || f.Kind == "INFINITE_LOOP" {
			_ = kanban.AppendTechDebt(t.workspace,
				fmt.Sprintf("AST %s in %s:%d", f.Kind, filepath.Base(file), f.Line),
				fmt.Sprintf("File: %s\nLine: %d\nKind: %s\nDetail: %s", file, f.Line, f.Kind, f.Message), "alta")
		}
	}
}
