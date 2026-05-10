package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// bufPool recycles bytes.Buffer instances used for JSON serialization of embed
// requests, eliminating per-call heap allocations on the hot embedding path.
// [SRE-36.1.1]
var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// errorsAs is a local wrapper so IsOverload can keep its declaration close
// to EmbedHTTPError without dragging "errors" into every call site.
func errorsAs(err error, target any) bool { return errors.As(err, target) }

type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Dimension() int
}

// BatchEmbedder is an optional capability — embedders that can serve a batch
// of texts in a single round-trip (Ollama /api/embed, vLLM, TEI, etc.) should
// implement it so hot-path callers can avoid the per-call HTTP overhead.
//
// Callers should type-assert and fall back to a sequential Embed loop when
// the embedder does not implement BatchEmbedder. EmbedBatch returns vectors
// in the same order as input. An empty input returns an empty slice and nil.
type BatchEmbedder interface {
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}

// EmbedMany is the canonical helper for callers who want batch behaviour
// without writing the type-assertion boilerplate at every site. It uses
// EmbedBatch when the embedder supports it, otherwise falls back to N
// sequential Embed calls. An empty input returns nil, nil.
func EmbedMany(ctx context.Context, embedder Embedder, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if be, ok := embedder.(BatchEmbedder); ok {
		return be.EmbedBatch(ctx, texts)
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := embedder.Embed(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("embed %d/%d: %w", i+1, len(texts), err)
		}
		out[i] = v
	}
	return out, nil
}

// EmbedHTTPError wraps a non-2xx response from the embedder server.
// Callers can distinguish 5xx overload from 4xx client errors to pick a
// backoff strategy.
type EmbedHTTPError struct {
	StatusCode int
}

func (e *EmbedHTTPError) Error() string {
	return fmt.Sprintf("ollama returned HTTP %d", e.StatusCode)
}

// IsOverload reports whether the error comes from a server-side 5xx response.
func IsOverload(err error) bool {
	var httpErr *EmbedHTTPError
	if !errorsAs(err, &httpErr) {
		return false
	}
	return httpErr.StatusCode >= 500 && httpErr.StatusCode < 600
}

// [SRE-98.A] IsBusy distinguishes "Ollama queue saturated" (429/503) from
// "Ollama crashed/OOM" (500/502/504). Busy = short backoff (transient),
// crash = long backoff (recovery takes longer). Both are subsets of IsOverload.
func IsBusy(err error) bool {
	var httpErr *EmbedHTTPError
	if !errorsAs(err, &httpErr) {
		return false
	}
	return httpErr.StatusCode == 429 || httpErr.StatusCode == 503
}

// [SRE-98.A] IsCrash reports server-side failures that indicate process state
// (OOM, segfault, bad gateway) — needs longer backoff than queue pressure.
func IsCrash(err error) bool {
	var httpErr *EmbedHTTPError
	if !errorsAs(err, &httpErr) {
		return false
	}
	return httpErr.StatusCode == 500 || httpErr.StatusCode == 502 || httpErr.StatusCode == 504
}

// IsPermanent reports errors that will NOT recover by retrying against the same
// endpoint — specifically HTTP 404 (Ollama: model not loaded/pulled) and HTTP
// 400/401/403 (bad request, auth). Unlike IsOverload/IsCrash, these won't fix
// themselves; the operator must pull the model or fix config. The ingest
// circuit breaker should skip the retry loop for permanent errors and surface
// them as boot-time warnings. [INC-20260424-133023]
func IsPermanent(err error) bool {
	var httpErr *EmbedHTTPError
	if !errorsAs(err, &httpErr) {
		return false
	}
	switch httpErr.StatusCode {
	case 400, 401, 403, 404:
		return true
	}
	return false
}

// [SRE-98.C] globalEmbedSems caps concurrent Embed() calls across multiple
// OllamaEmbedder instances that share the same BaseURL (e.g., one embedder
// per service, all pointing to the dedicated :11435 instance). Prevents
// collective burst that exceeds Ollama's process-wide NUM_PARALLEL.
var (
	globalEmbedSems   = map[string]chan struct{}{}
	globalEmbedSemsMu sync.Mutex
)

