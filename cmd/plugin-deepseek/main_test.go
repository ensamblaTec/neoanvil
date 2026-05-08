package main

import (
	"encoding/json"
	"testing"
)

func TestHandleInitialize(t *testing.T) {
	resp := handleInitialize(1)
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatal("result missing")
	}
	if result["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v", result["protocolVersion"])
	}
}

func TestHandleToolsList(t *testing.T) {
	resp := handleToolsList(1)
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatal("result missing")
	}
	tools, _ := result["tools"].([]map[string]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0]["name"] != "call" {
		t.Errorf("tool name = %v", tools[0]["name"])
	}
	// Verify required field is a non-nil array (LEY 9 — never nil).
	schema, _ := tools[0]["inputSchema"].(map[string]any)
	req, _ := schema["required"].([]string)
	if len(req) == 0 {
		t.Error("required must be non-empty array per JSON Schema spec (LEY 9)")
	}
}

func TestHandleToolsCall_Stub(t *testing.T) {
	st := &state{apiKey: "test-key"}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "call",
			"arguments": map[string]any{
				"action":        "distill_payload",
				"target_prompt": "Summarize the following context.",
			},
		},
	})
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	resp := st.handle(req)
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]map[string]any)
	if len(content) == 0 || content[0]["text"] == "" {
		t.Error("expected non-empty text content")
	}
}

func TestHandleToolsCall_MissingAction(t *testing.T) {
	st := &state{apiKey: "k"}
	req := map[string]any{
		"method": "tools/call",
		"id":     1,
		"params": map[string]any{
			"name":      "call",
			"arguments": map[string]any{"target_prompt": "hello"},
		},
	}
	resp := st.handle(req)
	if _, ok := resp["error"]; !ok {
		t.Error("expected error for missing action")
	}
}

func TestHandleUnknownMethod(t *testing.T) {
	st := &state{apiKey: "k"}
	resp := st.handle(map[string]any{"method": "ping", "id": 1})
	if _, ok := resp["error"]; !ok {
		t.Error("expected error for unknown method")
	}
}
