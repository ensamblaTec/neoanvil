package rag

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// tagsServer returns an httptest server that mimics Ollama's /api/tags with
// the given model list.
func tagsServer(t *testing.T, models ...string) *httptest.Server {
	t.Helper()
	handler := http.NewServeMux()
	handler.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		resp := ollamaTagsResponse{}
		for _, m := range models {
			resp.Models = append(resp.Models, struct {
				Name string `json:"name"`
			}{Name: m})
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	return httptest.NewServer(handler)
}

func TestValidateOllamaEmbedModel_Present(t *testing.T) {
	srv := tagsServer(t, "nomic-embed-text:latest", "qwen2.5-coder:7b")
	defer srv.Close()
	err := ValidateOllamaEmbedModel(context.Background(), srv.URL, "nomic-embed-text")
	if err != nil {
		t.Errorf("expected nil for present model, got: %v", err)
	}
}

func TestValidateOllamaEmbedModel_ExactTagMatch(t *testing.T) {
	srv := tagsServer(t, "nomic-embed-text:latest")
	defer srv.Close()
	// Full tag also matches.
	if err := ValidateOllamaEmbedModel(context.Background(), srv.URL, "nomic-embed-text:latest"); err != nil {
		t.Errorf("full-tag match failed: %v", err)
	}
}

func TestValidateOllamaEmbedModel_Missing(t *testing.T) {
	srv := tagsServer(t, "qwen2.5-coder:7b")
	defer srv.Close()
	err := ValidateOllamaEmbedModel(context.Background(), srv.URL, "nomic-embed-text")
	if !errors.Is(err, ErrOllamaModelNotFound) {
		t.Errorf("expected ErrOllamaModelNotFound, got: %v", err)
	}
}

func TestValidateOllamaEmbedModel_Unreachable(t *testing.T) {
	// Use a closed server to simulate unreachable endpoint.
	srv := tagsServer(t)
	srv.Close()
	err := ValidateOllamaEmbedModel(context.Background(), srv.URL, "nomic-embed-text")
	if !errors.Is(err, ErrOllamaUnreachable) {
		t.Errorf("expected ErrOllamaUnreachable, got: %v", err)
	}
}

func TestValidateOllamaEmbedModel_Non200(t *testing.T) {
	handler := http.NewServeMux()
	handler.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	err := ValidateOllamaEmbedModel(context.Background(), srv.URL, "nomic-embed-text")
	if !errors.Is(err, ErrOllamaUnreachable) {
		t.Errorf("expected ErrOllamaUnreachable on 500, got: %v", err)
	}
}

func TestValidateOllamaEmbedModel_NoArgs(t *testing.T) {
	// Empty baseURL → nil (nothing configured).
	if err := ValidateOllamaEmbedModel(context.Background(), "", "nomic"); err != nil {
		t.Errorf("empty baseURL should short-circuit to nil, got: %v", err)
	}
	// Empty model → nil.
	if err := ValidateOllamaEmbedModel(context.Background(), "http://x", ""); err != nil {
		t.Errorf("empty model should short-circuit to nil, got: %v", err)
	}
}

func TestValidateWithRetry_NotFoundShortCircuit(t *testing.T) {
	srv := tagsServer(t) // empty model list → not found
	defer srv.Close()
	start := time.Now()
	err := ValidateWithRetry(context.Background(), srv.URL, "nomic", 5, 200*time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrOllamaModelNotFound) {
		t.Errorf("expected ErrOllamaModelNotFound, got: %v", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("ErrOllamaModelNotFound should short-circuit (no retry sleep), took %v", elapsed)
	}
}

func TestIsPermanent(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{400, true},
		{401, true},
		{403, true},
		{404, true},
		{500, false},
		{502, false},
		{503, false},
		{200, false},
	}
	for _, c := range cases {
		err := &EmbedHTTPError{StatusCode: c.code}
		if got := IsPermanent(err); got != c.want {
			t.Errorf("IsPermanent(%d) = %v, want %v", c.code, got, c.want)
		}
	}
	if IsPermanent(nil) {
		t.Error("IsPermanent(nil) must be false")
	}
	if IsPermanent(errors.New("plain")) {
		t.Error("IsPermanent(plain error) must be false")
	}
}
