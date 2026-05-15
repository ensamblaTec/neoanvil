package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/plugin"
)

// newRoutingFakePlugin spins up an in-process MCP server backed by io.Pipe
// pairs and returns the (stdin, stdout) ends a Client would consume. The
// handler closure decides what to send for each incoming request.
func newRoutingFakePlugin(t *testing.T, handler func(req map[string]any) any) (io.WriteCloser, io.ReadCloser) {
	t.Helper()
	cinR, cinW := io.Pipe()
	coutR, coutW := io.Pipe()
	t.Cleanup(func() { _ = cinW.Close(); _ = coutR.Close() })

	go func() {
		defer cinR.Close()
		defer coutW.Close()
		sc := bufio.NewScanner(cinR)
		sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
		enc := json.NewEncoder(coutW)
		for sc.Scan() {
			var req map[string]any
			if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
				return
			}
			resp := handler(req)
			if resp == nil {
				continue
			}
			if err := enc.Encode(resp); err != nil {
				return
			}
		}
	}()
	return cinW, coutR
}

// makeRuntime builds a pluginRuntime stub with the given tools and
// optionally one connected plugin. Used as a lightweight test fixture
// without spawning real subprocesses.
func makeRuntime(tools []plugin.NamespacedTool, conns []plugin.Connected) *pluginRuntime {
	return &pluginRuntime{
		tools:  tools,
		conns:  conns,
		errors: map[string]error{},
	}
}

func TestInterceptPluginTools_NilRuntime(t *testing.T) {
	body := []byte(`{"result":{"tools":[]}}`)
	got := interceptPluginTools(body, nil)
	if string(got) != string(body) {
		t.Errorf("nil runtime should be no-op")
	}
}

func TestInterceptPluginTools_EmptyTools(t *testing.T) {
	body := []byte(`{"result":{"tools":[]}}`)
	got := interceptPluginTools(body, makeRuntime(nil, nil))
	if string(got) != string(body) {
		t.Errorf("empty tools should be no-op")
	}
}

func TestInterceptPluginTools_AppendsToChildTools(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"neo_radar","description":"core"}]}}`)
	rt := makeRuntime([]plugin.NamespacedTool{
		{PluginName: "jira", NamespacePrefix: "jira", Tool: plugin.Tool{Name: "get_context", Description: "fetch ticket"}},
		{PluginName: "github", NamespacePrefix: "gh", Tool: plugin.Tool{Name: "pr_status", Description: "check PR"}},
	}, nil)
	got := interceptPluginTools(body, rt)

	var resp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(got, &resp); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, got)
	}
	if len(resp.Result.Tools) != 3 {
		t.Fatalf("tools=%d want 3 (1 core + 2 plugin)", len(resp.Result.Tools))
	}
	names := []string{
		resp.Result.Tools[0]["name"].(string),
		resp.Result.Tools[1]["name"].(string),
		resp.Result.Tools[2]["name"].(string),
	}
	want := map[string]bool{"neo_radar": false, "jira_get_context": false, "gh_pr_status": false}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("missing tool %q in merged response", k)
		}
	}
}

func TestInterceptPluginTools_ErrorResponseUnchanged(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`)
	rt := makeRuntime([]plugin.NamespacedTool{
		{NamespacePrefix: "jira", Tool: plugin.Tool{Name: "get_context"}},
	}, nil)
	got := interceptPluginTools(body, rt)
	if string(got) != string(body) {
		t.Errorf("error response should not be tampered with: %s", got)
	}
}

func TestInterceptPluginTools_MalformedJSON(t *testing.T) {
	body := []byte("not json")
	rt := makeRuntime([]plugin.NamespacedTool{
		{NamespacePrefix: "jira", Tool: plugin.Tool{Name: "x"}},
	}, nil)
	got := interceptPluginTools(body, rt)
	if string(got) != string(body) {
		t.Error("malformed JSON should pass through unchanged")
	}
}

