package testmock

import (
	"bytes"
	"encoding/json"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// OllamaModel mirrors one entry of the /api/tags response.
type OllamaModel struct {
	Name string
}

// OllamaMock provides an Ollama HTTP API-compatible test server. Three
// endpoints are wired:
//
//	GET  /api/tags        — list available models
//	POST /api/embeddings  — return a deterministic embedding vector
//	POST /api/generate    — return a configurable text completion
//
// Defaults (overridable):
//   - Models: [{Name: "nomic-embed-text:latest"}, {Name: "qwen2.5-coder:7b"}]
//   - Embedding dim: 64 (deterministic from prompt+model via FNV-1a)
//   - Generate response: deterministic echo of the prompt
type OllamaMock struct {
	server *httptest.Server

	mu              sync.RWMutex
	models          []OllamaModel
	embeddingDim    int
	generateReply   string // empty = echo prompt
	embeddingsError int    // 0 = OK; otherwise HTTP status to return
	tagsError       int    // 0 = OK; otherwise HTTP status to return

	callCount int64

	callsMu sync.Mutex
	calls   []HTTPCall
}

// NewOllama boots an Ollama mock and registers Close as t.Cleanup.
func NewOllama(tb testing.TB) *OllamaMock {
	tb.Helper()
	m := &OllamaMock{
		models: []OllamaModel{
			{Name: "nomic-embed-text:latest"},
			{Name: "qwen2.5-coder:7b"},
		},
		embeddingDim: 64,
	}
	m.server = httptest.NewServer(m.routes())
	tb.Cleanup(m.Close)
	return m
}

// URL returns the mock server's base URL. Configure neo.yaml or test
// helpers to point Ollama clients at this URL.
func (m *OllamaMock) URL() string { return m.server.URL }

// Close stops the mock server. Safe to call multiple times.
func (m *OllamaMock) Close() { m.server.Close() }

// SetModels overrides the list returned by /api/tags. Useful for testing
// validation paths (e.g. ValidateOllamaEmbedModel rejecting an unknown
// model).
func (m *OllamaMock) SetModels(models []OllamaModel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.models = append(m.models[:0:0], models...)
}

// SetEmbeddingDim overrides the embedding vector length. Default 64.
// Real Ollama models produce 384-1024+; tests typically want a small
// dim to keep fixtures readable.
func (m *OllamaMock) SetEmbeddingDim(dim int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if dim > 0 {
		m.embeddingDim = dim
	}
}

// SetGenerateReply overrides the /api/generate response. Empty string
// restores the default (echo the request prompt).
func (m *OllamaMock) SetGenerateReply(text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generateReply = text
}

// SetEmbeddingsError makes /api/embeddings return the given HTTP status.
// 0 restores normal operation. Useful for testing breaker / retry logic.
func (m *OllamaMock) SetEmbeddingsError(status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.embeddingsError = status
}

// SetTagsError makes /api/tags return the given HTTP status.
// 0 restores normal operation. Useful for testing
// ValidateOllamaEmbedModel's ErrOllamaUnreachable path.
func (m *OllamaMock) SetTagsError(status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tagsError = status
}

// CallCount returns the total number of requests received since boot.
func (m *OllamaMock) CallCount() int64 { return atomic.LoadInt64(&m.callCount) }

// Calls returns a snapshot of all captured HTTP calls (oldest first).
func (m *OllamaMock) Calls() []HTTPCall {
	m.callsMu.Lock()
	defer m.callsMu.Unlock()
	out := make([]HTTPCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// routes wires the three supported endpoints. Ollama is unauthenticated.
func (m *OllamaMock) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/tags", m.capture(m.handleTags))
	mux.HandleFunc("POST /api/embeddings", m.capture(m.handleEmbeddings))
	mux.HandleFunc("POST /api/generate", m.capture(m.handleGenerate))
	return mux
}

// capture wraps a handler with body-buffering, call-counter, and history.
func (m *OllamaMock) capture(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

		next(w, r)
	}
}

// handleTags: GET /api/tags
func (m *OllamaMock) handleTags(w http.ResponseWriter, _ *http.Request) {
	m.mu.RLock()
	errStatus := m.tagsError
	models := append([]OllamaModel(nil), m.models...)
	m.mu.RUnlock()

	if errStatus != 0 {
		w.WriteHeader(errStatus)
		return
	}

	out := struct {
		Models []map[string]any `json:"models"`
	}{Models: make([]map[string]any, 0, len(models))}
	for _, mdl := range models {
		out.Models = append(out.Models, map[string]any{"name": mdl.Name})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleEmbeddings: POST /api/embeddings
func (m *OllamaMock) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	m.mu.RLock()
	errStatus := m.embeddingsError
	dim := m.embeddingDim
	m.mu.RUnlock()

	if errStatus != 0 {
		w.WriteHeader(errStatus)
		return
	}

	var req struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writePlainError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	out := map[string]any{
		"embedding": deterministicEmbedding(req.Model+"|"+req.Prompt, dim),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleGenerate: POST /api/generate
//
// Returns a single non-streamed JSON object {model, response, done}. Real
// Ollama defaults to STREAMING (newline-delimited JSON chunks) when
// stream is true or omitted. To avoid silently masking production bugs
// where the client expects streamed chunks, this mock rejects stream:true
// with HTTP 400. Production code in pkg/inference/gateway.go and friends
// always sets stream:false. [DS-AUDIT-3.1.C / Finding 1]
func (m *OllamaMock) handleGenerate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Stream bool   `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writePlainError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Stream {
		writePlainError(w, http.StatusBadRequest,
			"OllamaMock does not implement streaming /api/generate; set stream:false")
		return
	}

	m.mu.RLock()
	override := m.generateReply
	m.mu.RUnlock()

	text := override
	if text == "" {
		text = "echo: " + req.Prompt
	}
	out := map[string]any{
		"model":    req.Model,
		"response": text,
		"done":     true,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// deterministicEmbedding maps an input string + dim to a normalized
// float64 vector. Same input → same vector across runs and processes,
// so tests can assert specific embedding equality.
//
// The vector is L2-normalized so cosine similarity comparisons in
// production code don't blow up on the mock's synthetic numbers.
func deterministicEmbedding(seed string, dim int) []float64 {
	if dim <= 0 {
		dim = 64
	}
	out := make([]float64, dim)
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	state := h.Sum64()
	var sumSq float64
	for i := 0; i < dim; i++ {
		// Linear congruential step keeps it deterministic and dependency-free.
		state = state*6364136223846793005 + 1442695040888963407
		// Map upper 32 bits to [-1, 1] range.
		v := float64(int32(state>>32)) / float64(math.MaxInt32)
		out[i] = v
		sumSq += v * v
	}
	if sumSq > 0 {
		norm := math.Sqrt(sumSq)
		for i := range out {
			out[i] /= norm
		}
	}
	return out
}
