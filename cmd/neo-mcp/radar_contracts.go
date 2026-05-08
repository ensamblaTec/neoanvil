package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
	"github.com/ensamblatec/neoanvil/pkg/federation"
	"github.com/ensamblatec/neoanvil/pkg/rag"
)

// min returns the smaller of two ints (stdlib min not available pre-Go 1.21 in all build envs).
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// mcpText wraps a string as the standard MCP text content response.
func mcpText(s string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": s}}}
}

// fetchRef is a parsed fetch call from a TypeScript/JavaScript file. [316.B]
type fetchRef struct {
	path string
	file string
	line int
}

// compiled once at package init to avoid per-call allocation in hot paths.
var (
	fetchPathRe = regexp.MustCompile(
		`(?:fetch|axios\.(?:get|post|put|patch|delete)|api\.(?:get|post|put|patch|delete))\s*\(\s*[` + "`" + `'"]([/][^` + "`" + `'"` + `\s]+)`)
	goRouteRe = regexp.MustCompile(`(?:GET|POST|PUT|PATCH|DELETE|HandleFunc?)\s*\(\s*"([^"]+)"`)
)

// handleContractQuery returns a surgical view of a single HTTP endpoint contract. [PILAR-XXXVIII/290]
// Fields: target (path fragment), method (HTTP verb), validate_payload (JSON string).
// Caches result in TextCache with key "CONTRACT_QUERY:<target>:<method>".
func (t *RadarTool) handleContractQuery(_ context.Context, args map[string]any) (any, error) {
	target, _ := args["target"].(string)
	method, _ := args["method"].(string)
	method = strings.ToUpper(strings.TrimSpace(method))
	validatePayload, _ := args["validate_payload"].(string)

	if target == "" {
		return nil, fmt.Errorf("CONTRACT_QUERY requires target (path fragment)")
	}

	cacheVariant := 0
	if method != "" {
		cacheVariant = int(method[0]) // cheap differentiation by first byte of verb
	}
	cacheKey := rag.NewTextCacheKey("CONTRACT_QUERY", target, cacheVariant)

	if cached, ok := contractQueryCacheHit(t, cacheKey, target, method, cacheVariant, validatePayload); ok {
		return mcpText(cached), nil
	}

	contracts, coverage := t.resolveContracts()
	if len(contracts) == 0 {
		return mcpText("## CONTRACT_QUERY\n\n_No contracts resolved. Ensure OpenAPI spec or Go route handlers are present._"), nil
	}

	matched := filterContracts(contracts, target, method)

	var sb strings.Builder
	fmt.Fprintf(&sb, "## CONTRACT_QUERY: `%s`", target)
	if method != "" {
		fmt.Fprintf(&sb, " [%s]", method)
	}
	sb.WriteString("\n\n")
	fmt.Fprintf(&sb, "**coverage:** %s  \n\n", coverage)

	if len(matched) == 0 {
		sb.WriteString(buildContractNotFoundResponse(t, target, method))
		return mcpText(sb.String()), nil
	}

	for _, c := range matched {
		renderContractMatch(&sb, t, c, validatePayload)
	}

	body := sb.String()
	if t.textCache != nil && validatePayload == "" {
		t.textCache.PutAnnotated(cacheKey, body, t.graph.Gen.Load(), "CONTRACT_QUERY", target, cacheVariant)
	}
	return mcpText(body), nil
}

// contractQueryCacheHit checks TextCache and KnowledgeStore, returning cached content when found. [320.A]
func contractQueryCacheHit(t *RadarTool, cacheKey rag.TextCacheKey, target, method string, cacheVariant int, validatePayload string) (string, bool) {
	// TextCache lookup. [290.D]
	if t.textCache != nil && validatePayload == "" {
		if cached, ok := t.textCache.Get(cacheKey, t.graph.Gen.Load()); ok {
			return cached, true
		}
	}
	// KnowledgeStore hot-path: pre-cached contract specs in namespace "contracts". [298.A]
	if t.contractHotFetch != nil && validatePayload == "" {
		lookupKey := target
		if method != "" {
			lookupKey = method + " " + target
		}
		if content, ok := t.contractHotFetch("contracts", lookupKey); ok {
			body := "## CONTRACT_QUERY (knowledge_store): `" + target + "`\n\n" +
				"**source:** knowledge_store  \n\n" + content
			if t.textCache != nil {
				t.textCache.PutAnnotated(cacheKey, body, t.graph.Gen.Load(), "CONTRACT_QUERY", target, cacheVariant)
			}
			return body, true
		}
	}
	return "", false
}

