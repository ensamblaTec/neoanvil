package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/knowledge"
)

// TestMemoryTool_StoreReturnsMCPEnvelope verifies that execStore returns the
// MCP-required {"content":[{"type":"text","text":...}]} envelope. A raw map
// return (pre-fix) serialized as a silent no-op at the wire protocol level.
// [Épica 330.E regression]
func TestMemoryTool_StoreReturnsMCPEnvelope(t *testing.T) {
	tmp := t.TempDir()
	ks, err := knowledge.Open(tmp + "/knowledge.db")
	if err != nil {
		t.Fatalf("knowledge.Open: %v", err)
	}
	defer ks.Close()

	tool := &MemoryTool{ks: ks, workspace: tmp}
	out, err := tool.Execute(context.Background(), map[string]any{
		"action":    "store",
		"namespace": "test",
		"key":       "k1",
		"content":   "regression test body",
	})
	if err != nil {
		t.Fatalf("Execute store: %v", err)
	}
	assertMCPEnvelope(t, out, "ok")
}

// TestMemoryTool_FetchReturnsMCPEnvelope covers execFetch which also used
// to return a raw map. [Épica 330.E regression]
func TestMemoryTool_FetchReturnsMCPEnvelope(t *testing.T) {
	tmp := t.TempDir()
	ks, err := knowledge.Open(tmp + "/knowledge.db")
	if err != nil {
		t.Fatalf("knowledge.Open: %v", err)
	}
	defer ks.Close()
	tool := &MemoryTool{ks: ks, workspace: tmp}

	// Seed one entry via store.
	if _, err := tool.Execute(context.Background(), map[string]any{
		"action": "store", "namespace": "test", "key": "k1", "content": "body",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Fetch it.
	out, err := tool.Execute(context.Background(), map[string]any{
		"action": "fetch", "namespace": "test", "key": "k1",
	})
	if err != nil {
		t.Fatalf("Execute fetch: %v", err)
	}
	assertMCPEnvelope(t, out, "body")
}

// TestMemoryTool_ListReturnsMCPEnvelope covers execList + buildListResponse.
// [Épica 330.E regression]
func TestMemoryTool_ListReturnsMCPEnvelope(t *testing.T) {
	tmp := t.TempDir()
	ks, err := knowledge.Open(tmp + "/knowledge.db")
	if err != nil {
		t.Fatalf("knowledge.Open: %v", err)
	}
	defer ks.Close()
	tool := &MemoryTool{ks: ks, workspace: tmp}
	_, _ = tool.Execute(context.Background(), map[string]any{
		"action": "store", "namespace": "test", "key": "k1", "content": "body",
	})

	out, err := tool.Execute(context.Background(), map[string]any{
		"action": "list", "namespace": "test",
	})
	if err != nil {
		t.Fatalf("Execute list: %v", err)
	}
	assertMCPEnvelope(t, out, "k1")
}

// assertMCPEnvelope verifies the response matches the MCP wire format and
// optionally that its serialized text contains an expected substring.
func assertMCPEnvelope(t *testing.T, out any, expectSubstring string) {
	t.Helper()
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", out)
	}
	contentRaw, ok := m["content"]
	if !ok {
		t.Fatalf("response missing `content` key — raw map returned instead of MCP envelope: %v", m)
	}
	contentArr, ok := contentRaw.([]map[string]any)
	if !ok {
		t.Fatalf("`content` is not []map[string]any: %T", contentRaw)
	}
	if len(contentArr) == 0 {
		t.Fatalf("`content` array is empty")
	}
	first := contentArr[0]
	typ, _ := first["type"].(string)
	if typ != "text" {
		t.Errorf("content[0].type = %q, want \"text\"", typ)
	}
	text, _ := first["text"].(string)
	if text == "" {
		t.Errorf("content[0].text is empty")
	}
	if expectSubstring != "" {
		// The text is JSON-marshaled output; just check substring presence.
		if !containsString(text, expectSubstring) {
			t.Errorf("content[0].text missing expected substring %q; got:\n%s", expectSubstring, text)
		}
	}
	// Also verify the text is valid JSON (not raw Go map syntax).
	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Errorf("content[0].text is not valid JSON: %v\nbody: %s", err, text)
	}
}

func containsString(hay, needle string) bool {
	return len(hay) >= len(needle) && indexOf(hay, needle) >= 0
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
