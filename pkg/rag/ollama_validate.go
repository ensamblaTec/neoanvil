package rag

// ollama_validate.go — Boot-time Ollama model presence check to preempt the
// runaway circuit-breaker loop seen in INC-20260424-133023, where every
// ingestion worker got HTTP 404 from Ollama because the embedding model was
// not pulled. The circuit opened, reopened, reopened — 87 SRE-WARN lines in
// 3 minutes with zero forward progress. This validator runs once before the
// worker pool spawns, producing a single actionable error instead of a loop.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// ErrOllamaModelNotFound is returned when the target model is not present on
// the Ollama server. Operator must run `ollama pull <model>` to fix.
var ErrOllamaModelNotFound = errors.New("ollama: embedding model not present — run `ollama pull <model>`")

// ErrOllamaUnreachable is returned when /api/tags fails entirely (service down,
// wrong URL). Operator must start Ollama or fix `ai.base_url` / `ai.embed_base_url`.
var ErrOllamaUnreachable = errors.New("ollama: /api/tags unreachable")

// ollamaTagsResponse mirrors the relevant fields of Ollama's /api/tags payload.
type ollamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"` // e.g. "nomic-embed-text:latest"
	} `json:"models"`
}

// ValidateOllamaEmbedModel GETs <baseURL>/api/tags and returns nil iff the
// target model is present. Timeout 3s. Uses SafeInternalHTTPClient since the
// base URL is operator-controlled via ai.embed_base_url / ai.base_url.
// [INC-20260424-133023]
func ValidateOllamaEmbedModel(ctx context.Context, baseURL, model string) error {
	if baseURL == "" || model == "" {
		return nil // nothing configured, nothing to validate
	}
	url := baseURL + "/api/tags"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("build /api/tags request: %w", err)
	}
	// SafeOperatorHTTPClient (not SafeInternalHTTPClient) — see Bug-4
	// reasoning in embedder.go. Docker bridge IPs (172.16/12) are
	// neither loopback nor a SSRF threat when operator-configured.
	client := sre.SafeOperatorHTTPClient(3)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOllamaUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d", ErrOllamaUnreachable, resp.StatusCode)
	}
	var tags ollamaTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return fmt.Errorf("decode /api/tags: %w", err)
	}
	for _, m := range tags.Models {
		if m.Name == model || stripTag(m.Name) == stripTag(model) {
			return nil
		}
	}
	return fmt.Errorf("%w: %q not in %d-model list", ErrOllamaModelNotFound, model, len(tags.Models))
}

// stripTag removes the ":latest"/":N" suffix so "nomic-embed-text" and
// "nomic-embed-text:latest" compare equal.
func stripTag(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return s[:i]
		}
	}
	return s
}

// ValidateWithRetry retries ValidateOllamaEmbedModel up to maxAttempts times
// with linear backoff — useful at boot when Ollama may still be starting.
// Returns the last error when all attempts fail. [INC-20260424-133023]
func ValidateWithRetry(ctx context.Context, baseURL, model string, maxAttempts int, backoff time.Duration) error {
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		err := ValidateOllamaEmbedModel(ctx, baseURL, model)
		if err == nil {
			return nil
		}
		lastErr = err
		// If the model is genuinely missing, retrying won't help.
		if errors.Is(err, ErrOllamaModelNotFound) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return lastErr
}