// getGlobalEmbedSem returns (and lazily creates) the cross-instance semaphore
// bound to a given BaseURL. Capacity is the first non-zero concurrency seen
// for that URL — subsequent embedders sharing the URL inherit the slot count.
func getGlobalEmbedSem(baseURL string, capacity int) chan struct{} {
	if capacity <= 0 {
		capacity = 2
	}
	globalEmbedSemsMu.Lock()
	defer globalEmbedSemsMu.Unlock()
	if sem, ok := globalEmbedSems[baseURL]; ok {
		return sem
	}
	sem := make(chan struct{}, capacity)
	globalEmbedSems[baseURL] = sem
	return sem
}

type OllamaEmbedder struct {
	BaseURL      string
	Model        string
	embedTimeout time.Duration
	breaker      *sre.CircuitBreaker[[]float32]
	// [SRE-97.A] Reused HTTP client — one Transport per embedder means TCP
	// connections are pooled across Embed() calls instead of a fresh dial
	// every request. Critical under multi-worker ingestion bursts.
	client *http.Client
	// [SRE-97.B] Per-embedder semaphore gating concurrent Embed() calls. Capacity
	// comes from cfg.RAG.EmbedConcurrency. Prevents burst over Ollama's queue
	// depth within a single workspace.
	sem chan struct{}
	// [SRE-98.C] Shared semaphore bound to BaseURL — all embedders pointing to
	// the same Ollama instance acquire from here too, preventing cross-workspace
	// burst from saturating a shared embedding runner.
	globalSem chan struct{}
	// [273.A] MaxChars truncates text before sending to Ollama. nomic-embed-text
	// has a 2048-token context window; large code chunks exceed it → HTTP 500.
	// 0 = no truncation. See cfg.RAG.MaxEmbedChars (default 4000).
	maxChars int
	// [303.E] Round-robin pool of Ollama embedding instances. When non-empty,
	// nextURL() picks the next URL in the slice using an atomic counter.
	// Falls back to BaseURL when empty.
	embedURLs   []string
	embedURLIdx atomic.Uint64
}

// nextURL returns the next embedding URL using round-robin over embedURLs.
// Falls back to BaseURL when the pool is empty (single-instance path). [303.E]
func (o *OllamaEmbedder) nextURL() string {
	if len(o.embedURLs) == 0 {
		return o.BaseURL
	}
	idx := o.embedURLIdx.Add(1) % uint64(len(o.embedURLs))
	return o.embedURLs[idx]
}

