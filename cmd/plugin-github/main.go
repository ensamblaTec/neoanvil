// Command plugin-github is the GitHub MCP plugin for neoanvil.
// [Area 2 — copy-adapt from cmd/plugin-jira]
//
// Wire format: newline-delimited JSON-RPC over stdio (MCP stdio transport).
// Auth: env vars (injected by Nexus PluginPool from ~/.neo/credentials.json):
//
//	GITHUB_TOKEN  — Personal Access Token from github.com/settings/tokens (required)
//	GITHUB_BASE_URL — Override for GitHub Enterprise (optional; defaults to api.github.com)
//
// Tools exposed (subset matching Area 2.2.A-E):
//
//	list_prs       : list pull requests for an owner/repo
//	create_pr      : open a new PR (title+body+head+base)
//	merge_pr       : merge an open PR (merge|squash|rebase)
//	close_pr       : close without merging
//	pr_comments    : fetch comment thread for a PR/issue
//	create_review  : APPROVE | REQUEST_CHANGES | COMMENT review
//	list_issues    : list issues (PRs filtered out)
//	create_issue   : open a new issue with optional labels
//	update_issue   : PATCH issue (state, title, body, labels)
//	get_checks     : check_runs for a ref (SHA or branch)
//	list_branches  : enumerate branches for owner/repo
//	compare        : git compare base...head
//	cross_ref      : extract Jira ticket keys from PR body via regex
//	__health__     : Nexus health probe (mandatory)

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/github"
)

const (
	protocolVersion = "2024-11-05"
	pluginVersion   = "0.1.0"
)

type state struct {
	client *github.Client // single-tenant fallback (nil in multi-tenant mode)

	// Multi-tenant config (nil when GITHUB_TOKEN env var is the
	// authoritative source). [Area 2.1.A + 2.1.C]
	pluginCfg *PluginConfig
	pool      *clientPool

	// auditPath is ~/.neo/audit-github.log when set. [Area 2.2.F]
	auditPath string

	// Health counters consumed by __health__ — atomic so the Nexus
	// health poll is lock-free + sub-10ms (per PLUGIN-HEALTH-CONTRACT).
	startedAtUnix    int64
	lastDispatchUnix int64
	errorCount       int64
}

func main() {
	st, err := buildState()
	if err != nil {
		fmt.Fprintln(os.Stderr, "plugin-github: init failed:", err)
		os.Exit(1)
	}
	atomic.StoreInt64(&st.startedAtUnix, time.Now().Unix())

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)
	enc := json.NewEncoder(os.Stdout)

	for scanner.Scan() {
		var req map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			fmt.Fprintln(os.Stderr, "plugin-github: bad json:", err)
			continue
		}
		resp := st.handle(req)
		if resp == nil {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "plugin-github: encode:", err)
			return
		}
	}
}

// buildState picks the best available auth path:
// 1. ~/.neo/plugins/github.json (multi-tenant) → clientPool
// 2. GITHUB_TOKEN env var (legacy single-tenant) → single client
// Audit log path defaults to ~/.neo/audit-github.log; create it lazy
// so first-boot doesn't fail on a missing dir.
func buildState() (*state, error) {
	auditPath := defaultAuditLogPath()
	// Multi-tenant config takes priority.
	if githubConfigFileExists() {
		cfg, err := loadGithubPluginConfig(defaultGithubConfigPath)
		if err != nil {
			return nil, fmt.Errorf("plugin-github config: %w", err)
		}
		fmt.Fprintf(os.Stderr, "plugin-github: loaded config (active=%s, %d project(s), %d api_key(s))\n",
			cfg.ActiveProject, len(cfg.Projects), len(cfg.APIKeys))
		return &state{
			pluginCfg: cfg,
			pool:      newClientPool(cfg),
			auditPath: auditPath,
		}, nil
	}
	// Legacy single-tenant fallback.
	token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	if token == "" {
		return nil, errors.New("GITHUB_TOKEN env var is required (or set up ~/.neo/plugins/github.json)")
	}
	c, err := github.NewClient(github.Config{
		BaseURL: os.Getenv("GITHUB_BASE_URL"),
		Token:   token,
	})
	if err != nil {
		return nil, err
	}
	return &state{client: c, auditPath: auditPath}, nil
}

