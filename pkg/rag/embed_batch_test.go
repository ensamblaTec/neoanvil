package rag

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeEmbedder is a minimal Embedder used to drive the EmbedMany helper
// when testing the fallback path (no BatchEmbedder support).
type fakeEmbedder struct {
	calls atomic.Int64
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	f.calls.Add(1)
	out := make([]float32, 4)
	for i := range out {
		out[i] = float32(len(text)+i) / 10.0
	}
	return out, nil
}

func (f *fakeEmbedder) Dimension() int { return 4 }

func TestEmbedMany_FallbackToSequential(t *testing.T) {
	emb := &fakeEmbedder{}
	out, err := EmbedMany(context.Background(), emb, []string{"a", "bb", "ccc"})
	if err != nil {
		t.Fatalf("EmbedMany: %v", err)
	}
	if got := emb.calls.Load(); got != 3 {
		t.Errorf("expected 3 sequential calls, got %d", got)
	}
	if len(out) != 3 {
		t.Fatalf("expected 3 vectors, got %d", len(out))
	}
	if out[0][0] == out[1][0] {
		t.Errorf("vectors should differ — got identical %v vs %v", out[0], out[1])
	}
}

func TestEmbedMany_EmptyInput(t *testing.T) {
	emb := &fakeEmbedder{}
	out, err := EmbedMany(context.Background(), emb, nil)
	if err != nil || out != nil {
		t.Errorf("empty input: want (nil, nil), got (%v, %v)", out, err)
	}
}

// ollamaBatchServer simulates Ollama's /api/embed (plural) endpoint.
// Returns embeddings of dim 768 — same as nomic-embed-text — and tracks
// the number of HTTP calls so tests can assert the batch path is taken.
func ollamaBatchServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	handler := http.NewServeMux()
	handler.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var req ollamaBatchReq
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out := make([][]float64, len(req.Input))
		for i, s := range req.Input {
			vec := make([]float64, 768)
			for j := range vec {
				vec[j] = float64(len(s)+i+j) / 1000.0
			}
			out[i] = vec
		}
		_ = json.NewEncoder(w).Encode(ollamaBatchResp{Embeddings: out})
	})
	handler.HandleFunc("/api/embeddings", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		vec := make([]float64, 768)
		for j := range vec {
			vec[j] = float64(j) / 1000.0
		}
		_ = json.NewEncoder(w).Encode(ollamaResp{Embedding: vec})
	})
	return httptest.NewServer(handler), &calls
}

func TestOllamaEmbedder_EmbedBatch_HappyPath(t *testing.T) {
	srv, calls := ollamaBatchServer(t)
	defer srv.Close()
	emb := NewOllamaEmbedder(srv.URL, "nomic-embed-text", 30, 4, 0)
	texts := []string{"alpha", "beta", "gamma", "delta"}
	out, err := emb.EmbedBatch(context.Background(), texts)
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(out) != len(texts) {
		t.Fatalf("got %d vectors, want %d", len(out), len(texts))
	}
	for i, v := range out {
		if len(v) != 768 {
			t.Errorf("vec[%d] dim=%d, want 768", i, len(v))
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 HTTP call (batch), got %d", got)
	}
}

func TestOllamaEmbedder_EmbedBatch_EmbedManyDispatch(t *testing.T) {
	srv, calls := ollamaBatchServer(t)
	defer srv.Close()
	emb := NewOllamaEmbedder(srv.URL, "nomic-embed-text", 30, 4, 0)
	// EmbedMany should detect BatchEmbedder support and use single round-trip.
	out, err := EmbedMany(context.Background(), emb, []string{"x", "y", "z"})
	if err != nil {
		t.Fatalf("EmbedMany: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d vectors, want 3", len(out))
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("EmbedMany should hit batch endpoint once, got %d HTTP calls", got)
	}
}

func TestOllamaEmbedder_EmbedBatch_FallbackOn404(t *testing.T) {
	// Older Ollama returns 404 on /api/embed (plural); EmbedBatch must
	// fall back to N sequential /api/embeddings (singular) calls.
	var calls atomic.Int64
	handler := http.NewServeMux()
	handler.HandleFunc("/api/embed", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.NotFound(w, nil)
	})
	handler.HandleFunc("/api/embeddings", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		vec := make([]float64, 768)
		_ = json.NewEncoder(w).Encode(ollamaResp{Embedding: vec})
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	emb := NewOllamaEmbedder(srv.URL, "nomic-embed-text", 30, 4, 0)
	out, err := emb.EmbedBatch(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d vectors, want 3", len(out))
	}
	// 1 batch attempt (404) + 3 singular fallbacks = 4 total.
	if got := calls.Load(); got != 4 {
		t.Errorf("expected 4 HTTP calls (1 batch + 3 fallback), got %d", got)
	}
}

func TestOllamaEmbedder_EmbedBatch_SingleTextShortCircuit(t *testing.T) {
	srv, calls := ollamaBatchServer(t)
	defer srv.Close()
	emb := NewOllamaEmbedder(srv.URL, "nomic-embed-text", 30, 4, 0)
	// len(texts)==1 routes to Embed() (singular endpoint) per the impl —
	// no point paying batch HTTP overhead for one text.
	_, err := emb.EmbedBatch(context.Background(), []string{"only"})
	if err != nil {
		t.Fatalf("EmbedBatch single: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 call to /api/embeddings, got %d", got)
	}
}

func TestOllamaEmbedder_EmbedBatch_LengthMismatch(t *testing.T) {
	// Server returns N-1 embeddings for N inputs → caller must error,
	// not silently truncate.
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		out := make([][]float64, 2)
		for i := range out {
			out[i] = make([]float64, 768)
		}
		_ = json.NewEncoder(w).Encode(ollamaBatchResp{Embeddings: out})
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	emb := NewOllamaEmbedder(srv.URL, "nomic-embed-text", 30, 4, 0)
	_, err := emb.EmbedBatch(context.Background(), []string{"a", "b", "c"})
	if err == nil {
		t.Fatal("expected error on length mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "returned 2 embeddings for 3 texts") {
		t.Errorf("error should mention mismatch, got: %v", err)
	}
}

func TestOllamaEmbedder_EmbedBatch_500Error(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	emb := NewOllamaEmbedder(srv.URL, "nomic-embed-text", 30, 4, 0)
	_, err := emb.EmbedBatch(context.Background(), []string{"a", "b"})
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
	var hpe *EmbedHTTPError
	if !errors.As(err, &hpe) {
		t.Errorf("expected EmbedHTTPError, got: %v", err)
	}
	if hpe.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", hpe.StatusCode)
	}
}
