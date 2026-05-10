// Package deepseek provides a DeepSeek API client for the Fan-Out Engine (PILAR XXIV / 131.K).
//
// DeepSeek uses an OpenAI-compatible REST API. Base URL: https://api.deepseek.com/v1.
//
// Session modes:
//   - Ephemeral: stateless single-turn calls (distill_payload, map_reduce_refactor).
//   - Threaded: multi-turn with ThreadID persisted in BoltDB deepseek_threads bucket.
//     ThreadID format: ds_thread_<8 random hex bytes>.
//
// Cache key (Block 1 static): SHA-256 of sorted file content hashes.
// Invalidated by: (1) SHA-256 change on any included file, (2) token count > 30000
// triggers auto-distill and reset, (3) TTL reaper goroutine every 5 min expires
// threads idle > 15 min.
//
// Billing circuit breaker: per-call hard cap (default 50000 tokens) enforced before
// the HTTP call. Session counter persisted in BoltDB deepseek_billing bucket.
//
// BoltDB checkpointing: checkpoint_key = SHA-256(action + sorted_files + instructions).
// Idempotent within TTL (default 3600s). Compatible with P-IDEM plugin layer.
package deepseek

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/ensamblatec/neoanvil/pkg/ratelimit"
)

const (
	defaultBaseURL       = "https://api.deepseek.com/v1"
	defaultModel         = "deepseek-v4-flash" // canonical ID; "deepseek-chat" alias still works but is deprecated
	defaultMaxTokens     = 4096
	maxTokensPerTxHard   = 50000
	threadTTL            = 15 * time.Minute
	checkpointTTL        = 3600
	autoDistillThreshold = 30000

	// ModelV4Flash and ModelV4Pro are the canonical model IDs as of 2026-04-26.
	// `deepseek-chat` and `deepseek-reasoner` are deprecated aliases that map to
	// V4Flash with thinking disabled / enabled respectively. Prefer canonical IDs
	// in new callers — when the alias is finally removed, only callers using the
	// alias will need updates.
	ModelV4Flash = "deepseek-v4-flash"
	ModelV4Pro   = "deepseek-v4-pro"

	// ThinkingType values per the unified `thinking` request parameter.
	ThinkingDisabled = "disabled"
	ThinkingEnabled  = "enabled"

	// ReasoningEffort levels accepted by `thinking.reasoning_effort`.
	ReasoningEffortHigh = "high"
	ReasoningEffortMax  = "max"

	bucketThreads  = "deepseek_threads"
	bucketBilling  = "deepseek_billing"
	bucketCheckpts = "deepseek_checkpoints"
)

// ThinkingConfig controls the `thinking` parameter on chat-completions
// requests. Empty fields delegate to whatever the server's per-model
// default is (which today is "enabled/high" on the canonical models and
// "disabled" on the `deepseek-chat` alias).
type ThinkingConfig struct {
	Type            string `yaml:"type" json:"type,omitempty"`                         // "enabled" | "disabled"
	ReasoningEffort string `yaml:"reasoning_effort" json:"reasoning_effort,omitempty"` // "high" | "max"
}

// IsZero is true when neither field is set.
func (t ThinkingConfig) IsZero() bool { return t.Type == "" && t.ReasoningEffort == "" }

// SessionMode controls thread lifecycle.
type SessionMode int

const (
	SessionModeEphemeral SessionMode = iota // fire-and-forget; no state persisted
	SessionModeThreaded                     // ThreadID persisted; context window managed
)

// Config configures the DeepSeek client.
type Config struct {
	APIKey              string
	BaseURL             string // defaults to defaultBaseURL
	Model               string // defaults to defaultModel
	Thinking            ThinkingConfig // process-wide default; overridable per-call via CallRequest.Thinking
	DBPath              string         // BoltDB path for thread/billing state; empty = in-memory (no persistence)
	RateLimitTPM        int64          // tokens per minute; 0 = 60000 (deepseek.rate_limit_tokens_per_minute)
	RateLimitBurst      int64          // burst capacity; 0 = 10000 (deepseek.burst)
	MaxTokensPerSession int64          // [131.J] session billing circuit breaker; 0 = 500000
}