// NewOllamaEmbedder creates an embedder with a hard per-call timeout and a
// concurrency cap. embedTimeoutSeconds caps each Ollama HTTP round-trip; fail
// fast when Ollama is overloaded rather than blocking callers for 15s+.
// embedConcurrency sizes the per-embedder semaphore (rag.embed_concurrency).
// maxChars truncates input to prevent HTTP 500 on context-window overflow (0 = no truncation).
// embedURLs is an optional round-robin pool of Ollama URLs (ai.embedding_urls). [303.E]
func NewOllamaEmbedder(baseURL, model string, embedTimeoutSeconds, embedConcurrency, maxChars int, embedURLs ...string) *OllamaEmbedder {
	timeout := time.Duration(embedTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	if embedConcurrency <= 0 {
		embedConcurrency = 2
	}
	// threshold=5: tolerates a full concurrent burst (embedConcurrency=3) plus
	// 2 additional failures before tripping. Prevents eager trips on transient spikes.
	// resetTimeout=BreakerResetTimeout (30s): gives Ollama's embed runner time to drain.
	// [SRE-35-hotfix]
	e := &OllamaEmbedder{
		BaseURL:      baseURL,
		Model:        model,
		embedTimeout: timeout,
		breaker:      sre.NewCircuitBreaker[[]float32](5, sre.BreakerResetTimeout),
		// SafeOperatorHTTPClient (not SafeHTTPClient) — the Ollama URL
		// is operator-configured via neo.yaml/OLLAMA_HOST/OLLAMA_EMBED_HOST,
		// not user-influenced. SafeHTTPClient's RFC 1918 block falsely-
		// positives Docker bridge networks (172.16/12) where compose-
		// managed Ollama lives. [Bug-4 fix]
		client:       sre.SafeOperatorHTTPClient(30),
		sem:          make(chan struct{}, embedConcurrency),
		globalSem:    getGlobalEmbedSem(baseURL, embedConcurrency),
		maxChars:     maxChars,
		embedURLs:    embedURLs,
	}
	return e
}

func (o *OllamaEmbedder) Dimension() int {
	return 768
}

type ollamaReq struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type ollamaResp struct {
	Embedding []float64 `json:"embedding"`
}

// ollamaBatchReq is the schema for /api/embed (plural) — Ollama v0.3+. The
// `input` field accepts either a string or []string; we always send []string.
type ollamaBatchReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaBatchResp struct {
	Embeddings [][]float64 `json:"embeddings"`
}

// EmbedBatch sends N texts in a single /api/embed (plural) round-trip and
// returns the vectors in the same order. Falls back to N sequential Embed
// calls if Ollama returns 404 (older Ollama without /api/embed). The whole
// batch shares one semaphore slot — Ollama's runner already parallelises
// internally, so client-side fan-out would just add HTTP overhead.
func (o *OllamaEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) == 1 {
		v, err := o.Embed(ctx, texts[0])
		if err != nil {
			return nil, err
		}
		return [][]float32{v}, nil
	}
	texts = truncateTexts(texts, o.maxChars)
	release, err := o.acquireBatchSlots(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	batchTimeout := o.embedTimeout + time.Duration(len(texts))*200*time.Millisecond
	embedCtx, cancel := context.WithTimeout(ctx, batchTimeout)
	defer cancel()

	resp, err := o.dispatchBatchHTTP(embedCtx, texts)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Ollama < 0.3 has no /api/embed (plural).
		_, _ = io.Copy(io.Discard, resp.Body)
		return o.embedSequentialFallback(ctx, texts)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &EmbedHTTPError{StatusCode: resp.StatusCode}
	}
	return decodeBatchEmbeddings(resp.Body, len(texts))
}

// truncateTexts mirrors the per-call guard in Embed(): cap each input to
// maxChars so nomic-embed-text doesn't 500 on context-window overflow.
// Returns the input unchanged when maxChars <= 0 (no truncation configured).
func truncateTexts(texts []string, maxChars int) []string {
	if maxChars <= 0 {
		return texts
	}
	out := make([]string, len(texts))
	for i, t := range texts {
		if len(t) > maxChars {
			out[i] = t[:maxChars]
		} else {
			out[i] = t
		}
	}
	return out
}

// acquireBatchSlots takes one slot from the per-embedder + global semaphores.
// Returns a release func that the caller must defer. The whole batch shares
// one slot — Ollama parallelises server-side, so client-side fan-out would
// just add HTTP overhead.
func (o *OllamaEmbedder) acquireBatchSlots(ctx context.Context) (func(), error) {
	select {
	case o.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case o.globalSem <- struct{}{}:
	case <-ctx.Done():
		<-o.sem
		return nil, ctx.Err()
	}
	return func() {
		<-o.globalSem
		<-o.sem
	}, nil
}

// dispatchBatchHTTP encodes + POSTs the batch request. The body buffer is
// returned to the pool before this function returns regardless of outcome.
func (o *OllamaEmbedder) dispatchBatchHTTP(ctx context.Context, texts []string) (*http.Response, error) {
	reqBody := ollamaBatchReq{Model: o.Model, Input: texts}
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	if err := json.NewEncoder(buf).Encode(reqBody); err != nil {
		bufPool.Put(buf)
		return nil, fmt.Errorf("failed to marshal ollama batch request: %w", err)
	}
	endpoint := fmt.Sprintf("%s/api/embed", o.nextURL())
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, buf)
	if err != nil {
		bufPool.Put(buf)
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	bufPool.Put(buf)
	if err != nil {
		return nil, fmt.Errorf("failed to contact ollama batch endpoint: %w", err)
	}
	return resp, nil
}

