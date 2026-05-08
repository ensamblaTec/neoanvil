// pkg/llm/client.go — Local LLM client for offline operations. [SRE-95.A.1]
//
// Provides a unified interface to Ollama (primary) and llama.cpp (fallback)
// for intent classification and natural language processing.
package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// Client interfaces with a local LLM (Ollama or llama.cpp). [SRE-95.A.1]
type Client struct {
	OllamaURL  string
	Model      string
	MaxTokens  int
	Temperature float64
	httpClient *http.Client
}

// NewClient creates an LLM client configured for the given Ollama endpoint.
// [SRE-110.A] Uses SafeHTTPClient (anti-SSRF) — Ollama URL comes from neo.yaml
// (potentially external host) so the same SSRF guard as user-config endpoints applies.
func NewClient(ollamaURL, model string, maxTokens int, temperature float64) *Client {
	return &Client{
		OllamaURL:   ollamaURL,
		Model:       model,
		MaxTokens:   maxTokens,
		Temperature: temperature,
		httpClient:  sre.SafeHTTPClient(),
	}
}

// GenerateOpts holds optional parameters for a generation request.
type GenerateOpts struct {
	Format      string  // "json" for structured output
	Temperature float64 // override default
	MaxTokens   int     // override default
}

// Generate sends a prompt to the LLM and returns the response text. [SRE-95.A.1]
func (c *Client) Generate(prompt string, opts *GenerateOpts) (string, error) {
	temp := c.Temperature
	maxTok := c.MaxTokens
	format := ""

	if opts != nil {
		if opts.Temperature > 0 {
			temp = opts.Temperature
		}
		if opts.MaxTokens > 0 {
			maxTok = opts.MaxTokens
		}
		format = opts.Format
	}

	reqBody := map[string]any{
		"model":  c.Model,
		"prompt": prompt,
		"stream": false,
		"options": map[string]any{
			"temperature": temp,
			"num_predict": maxTok,
		},
	}
	if format != "" {
		reqBody["format"] = format
	}

	body, _ := json.Marshal(reqBody)

	req, err := http.NewRequest(http.MethodPost, c.OllamaURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("ollama status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	return result.Response, nil
}

// HealthCheck verifies that Ollama is reachable and the model is available.
func (c *Client) HealthCheck() (bool, error) {
	resp, err := c.httpClient.Get(c.OllamaURL + "/api/tags")
	if err != nil {
		return false, fmt.Errorf("ollama unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("ollama status %d", resp.StatusCode)
	}

	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return false, fmt.Errorf("decode tags: %w", err)
	}

	for _, m := range tags.Models {
		if m.Name == c.Model {
			return true, nil
		}
	}

	return false, fmt.Errorf("model %q not found in Ollama", c.Model)
}

// Status returns the current LLM backend status as a string for BRIEFING.
func (c *Client) Status() string {
	ok, err := c.HealthCheck()
	if ok {
		return fmt.Sprintf("ollama:ok (%s)", c.Model)
	}
	return fmt.Sprintf("ollama:error (%v)", err)
}