// filterContracts returns contracts whose path contains target and (optionally) matches method. [320.A]
func filterContracts(contracts []cpg.ContractNode, target, method string) []cpg.ContractNode {
	var matched []cpg.ContractNode
	for _, c := range contracts {
		if !strings.Contains(c.Path, target) && !strings.Contains(strings.ToLower(c.Path), strings.ToLower(target)) {
			continue
		}
		if method != "" && c.Method != method {
			continue
		}
		matched = append(matched, c)
	}
	return matched
}

// buildContractNotFoundResponse builds the not-found section and logs to SHARED_DEBT.md. [320.A/316.A]
func buildContractNotFoundResponse(t *RadarTool, target, method string) string {
	var sb strings.Builder
	sb.WriteString("**found:** false  \n**confidence:** none  \n\n")
	sb.WriteString("_No endpoint matching path `" + target + "`")
	if method != "" {
		fmt.Fprintf(&sb, " with method `%s`", method)
	}
	sb.WriteString(" found in contract graph._\n\n")
	sharedDebtLogged := false
	if projDir, ok := federation.FindNeoProjectDir(t.workspace); ok {
		wsName := filepath.Base(t.workspace)
		if err := federation.AppendMissingContract(projDir, target, "CONTRACT_QUERY", wsName); err != nil {
			log.Printf("[CONTRACT_QUERY] shared_debt append: %v", err)
		} else {
			sharedDebtLogged = true
		}
	}
	sb.WriteString("**suggestion:** Endpoint not defined.")
	if sharedDebtLogged {
		sb.WriteString(" Logged to `.neo-project/SHARED_DEBT.md`.")
	}
	sb.WriteString(" Do NOT implement speculatively — raise to backend workspace.\n")
	return sb.String()
}

// renderContractMatch appends one matched contract's schema, payload validation, and frontend callers. [320.A]
func renderContractMatch(sb *strings.Builder, t *RadarTool, c cpg.ContractNode, validatePayload string) {
	fmt.Fprintf(sb, "### %s %s\n\n", c.Method, c.Path)
	fmt.Fprintf(sb, "| Field | Value |\n|-------|-------|\n")
	fmt.Fprintf(sb, "| source | `%s` |\n", c.Source)
	fmt.Fprintf(sb, "| handler | `%s` |\n", c.BackendFn)
	fmt.Fprintf(sb, "| file | `%s:%d` |\n", c.BackendFile, c.BackendLine)
	// Request schema extraction. [290.B]
	if c.BackendFn != "" {
		schema, partial, serr := cpg.ExtractRequestSchema(t.workspace, c.BackendFn)
		if serr == nil && len(schema) > 0 {
			if partial {
				sb.WriteString("\n**Request schema** _(partial — cross-package type)_:\n\n")
			} else {
				sb.WriteString("\n**Request schema:**\n\n")
			}
			sb.WriteString("| field | type | required | tags |\n|-------|------|----------|------|\n")
			for _, f := range schema {
				req := ""
				if f.Required {
					req = "✓"
				}
				fmt.Fprintf(sb, "| `%s` | `%s` | %s | `%s` |\n", f.Field, f.Type, req, f.Tags)
			}
			sb.WriteString("\n")
			// Payload validation. [290.B]
			if validatePayload != "" {
				sb.WriteString("**Payload validation:**\n\n")
				violations := contractValidatePayload(validatePayload, schema)
				if len(violations) == 0 {
					sb.WriteString("✅ Payload satisfies required fields.\n\n")
				} else {
					for _, v := range violations {
						fmt.Fprintf(sb, "- ❌ %s\n", v)
					}
					sb.WriteString("\n")
				}
			}
		}
	}
	// Frontend callers.
	if len(c.FrontendCallers) > 0 {
		fmt.Fprintf(sb, "**Frontend callers** (%d):\n\n", len(c.FrontendCallers))
		for _, caller := range c.FrontendCallers {
			fmt.Fprintf(sb, "- `%s:%d`\n", caller.File, caller.Line)
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("**Frontend callers:** _none detected_\n\n")
	}
}

func extractTSFetchPaths(dir string) []fetchRef {
	var refs []fetchRef
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".ts" && ext != ".tsx" && ext != ".js" && ext != ".jsx" {
			return nil
		}
		data, rerr := os.ReadFile(path) //nolint:gosec // G304-DIR-WALK: walking workspace tree under process control
		if rerr != nil {
			return nil
		}
		lineNum := 1
		for line := range strings.SplitSeq(string(data), "\n") {
			for _, m := range fetchPathRe.FindAllStringSubmatch(line, -1) {
				if len(m) >= 2 {
					refs = append(refs, fetchRef{path: m[1], file: path, line: lineNum})
				}
			}
			lineNum++
		}
		return nil
	})
	if walkErr != nil {
		log.Printf("[CONTRACT-WARN] extractTSFetchPaths: walk %s failed: %v", dir, walkErr)
	}
	return refs
}