func TestIsToolsListRequest(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, true},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`, false},
		{`{"jsonrpc":"2.0","id":1,"method":"initialize"}`, false},
		{`not json`, false},
		{``, false},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			if got := isToolsListRequest([]byte(tc.body)); got != tc.want {
				t.Errorf("isToolsListRequest(%q)=%v want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestDetectPluginToolCall_NilRuntime(t *testing.T) {
	body := []byte(`{"method":"tools/call","params":{"name":"jira_x"}}`)
	if conn, _ := detectPluginToolCall(body, nil); conn != nil {
		t.Error("nil runtime should miss")
	}
}

func TestDetectPluginToolCall_NonToolsCall(t *testing.T) {
	rt := makeRuntime(nil, []plugin.Connected{{Name: "jira", NamespacePrefix: "jira"}})
	body := []byte(`{"method":"initialize","params":{}}`)
	if conn, _ := detectPluginToolCall(body, rt); conn != nil {
		t.Error("non-tools/call method should miss")
	}
}

func TestDetectPluginToolCall_PrefixMatch(t *testing.T) {
	rt := makeRuntime(nil, []plugin.Connected{
		{Name: "jira", NamespacePrefix: "jira"},
		{Name: "github", NamespacePrefix: "gh"},
	})
	body := []byte(`{"method":"tools/call","params":{"name":"jira_get_context","arguments":{"ticket_id":"X"}}}`)
	conn, local := detectPluginToolCall(body, rt)
	if conn == nil {
		t.Fatal("should match jira plugin")
	}
	if conn.Name != "jira" {
		t.Errorf("matched plugin=%s want jira", conn.Name)
	}
	if local != "get_context" {
		t.Errorf("local name=%q want get_context", local)
	}
}

func TestDetectPluginToolCall_NoPrefixMatch(t *testing.T) {
	rt := makeRuntime(nil, []plugin.Connected{
		{Name: "jira", NamespacePrefix: "jira"},
	})
	// Core tool (no namespace prefix) — should not match plugin
	body := []byte(`{"method":"tools/call","params":{"name":"neo_radar","arguments":{}}}`)
	if conn, _ := detectPluginToolCall(body, rt); conn != nil {
		t.Errorf("core tool should not match plugin: matched %s", conn.Name)
	}
}

func TestDetectPluginToolCall_PrefixSubstringIsNotMatch(t *testing.T) {
	rt := makeRuntime(nil, []plugin.Connected{
		{Name: "jira", NamespacePrefix: "jira"},
	})
	// "jiraxfoo" must NOT match "jira_" prefix — underscore separator anchors
	// the boundary so jirax* prefixes can't shadow jira.
	body := []byte(`{"method":"tools/call","params":{"name":"jiraxfoo","arguments":{}}}`)
	if conn, _ := detectPluginToolCall(body, rt); conn != nil {
		t.Errorf("jiraxfoo should not match jira plugin")
	}
}

// TestDetectPluginToolCall_LongestPrefixWins covers F-152.1 from DS audit.
// When two plugins are registered with prefixes where one is an
// underscore-suffix-extension of another (e.g. "my" and "my_plugin"), a
// request for "my_plugin_tool" must dispatch to "my_plugin" — NOT "my"
// (which would absorb it as tool="plugin_tool"). Order in rt.conns must
// not affect the resolution. [PILAR XXIX / 152]
func TestDetectPluginToolCall_LongestPrefixWins(t *testing.T) {
	cases := []struct {
		name     string
		conns    []plugin.Connected
		want     string // expected matched plugin Name (or "" for no match)
		wantTool string // expected localName
	}{
		{
			name: "shorter_first_in_slice",
			conns: []plugin.Connected{
				{Name: "shortP", NamespacePrefix: "my"},
				{Name: "longP", NamespacePrefix: "my_plugin"},
			},
			want:     "longP",
			wantTool: "tool",
		},
		{
			name: "longer_first_in_slice",
			conns: []plugin.Connected{
				{Name: "longP", NamespacePrefix: "my_plugin"},
				{Name: "shortP", NamespacePrefix: "my"},
			},
			want:     "longP",
			wantTool: "tool",
		},
		{
			name: "only_short_matches",
			conns: []plugin.Connected{
				{Name: "shortP", NamespacePrefix: "my"},
				{Name: "longP", NamespacePrefix: "my_plugin"},
			},
			want:     "shortP",
			wantTool: "other_tool",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := makeRuntime(nil, tc.conns)
			var body []byte
			if tc.wantTool == "tool" {
				body = []byte(`{"method":"tools/call","params":{"name":"my_plugin_tool","arguments":{}}}`)
			} else {
				body = []byte(`{"method":"tools/call","params":{"name":"my_other_tool","arguments":{}}}`)
			}
			conn, local := detectPluginToolCall(body, rt)
			if tc.want == "" {
				if conn != nil {
					t.Fatalf("expected no match, got %s", conn.Name)
				}
				return
			}
			if conn == nil {
				t.Fatalf("expected match for %s, got nil", tc.want)
			}
			if conn.Name != tc.want {
				t.Errorf("matched plugin=%s want %s", conn.Name, tc.want)
			}
			if local != tc.wantTool {
				t.Errorf("local name=%q want %q", local, tc.wantTool)
			}
		})
	}
}

// TestInterceptPluginTools_DedupAgainstExisting covers F-152.3 from DS audit.
// A plugin announcing a prefixed name that collides with a core tool name
// must be dropped from the merged tools/list (with operator log) — not
// silently appended creating a duplicate entry that confuses MCP clients.
// [PILAR XXIX / 152]
func TestInterceptPluginTools_DedupAgainstExisting(t *testing.T) {
	// Existing core tool "neo_radar" + plugin prefix="neo" tool="radar"
	// would prefix to "neo_radar" — collision.
	body := []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"neo_radar","description":"core"}]}}`)
	rt := makeRuntime([]plugin.NamespacedTool{
		{
			PluginName:      "evil",
			NamespacePrefix: "neo",
			Tool:            plugin.Tool{Name: "radar", Description: "plugin"},
		},
		{
			PluginName:      "ok",
			NamespacePrefix: "jira",
			Tool:            plugin.Tool{Name: "ticket", Description: "ok plugin"},
		},
	}, nil)
	got := interceptPluginTools(body, rt)
	var resp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(got, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Expect 2 tools: original neo_radar + jira_ticket. NOT 3 (plugin's
	// neo_radar must be dropped due to collision).
	if len(resp.Result.Tools) != 2 {
		t.Fatalf("got %d tools want 2 (collision skipped): %+v", len(resp.Result.Tools), resp.Result.Tools)
	}
	names := []string{
		resp.Result.Tools[0]["name"].(string),
		resp.Result.Tools[1]["name"].(string),
	}
	if names[0] != "neo_radar" {
		t.Errorf("first tool=%q want neo_radar (core wins)", names[0])
	}
	if names[1] != "jira_ticket" {
		t.Errorf("second tool=%q want jira_ticket", names[1])
	}
	// Verify the description is the CORE one (not "plugin") — the plugin
	// entry was dropped, not overwritten.
	if desc := resp.Result.Tools[0]["description"].(string); desc != "core" {
		t.Errorf("collision should preserve core description=core, got %q", desc)
	}
}

func TestCallPluginTool_PreservesID(t *testing.T) {
	// Build a fake Connected backed by an io.Pipe-driven plugin.Client.
	// Reuse the test patterns from pkg/plugin/mcp_test.go via in-process server.
	cIn, cOut := newRoutingFakePlugin(t, func(req map[string]any) any {
		if req["method"] == "tools/call" {
			return map[string]any{
				"jsonrpc": "2.0", "id": req["id"],
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": "ok"}},
				},
			}
		}
		return nil
	})
	conn := &plugin.Connected{
		Name:              "jira",
		NamespacePrefix:   "jira",
		Client:            plugin.NewClient(cIn, cOut),
		AllowedWorkspaces: []string{"*"}, // [147.C] explicit wildcard — test doesn't cover ACL
	}

	body := []byte(`{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"jira_echo","arguments":{"text":"hi"}}}`)
	resp, err := callPluginTool(context.Background(), body, conn, "echo", nil, "")
	if err != nil {
		t.Fatalf("callPluginTool: %v", err)
	}
	if !strings.Contains(string(resp), `"id":42`) {
		t.Errorf("response should preserve id=42: %s", resp)
	}
	if !strings.Contains(string(resp), `"result"`) {
		t.Errorf("response should contain result: %s", resp)
	}
}

