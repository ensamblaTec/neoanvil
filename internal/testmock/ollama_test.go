package testmock

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
)

func TestOllamaMock_TagsReturnsDefaultModels(t *testing.T) {
	m := NewOllama(t)
	resp := mustGet(t, m.URL()+"/api/tags")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var got struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Models) != 2 {
		t.Fatalf("len=%d want 2", len(got.Models))
	}
	if got.Models[0].Name != "nomic-embed-text:latest" {
		t.Errorf("model[0]=%q", got.Models[0].Name)
	}
}

func TestOllamaMock_SetModelsOverride(t *testing.T) {
	m := NewOllama(t)
	m.SetModels([]OllamaModel{{Name: "tiny:1b"}})

	resp := mustGet(t, m.URL()+"/api/tags")
	defer resp.Body.Close()
	var got struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if len(got.Models) != 1 || got.Models[0].Name != "tiny:1b" {
		t.Errorf("models=%v", got.Models)
	}
}

func TestOllamaMock_TagsErrorTriggersStatus(t *testing.T) {
	m := NewOllama(t)
	m.SetTagsError(http.StatusServiceUnavailable)
	resp := mustGet(t, m.URL()+"/api/tags")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", resp.StatusCode)
	}
}

func TestOllamaMock_EmbeddingsDeterministic(t *testing.T) {
	m := NewOllama(t)
	body := strings.NewReader(`{"model":"nomic-embed-text:latest","prompt":"hello world"}`)
	resp := mustPost(t, m.URL()+"/api/embeddings", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var got struct {
		Embedding []float64 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Embedding) != 64 {
		t.Errorf("dim=%d want 64", len(got.Embedding))
	}
	// Verify deterministic: same input on a fresh mock produces the same vector.
	m2 := NewOllama(t)
	body2 := strings.NewReader(`{"model":"nomic-embed-text:latest","prompt":"hello world"}`)
	resp2 := mustPost(t, m2.URL()+"/api/embeddings", body2)
	defer resp2.Body.Close()
	var got2 struct {
		Embedding []float64 `json:"embedding"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&got2)
	if len(got2.Embedding) != len(got.Embedding) {
		t.Fatalf("dim mismatch")
	}
	for i := range got.Embedding {
		if got.Embedding[i] != got2.Embedding[i] {
			t.Fatalf("embedding[%d] not deterministic: %f vs %f", i, got.Embedding[i], got2.Embedding[i])
		}
	}
}

func TestOllamaMock_EmbeddingsAreNormalized(t *testing.T) {
	m := NewOllama(t)
	body := strings.NewReader(`{"model":"x","prompt":"y"}`)
	resp := mustPost(t, m.URL()+"/api/embeddings", body)
	defer resp.Body.Close()
	var got struct {
		Embedding []float64 `json:"embedding"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)

	var sumSq float64
	for _, v := range got.Embedding {
		sumSq += v * v
	}
	norm := math.Sqrt(sumSq)
	if math.Abs(norm-1.0) > 1e-9 {
		t.Errorf("L2 norm=%f want ≈1.0 (vector should be normalized)", norm)
	}
}

func TestOllamaMock_DifferentPromptsGiveDifferentEmbeddings(t *testing.T) {
	m := NewOllama(t)
	v1 := embedHelper(t, m, "alpha")
	v2 := embedHelper(t, m, "beta")
	identical := true
	for i := range v1 {
		if v1[i] != v2[i] {
			identical = false
			break
		}
	}
	if identical {
		t.Errorf("two distinct prompts produced the same vector — hashing collision or seed bug")
	}
}

func TestOllamaMock_SetEmbeddingDimOverride(t *testing.T) {
	m := NewOllama(t)
	m.SetEmbeddingDim(8)
	v := embedHelper(t, m, "x")
	if len(v) != 8 {
		t.Errorf("dim=%d want 8", len(v))
	}
}

func TestOllamaMock_EmbeddingsErrorTriggersStatus(t *testing.T) {
	m := NewOllama(t)
	m.SetEmbeddingsError(http.StatusInternalServerError)
	body := strings.NewReader(`{"model":"x","prompt":"y"}`)
	resp := mustPost(t, m.URL()+"/api/embeddings", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", resp.StatusCode)
	}
}

func TestOllamaMock_GenerateEchoesByDefault(t *testing.T) {
	m := NewOllama(t)
	body := strings.NewReader(`{"model":"qwen","prompt":"What is 2+2?","stream":false}`)
	resp := mustPost(t, m.URL()+"/api/generate", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got struct {
		Response string `json:"response"`
		Done     bool   `json:"done"`
		Model    string `json:"model"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Response != "echo: What is 2+2?" {
		t.Errorf("response=%q", got.Response)
	}
	if !got.Done {
		t.Errorf("done=false want true")
	}
	if got.Model != "qwen" {
		t.Errorf("model=%q want qwen", got.Model)
	}
}

func TestOllamaMock_SetGenerateReplyOverride(t *testing.T) {
	m := NewOllama(t)
	m.SetGenerateReply("4")
	body := strings.NewReader(`{"model":"x","prompt":"What is 2+2?"}`)
	resp := mustPost(t, m.URL()+"/api/generate", body)
	defer resp.Body.Close()
	var got struct {
		Response string `json:"response"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Response != "4" {
		t.Errorf("response=%q want 4", got.Response)
	}
}

func TestOllamaMock_GenerateRejectsStreamTrue(t *testing.T) {
	m := NewOllama(t)
	body := strings.NewReader(`{"model":"x","prompt":"y","stream":true}`)
	resp := mustPost(t, m.URL()+"/api/generate", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 (mock does not stream)", resp.StatusCode)
	}
}

func TestOllamaMock_GenerateBadJSON(t *testing.T) {
	m := NewOllama(t)
	resp := mustPost(t, m.URL()+"/api/generate", strings.NewReader("not json"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}
}

func TestOllamaMock_CallCountAndHistory(t *testing.T) {
	m := NewOllama(t)
	resp := mustGet(t, m.URL()+"/api/tags")
	_ = resp.Body.Close()
	resp2 := mustPost(t, m.URL()+"/api/embeddings", strings.NewReader(`{"model":"x","prompt":"y"}`))
	_ = resp2.Body.Close()
	resp3 := mustPost(t, m.URL()+"/api/generate", strings.NewReader(`{"model":"x","prompt":"y"}`))
	_ = resp3.Body.Close()

	if got := m.CallCount(); got != 3 {
		t.Errorf("CallCount=%d want 3", got)
	}
	calls := m.Calls()
	if len(calls) != 3 {
		t.Fatalf("len=%d want 3", len(calls))
	}
	wantPaths := []string{"/api/tags", "/api/embeddings", "/api/generate"}
	for i, c := range calls {
		if c.Path != wantPaths[i] {
			t.Errorf("call[%d].Path=%q want %q", i, c.Path, wantPaths[i])
		}
	}
}

func embedHelper(t *testing.T, m *OllamaMock, prompt string) []float64 {
	t.Helper()
	body := strings.NewReader(`{"model":"x","prompt":"` + prompt + `"}`)
	resp := mustPost(t, m.URL()+"/api/embeddings", body)
	defer resp.Body.Close()
	var got struct {
		Embedding []float64 `json:"embedding"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	return got.Embedding
}

// mustPost is a small helper that panics-via-t.Fatal on http error,
// so the caller can safely defer Body.Close() without a nil panic risk.
func mustPost(t *testing.T, url string, body *strings.Reader) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", body)
	if err != nil {
		t.Fatalf("http POST %s: %v", url, err)
	}
	return resp
}

// mustGet mirrors mustPost for GET requests.
func mustGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("http GET %s: %v", url, err)
	}
	return resp
}