// Client is a long-lived DeepSeek API client.
type Client struct {
	cfg     Config
	http    *http.Client
	db      *bolt.DB               // nil when DBPath is empty
	limiter *ratelimit.TokenBucket // [131.B] per-client rate limiter
}

// Message is a single chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// CallRequest is the input to Client.Call.
type CallRequest struct {
	Action        string         // distill_payload | map_reduce_refactor | red_team_audit
	Prompt        string         // target_prompt (Babel Pattern: must be English)
	SystemMsg     string         // Block 1 static system instructions
	Files         []FileEntry    // Block 1 static code files (cached by SHA-256)
	ThreadID      string         // threaded mode: existing thread to continue (empty = new)
	MaxTokens     int            // 0 = defaultMaxTokens; hard cap maxTokensPerTxHard
	Mode          SessionMode
	CheckpointKey string // set by caller for P-IDEM; empty = computed from action+files+prompt

	// Per-call overrides. When unset (empty / nil), client falls back to
	// Config.Model / Config.Thinking. Useful for routing one tool action
	// (e.g. red_team_audit on crypto code) to v4-pro+max while leaving
	// the rest of the session on flash defaults.
	Model    string          // overrides Config.Model when non-empty
	Thinking *ThinkingConfig // overrides Config.Thinking when non-nil
}

// FileEntry is a code file included in Block 1 static context.
type FileEntry struct {
	Path    string
	Content string
}

// CallResponse is the result of Client.Call.
type CallResponse struct {
	Text         string
	ThreadID     string // set for threaded mode; empty for ephemeral
	InputTokens  int
	OutputTokens int
	CacheHit     bool // true if local checkpoint short-circuit (BoltDB-level idempotency)

	// Server-reported usage detail — populated when the API returns these
	// fields (added 2026-04-26). All zero on error / older API revisions.
	CacheHitTokens    int    `json:"cache_hit_tokens,omitempty"`     // prompt_cache_hit_tokens (50× cheaper)
	CacheMissTokens   int    `json:"cache_miss_tokens,omitempty"`    // prompt_cache_miss_tokens
	ReasoningTokens   int    `json:"reasoning_tokens,omitempty"`     // completion_tokens_details.reasoning_tokens (CoT volume)
	SystemFingerprint string `json:"system_fingerprint,omitempty"`   // for model-version drift detection
	ModelUsed         string `json:"model_used,omitempty"`           // server-echoed model id (may differ from Config.Model when alias)
}

// New creates a DeepSeek client. Opens BoltDB when cfg.DBPath is non-empty.
func New(cfg Config) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("deepseek: APIKey is required")
	}
	resolvedBase, err := validateBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("deepseek: %w", err)
	}
	cfg.BaseURL = resolvedBase
	if cfg.Model == "" {
		cfg.Model = defaultModel
	}

	tpm := cfg.RateLimitTPM
	if tpm <= 0 {
		tpm = 60000
	}
	burst := cfg.RateLimitBurst
	if burst <= 0 {
		burst = 10000
	}
	if cfg.MaxTokensPerSession <= 0 {
		cfg.MaxTokensPerSession = 500000
	}
	c := &Client{
		cfg:     cfg,
		http:    &http.Client{Timeout: 120 * time.Second},
		limiter: ratelimit.New(burst, tpm), // [131.B]
	}

	if cfg.DBPath != "" {
		db, openErr := bolt.Open(cfg.DBPath, 0600, &bolt.Options{Timeout: 5 * time.Second})
		if openErr != nil {
			return nil, fmt.Errorf("deepseek: open db %s: %w", cfg.DBPath, openErr)
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			for _, name := range []string{bucketThreads, bucketBilling, bucketCheckpts} {
				if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			db.Close()
			return nil, fmt.Errorf("deepseek: init buckets: %w", err)
		}
		c.db = db
	}

	return c, nil
}

