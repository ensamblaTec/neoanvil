package deepseek

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDeepSeekServer returns a test HTTP server that mimics the DeepSeek chat completions endpoint.
func fakeDeepSeekServer(t *testing.T, reply string, inputToks, outputToks int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": reply}},
			},
			"usage": map[string]any{
				"prompt_tokens":     inputToks,
				"completion_tokens": outputToks,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
}

func TestNew_MissingAPIKey(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Error("expected error for missing APIKey")
	}
}

func TestCacheKey_Deterministic(t *testing.T) {
	files := []FileEntry{
		{Path: "b.go", Content: "package b"},
		{Path: "a.go", Content: "package a"},
	}
	k1 := CacheKey(files)
	// Reverse order — result must be identical (sorted internally).
	files2 := []FileEntry{files[1], files[0]}
	k2 := CacheKey(files2)
	if k1 != k2 {
		t.Errorf("CacheKey not deterministic: %s vs %s", k1, k2)
	}
}

func TestCacheKey_ChangesOnContentChange(t *testing.T) {
	files := []FileEntry{{Path: "x.go", Content: "v1"}}
	k1 := CacheKey(files)
	files[0].Content = "v2"
	k2 := CacheKey(files)
	if k1 == k2 {
		t.Error("CacheKey should change when content changes")
	}
}

func TestComputeCheckpointKey_Deterministic(t *testing.T) {
	files := []FileEntry{{Path: "z.go", Content: "code"}}
	k1 := computeCheckpointKey("distill_payload", files, "prompt")
	k2 := computeCheckpointKey("distill_payload", files, "prompt")
	if k1 != k2 {
		t.Errorf("checkpoint key not deterministic: %s vs %s", k1, k2)
	}
}

func TestNewThreadID_UniqueAndFormat(t *testing.T) {
	ids := make(map[string]bool)
	for range 20 {
		id := newThreadID()
		if !strings.HasPrefix(id, "ds_thread_") {
			t.Errorf("bad thread ID format: %s", id)
		}
		if ids[id] {
			t.Errorf("duplicate thread ID: %s", id)
		}
		ids[id] = true
	}
}

func TestCall_Ephemeral(t *testing.T) {
	srv := fakeDeepSeekServer(t, "distilled content", 100, 50)
	defer srv.Close()

	c, err := New(Config{APIKey: "test-key", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	resp, err := c.Call(context.Background(), CallRequest{
		Action:    "distill_payload",
		Prompt:    "Summarize this context.",
		Mode:      SessionModeEphemeral,
		MaxTokens: 1000,
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Text != "distilled content" {
		t.Errorf("unexpected text: %q", resp.Text)
	}
	if resp.ThreadID != "" {
		t.Error("ephemeral call should not return thread ID")
	}
	if resp.InputTokens != 100 || resp.OutputTokens != 50 {
		t.Errorf("token counts: in=%d out=%d", resp.InputTokens, resp.OutputTokens)
	}
}

func TestCall_Threaded_NewThread(t *testing.T) {
	srv := fakeDeepSeekServer(t, "audit result", 200, 80)
	defer srv.Close()

	dir := t.TempDir()
	c, err := New(Config{APIKey: "k", BaseURL: srv.URL, DBPath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	resp, err := c.Call(context.Background(), CallRequest{
		Action:    "red_team_audit",
		Prompt:    "Audit this code for vulnerabilities.",
		Mode:      SessionModeThreaded,
		MaxTokens: 2000,
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.HasPrefix(resp.ThreadID, "ds_thread_") {
		t.Errorf("expected thread ID, got %q", resp.ThreadID)
	}
}

func TestCall_CheckpointHit(t *testing.T) {
	srv := fakeDeepSeekServer(t, "original", 10, 10)
	defer srv.Close()

	dir := t.TempDir()
	c, err := New(Config{APIKey: "k", BaseURL: srv.URL, DBPath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	req := CallRequest{
		Action:        "distill_payload",
		Prompt:        "same prompt",
		Mode:          SessionModeEphemeral,
		CheckpointKey: "fixed-key-for-test",
	}

	resp1, err := c.Call(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp1.CacheHit {
		t.Error("first call should not be a cache hit")
	}

	resp2, err := c.Call(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !resp2.CacheHit {
		t.Error("second call with same checkpoint key should be a cache hit")
	}
}

func TestCall_MaxTokensHardCap(t *testing.T) {
	srv := fakeDeepSeekServer(t, "ok", 10, 10)
	defer srv.Close()

	c, _ := New(Config{APIKey: "k", BaseURL: srv.URL})
	defer c.Close()

	resp, err := c.Call(context.Background(), CallRequest{
		Action:    "distill_payload",
		Prompt:    "test",
		MaxTokens: 99999, // over the hard cap
	})
	// Should clamp to 50000 and succeed, not error.
	if err != nil {
		t.Fatalf("expected clamp, got error: %v", err)
	}
	if resp == nil {
		t.Error("expected non-nil response")
	}
}

func TestBillingStats(t *testing.T) {
	srv := fakeDeepSeekServer(t, "ok", 100, 50)
	defer srv.Close()

	dir := t.TempDir()
	c, _ := New(Config{APIKey: "k", BaseURL: srv.URL, DBPath: filepath.Join(dir, "test.db")})
	defer c.Close()

	c.Call(context.Background(), CallRequest{Action: "distill_payload", Prompt: "p", Mode: SessionModeEphemeral}) //nolint:errcheck

	tokens, calls := c.BillingStats()
	if tokens != 150 {
		t.Errorf("tokens = %d, want 150", tokens)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
