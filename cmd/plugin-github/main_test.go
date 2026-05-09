package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/github"
)

// TestHandleToolsList_HasAllActionsInEnum verifies the tools/list
// schema enumerates every action the dispatcher supports — keeps the
// schema honest as new actions land.
func TestHandleToolsList_HasAllActionsInEnum(t *testing.T) {
	resp := handleToolsList(1)
	result, _ := resp["result"].(map[string]any)
	tools, _ := result["tools"].([]map[string]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	schema, _ := tools[0]["inputSchema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	action, _ := props["action"].(map[string]any)
	enum, _ := action["enum"].([]string)

	expected := []string{
		"list_prs", "create_pr", "merge_pr", "close_pr",
		"pr_comments", "create_review",
		"list_issues", "create_issue", "update_issue",
		"get_checks", "list_branches", "compare",
		"cross_ref", "__health__",
	}
	for _, want := range expected {
		if !slices.Contains(enum, want) {
			t.Errorf("action enum missing %q", want)
		}
	}
}

// TestCallHealth_ShortCircuitsLocalOnly verifies the __health__ path
// returns immediately without touching the upstream client. Critical
// for PLUGIN-HEALTH-CONTRACT (must be <10ms, no API call).
func TestCallHealth_ShortCircuitsLocalOnly(t *testing.T) {
	st := &state{client: &github.Client{Token: "ghp_x"}}
	resp := st.callHealth(1)
	result, _ := resp["result"].(map[string]any)
	if alive, _ := result["plugin_alive"].(bool); !alive {
		t.Errorf("plugin_alive false")
	}
	if got, _ := result["api_key_present"].(bool); !got {
		t.Errorf("api_key_present should be true when token set")
	}
	tools, _ := result["tools_registered"].([]string)
	if len(tools) != 1 || tools[0] != "github" {
		t.Errorf("tools_registered = %v, want [github]", tools)
	}
}

// TestCallCrossRef_ExtractsAndDedups verifies the regex action
// pulls Jira keys out of free text + removes duplicates while
// preserving first-seen order.
func TestCallCrossRef_ExtractsAndDedups(t *testing.T) {
	st := &state{}
	resp := st.callCrossRef(1, map[string]any{
		"text": "Fixes MCPI-1 and MCPI-2 (relates MCPI-1 again, see ABC-99)",
	})
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]map[string]any)
	textBlob, _ := content[0]["text"].(string)
	var parsed struct {
		Keys  []string `json:"keys"`
		Count int      `json:"count"`
	}
	if err := json.Unmarshal([]byte(textBlob), &parsed); err != nil {
		t.Fatalf("invalid JSON in cross_ref text: %v\n%s", err, textBlob)
	}
	if parsed.Count != 3 {
		t.Errorf("count = %d, want 3 (MCPI-1, MCPI-2, ABC-99)", parsed.Count)
	}
	if len(parsed.Keys) != 3 || parsed.Keys[0] != "MCPI-1" || parsed.Keys[1] != "MCPI-2" || parsed.Keys[2] != "ABC-99" {
		t.Errorf("keys order/dedup wrong: %v", parsed.Keys)
	}
}

// TestCallCrossRef_AcceptsCustomPattern verifies the operator can
// pass jira_pattern to match a non-standard key shape.
func TestCallCrossRef_AcceptsCustomPattern(t *testing.T) {
	st := &state{}
	resp := st.callCrossRef(1, map[string]any{
		"text":         "Linear: NEO-42 and BUG-999. Ignored: lowercase-1.",
		"jira_pattern": `[A-Z]{3}-\d+`,
	})
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]map[string]any)
	textBlob, _ := content[0]["text"].(string)
	if !strings.Contains(textBlob, "NEO-42") || !strings.Contains(textBlob, "BUG-999") {
		t.Errorf("custom pattern missed expected keys: %s", textBlob)
	}
	if strings.Contains(textBlob, "lowercase") {
		t.Errorf("custom pattern accidentally matched lowercase")
	}
}

// TestRequireOwnerRepo_ReturnsErrorOnMissing verifies the shared
// validator surfaces a clean MCP error envelope rather than letting
// the action handler call upstream with empty owner/repo (which
// would 404 from GitHub).
func TestRequireOwnerRepo_ReturnsErrorOnMissing(t *testing.T) {
	cases := []map[string]any{
		{},
		{"owner": ""},
		{"owner": "x"},
		{"repo": "y"},
	}
	for _, args := range cases {
		_, _, errResp := requireOwnerRepo(1, args)
		if errResp == nil {
			t.Errorf("missing owner/repo accepted: %v", args)
		}
	}
	owner, repo, errResp := requireOwnerRepo(1, map[string]any{"owner": "x", "repo": "y"})
	if errResp != nil {
		t.Errorf("valid args rejected: %v", errResp)
	}
	if owner != "x" || repo != "y" {
		t.Errorf("got %q/%q, want x/y", owner, repo)
	}
}

// TestIntFromArgs_HandlesFloatStringInt verifies the multi-shape
// integer extraction (encoding/json decodes numbers as float64,
// some MCP clients send them as strings).
func TestIntFromArgs_HandlesFloatStringInt(t *testing.T) {
	cases := []struct {
		in   any
		want int
	}{
		{float64(42), 42},
		{int(7), 7},
		{"99", 99},
		{nil, 0},
	}
	for _, c := range cases {
		args := map[string]any{"number": c.in}
		got := intFromArgs(args, "number")
		if got != c.want {
			t.Errorf("intFromArgs(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}