func extractGoRoutePaths(dir string) map[string]string {
	routes := make(map[string]string)
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, rerr := os.ReadFile(path) //nolint:gosec // G304-DIR-WALK: walking workspace tree under process control
		if rerr != nil {
			return nil
		}
		lineNum := 1
		for line := range strings.SplitSeq(string(data), "\n") {
			for _, m := range goRouteRe.FindAllStringSubmatch(line, -1) {
				if len(m) >= 2 && strings.HasPrefix(m[1], "/") {
					loc := fmt.Sprintf("%s:%d", path, lineNum)
					routes[m[1]] = loc
				}
			}
			lineNum++
		}
		return nil
	})
	if walkErr != nil {
		log.Printf("[CONTRACT-WARN] extractGoRoutePaths: walk %s failed: %v", dir, walkErr)
	}
	return routes
}

// handleContractGap diffs TypeScript fetch calls against defined Go routes, logs gaps. [316.B]
func (t *RadarTool) handleContractGap(_ context.Context, args map[string]any) (any, error) {
	scanDir := t.workspace
	if tgt, _ := args["target"].(string); tgt != "" {
		scanDir = filepath.Join(t.workspace, tgt)
	}
	cacheKey := rag.NewTextCacheKey("CONTRACT_GAP", scanDir, 0)
	if t.textCache != nil {
		if cached, ok := t.textCache.Get(cacheKey, t.graph.Gen.Load()); ok {
			return mcpText(cached), nil
		}
	}
	fetchRefs := extractTSFetchPaths(scanDir)
	goRoutes := extractGoRoutePaths(scanDir)
	var sb strings.Builder
	fmt.Fprintf(&sb, "## CONTRACT_GAP Report — %s\n\n", time.Now().UTC().Format("2006-01-02"))
	if len(fetchRefs) == 0 {
		sb.WriteString("_No TypeScript/JavaScript fetch calls detected in scanned directory._\n")
		return mcpText(sb.String()), nil
	}
	sb.WriteString("| Endpoint | Called in | Status |\n|----------|-----------|--------|\n")
	projDir, hasProjDir := federation.FindNeoProjectDir(t.workspace)
	wsName := filepath.Base(t.workspace)
	seen := make(map[string]bool)
	gapCount := 0
	for _, ref := range fetchRefs {
		if seen[ref.path] {
			continue
		}
		seen[ref.path] = true
		relFile := strings.TrimPrefix(ref.file, t.workspace+"/")
		caller := fmt.Sprintf("%s:%d", relFile, ref.line)
		if loc, ok := goRoutes[ref.path]; ok {
			relLoc := strings.TrimPrefix(loc, t.workspace+"/")
			fmt.Fprintf(&sb, "| `%s` | `%s` | ✅ `%s` |\n", ref.path, caller, relLoc)
		} else {
			fmt.Fprintf(&sb, "| `%s` | `%s` | 🔴 not implemented |\n", ref.path, caller)
			gapCount++
			if hasProjDir {
				if err := federation.AppendMissingContract(projDir, ref.path, caller, wsName); err != nil {
					log.Printf("[CONTRACT_GAP] shared_debt: %v", err)
				}
			}
		}
	}
	sb.WriteString("\n")
	if gapCount > 0 {
		fmt.Fprintf(&sb, "**%d gap(s) detected.", gapCount)
		if hasProjDir {
			sb.WriteString(" Logged to `.neo-project/SHARED_DEBT.md`.")
		}
		sb.WriteString("**\n")
	} else {
		sb.WriteString("**All detected fetch calls have matching Go routes.** ✅\n")
	}
	body := sb.String()
	if t.textCache != nil {
		t.textCache.PutAnnotated(cacheKey, body, t.graph.Gen.Load(), "CONTRACT_GAP", scanDir, 0)
	}
	return mcpText(body), nil
}

// contractValidatePayload checks a JSON string against required schema fields. [290.B]
func contractValidatePayload(jsonPayload string, schema []cpg.SchemaNode) []string {
	var m map[string]any
	if err := json.Unmarshal([]byte(jsonPayload), &m); err != nil {
		return []string{fmt.Sprintf("invalid JSON: %v", err)}
	}
	var violations []string
	for _, f := range schema {
		if !f.Required {
			continue
		}
		if _, ok := m[f.Field]; !ok {
			violations = append(violations, fmt.Sprintf("missing required field `%s` (type: %s)", f.Field, f.Type))
		}
	}
	return violations
}