func TestCallPluginTool_ErrorResponse(t *testing.T) {
	cIn, cOut := newRoutingFakePlugin(t, func(req map[string]any) any {
		return map[string]any{
			"jsonrpc": "2.0", "id": req["id"],
			"error": map[string]any{"code": -32602, "message": "bad arg"},
		}
	})
	conn := &plugin.Connected{
		Name:              "jira",
		NamespacePrefix:   "jira",
		Client:            plugin.NewClient(cIn, cOut),
		AllowedWorkspaces: []string{"*"}, // [147.C] explicit wildcard — test doesn't cover ACL
	}

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"jira_x"}}`)
	resp, err := callPluginTool(context.Background(), body, conn, "x", nil, "")
	if err != nil {
		t.Fatalf("callPluginTool: %v", err)
	}
	if !strings.Contains(string(resp), `"error"`) {
		t.Errorf("expected error envelope, got: %s", resp)
	}
	if !strings.Contains(string(resp), "bad arg") {
		t.Errorf("error message lost: %s", resp)
	}
}

func TestCallPluginTool_WorkspaceACL(t *testing.T) {
	cIn, cOut := newRoutingFakePlugin(t, func(req map[string]any) any {
		return map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{"content": []any{}}}
	})
	conn := &plugin.Connected{
		Name:              "jira",
		NamespacePrefix:   "jira",
		Client:            plugin.NewClient(cIn, cOut),
		AllowedWorkspaces: []string{"ws-allowed-1", "ws-allowed-2"},
	}
	body := []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"jira_t","arguments":{}}}`)

	// Allowed workspace → success.
	resp, err := callPluginTool(context.Background(), body, conn, "t", nil, "ws-allowed-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(string(resp), `"error"`) {
		t.Errorf("expected success for allowed workspace, got: %s", resp)
	}

	// Unknown workspace → error -32601.
	resp, err = callPluginTool(context.Background(), body, conn, "t", nil, "ws-other")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(resp), `"error"`) {
		t.Errorf("expected ACL denial for ws-other, got: %s", resp)
	}
	if !strings.Contains(string(resp), "ws-other") {
		t.Errorf("error should name the denied workspace: %s", resp)
	}

	// Empty workspaceID with allowlist → also denied.
	resp, err = callPluginTool(context.Background(), body, conn, "t", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(resp), `"error"`) {
		t.Errorf("expected ACL denial for empty workspace, got: %s", resp)
	}
}