// validateBaseURL hardens the BaseURL Config field against accidental or
// malicious env-var injection. Trims whitespace + trailing slash, ensures
// the scheme is http or https, and ensures a host is present. Empty
// override returns defaultBaseURL.
//
// Defense-in-depth — if an attacker can write the DEEPSEEK_BASE_URL env
// var of a running plugin they likely already own the box, but failing
// fast on a malformed URL is cheap and prevents accidental misconfig
// from leaking the API key to a wrong host. [Area 3.2.A — DS-AUDIT Finding 1]
func validateBaseURL(override string) (string, error) {
	override = strings.TrimSpace(override)
	override = strings.TrimRight(override, "/")
	if override == "" {
		return defaultBaseURL, nil
	}
	parsed, err := neturl.Parse(override)
	if err != nil {
		return "", fmt.Errorf("BaseURL %q is malformed: %w", override, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("BaseURL %q must use http or https scheme (got %q)", override, parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("BaseURL %q is missing a host", override)
	}
	return override, nil
}

// Close releases the BoltDB handle.
func (c *Client) Close() error {
	if c.db != nil {
		return c.db.Close()
	}
	return nil
}

// Call dispatches a task to the DeepSeek API, applying caching, billing checks,
// and thread management according to req.Mode.
func (c *Client) Call(ctx context.Context, req CallRequest) (*CallResponse, error) {
	// Clamp max tokens.
	if req.MaxTokens <= 0 {
		req.MaxTokens = defaultMaxTokens
	}
	if req.MaxTokens > maxTokensPerTxHard {
		req.MaxTokens = maxTokensPerTxHard
	}

	// Compute checkpoint key for P-IDEM.
	ckKey := req.CheckpointKey
	if ckKey == "" {
		ckKey = computeCheckpointKey(req.Action, req.Files, req.Prompt)
	}

	// Check idempotency checkpoint.
	if hit, resp := c.checkpointLoad(ckKey); hit {
		return resp, nil
	}

	// [131.J] Session billing circuit breaker: reject when session total ≥ MaxTokensPerSession.
	if c.db != nil {
		sessionTokens, _ := c.BillingStats()
		if int64(sessionTokens) >= c.cfg.MaxTokensPerSession {
			return nil, fmt.Errorf("deepseek: BILLING_CIRCUIT_OPEN session tokens %d >= limit %d",
				sessionTokens, c.cfg.MaxTokensPerSession)
		}
	}

	// Build message list.
	msgs := c.buildMessages(ctx, req)

	// Billing pre-check: reject if estimated input alone exceeds the hard cap.
	// Output is already bounded by req.MaxTokens (clamped above). 1 token ≈ 4 chars.
	estimatedIn := 0
	for _, m := range msgs {
		estimatedIn += len(m.Content) / 4
	}
	if estimatedIn > maxTokensPerTxHard {
		return nil, fmt.Errorf("deepseek: estimated input tokens %d exceeds hard cap %d", estimatedIn, maxTokensPerTxHard)
	}

	// HTTP call. Pass req-level overrides so per-call routing (e.g.
	// crypto file → v4-pro+max while session default is flash+high) works.
	apiResp, err := c.callAPI(ctx, msgs, req.MaxTokens, c.resolveModel(req), c.resolveThinking(req))
	if err != nil {
		return nil, err
	}

	result := &CallResponse{
		Text:              apiResp.text,
		InputTokens:       apiResp.inputTokens,
		OutputTokens:      apiResp.outputTokens,
		CacheHitTokens:    apiResp.cacheHitTokens,
		CacheMissTokens:   apiResp.cacheMissTokens,
		ReasoningTokens:   apiResp.reasoningTokens,
		SystemFingerprint: apiResp.systemFingerprint,
		ModelUsed:         apiResp.modelUsed,
	}

	// Thread management for threaded mode.
	if req.Mode == SessionModeThreaded {
		tid := req.ThreadID
		if tid == "" {
			tid = newThreadID()
		}
		c.threadAppend(tid, msgs, apiResp.text, apiResp.inputTokens)
		result.ThreadID = tid
	}

	// Persist billing counter.
	c.billingRecord(apiResp.inputTokens + apiResp.outputTokens)

	// Store checkpoint.
	c.checkpointStore(ckKey, result)

	return result, nil
}

// buildMessages assembles the final message slice from Block 1 (static) + Block 2 (dynamic).
func (c *Client) buildMessages(_ context.Context, req CallRequest) []Message {
	var msgs []Message

	// Block 1 static: system instructions.
	sys := req.SystemMsg
	if sys == "" {
		sys = "You are a precise software engineering assistant. Respond only in English."
	}

	// Block 1 static: code files (appended to system message to leverage API prefix caching).
	if len(req.Files) > 0 {
		var sb bytes.Buffer
		sb.WriteString(sys)
		sb.WriteString("\n\n### Context files\n")
		for _, f := range req.Files {
			fmt.Fprintf(&sb, "\n#### %s\n```\n%s\n```\n", f.Path, f.Content)
		}
		sys = sb.String()
	}
	msgs = append(msgs, Message{Role: "system", Content: sys})

	// Threaded mode: prepend prior history from BoltDB.
	if req.Mode == SessionModeThreaded && req.ThreadID != "" {
		history := c.threadLoad(req.ThreadID)
		msgs = append(msgs, history...)
	}

	// Block 2 dynamic: user task.
	msgs = append(msgs, Message{Role: "user", Content: req.Prompt})
	return msgs
}

// apiResponse is the internal parsed API result.
type apiResponse struct {
	text              string
	inputTokens       int
	outputTokens      int
	cacheHitTokens    int
	cacheMissTokens   int
	reasoningTokens   int
	systemFingerprint string
	modelUsed         string
}

// resolveModel picks the model id for one call: req override → cfg → default.
func (c *Client) resolveModel(req CallRequest) string {
	if req.Model != "" {
		return req.Model
	}
	if c.cfg.Model != "" {
		return c.cfg.Model
	}
	return defaultModel
}

// resolveThinking picks the thinking config for one call: req override → cfg.
// Returns zero-value when neither is set; callers omit the body field in that
// case so the server applies its per-model default.
func (c *Client) resolveThinking(req CallRequest) ThinkingConfig {
	if req.Thinking != nil {
		return *req.Thinking
	}
	return c.cfg.Thinking
}

func (c *Client) callAPI(ctx context.Context, msgs []Message, maxTokens int, model string, thinking ThinkingConfig) (*apiResponse, error) {
	// [131.B] Rate limit: consume estimated tokens (capped at burst capacity).
	// The billing hard-cap already rejected oversized requests; here we pace throughput.
	estimatedTokens := int64(maxTokens)
	for _, m := range msgs {
		estimatedTokens += int64(len(m.Content) / 4)
	}
	if estimatedTokens > c.cfg.RateLimitBurst {
		estimatedTokens = c.cfg.RateLimitBurst
	}
	if c.cfg.RateLimitBurst == 0 {
		estimatedTokens = min64(estimatedTokens, 10000)
	}
	if err := c.limiter.WaitFor(ctx, estimatedTokens); err != nil {
		return nil, fmt.Errorf("deepseek: rate limit: %w", err)
	}

	bodyMap := map[string]any{
		"model":      model,
		"messages":   msgs,
		"max_tokens": maxTokens,
	}
	// Only include `thinking` when at least one field is set; otherwise
	// let the server pick its per-model default. The DeepSeek API requires
	// `type` to be present whenever the `thinking` object is sent — if the
	// caller only specified `reasoning_effort`, default `type` to "enabled"
	// (the only meaningful value for adjusting reasoning effort). Without
	// this default, the API returns HTTP 400: "missing field `type`".
	if !thinking.IsZero() {
		t := map[string]any{}
		typeVal := thinking.Type
		if typeVal == "" {
			typeVal = ThinkingEnabled
		}
		t["type"] = typeVal
		if thinking.ReasoningEffort != "" {
			t["reasoning_effort"] = thinking.ReasoningEffort
		}
		bodyMap["thinking"] = t
	}
	body, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, fmt.Errorf("deepseek: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("deepseek: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("deepseek: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("deepseek: api status %d: %s", resp.StatusCode, b)
	}

	var out struct {
		Model             string `json:"model"`
		SystemFingerprint string `json:"system_fingerprint"`
		Choices           []struct {
			Message struct {
				Content string `json:"content"`
				// reasoning_content is intentionally NOT carried into thread
				// history — including it in subsequent input messages
				// triggers HTTP 400 on the reasoner endpoint. Decoded here
				// only so JSON parsing is exhaustive; ignored downstream.
				ReasoningContent string `json:"reasoning_content,omitempty"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens          int `json:"prompt_tokens"`
			CompletionTokens      int `json:"completion_tokens"`
			TotalTokens           int `json:"total_tokens"`
			PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
			CompletionTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("deepseek: decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("deepseek: empty choices in response")
	}
	return &apiResponse{
		text:              out.Choices[0].Message.Content,
		inputTokens:       out.Usage.PromptTokens,
		outputTokens:      out.Usage.CompletionTokens,
		cacheHitTokens:    out.Usage.PromptCacheHitTokens,
		cacheMissTokens:   out.Usage.PromptCacheMissTokens,
		reasoningTokens:   out.Usage.CompletionTokensDetails.ReasoningTokens,
		systemFingerprint: out.SystemFingerprint,
		modelUsed:         out.Model,
	}, nil
}

// threadLoad reads prior messages for a thread from BoltDB.
func (c *Client) threadLoad(threadID string) []Message {
	if c.db == nil {
		return nil
	}
	var msgs []Message
	c.db.View(func(tx *bolt.Tx) error { //nolint:errcheck
		b := tx.Bucket([]byte(bucketThreads))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(threadID))
		if v == nil {
			return nil
		}
		var entry threadEntry
		if err := json.Unmarshal(v, &entry); err != nil {
			return nil
		}
		// Auto-distill: if token count > threshold, return empty (trigger re-summarise).
		if entry.TokenCount > autoDistillThreshold {
			return nil
		}
		msgs = entry.Messages
		return nil
	})
	return msgs
}

// threadAppend adds assistant reply to thread history and updates token count.
func (c *Client) threadAppend(threadID string, sent []Message, reply string, inputTokens int) {
	if c.db == nil {
		return
	}
	c.db.Update(func(tx *bolt.Tx) error { //nolint:errcheck
		b := tx.Bucket([]byte(bucketThreads))
		if b == nil {
			return nil
		}
		var entry threadEntry
		if v := b.Get([]byte(threadID)); v != nil {
			json.Unmarshal(v, &entry) //nolint:errcheck
		}
		// Append only the user turn + assistant reply (system already in block 1).
		for _, m := range sent {
			if m.Role == "user" {
				entry.Messages = append(entry.Messages, m)
			}
		}
		entry.Messages = append(entry.Messages, Message{Role: "assistant", Content: reply})
		entry.TokenCount += inputTokens
		entry.LastActive = time.Now().Unix()

		v, _ := json.Marshal(entry)
		return b.Put([]byte(threadID), v)
	})
}

// billingRecord increments the session token counter.
func (c *Client) billingRecord(tokens int) {
	if c.db == nil {
		return
	}
	c.db.Update(func(tx *bolt.Tx) error { //nolint:errcheck
		b := tx.Bucket([]byte(bucketBilling))
		if b == nil {
			return nil
		}
		var counter billingEntry
		if v := b.Get([]byte("session")); v != nil {
			json.Unmarshal(v, &counter) //nolint:errcheck
		}
		counter.Tokens += tokens
		counter.Calls++
		v, _ := json.Marshal(counter)
		return b.Put([]byte("session"), v)
	})
}

// checkpointLoad returns a cached response if a valid checkpoint exists.
func (c *Client) checkpointLoad(key string) (bool, *CallResponse) {
	if c.db == nil {
		return false, nil
	}
	var resp *CallResponse
	c.db.View(func(tx *bolt.Tx) error { //nolint:errcheck
		b := tx.Bucket([]byte(bucketCheckpts))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(key))
		if v == nil {
			return nil
		}
		var cp checkpointEntry
		if err := json.Unmarshal(v, &cp); err != nil {
			return nil
		}
		if time.Now().Unix()-cp.CreatedAt > checkpointTTL {
			return nil
		}
		cp.Response.CacheHit = true
		resp = &cp.Response
		return nil
	})
	return resp != nil, resp
}

// checkpointStore persists a response under the given key.
func (c *Client) checkpointStore(key string, resp *CallResponse) {
	if c.db == nil {
		return
	}
	c.db.Update(func(tx *bolt.Tx) error { //nolint:errcheck
		b := tx.Bucket([]byte(bucketCheckpts))
		if b == nil {
			return nil
		}
		cp := checkpointEntry{CreatedAt: time.Now().Unix(), Response: *resp}
		v, _ := json.Marshal(cp)
		return b.Put([]byte(key), v)
	})
}

// StartTTLReaper launches a background goroutine that purges threads idle > threadTTL.
// Call once per process; ctx cancellation stops the reaper.
func (c *Client) StartTTLReaper(ctx context.Context) {
	if c.db == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.reapExpiredThreads()
			}
		}
	}()
}

func (c *Client) reapExpiredThreads() {
	cutoff := time.Now().Add(-threadTTL).Unix()
	c.db.Update(func(tx *bolt.Tx) error { //nolint:errcheck
		b := tx.Bucket([]byte(bucketThreads))
		if b == nil {
			return nil
		}
		var stale [][]byte
		b.ForEach(func(k, v []byte) error { //nolint:errcheck
			var e threadEntry
			if json.Unmarshal(v, &e) == nil && e.LastActive < cutoff {
				stale = append(stale, append([]byte{}, k...))
			}
			return nil
		})
		for _, k := range stale {
			b.Delete(k) //nolint:errcheck
		}
		return nil
	})
}

// BillingStats returns the current session billing counters.
func (c *Client) BillingStats() (tokens, calls int) {
	if c.db == nil {
		return 0, 0
	}
	c.db.View(func(tx *bolt.Tx) error { //nolint:errcheck
		b := tx.Bucket([]byte(bucketBilling))
		if b == nil {
			return nil
		}
		var counter billingEntry
		if v := b.Get([]byte("session")); v != nil {
			json.Unmarshal(v, &counter) //nolint:errcheck
		}
		tokens = counter.Tokens
		calls = counter.Calls
		return nil
	})
	return
}

// computeCheckpointKey returns a deterministic SHA-256 hex key for P-IDEM.
func computeCheckpointKey(action string, files []FileEntry, prompt string) string {
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	sort.Strings(paths)
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00", action, prompt)
	for _, p := range paths {
		fmt.Fprintf(h, "%s\x00", p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CacheKey returns the Block 1 static cache key for a set of files (SHA-256 of sorted content hashes).
func CacheKey(files []FileEntry) string {
	type entry struct {
		path string
		hash string
	}
	entries := make([]entry, len(files))
	for i, f := range files {
		h := sha256.Sum256([]byte(f.Content))
		entries[i] = entry{path: f.Path, hash: hex.EncodeToString(h[:])}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s:%s\x00", e.path, e.hash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func newThreadID() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return "ds_thread_" + hex.EncodeToString(b)
}

// BoltDB entry types.

type threadEntry struct {
	Messages   []Message `json:"messages"`
	TokenCount int       `json:"token_count"`
	LastActive int64     `json:"last_active"`
}

type billingEntry struct {
	Tokens int `json:"tokens"`
	Calls  int `json:"calls"`
}

type checkpointEntry struct {
	CreatedAt int64        `json:"created_at"`
	Response  CallResponse `json:"response"`
}
