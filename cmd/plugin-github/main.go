// Command plugin-github is the GitHub MCP plugin for neoanvil.
// [Area 2 — copy-adapt from cmd/plugin-jira]
//
// Wire format: newline-delimited JSON-RPC over stdio (MCP stdio transport).
// Auth: env vars (injected by Nexus PluginPool from ~/.neo/credentials.json):
//
//	GITHUB_TOKEN  — Personal Access Token from github.com/settings/tokens (required)
//	GITHUB_BASE_URL — Override for GitHub Enterprise (optional; defaults to api.github.com)
//
// Tools exposed (initial subset, more land as 2.2 progresses):
//
//	list_prs    : list pull requests for an owner/repo
//	list_issues : list issues (PRs filtered out)
//	__health__  : Nexus health probe (mandatory per PLUGIN-HEALTH-CONTRACT)
//
// More actions (create_pr, merge_pr, get_checks, etc.) land in
// follow-up commits — the dispatch table is the only place to wire them.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	client *github.Client

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

// buildState reads required env vars and constructs the GitHub client.
// Mirrors plugin-jira's buildState shape (config-first then legacy
// env vars) but without the multi-tenant Project map — GitHub PAT is
// global per token so a single Client is sufficient for v1.
func buildState() (*state, error) {
	token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	if token == "" {
		return nil, errors.New("GITHUB_TOKEN env var is required")
	}
	c, err := github.NewClient(github.Config{
		BaseURL: os.Getenv("GITHUB_BASE_URL"),
		Token:   token,
	})
	if err != nil {
		return nil, err
	}
	return &state{client: c}, nil
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
	return ok(id, map[string]any{
		"tools": []map[string]any{
			{
				"name":        "github",
				"description": "GitHub REST v3 actions: list_prs, list_issues, __health__.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action": map[string]any{
							"type":        "string",
							"description": "Required. One of: list_prs, list_issues, __health__.",
							"enum":        []string{"list_prs", "list_issues", "__health__"},
						},
						"owner": map[string]any{"type": "string", "description": "[list_prs, list_issues] Repo owner (org or user)."},
						"repo":  map[string]any{"type": "string", "description": "[list_prs, list_issues] Repo name."},
						"state": map[string]any{"type": "string", "description": "[list_prs, list_issues] open|closed|all (default open)."},
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
	case "list_issues":
		return s.callListIssues(id, args)
	case "__health__":
		return s.callHealth(id)
	}
	atomic.AddInt64(&s.errorCount, 1)
	return rpcErr(id, -32602, "unknown action: "+action)
}

func (s *state) callListPRs(id any, args map[string]any) map[string]any {
	owner := strings.TrimSpace(strFromArgs(args, "owner"))
	repo := strings.TrimSpace(strFromArgs(args, "repo"))
	state := strFromArgs(args, "state")
	if owner == "" || repo == "" {
		return rpcErr(id, -32602, "owner and repo are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prs, err := s.client.ListPRs(ctx, owner, repo, state)
	if err != nil {
		atomic.AddInt64(&s.errorCount, 1)
		return rpcErr(id, -32603, fmt.Sprintf("list_prs %s/%s: %v", owner, repo, err))
	}
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