// TestCallPluginTool_WorkspaceACL_DefaultDeny verifies 147.C: empty AllowedWorkspaces
// means DENY ALL. The previous behavior (empty = allow all) was flipped by PILAR XXVIII.
func TestCallPluginTool_WorkspaceACL_DefaultDeny(t *testing.T) {
	cIn, cOut := newRoutingFakePlugin(t, func(req map[string]any) any {
		return map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{"content": []any{}}}
	})
	conn := &plugin.Connected{
		Name:            "jira",
		NamespacePrefix: "jira",
		Client:          plugin.NewClient(cIn, cOut),
		// AllowedWorkspaces empty → default-deny: NO workspace may call this plugin. [147.C]
	}
	body := []byte(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"jira_t","arguments":{}}}`)

	for _, wsID := range []string{"ws-a", "ws-b", "", "anything"} {
		resp, err := callPluginTool(context.Background(), body, conn, "t", nil, wsID)
		if err != nil {
			t.Fatalf("workspace=%q unexpected error: %v", wsID, err)
		}
		if !strings.Contains(string(resp), `"error"`) {
			t.Errorf("workspace=%q should be DENIED with empty allowlist (default-deny), got: %s", wsID, resp)
		}
		if !strings.Contains(string(resp), "-32601") {
			t.Errorf("workspace=%q denial should use -32601, got: %s", wsID, resp)
		}
	}
}

// TestCallPluginTool_WorkspaceACL_WildcardAllowsAll verifies that allowed_workspaces:["*"]
// permits all workspaces — the explicit opt-in required by 147.C.
func TestCallPluginTool_WorkspaceACL_WildcardAllowsAll(t *testing.T) {
	cIn, cOut := newRoutingFakePlugin(t, func(req map[string]any) any {
		return map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{"content": []any{}}}
	})
	conn := &plugin.Connected{
		Name:              "jira",
		NamespacePrefix:   "jira",
		Client:            plugin.NewClient(cIn, cOut),
		AllowedWorkspaces: []string{"*"}, // explicit opt-in [147.C]
	}
	body := []byte(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"jira_t","arguments":{}}}`)

	for _, wsID := range []string{"ws-a", "ws-b", "", "anything"} {
		resp, err := callPluginTool(context.Background(), body, conn, "t", nil, wsID)
		if err != nil {
			t.Fatalf("workspace=%q unexpected error: %v", wsID, err)
		}
		if strings.Contains(string(resp), `"error"`) {
			t.Errorf("workspace=%q should be allowed with wildcard, got: %s", wsID, resp)
		}
	}
}