// resolveClient picks the right client per request. Multi-tenant
// callers can pass an explicit project name in args["project"];
// legacy callers fall through to the single-tenant client.
func (s *state) resolveClient(args map[string]any) (*github.Client, *Project, error) {
	if s.pool != nil {
		projName := strFromArgs(args, "project")
		return s.pool.clientFor(projName)
	}
	if s.client != nil {
		return s.client, nil, nil
	}
	return nil, nil, errors.New("no GitHub client configured")
}

// auditCall writes one ledger entry for an action's outcome. Errors
// are logged to stderr but never abort dispatch — audit MUST be
// best-effort to avoid turning a chat-channel issue into a 500.
// [Area 2.2.F]
func (s *state) auditCall(proj *Project, action, result string, details map[string]any) {
	if s.auditPath == "" {
		return
	}
	tenant, owner, repo, projName := "", "", "", ""
	if proj != nil {
		tenant = proj.APIKeyRef
		owner, repo = proj.Owner, proj.Repo
	}
	if s.pluginCfg != nil {
		projName = s.pluginCfg.ActiveProject
	}
	if err := appendAuditEvent(s.auditPath, auditEvent{
		Tenant:  tenant,
		Project: projName,
		Owner:   owner,
		Repo:    repo,
		Action:  action,
		Result:  result,
		Details: details,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "plugin-github: AUDIT WRITE FAILED for %s: %v\n", action, err)
	}
}

// handle is the JSON-RPC dispatch entrypoint. Mirrors plugin-jira's
// handle exactly so future shared infra (drain, audit, multi-tenant)
// migrates with minimal diff.
func (s *state) handle(req map[string]any) map[string]any {
	method, _ := req["method"].(string)
	id := req["id"]
	switch method {
	case "initialize":
		return handleInitialize(id)
	case "notifications/initialized":
		return nil
	case "tools/list":
		return handleToolsList(id)
	case "tools/call":
		return s.handleToolsCall(id, req)
	}
	return rpcErr(id, -32601, "method not found: "+method)
}

func handleInitialize(id any) map[string]any {
	return ok(id, map[string]any{
		"protocolVersion": protocolVersion,
		"serverInfo": map[string]any{
			"name":    "plugin-github",
			"version": pluginVersion,
		},
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
	})
}

func handleToolsList(id any) map[string]any {
	allActions := []string{
		"list_prs", "create_pr", "merge_pr", "close_pr",
		"pr_comments", "create_review",
		"list_issues", "create_issue", "update_issue",
		"get_checks", "list_branches", "compare",
		"cross_ref", "__health__",
	}
	return ok(id, map[string]any{
		"tools": []map[string]any{
			{
				"name":        "github",
				"description": "GitHub REST v3 actions: PRs, issues, reviews, CI, branches, cross-ref, __health__.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action": map[string]any{
							"type":        "string",
							"description": "Required. One of the listed actions.",
							"enum":        allActions,
						},
						"owner":         map[string]any{"type": "string", "description": "Repo owner (org or user)."},
						"repo":          map[string]any{"type": "string", "description": "Repo name."},
						"state":         map[string]any{"type": "string", "description": "[list_prs/issues] open|closed|all (default open)."},
						"number":        map[string]any{"type": "integer", "description": "[merge_pr/close_pr/pr_comments/create_review/update_issue] PR or issue number."},
						"title":         map[string]any{"type": "string", "description": "[create_pr/create_issue] Title."},
						"body":          map[string]any{"type": "string", "description": "[create_pr/create_issue/create_review] Body / description."},
						"head":          map[string]any{"type": "string", "description": "[create_pr] Source branch."},
						"base":          map[string]any{"type": "string", "description": "[create_pr/compare] Target branch."},
						"merge_method":  map[string]any{"type": "string", "description": "[merge_pr] merge|squash|rebase (default merge)."},
						"event":         map[string]any{"type": "string", "description": "[create_review] APPROVE|REQUEST_CHANGES|COMMENT."},
						"labels":        map[string]any{"type": "array", "description": "[create_issue] Optional label names."},
						"fields":        map[string]any{"type": "object", "description": "[update_issue] Patch fields (state/title/body/labels)."},
						"ref":           map[string]any{"type": "string", "description": "[get_checks] Commit SHA or branch name."},
						"text":          map[string]any{"type": "string", "description": "[cross_ref] Free text to scan for Jira ticket keys."},
						"jira_pattern":  map[string]any{"type": "string", "description": "[cross_ref] Regex (default `[A-Z][A-Z0-9]{1,9}-\\d+`)."},
					},
					"required": []string{"action"},
				},
			},
		},
	})
}

