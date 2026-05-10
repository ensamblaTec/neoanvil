package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func ollamaGenerateMock(t *testing.T, response string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": response,
		})
	})
	return httptest.NewServer(mux)
}

func TestLocalLLMTool_Schema(t *testing.T) {
	tool := NewLocalLLMTool("http://example", "qwen2.5-coder:7b")
	if tool.Name() != "neo_local_llm" {
		t.Errorf("Name(): got %q", tool.Name())
	}
	schema := tool.InputSchema()
	if got := schema.Required; len(got) != 1 || got[0] != "prompt" {
		t.Errorf("Required: got %v, want [prompt]", got)
	}
	if _, ok := schema.Properties["model"]; !ok {
		t.Errorf("schema should expose model field")
	}
	if _, ok := schema.Properties["system"]; !ok {
		t.Errorf("schema should expose system field")
	}
}

func TestLocalLLMTool_Execute_HappyPath(t *testing.T) {
	// extractMCPText helper is defined in radar_inbox_test.go and reused.
	srv := ollamaGenerateMock(t, "func Foo() error { return nil }")
	defer srv.Close()

	tool := NewLocalLLMTool(srv.URL, "qwen2.5-coder:7b")
	out, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "write a stub for Foo",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	text := extractMCPText(t, out)
	if !strings.Contains(text, "Foo") {
		t.Errorf("response body missing expected token: %q", text)
	}
	if !strings.Contains(text, "qwen2.5-coder:7b") {
		t.Errorf("metadata footer missing model: %q", text)
	}
	if !strings.Contains(text, "latency:") {
		t.Errorf("metadata footer missing latency: %q", text)
	}
}

func TestLocalLLMTool_Execute_SystemPromptInjected(t *testing.T) {
	// Capture the prompt the server sees so we can prove the system-prefix
	// concatenation happens before the HTTP call.
	var captured string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		captured = req.Prompt
		_ = json.NewEncoder(w).Encode(map[string]any{"response": "ok"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tool := NewLocalLLMTool(srv.URL, "qwen2.5-coder:7b")
	_, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "second part",
		"system": "first part",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(captured, "SYSTEM:\nfirst part") {
		t.Errorf("system prompt not prefixed: %q", captured)
	}
	if !strings.Contains(captured, "USER:\nsecond part") {
		t.Errorf("user section missing: %q", captured)
	}
}

func TestLocalLLMTool_Execute_ModelOverride(t *testing.T) {
	var captured string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		captured = req.Model
		_ = json.NewEncoder(w).Encode(map[string]any{"response": "ok"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tool := NewLocalLLMTool(srv.URL, "qwen2.5-coder:32b")
	_, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "hi",
		"model":  "qwen2.5-coder:7b",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if captured != "qwen2.5-coder:7b" {
		t.Errorf("override not applied: got %q", captured)
	}
}

func TestLocalLLMTool_Execute_EmptyPromptError(t *testing.T) {
	tool := NewLocalLLMTool("http://example", "qwen2.5-coder:7b")
	_, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "   ",
	})
	if err == nil {
		t.Error("expected error on empty prompt, got nil")
	}
	if !strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("error should mention prompt requirement, got: %v", err)
	}
}

func TestLocalLLMTool_Execute_ServerError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "ollama down", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tool := NewLocalLLMTool(srv.URL, "qwen2.5-coder:7b")
	_, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "hi",
	})
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
	if !strings.Contains(err.Error(), "local llm") {
		t.Errorf("error should be wrapped: %v", err)
	}
}