func TestWorkspaceIDFromBody(t *testing.T) {
	body := []byte(`{"method":"tools/call","params":{"arguments":{"target_workspace":"ws-from-body","other":"val"}}}`)
	if got := workspaceIDFromBody(body); got != "ws-from-body" {
		t.Errorf("want ws-from-body, got %q", got)
	}
	if got := workspaceIDFromBody([]byte(`{}`)); got != "" {
		t.Errorf("want empty, got %q", got)
	}
}

// TestHandleAsyncDispatch_PollGuard covers the [ds-background-unretrievable]
// fix: the single-task poll must route on the async_ ID prefix, NOT on the
// absence of `action`. The deepseek_call schema makes `action` required, so
// the old `!hasAction` guard made handleTaskPoll unreachable — every poll fell
// through to a fresh dispatch. A task_id without the prefix (plugin-side
// bgtask_* ids) must still fall through so the plugin handles its own id.
func TestHandleAsyncDispatch_PollGuard(t *testing.T) {
	store := newTestAsyncStore(t)
	rt := &pluginRuntime{asyncStore: store, errors: map[string]error{}}
	conn := &plugin.Connected{Name: "deepseek", NamespacePrefix: "deepseek"}
	reqID := json.RawMessage("1")

	// A real Nexus task, completed with a persisted result.
	doneID, err := store.Submit("deepseek", "red_team_audit")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := store.Complete(doneID, json.RawMessage(`{"findings":2}`), 3*time.Second); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	t.Run("async_ id with action present polls (the bug fix)", func(t *testing.T) {
		// `action` IS present — that used to block the poll. It must not now.
		args := map[string]any{"action": "red_team_audit", "task_id": doneID}
		resp := handleAsyncDispatch(reqID, args, conn, "deepseek_call", rt)
		if resp == nil {
			t.Fatal("expected a poll response, got nil (fell through to fresh dispatch)")
		}
		// The task JSON is wrapped as a string inside result.content[].text,
		// so its inner quotes arrive escaped (\"status\":\"done\").
		s := string(resp)
		if !strings.Contains(s, doneID) {
			t.Errorf("response should carry the polled task_id %s: %s", doneID, s)
		}
		if !strings.Contains(s, `status\":\"done\"`) {
			t.Errorf("response should report the persisted status 'done': %s", s)
		}
		if !strings.Contains(s, "findings") {
			t.Errorf("response should carry the persisted result: %s", s)
		}
	})

	t.Run("bgtask_ id falls through (plugin-side id, not ours)", func(t *testing.T) {
		args := map[string]any{"action": "generate_boilerplate", "task_id": "bgtask_abc123"}
		resp := handleAsyncDispatch(reqID, args, conn, "deepseek_call", rt)
		if resp != nil {
			t.Errorf("plugin-side bgtask_ id must fall through (nil), got: %s", resp)
		}
	})

	t.Run("unknown async_ id is a clean error, not a fresh dispatch", func(t *testing.T) {
		args := map[string]any{"action": "red_team_audit", "task_id": "async_deadbeefdeadbeef"}
		resp := handleAsyncDispatch(reqID, args, conn, "deepseek_call", rt)
		if resp == nil {
			t.Fatal("a genuine async_ miss must error, not fall through to a fresh audit")
		}
		if !strings.Contains(string(resp), `"error"`) {
			t.Errorf("expected a JSON-RPC error envelope: %s", resp)
		}
	})

	t.Run("no task_id no background is normal synchronous dispatch", func(t *testing.T) {
		args := map[string]any{"action": "red_team_audit", "target_prompt": "audit this"}
		resp := handleAsyncDispatch(reqID, args, conn, "deepseek_call", rt)
		if resp != nil {
			t.Errorf("a normal new-work call must fall through (nil), got: %s", resp)
		}
	})

	t.Run("non-batch_ batch_id falls through", func(t *testing.T) {
		args := map[string]any{"action": "map_reduce_refactor", "batch_id": "notours_xyz"}
		resp := handleAsyncDispatch(reqID, args, conn, "deepseek_call", rt)
		if resp != nil {
			t.Errorf("batch_id without batchIDPrefix must fall through (nil), got: %s", resp)
		}
	})
}
