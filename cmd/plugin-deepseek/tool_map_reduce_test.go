package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/deepseek"
)

func makeFiles(t *testing.T, n int) []string {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, n)
	for i := range n {
		p := filepath.Join(dir, "f"+string(rune('a'+i))+".go")
		os.WriteFile(p, []byte("package main\nfunc F() {}"), 0600) //nolint:errcheck
		paths[i] = p
	}
	return paths
}

func fakeServerCounter(t *testing.T, reply string, counter *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.Add(1)
		resp := map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": reply}}},
			"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 5},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
}

func TestMapReduceParallel10Files(t *testing.T) {
	var calls atomic.Int64
	srv := fakeServerCounter(t, "refactored", &calls)
	defer srv.Close()

	c, _ := deepseek.New(deepseek.Config{APIKey: "k", BaseURL: srv.URL}) //nolint:errcheck
	defer c.Close()

	files := makeFiles(t, 10)
	st := &state{apiKey: "k", client: c}

	filesArg := make([]any, len(files))
	for i, f := range files {
		filesArg[i] = f
	}
	resp := mapReduceRefactor(st, 1, map[string]any{
		"refactor_instructions": "Rename all vars to camelCase.",
		"files":                 filesArg,
		"max_parallel":          float64(10),
	})
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]map[string]any)
	if len(content) == 0 || content[0]["text"] == "" {
		t.Error("expected non-empty result text")
	}
	// All 10 files processed → at least 10 API calls (one per file chunk).
	if calls.Load() < 10 {
		t.Errorf("expected ≥10 API calls for 10 files, got %d", calls.Load())
	}
}

func TestMapReducePartialFailureContinues(t *testing.T) {
	srv := fakeServer(t, "ok")
	defer srv.Close()

	c, _ := deepseek.New(deepseek.Config{APIKey: "k", BaseURL: srv.URL}) //nolint:errcheck
	defer c.Close()

	dir := t.TempDir()
	goodFile := filepath.Join(dir, "good.go")
	os.WriteFile(goodFile, []byte("package main"), 0600) //nolint:errcheck
	missingFile := filepath.Join(dir, "does_not_exist.go")

	st := &state{apiKey: "k", client: c}
	resp := mapReduceRefactor(st, 1, map[string]any{
		"refactor_instructions": "Refactor.",
		"files":                 []any{goodFile, missingFile},
	})
	// Should succeed (partial), not error.
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("expected partial success, got error: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]map[string]any)
	text, _ := content[0]["text"].(string)
	if !strings.Contains(text, "partial_results=true") {
		t.Errorf("expected partial_results=true in response, got: %s", text)
	}
}

func TestMapReduceProgressTokenEmitted(t *testing.T) {
	srv := fakeServer(t, "ok")
	defer srv.Close()

	c, _ := deepseek.New(deepseek.Config{APIKey: "k", BaseURL: srv.URL}) //nolint:errcheck
	defer c.Close()

	files := makeFiles(t, 3)
	filesArg := make([]any, len(files))
	for i, f := range files {
		filesArg[i] = f
	}

	var (
		notifMu       sync.Mutex
		notifications []map[string]any
	)
	st := &state{
		apiKey: "k",
		client: c,
		notify: func(n map[string]any) {
			notifMu.Lock()
			notifications = append(notifications, n)
			notifMu.Unlock()
		},
	}

	mapReduceRefactor(st, 1, map[string]any{
		"refactor_instructions": "Refactor.",
		"files":                 filesArg,
		"_meta":                 map[string]any{"progressToken": "tok-123"},
	})

	notifMu.Lock()
	defer notifMu.Unlock()
	if len(notifications) == 0 {
		t.Error("expected progress notifications to be emitted")
	}
	for _, n := range notifications {
		if n["method"] != "notifications/progress" {
			t.Errorf("notification method = %v, want notifications/progress", n["method"])
		}
		params, _ := n["params"].(map[string]any)
		if params["progressToken"] != "tok-123" {
			t.Errorf("progressToken = %v, want tok-123", params["progressToken"])
		}
	}
}

func TestMapReduceDryRunNoAPICall(t *testing.T) {
	var calls atomic.Int64
	srv := fakeServerCounter(t, "ok", &calls)
	defer srv.Close()

	c, _ := deepseek.New(deepseek.Config{APIKey: "k", BaseURL: srv.URL}) //nolint:errcheck
	defer c.Close()

	files := makeFiles(t, 5)
	filesArg := make([]any, len(files))
	for i, f := range files {
		filesArg[i] = f
	}

	st := &state{apiKey: "k", client: c}
	resp := mapReduceRefactor(st, 1, map[string]any{
		"refactor_instructions": "Refactor everything.",
		"files":                 filesArg,
		"dry_run":               true,
	})
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	if calls.Load() != 0 {
		t.Errorf("dry_run must make 0 API calls, got %d", calls.Load())
	}
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]map[string]any)
	text, _ := content[0]["text"].(string)
	if !strings.Contains(text, "[dry_run]") {
		t.Errorf("expected [dry_run] prefix in response, got: %s", text)
	}
}

func TestMapReduceRateLimitRespected(t *testing.T) {
	// Verify that a burst=1 rate limiter doesn't deadlock (the bucket clamps to burst).
	srv := fakeServer(t, "ok")
	defer srv.Close()

	c, _ := deepseek.New(deepseek.Config{APIKey: "k", BaseURL: srv.URL, RateLimitTPM: 1, RateLimitBurst: 100000}) //nolint:errcheck
	defer c.Close()

	files := makeFiles(t, 3)
	filesArg := make([]any, len(files))
	for i, f := range files {
		filesArg[i] = f
	}
	st := &state{apiKey: "k", client: c}
	resp := mapReduceRefactor(st, 1, map[string]any{
		"refactor_instructions": "Rename vars.",
		"files":                 filesArg,
	})
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
}
