package testmock

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// DeepSeekUsage matches the usage block emitted by the real DeepSeek API
// in /chat/completions responses. The mock returns these as-is so the
// production client's token accounting (cache hit ratio, billing
// counters) round-trips deterministically.
type DeepSeekUsage struct {
	PromptTokens          int
	CompletionTokens      int
	TotalTokens           int
	PromptCacheHitTokens  int
	PromptCacheMissTokens int
	ReasoningTokens       int
}

// DeepSeekReply is the configurable response a DeepSeekMock returns.
// Empty fields take sensible test defaults.
type DeepSeekReply struct {
	// Content goes into choices[0].message.content. Empty defaults to
	// a deterministic echo of the last user message.
	Content string
	// ReasoningContent populates choices[0].message.reasoning_content.
	// Production code intentionally drops this from thread history; tests
	// can use it to verify decode but should not feed it back as input.
	ReasoningContent string
	// Model is echoed back. Empty defaults to the request's model field.
	Model string
	// SystemFingerprint is echoed back. Empty defaults to "fp_test_mock".
	SystemFingerprint string
	// Usage populates the usage block. Zero-valued fields stay zero.
	Usage DeepSeekUsage
	// Status overrides the HTTP status code. 0 = 200.
	Status int
	// StatusBody overrides the body when Status != 0. Empty = generic.
	StatusBody string
}

// DeepSeekMock provides a DeepSeek Chat API-compatible test server.
//
// Defaults (overridable):
//   - Bearer token: "fake-deepseek-token"
//   - Reply: echoes the last user message back as content
//   - Status: 200
type DeepSeekMock struct {
	server *httptest.Server

	mu            sync.RWMutex
	expectedToken string
	reply         DeepSeekReply

	callCount int64

	callsMu sync.Mutex
	calls   []HTTPCall
}

// NewDeepSeek boots a DeepSeek mock and registers Close as t.Cleanup.
func NewDeepSeek(tb testing.TB) *DeepSeekMock {
	tb.Helper()
	m := &DeepSeekMock{
		expectedToken: "fake-deepseek-token",
	}
	m.server = httptest.NewServer(m.routes())
	tb.Cleanup(m.Close)
	return m
}

// URL returns the mock server's base URL. Configure
// deepseek.Config{BaseURL: m.URL()} to point a production client at the mock.
func (m *DeepSeekMock) URL() string { return m.server.URL }

// Close stops the mock server. Safe to call multiple times.
func (m *DeepSeekMock) Close() { m.server.Close() }

// SetToken overrides the expected Bearer token.
func (m *DeepSeekMock) SetToken(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expectedToken = token
}

// SetReply replaces the next response. Subsequent calls keep using the same
// reply until SetReply is called again — this is intentional, tests that
// need different responses per call should re-call SetReply between them.
func (m *DeepSeekMock) SetReply(reply DeepSeekReply) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reply = reply
}

// CallCount returns the total number of /chat/completions requests received.
func (m *DeepSeekMock) CallCount() int64 { return atomic.LoadInt64(&m.callCount) }

// Calls returns a snapshot of all captured HTTP calls (oldest first).
func (m *DeepSeekMock) Calls() []HTTPCall {
	m.callsMu.Lock()
	defer m.callsMu.Unlock()
	out := make([]HTTPCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// routes wires the single supported endpoint.
func (m *DeepSeekMock) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat/completions", m.handleChatCompletions)
	return mux
}

// handleChatCompletions: POST /chat/completions
func (m *DeepSeekMock) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	r.Body = io.NopCloser(bytes.NewReader(body))

	atomic.AddInt64(&m.callCount, 1)
	m.callsMu.Lock()
	m.calls = append(m.calls, HTTPCall{
		Method: r.Method,
		Path:   r.URL.Path,
		Header: r.Header.Clone(),
		Body:   body,
	})
	m.callsMu.Unlock()

	m.mu.RLock()
	wantToken := m.expectedToken
	reply := m.reply
	m.mu.RUnlock()

	if !checkBearer(r.Header.Get("Authorization"), wantToken) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	if reply.Status != 0 && reply.Status != http.StatusOK {
		body := reply.StatusBody
		if body == "" {
			// Real DeepSeek API always returns a JSON error envelope on
			// non-2xx. Default to a minimal one so production clients
			// exercise their decode path identically. [DS-AUDIT-3.1.B]
			body = `{"error":{"message":"` + http.StatusText(reply.Status) + `"}}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.Status)
		_, _ = io.WriteString(w, body)
		return
	}

	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writePlainError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	content := reply.Content
	if content == "" {
		content = echoLastUserMessage(req.Messages)
	}
	model := reply.Model
	if model == "" {
		model = req.Model
	}
	fingerprint := reply.SystemFingerprint
	if fingerprint == "" {
		fingerprint = "fp_test_mock"
	}

	out := map[string]any{
		"model":              model,
		"system_fingerprint": fingerprint,
		"choices": []map[string]any{
			{
				"message": map[string]any{
					"content":           content,
					"reasoning_content": reply.ReasoningContent,
				},
			},
		},
		"usage": map[string]any{
			"prompt_tokens":            reply.Usage.PromptTokens,
			"completion_tokens":        reply.Usage.CompletionTokens,
			"total_tokens":             reply.Usage.TotalTokens,
			"prompt_cache_hit_tokens":  reply.Usage.PromptCacheHitTokens,
			"prompt_cache_miss_tokens": reply.Usage.PromptCacheMissTokens,
			"completion_tokens_details": map[string]any{
				"reasoning_tokens": reply.Usage.ReasoningTokens,
			},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// checkBearer validates an Authorization header against the expected
// bearer token. Comparison is case-insensitive on the scheme and exact
// on the token value.
//
// When `want` is empty, the check ALWAYS fails — accepting any caller
// when the configured token is empty would be an auth-bypass footgun
// (a test calling SetToken("") would silently turn off the auth check).
// [DS-AUDIT-3.1.B]
func checkBearer(header, want string) bool {
	if want == "" {
		return false
	}
	const prefix = "Bearer "
	if len(header) < len(prefix) {
		return false
	}
	if !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	return header[len(prefix):] == want
}

// echoLastUserMessage builds a deterministic test reply by echoing the
// content of the final message with role=user. Falls back to a fixed
// string when no user message is present.
func echoLastUserMessage(msgs []struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return "echo: " + msgs[i].Content
		}
	}
	return "echo: (no user message)"
}

// writePlainError emits a small JSON-flavored error body without setting
// the rich Atlassian envelope (DeepSeek's error shape is simpler).
func writePlainError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, `{"error":{"message":"`+msg+`"}}`)
}
