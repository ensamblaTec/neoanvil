package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/deepseek"
)

// fakeServer returns a minimal DeepSeek-compatible HTTP server.
func fakeServer(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": reply}}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
}

func TestDistillPayloadCacheHitSameFiles(t *testing.T) {
	srv := fakeServer(t, "distilled")
	defer srv.Close()

	dir := t.TempDir()
	goFile := filepath.Join(dir, "demo.go")
	os.WriteFile(goFile, []byte("package demo\nfunc Foo() {}"), 0600) //nolint:errcheck

	c, err := deepseek.New(deepseek.Config{APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	st := &state{apiKey: "k", client: c}

	args := map[string]any{
		"target_prompt": "Summarize this code.",
		"files":         []any{goFile},
	}

	// First call — builds and caches Block 1.
	resp1 := distillPayload(st, 1, args)
	if _, hasErr := resp1["error"]; hasErr {
		t.Fatalf("first call error: %v", resp1["error"])
	}
	// Second call with same file — Block 1 served from cache.
	resp2 := distillPayload(st, 2, args)
	if _, hasErr := resp2["error"]; hasErr {
		t.Fatalf("second call error: %v", resp2["error"])
	}
	result2, _ := resp2["result"].(map[string]any)
	content2, _ := result2["content"].([]map[string]any)
	if len(content2) == 0 {
		t.Fatal("expected content in second call")
	}
}

func TestDistillPayloadBabelFieldDescription(t *testing.T) {
	resp := handleToolsList(1)
	result, _ := resp["result"].(map[string]any)
	tools, _ := result["tools"].([]map[string]any)
	if len(tools) == 0 {
		t.Fatal("no tools returned")
	}
	schema, _ := tools[0]["inputSchema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	promptProp, _ := props["target_prompt"].(map[string]any)
	desc, _ := promptProp["description"].(string)
	if !strings.Contains(desc, "English") {
		t.Errorf("target_prompt description must mention 'English' (Babel Pattern), got: %q", desc)
	}
}
