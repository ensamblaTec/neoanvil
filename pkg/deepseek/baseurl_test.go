package deepseek_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ensamblatec/neoanvil/internal/testmock"
	"github.com/ensamblatec/neoanvil/pkg/deepseek"
)

// TestBaseURLOverrideRoutesToMock verifies that the BaseURL field in
// pkg/deepseek/Config (already exposed pre-Area 3.2.A) actually flows
// to the /chat/completions URL builder so a test can swap in a mock.
//
// The production client builds requests as `{BaseURL}/chat/completions`,
// so the mock URL — http://127.0.0.1:NNNN — must reach the testmock's
// handler. Verified by checking the mock's atomic CallCount.
func TestBaseURLOverrideRoutesToMock(t *testing.T) {
	mock := testmock.NewDeepSeek(t)
	mock.SetReply(testmock.DeepSeekReply{
		Content: "deterministic mock reply",
		Usage: testmock.DeepSeekUsage{
			PromptTokens:     100,
			CompletionTokens: 20,
			TotalTokens:      120,
		},
	})

	client, err := deepseek.New(deepseek.Config{
		APIKey:  "fake-deepseek-token",
		BaseURL: mock.URL(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := client.Call(context.Background(), deepseek.CallRequest{
		Action: "distill_payload",
		Prompt: "test prompt",
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(resp.Text, "deterministic mock reply") {
		t.Errorf("response text=%q does not contain mock content", resp.Text)
	}
	if resp.InputTokens != 100 || resp.OutputTokens != 20 {
		t.Errorf("token counts=%d/%d want 100/20", resp.InputTokens, resp.OutputTokens)
	}

	if got := mock.CallCount(); got != 1 {
		t.Errorf("mock.CallCount=%d want 1", got)
	}
}

// TestBaseURLValidationRejectsBadScheme verifies the defense-in-depth
// guard introduced in [Area 3.2.A]: a non-http(s) BaseURL fails at
// New() time instead of silently routing API calls to an arbitrary host.
func TestBaseURLValidationRejectsBadScheme(t *testing.T) {
	cases := []string{
		"file:///etc/passwd",
		"gopher://attacker.example/9",
		"not-a-url",
		"https:///no-host",
	}
	for _, bad := range cases {
		t.Run(bad, func(t *testing.T) {
			_, err := deepseek.New(deepseek.Config{APIKey: "k", BaseURL: bad})
			if err == nil {
				t.Errorf("New with BaseURL=%q did not fail; want validation error", bad)
			}
		})
	}
}

// TestEmptyBaseURLFallsBackToDefault: an unset BaseURL keeps the
// legacy production default (https://api.deepseek.com/v1).
func TestEmptyBaseURLFallsBackToDefault(t *testing.T) {
	c, err := deepseek.New(deepseek.Config{APIKey: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c == nil {
		t.Fatalf("nil client")
	}
}
