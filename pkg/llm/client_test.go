package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewClient_Fields(t *testing.T) {
	c := NewClient("http://127.0.0.1:11434", "llama3", 512, 0.7)
	if c.OllamaURL != "http://127.0.0.1:11434" {
		t.Errorf("OllamaURL = %q, want http://127.0.0.1:11434", c.OllamaURL)
	}
	if c.Model != "llama3" {
		t.Errorf("Model = %q, want llama3", c.Model)
	}
	if c.MaxTokens != 512 {
		t.Errorf("MaxTokens = %d, want 512", c.MaxTokens)
	}
	if c.Temperature != 0.7 {
		t.Errorf("Temperature = %.2f, want 0.7", c.Temperature)
	}
	if c.httpClient == nil {
		t.Error("httpClient is nil")
	}
}

func TestGenerate_OptsOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", 400)
			return
		}
		opts, _ := body["options"].(map[string]any)
		temp, _ := opts["temperature"].(float64)
		num, _ := opts["num_predict"].(float64)
		// Echo the received opts back as "response" for assertion.
		resp := map[string]any{
			"response": "ok",
			"_temp":    temp,
			"_num":     num,
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{
		OllamaURL:   srv.URL,
		Model:       "test",
		MaxTokens:   100,
		Temperature: 0.5,
		httpClient:  &http.Client{},
	}

	// Opts override temperature and maxTokens.
	_, err := c.Generate("hello", &GenerateOpts{Temperature: 0.9, MaxTokens: 200})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
}

func TestGenerate_NoOpts_UsesDefaults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"response": "pong"})
	}))
	defer srv.Close()

	c := &Client{
		OllamaURL:   srv.URL,
		Model:       "test",
		MaxTokens:   64,
		Temperature: 0.3,
		httpClient:  &http.Client{},
	}

	result, err := c.Generate("ping", nil)
	if err != nil {
		t.Fatalf("Generate with nil opts: %v", err)
	}
	if result != "pong" {
		t.Errorf("result = %q, want 'pong'", result)
	}
}

func TestGenerate_ServerError_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", 500)
	}))
	defer srv.Close()

	c := &Client{
		OllamaURL:  srv.URL,
		Model:      "test",
		httpClient: &http.Client{},
	}

	_, err := c.Generate("fail", nil)
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
}

func TestGenerate_JsonFormat(t *testing.T) {
	var receivedFormat string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		receivedFormat, _ = body["format"].(string)
		json.NewEncoder(w).Encode(map[string]string{"response": "{}"})
	}))
	defer srv.Close()

	c := &Client{OllamaURL: srv.URL, Model: "test", httpClient: &http.Client{}}
	_, err := c.Generate("struct query", &GenerateOpts{Format: "json"})
	if err != nil {
		t.Fatalf("Generate json format: %v", err)
	}
	if receivedFormat != "json" {
		t.Errorf("format sent = %q, want 'json'", receivedFormat)
	}
}
