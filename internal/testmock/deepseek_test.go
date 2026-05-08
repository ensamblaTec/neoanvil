package testmock

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func bearerAuth(token string) [2]string { return [2]string{"Bearer", token} }

// makeDeepSeekRequest issues a POST /chat/completions with optional Bearer.
// Pass auth=[Bearer, token]; first element is the scheme (matched
// case-insensitively), second is the token.
func makeDeepSeekRequest(t *testing.T, mock *DeepSeekMock, auth [2]string, body any) *http.Response {
	t.Helper()
	req := simpleJSONRequest(t, http.MethodPost, mock.URL()+"/chat/completions", body)
	if auth[0] != "" {
		req.Header.Set("Authorization", auth[0]+" "+auth[1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http: %v", err)
	}
	return resp
}

// simpleJSONRequest builds a JSON request without auth helpers — used by
// the DeepSeek mock test (Bearer) and any future Bearer-flavored mock.
func simpleJSONRequest(t *testing.T, method, url string, body any) *http.Request {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(method, url, strings.NewReader(string(buf)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func chatRequestBody(model, userMsg string) map[string]any {
	return map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "system", "content": "you are a helpful assistant"},
			{"role": "user", "content": userMsg},
		},
		"max_tokens": 100,
	}
}

func TestDeepSeekMock_RejectsMissingBearer(t *testing.T) {
	m := NewDeepSeek(t)
	resp := makeDeepSeekRequest(t, m, [2]string{}, chatRequestBody("v4-flash", "hi"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", resp.StatusCode)
	}
}

func TestDeepSeekMock_RejectsWrongBearer(t *testing.T) {
	m := NewDeepSeek(t)
	resp := makeDeepSeekRequest(t, m, bearerAuth("wrong-token"), chatRequestBody("v4-flash", "hi"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", resp.StatusCode)
	}
}

func TestDeepSeekMock_AcceptsCaseInsensitiveScheme(t *testing.T) {
	m := NewDeepSeek(t)
	cases := []string{"Bearer", "bearer", "BEARER", "BeArEr"}
	for _, scheme := range cases {
		t.Run(scheme, func(t *testing.T) {
			resp := makeDeepSeekRequest(t, m, [2]string{scheme, "fake-deepseek-token"},
				chatRequestBody("v4-flash", "hi"))
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d want 200 with scheme=%q", resp.StatusCode, scheme)
			}
		})
	}
}

func TestDeepSeekMock_DefaultEchoesLastUserMessage(t *testing.T) {
	m := NewDeepSeek(t)
	resp := makeDeepSeekRequest(t, m, bearerAuth("fake-deepseek-token"),
		chatRequestBody("v4-flash", "ping"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	got := decodeChatResponse(t, resp)
	if got.Choices[0].Message.Content != "echo: ping" {
		t.Errorf("content=%q want \"echo: ping\"", got.Choices[0].Message.Content)
	}
	if got.Model != "v4-flash" {
		t.Errorf("model=%q want v4-flash (echoed from request)", got.Model)
	}
	if got.SystemFingerprint != "fp_test_mock" {
		t.Errorf("fingerprint=%q want fp_test_mock", got.SystemFingerprint)
	}
}

func TestDeepSeekMock_SetReplyOverridesContent(t *testing.T) {
	m := NewDeepSeek(t)
	m.SetReply(DeepSeekReply{
		Content:           "static reply",
		ReasoningContent:  "internal CoT",
		Model:             "v4-pro",
		SystemFingerprint: "fp_custom",
		Usage: DeepSeekUsage{
			PromptTokens:          200,
			CompletionTokens:      50,
			TotalTokens:           250,
			PromptCacheHitTokens:  150,
			PromptCacheMissTokens: 50,
			ReasoningTokens:       30,
		},
	})

	resp := makeDeepSeekRequest(t, m, bearerAuth("fake-deepseek-token"),
		chatRequestBody("v4-flash", "ignored"))
	defer resp.Body.Close()
	got := decodeChatResponse(t, resp)
	if got.Choices[0].Message.Content != "static reply" {
		t.Errorf("content override failed: %q", got.Choices[0].Message.Content)
	}
	if got.Choices[0].Message.ReasoningContent != "internal CoT" {
		t.Errorf("reasoning override failed: %q", got.Choices[0].Message.ReasoningContent)
	}
	if got.Model != "v4-pro" {
		t.Errorf("model override failed: %q", got.Model)
	}
	if got.SystemFingerprint != "fp_custom" {
		t.Errorf("fingerprint override failed: %q", got.SystemFingerprint)
	}
	if got.Usage.PromptCacheHitTokens != 150 {
		t.Errorf("cache hit tokens=%d want 150", got.Usage.PromptCacheHitTokens)
	}
	if got.Usage.CompletionTokensDetails.ReasoningTokens != 30 {
		t.Errorf("reasoning tokens=%d want 30", got.Usage.CompletionTokensDetails.ReasoningTokens)
	}
}

func TestDeepSeekMock_SetReplyStatusReturnsError(t *testing.T) {
	m := NewDeepSeek(t)
	m.SetReply(DeepSeekReply{
		Status:     http.StatusTooManyRequests,
		StatusBody: `{"error":{"message":"rate_limit"}}`,
	})

	resp := makeDeepSeekRequest(t, m, bearerAuth("fake-deepseek-token"),
		chatRequestBody("v4-flash", "x"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429", resp.StatusCode)
	}
}

func TestDeepSeekMock_SetTokenOverride(t *testing.T) {
	m := NewDeepSeek(t)
	m.SetToken("custom-secret")

	resp := makeDeepSeekRequest(t, m, bearerAuth("custom-secret"),
		chatRequestBody("v4-flash", "x"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200 with overridden token", resp.StatusCode)
	}

	resp2 := makeDeepSeekRequest(t, m, bearerAuth("fake-deepseek-token"),
		chatRequestBody("v4-flash", "x"))
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("default token should now fail, got %d", resp2.StatusCode)
	}
}

func TestDeepSeekMock_BadRequestOnInvalidJSON(t *testing.T) {
	m := NewDeepSeek(t)
	req, _ := http.NewRequest(http.MethodPost, m.URL()+"/chat/completions",
		strings.NewReader("not json"))
	req.Header.Set("Authorization", "Bearer fake-deepseek-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
}

func TestDeepSeekMock_CallCountAndHistory(t *testing.T) {
	m := NewDeepSeek(t)
	for range 4 {
		resp := makeDeepSeekRequest(t, m, bearerAuth("fake-deepseek-token"),
			chatRequestBody("v4-flash", "x"))
		_ = resp.Body.Close()
	}
	if got := m.CallCount(); got != 4 {
		t.Errorf("CallCount=%d want 4", got)
	}
	calls := m.Calls()
	if len(calls) != 4 {
		t.Fatalf("len(Calls)=%d want 4", len(calls))
	}
	for i, c := range calls {
		if c.Method != http.MethodPost {
			t.Errorf("call[%d].Method=%q", i, c.Method)
		}
		if !strings.Contains(c.Path, "/chat/completions") {
			t.Errorf("call[%d].Path=%q", i, c.Path)
		}
	}
}

func TestDeepSeekMock_StatusBodyDefaultsToJSONError(t *testing.T) {
	m := NewDeepSeek(t)
	m.SetReply(DeepSeekReply{Status: http.StatusTooManyRequests})
	resp := makeDeepSeekRequest(t, m, bearerAuth("fake-deepseek-token"),
		chatRequestBody("v4-flash", "x"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type=%q want application/json", got)
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode default error envelope: %v", err)
	}
	if env.Error.Message == "" {
		t.Errorf("default error message empty, want non-empty fallback")
	}
}

func TestDeepSeekMock_EmptyTokenRejectsAllRequests(t *testing.T) {
	m := NewDeepSeek(t)
	m.SetToken("")

	cases := []string{"Bearer ", "Bearer something", "Bearer fake-deepseek-token"}
	for _, hdr := range cases {
		t.Run(hdr, func(t *testing.T) {
			req := simpleJSONRequest(t, http.MethodPost, m.URL()+"/chat/completions",
				chatRequestBody("v4-flash", "x"))
			req.Header.Set("Authorization", hdr)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("http: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status=%d want 401 with empty expected token + header=%q",
					resp.StatusCode, hdr)
			}
		})
	}
}

// chatResponse is the test-side projection of DeepSeek's response shape.
type chatResponse struct {
	Model             string `json:"model"`
	SystemFingerprint string `json:"system_fingerprint"`
	Choices           []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens            int `json:"prompt_tokens"`
		CompletionTokens        int `json:"completion_tokens"`
		TotalTokens             int `json:"total_tokens"`
		PromptCacheHitTokens    int `json:"prompt_cache_hit_tokens"`
		PromptCacheMissTokens   int `json:"prompt_cache_miss_tokens"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

func decodeChatResponse(t *testing.T, resp *http.Response) chatResponse {
	t.Helper()
	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Choices) == 0 {
		t.Fatalf("response had no choices")
	}
	return out
}