func (s *state) handleToolsCall(id any, req map[string]any) map[string]any {
	atomic.StoreInt64(&s.lastDispatchUnix, time.Now().Unix())
	params, _ := req["params"].(map[string]any)
	args, _ := params["arguments"].(map[string]any)
	action, _ := args["action"].(string)
	switch action {
	case "list_prs":
		return s.callListPRs(id, args)
	case "create_pr":
		return s.callCreatePR(id, args)
	case "merge_pr":
		return s.callMergePR(id, args)
	case "close_pr":
		return s.callClosePR(id, args)
	case "pr_comments":
		return s.callPRComments(id, args)
	case "create_review":
		return s.callCreateReview(id, args)
	case "list_issues":
		return s.callListIssues(id, args)
	case "create_issue":
		return s.callCreateIssue(id, args)
	case "update_issue":
		return s.callUpdateIssue(id, args)
	case "get_checks":
		return s.callGetChecks(id, args)
	case "list_branches":
		return s.callListBranches(id, args)
	case "compare":
		return s.callCompare(id, args)
	case "cross_ref":
		return s.callCrossRef(id, args)
	case "__health__":
		return s.callHealth(id)
	}
	atomic.AddInt64(&s.errorCount, 1)
	return rpcErr(id, -32602, "unknown action: "+action)
}

// requireOwnerRepo extracts owner+repo or returns a clean error envelope.
// All multi-action handlers below share this contract.
func requireOwnerRepo(id any, args map[string]any) (string, string, map[string]any) {
	owner := strings.TrimSpace(strFromArgs(args, "owner"))
	repo := strings.TrimSpace(strFromArgs(args, "repo"))
	if owner == "" || repo == "" {
		return "", "", rpcErr(id, -32602, "owner and repo are required")
	}
	return owner, repo, nil
}

func (s *state) callCreatePR(id any, args map[string]any) map[string]any {
	owner, repo, errResp := requireOwnerRepo(id, args)
	if errResp != nil {
		return errResp
	}
	title := strFromArgs(args, "title")
	body := strFromArgs(args, "body")
	head := strFromArgs(args, "head")
	base := strFromArgs(args, "base")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pr, err := s.client.CreatePR(ctx, owner, repo, title, body, head, base)
	if err != nil {
		atomic.AddInt64(&s.errorCount, 1)
		return rpcErr(id, -32603, fmt.Sprintf("create_pr %s/%s: %v", owner, repo, err))
	}
	return ok(id, textContent(fmt.Sprintf("✅ Opened PR #%d %q\n%s", pr.Number, pr.Title, pr.HTMLURL)))
}

func (s *state) callMergePR(id any, args map[string]any) map[string]any {
	owner, repo, errResp := requireOwnerRepo(id, args)
	if errResp != nil {
		return errResp
	}
	number := intFromArgs(args, "number")
	if number == 0 {
		return rpcErr(id, -32602, "number is required")
	}
	mergeMethod := strFromArgs(args, "merge_method")
	commitTitle := strFromArgs(args, "title")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.client.MergePR(ctx, owner, repo, number, mergeMethod, commitTitle); err != nil {
		atomic.AddInt64(&s.errorCount, 1)
		return rpcErr(id, -32603, fmt.Sprintf("merge_pr %s/%s#%d: %v", owner, repo, number, err))
	}
	return ok(id, textContent(fmt.Sprintf("✅ Merged %s/%s#%d (method=%s)", owner, repo, number, mergeMethod)))
}

