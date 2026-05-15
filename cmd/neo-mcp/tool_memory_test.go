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

// TestWithRemSleepDefaults covers the [neo_memory(rem_sleep) schema drift] P3
// fix. The case "rem_sleep" handler used to pass raw args straight to
// RemSleepTool.Execute, which requires learning_rate + session_success_ratio
// as float64 — but neo_memory's schema did not expose them, making manual
// rem_sleep unreachable. withRemSleepDefaults injects the canonical defaults
// when omitted; explicit caller values still win.
func TestWithRemSleepDefaults(t *testing.T) {
	t.Run("empty args fills both defaults", func(t *testing.T) {
		out := withRemSleepDefaults(map[string]any{})
		if got := out["learning_rate"]; got != defaultRemLearningRate {
			t.Errorf("learning_rate=%v want %v", got, defaultRemLearningRate)
		}
		if got := out["session_success_ratio"]; got != defaultRemSuccessRatio {
			t.Errorf("session_success_ratio=%v want %v", got, defaultRemSuccessRatio)
		}
	})

	t.Run("explicit values win", func(t *testing.T) {
		out := withRemSleepDefaults(map[string]any{
			"learning_rate":         0.5,
			"session_success_ratio": 0.9,
		})
		if got := out["learning_rate"]; got != 0.5 {
			t.Errorf("learning_rate=%v want 0.5", got)
		}
		if got := out["session_success_ratio"]; got != 0.9 {
			t.Errorf("session_success_ratio=%v want 0.9", got)
		}
	})

	t.Run("partial — only missing field is defaulted", func(t *testing.T) {
		out := withRemSleepDefaults(map[string]any{"learning_rate": 0.25})
		if got := out["learning_rate"]; got != 0.25 {
			t.Errorf("learning_rate=%v want 0.25", got)
		}
		if got := out["session_success_ratio"]; got != defaultRemSuccessRatio {
			t.Errorf("session_success_ratio=%v want default %v", got, defaultRemSuccessRatio)
		}
	})

	t.Run("does not mutate caller args", func(t *testing.T) {
		in := map[string]any{}
		_ = withRemSleepDefaults(in)
		if _, has := in["learning_rate"]; has {
			t.Error("caller args were mutated — withRemSleepDefaults must return a fresh map")
		}
	})

	t.Run("non-float64 type is treated as missing", func(t *testing.T) {
		// JSON unmarshal always yields float64 for numbers, so this guards
		// direct programmatic calls that pass an int by mistake — the default
		// must kick in rather than letting RemSleepTool reject the bad type.
		out := withRemSleepDefaults(map[string]any{
			"learning_rate":         42,    // int, not float64
			"session_success_ratio": "0.7", // string
		})
		if got := out["learning_rate"]; got != defaultRemLearningRate {
			t.Errorf("learning_rate=%v want default (int should be ignored)", got)
		}
		if got := out["session_success_ratio"]; got != defaultRemSuccessRatio {
			t.Errorf("session_success_ratio=%v want default (string should be ignored)", got)
		}
	})
}