// embedSequentialFallback is the per-text path used when /api/embed (plural)
// returns 404 — Ollama < 0.3 only has /api/embeddings (singular).
func (o *OllamaEmbedder) embedSequentialFallback(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := o.Embed(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("batch fallback %d/%d: %w", i+1, len(texts), err)
		}
		out[i] = v
	}
	return out, nil
}

// decodeBatchEmbeddings parses an Ollama /api/embed response and converts
// every []float64 row to []float32. Errors out on count mismatch (server
// returned fewer/more embeddings than inputs) or any empty row.
func decodeBatchEmbeddings(body io.Reader, expected int) ([][]float32, error) {
	var parsed ollamaBatchResp
	if err := json.NewDecoder(body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("failed to decode ollama batch response: %w", err)
	}
	if len(parsed.Embeddings) != expected {
		return nil, fmt.Errorf("ollama batch returned %d embeddings for %d texts", len(parsed.Embeddings), expected)
	}
	out := make([][]float32, len(parsed.Embeddings))
	for i, emb := range parsed.Embeddings {
		if len(emb) == 0 {
			return nil, fmt.Errorf("ollama batch returned empty embedding at index %d", i)
		}
		v := make([]float32, len(emb))
		for j, f := range emb {
			v[j] = float32(f)
		}
		out[i] = v
	}
	return out, nil
}

func (o *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	// [273.A] Truncate before acquiring semaphores — cheap guard against HTTP 500
	// "input length exceeds context length" from nomic-embed-text (2048-token window).
	if o.maxChars > 0 && len(text) > o.maxChars {
		text = text[:o.maxChars]
	}
	// [SRE-97.B/98.C] Acquire per-embedder slot AND the global (per-BaseURL) slot.
	// Order matters: local first (fast cap per workspace), then global (fleet-wide).
	select {
	case o.sem <- struct{}{}:
		defer func() { <-o.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case o.globalSem <- struct{}{}:
		defer func() { <-o.globalSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	embedCtx, cancel := context.WithTimeout(ctx, o.embedTimeout)
	defer cancel()
	result, err := o.breaker.Execute(embedCtx, func(c context.Context) ([]float32, error) {
		reqBody := ollamaReq{
			Model:  o.Model,
			Prompt: text,
		}

		// Acquire a pooled buffer to avoid heap allocation per embed call. [SRE-36.1.1]
		buf := bufPool.Get().(*bytes.Buffer)
		buf.Reset()
		if err := json.NewEncoder(buf).Encode(reqBody); err != nil {
			bufPool.Put(buf)
			return nil, fmt.Errorf("failed to marshal ollama request: %w", err)
		}

		endpoint := fmt.Sprintf("%s/api/embeddings", o.nextURL()) // [303.E] round-robin
		req, err := http.NewRequestWithContext(c, "POST", endpoint, buf)
		if err != nil {
			bufPool.Put(buf)
			return nil, fmt.Errorf("failed to create http request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		// [SRE-97.A] Reuse pooled client (built once in NewOllamaEmbedder).
		resp, err := o.client.Do(req)
		// Return buffer to pool once the request body has been consumed by Do().
		bufPool.Put(buf)
		if err != nil {
			return nil, fmt.Errorf("failed to contact ollama endpoint: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, &EmbedHTTPError{StatusCode: resp.StatusCode}
		}

		var parsedResp ollamaResp
		if err := json.NewDecoder(resp.Body).Decode(&parsedResp); err != nil {
			return nil, fmt.Errorf("failed to decode ollama response: %w", err)
		}

		if len(parsedResp.Embedding) == 0 {
			return nil, fmt.Errorf("ollama returned an empty embedding")
		}

		float32V := make([]float32, len(parsedResp.Embedding))
		for i, v := range parsedResp.Embedding {
			float32V[i] = float32(v)
		}

		return float32V, nil
	})

	if err != nil {
		return nil, fmt.Errorf("fallo en generacion de embedding: %w", err)
	}
	return result, nil
}