func (s *state) callClosePR(id any, args map[string]any) map[string]any {
	owner, repo, errResp := requireOwnerRepo(id, args)
	if errResp != nil {
		return errResp
	}
	number := intFromArgs(args, "number")
	if number == 0 {
		return rpcErr(id, -32602, "number is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.client.ClosePR(ctx, owner, repo, number); err != nil {
		atomic.AddInt64(&s.errorCount, 1)
		return rpcErr(id, -32603, fmt.Sprintf("close_pr %s/%s#%d: %v", owner, repo, number, err))
	}
	return ok(id, textContent(fmt.Sprintf("✅ Closed %s/%s#%d", owner, repo, number)))
}

func (s *state) callPRComments(id any, args map[string]any) map[string]any {
	owner, repo, errResp := requireOwnerRepo(id, args)
	if errResp != nil {
		return errResp
	}
	number := intFromArgs(args, "number")
	if number == 0 {
		return rpcErr(id, -32602, "number is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	comments, err := s.client.PRComments(ctx, owner, repo, number)
	if err != nil {
		atomic.AddInt64(&s.errorCount, 1)
		return rpcErr(id, -32603, fmt.Sprintf("pr_comments %s/%s#%d: %v", owner, repo, number, err))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## %s/%s#%d — %d comment(s)\n\n", owner, repo, number, len(comments))
	for _, c := range comments {
		fmt.Fprintf(&sb, "- **%s** (%s)\n  %s\n", c.User.Login, c.CreatedAt, truncateText(c.Body, 200))
	}
	return ok(id, textContent(sb.String()))
}

func (s *state) callCreateReview(id any, args map[string]any) map[string]any {
	owner, repo, errResp := requireOwnerRepo(id, args)
	if errResp != nil {
		return errResp
	}
	number := intFromArgs(args, "number")
	if number == 0 {
		return rpcErr(id, -32602, "number is required")
	}
	event := strings.ToUpper(strFromArgs(args, "event"))
	switch event {
	case "APPROVE", "REQUEST_CHANGES", "COMMENT":
	default:
		return rpcErr(id, -32602, "event must be APPROVE | REQUEST_CHANGES | COMMENT")
	}
	body := strFromArgs(args, "body")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.client.CreateReview(ctx, owner, repo, number, event, body); err != nil {
		atomic.AddInt64(&s.errorCount, 1)
		return rpcErr(id, -32603, fmt.Sprintf("create_review %s/%s#%d: %v", owner, repo, number, err))
	}
	return ok(id, textContent(fmt.Sprintf("✅ Reviewed %s/%s#%d (%s)", owner, repo, number, event)))
}

func (s *state) callCreateIssue(id any, args map[string]any) map[string]any {
	owner, repo, errResp := requireOwnerRepo(id, args)
	if errResp != nil {
		return errResp
	}
	title := strFromArgs(args, "title")
	body := strFromArgs(args, "body")
	labels := stringSliceFromArgs(args, "labels")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	issue, err := s.client.CreateIssue(ctx, owner, repo, title, body, labels)
	if err != nil {
		atomic.AddInt64(&s.errorCount, 1)
		return rpcErr(id, -32603, fmt.Sprintf("create_issue %s/%s: %v", owner, repo, err))
	}
	return ok(id, textContent(fmt.Sprintf("✅ Opened issue #%d %q\n%s", issue.Number, issue.Title, issue.HTMLURL)))
}

func (s *state) callUpdateIssue(id any, args map[string]any) map[string]any {
	owner, repo, errResp := requireOwnerRepo(id, args)
	if errResp != nil {
		return errResp
	}
	number := intFromArgs(args, "number")
	if number == 0 {
		return rpcErr(id, -32602, "number is required")
	}
	fields, _ := args["fields"].(map[string]any)
	if len(fields) == 0 {
		return rpcErr(id, -32602, "fields object with at least one key is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.client.UpdateIssue(ctx, owner, repo, number, fields); err != nil {
		atomic.AddInt64(&s.errorCount, 1)
		return rpcErr(id, -32603, fmt.Sprintf("update_issue %s/%s#%d: %v", owner, repo, number, err))
	}
	return ok(id, textContent(fmt.Sprintf("✅ Patched %s/%s#%d (%d field(s))", owner, repo, number, len(fields))))
}

func (s *state) callGetChecks(id any, args map[string]any) map[string]any {
	owner, repo, errResp := requireOwnerRepo(id, args)
	if errResp != nil {
		return errResp
	}
	ref := strFromArgs(args, "ref")
	if ref == "" {
		return rpcErr(id, -32602, "ref (commit SHA or branch) is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runs, err := s.client.GetChecks(ctx, owner, repo, ref)
	if err != nil {
		atomic.AddInt64(&s.errorCount, 1)
		return rpcErr(id, -32603, fmt.Sprintf("get_checks %s/%s@%s: %v", owner, repo, ref, err))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Checks for %s/%s @ %s — %d run(s)\n\n", owner, repo, ref, len(runs))
	for _, r := range runs {
		conclusion := r.Conclusion
		if conclusion == "" {
			conclusion = "(running)"
		}
		fmt.Fprintf(&sb, "- **%s** [%s] %s\n  %s\n", r.Name, r.Status, conclusion, r.HTMLURL)
	}
	return ok(id, textContent(sb.String()))
}

func (s *state) callListBranches(id any, args map[string]any) map[string]any {
	owner, repo, errResp := requireOwnerRepo(id, args)
	if errResp != nil {
		return errResp
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	branches, err := s.client.ListBranches(ctx, owner, repo)
	if err != nil {
		atomic.AddInt64(&s.errorCount, 1)
		return rpcErr(id, -32603, fmt.Sprintf("list_branches %s/%s: %v", owner, repo, err))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## %s/%s — %d branch(es)\n\n", owner, repo, len(branches))
	for _, b := range branches {
		flag := ""
		if b.Protected {
			flag = " 🔒"
		}
		fmt.Fprintf(&sb, "- `%s`%s @ `%s`\n", b.Name, flag, b.Commit.SHA[:8])
	}
	return ok(id, textContent(sb.String()))
}

func (s *state) callCompare(id any, args map[string]any) map[string]any {
	owner, repo, errResp := requireOwnerRepo(id, args)
	if errResp != nil {
		return errResp
	}
	base := strFromArgs(args, "base")
	head := strFromArgs(args, "head")
	if base == "" || head == "" {
		return rpcErr(id, -32602, "base and head are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmp, err := s.client.CompareCommits(ctx, owner, repo, base, head)
	if err != nil {
		atomic.AddInt64(&s.errorCount, 1)
		return rpcErr(id, -32603, fmt.Sprintf("compare %s/%s %s...%s: %v", owner, repo, base, head, err))
	}
	// Surface the headline numbers from the GitHub response.
	ahead, _ := cmp["ahead_by"].(float64)
	behind, _ := cmp["behind_by"].(float64)
	totalCommits := 0
	if commits, ok := cmp["commits"].([]any); ok {
		totalCommits = len(commits)
	}
	return ok(id, textContent(fmt.Sprintf("## %s/%s — %s...%s\n\nahead: %d · behind: %d · commits: %d",
		owner, repo, base, head, int(ahead), int(behind), totalCommits)))
}

// callCrossRef extracts Jira ticket keys from arbitrary text. Useful
// when an MCP client wants to walk PR descriptions and surface the
// related Jira tickets — passive only, no inter-plugin call.
// [Area 2.2.E]
func (s *state) callCrossRef(id any, args map[string]any) map[string]any {
	text := strFromArgs(args, "text")
	if text == "" {
		return rpcErr(id, -32602, "text is required")
	}
	pattern := strFromArgs(args, "jira_pattern")
	if pattern == "" {
		pattern = `[A-Z][A-Z0-9]{1,9}-\d+`
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return rpcErr(id, -32602, fmt.Sprintf("invalid regex: %v", err))
	}
	hits := re.FindAllString(text, -1)
	// Deduplicate while preserving first-seen order.
	seen := make(map[string]bool, len(hits))
	out := make([]string, 0, len(hits))
	for _, k := range hits {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	body, _ := json.Marshal(map[string]any{
		"keys":  out,
		"count": len(out),
	})
	return ok(id, textContent(string(body)))
}

// truncateText caps body display in chat-style summaries.
func truncateText(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// intFromArgs extracts integer-shaped values whether the JSON arrived
// as float64 (the encoding/json default for numbers) or as a string.
func intFromArgs(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		var n int
		_, _ = fmt.Sscanf(v, "%d", &n)
		return n
	}
	return 0
}

// stringSliceFromArgs unwraps a []any of strings ([]any is what
// encoding/json decodes JSON arrays into).
func stringSliceFromArgs(args map[string]any, key string) []string {
	raw, _ := args[key].([]any)
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (s *state) callListPRs(id any, args map[string]any) map[string]any {
	owner := strings.TrimSpace(strFromArgs(args, "owner"))
	repo := strings.TrimSpace(strFromArgs(args, "repo"))
	state := strFromArgs(args, "state")
	if owner == "" || repo == "" {
		return rpcErr(id, -32602, "owner and repo are required")
	}
	// Multi-tenant: resolve the right client (and project context for
	// audit). Single-tenant: returns s.client + nil project.
	client, proj, err := s.resolveClient(args)
	if err != nil {
		atomic.AddInt64(&s.errorCount, 1)
		return rpcErr(id, -32603, fmt.Sprintf("resolve client: %v", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prs, err := client.ListPRs(ctx, owner, repo, state)
	if err != nil {
		atomic.AddInt64(&s.errorCount, 1)
		s.auditCall(proj, "list_prs", "error", map[string]any{"owner": owner, "repo": repo, "err": err.Error()})
		return rpcErr(id, -32603, fmt.Sprintf("list_prs %s/%s: %v", owner, repo, err))
	}
	s.auditCall(proj, "list_prs", "ok", map[string]any{"owner": owner, "repo": repo, "count": len(prs)})
	return ok(id, textContent(formatPRs(owner, repo, state, prs)))
}

func (s *state) callListIssues(id any, args map[string]any) map[string]any {
	owner := strings.TrimSpace(strFromArgs(args, "owner"))
	repo := strings.TrimSpace(strFromArgs(args, "repo"))
	state := strFromArgs(args, "state")
	if owner == "" || repo == "" {
		return rpcErr(id, -32602, "owner and repo are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	issues, err := s.client.ListIssues(ctx, owner, repo, state)
	if err != nil {
		atomic.AddInt64(&s.errorCount, 1)
		return rpcErr(id, -32603, fmt.Sprintf("list_issues %s/%s: %v", owner, repo, err))
	}
	return ok(id, textContent(formatIssues(owner, repo, state, issues)))
}

// callHealth implements the PLUGIN-HEALTH-CONTRACT __health__ action.
// MUST be local-only — never call upstream GitHub API from here.
// Target latency <10ms. Counters are atomic so the read path is
// lock-free.
func (s *state) callHealth(id any) map[string]any {
	now := time.Now().Unix()
	startedAt := atomic.LoadInt64(&s.startedAtUnix)
	uptime := int64(0)
	if startedAt > 0 {
		uptime = now - startedAt
	}
	return ok(id, map[string]any{
		"plugin_alive":      true,
		"tools_registered":  []string{"github"},
		"uptime_seconds":    uptime,
		"last_dispatch_unix": atomic.LoadInt64(&s.lastDispatchUnix),
		"error_count":       atomic.LoadInt64(&s.errorCount),
		"api_key_present":   s.client != nil && s.client.Token != "",
	})
}

func formatPRs(owner, repo, state string, prs []github.PullRequest) string {
	if state == "" {
		state = "open"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## %s/%s — %d %s PR(s)\n\n", owner, repo, len(prs), state)
	for _, p := range prs {
		fmt.Fprintf(&sb, "- **#%d** [%s] %s\n  by `%s` · %s ← %s\n  %s\n",
			p.Number, p.State, p.Title, p.User.Login, p.Base.Ref, p.Head.Ref, p.HTMLURL)
	}
	return sb.String()
}

func formatIssues(owner, repo, state string, issues []github.Issue) string {
	if state == "" {
		state = "open"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## %s/%s — %d %s issue(s)\n\n", owner, repo, len(issues), state)
	for _, i := range issues {
		fmt.Fprintf(&sb, "- **#%d** [%s] %s\n  by `%s`\n  %s\n",
			i.Number, i.State, i.Title, i.User.Login, i.HTMLURL)
	}
	return sb.String()
}

// ── helpers (mirror plugin-jira) ─────────────────────────────────────

func strFromArgs(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func ok(id any, result any) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
}

func rpcErr(id any, code int, msg string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	}
}

func textContent(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	}
}
