# DeepSeek API Reference — Plugin Integration

> Captured 2026-04-30 from a live smoke test against `deepseek-chat`.
> Reflects the exact wire format produced by `pkg/deepseek/client.go` after
> Épicas 131.A-K + bootstrap Épica 143.

This doc captures what neoanvil's DeepSeek plugin sends to and receives from
the DeepSeek API, plus session and cache mechanics. Use it to debug billing
surprises, validate cache hits, or design new actions.

---

## 1. Architecture — Block 1 / Block 2 split

Each call assembles two blocks that get concatenated into the OpenAI-style
chat-completions payload:

```
Block 1 (STATIC — cached by DeepSeek's prefix cache)
├── system message: "You are a precise software engineering assistant..."
└── ### Context files
    ├── #### path/to/file1.go
    │   ```
    │   <full file contents>
    │   ```
    └── #### path/to/file2.go
        ```
        ...
        ```

Block 2 (DYNAMIC — fresh on every call)
└── user message: <target_prompt + chunk body>
```

The DeepSeek API computes a SHA-256 fingerprint over the **prefix** (Block 1).
If that fingerprint matches a previous call within the cache window
(currently ~1 hour), the prefix tokens are billed at the cache-hit rate
(~25% of the miss rate). Block 2 is always re-priced.

**Implication:** running `distill_payload` over the same files multiple
times cheap; varying files between calls invalidates the prefix.

---

## 2. HTTP request shape

`POST https://api.deepseek.com/v1/chat/completions`

### Headers

```
Content-Type: application/json
Authorization: Bearer sk-XXXXXXXXXXXXXXXXXXXXXX
```

The `Authorization` header value comes from `~/.neo/credentials.json`
provider="deepseek" entry, resolved per-spawn by Nexus's vault-bridge
(`pkg/auth/vault.go::resolveCredField` — `API_KEY`, `KEY`, and `TOKEN` are
all aliases of `e.Token`).

### Body (real example from smoke test)

```json
{
  "model": "deepseek-chat",
  "messages": [
    {
      "role": "system",
      "content": "You are a code-analysis assistant.\n\n### Context files\n\n#### pkg/state/daemon_certify.go\n```\n// Package state — daemon TTL seal auto-renew helpers. [132.D]\npackage state\n\nimport (\n\t\"fmt\"\n\t...\n)\n\nfunc GetSealedFilesNeedingRenewal(lockPath string, ttlSeconds, bufferSec int) ([]string, error) {\n\t...\n}\n```\n"
    },
    {
      "role": "user",
      "content": "Summarize this file in 2 sentences: what it does and the key function signatures it exports.\n\n---CHUNK 1---\nfunc GetSealedFilesNeedingRenewal(lockPath string, ttlSeconds, bufferSec int) ([]string, error) {\n  ...\n}"
    }
  ],
  "max_tokens": 300
}
```

**Body fields documented in our schema:**

| Field | Source | Notes |
|---|---|---|
| `model` | `Config.Model` (default `deepseek-chat`) | `deepseek-reasoner` for thinking mode (R1) |
| `messages` | Built by `client.buildMessages()` | system + (optional thread history) + user |
| `max_tokens` | `CallRequest.MaxTokens` | Default 4096, hard cap 50000 |

**NOT sent today (gap — opt-in if needed later):**

| Field | Why we skip | When to enable |
|---|---|---|
| `temperature` | Default deterministic-ish (~0.7) | Tweak if outputs drift |
| `top_p` | Default 1.0 | Rarely useful for code tasks |
| `stream: true` | We collect full response | Enable for live progress UI |
| `tools` (function-calling) | Not used; we drive flow client-side | Future epic if needed |

---

## 3. HTTP response shape

```json
{
  "id": "chatcmpl-...",
  "object": "chat.completion",
  "created": 1714521743,
  "model": "deepseek-chat",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "This file provides daemon TTL seal auto-renewal logic..."
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 1620,
    "completion_tokens": 260,
    "total_tokens": 1880,
    "prompt_cache_hit_tokens": 0,
    "prompt_cache_miss_tokens": 1620
  }
}
```

**Currently captured by our client (`pkg/deepseek/client.go::apiResponse`):**

```go
type apiResponse struct {
    text         string  // ← out.Choices[0].Message.Content
    inputTokens  int     // ← out.Usage.PromptTokens
    outputTokens int     // ← out.Usage.CompletionTokens
}
```

**Gap (follow-up work):** we don't capture `prompt_cache_hit_tokens` /
`prompt_cache_miss_tokens` from the response. Adding them would let us
verify cache hits in real time (today the `cache_hit` field returned by
`distill_payload` is computed locally from our own SHA-256 tracker, not
from DeepSeek's API metadata). Trivial 1-line struct addition + plumbing.

---

## 4. Models available

> **🚨 Update 2026-04-26 (verified against `api-docs.deepseek.com`):** `deepseek-chat`
> and `deepseek-reasoner` are now both aliases of **`deepseek-v4-flash`** —
> the former in non-thinking mode, the latter in thinking (CoT) mode. The
> aliases are kept for backward compatibility but are flagged as
> "deprecated in the future" by the official docs.

### Unified V4-flash pricing (effective 2026-04-26)

| Metric | deepseek-chat (V4-flash non-thinking) | deepseek-reasoner (V4-flash thinking) |
|---|---|---|
| Input — cache hit | $0.0028/M | $0.0028/M |
| Input — cache miss | $0.14/M | $0.14/M |
| Output | $0.28/M | $0.28/M |
| Max context | **1M tokens** | **1M tokens** |
| Max output | 384K tokens | 384K tokens (includes CoT tokens) |

**Two facts that drive every cost decision:**

1. **Cache hit is 50× cheaper than cache miss** ($0.0028 vs $0.14 per M
   input). Repeating the same Block 1 (same `Files[]` array, same
   `SystemMsg`) across calls drops the input bill from $1 to $0.02 for
   100K tokens. Cache discipline dominates total cost — keep file lists
   stable across related calls.

2. **Reasoner emits CoT tokens that count as output.** A 500-token visible
   answer can carry 2000 CoT tokens → 2500 output tokens billed. Same
   per-token rate as chat, but typically 3-5× more output volume. Use
   reasoner only when judgment quality justifies the extra output cost.

### Per-action recommendation

| Action | Recommended model | Why |
|---|---|---|
| `distill_payload` | `deepseek-chat` | Mechanical compression; non-thinking sufficient |
| `map_reduce_refactor` | `deepseek-chat` | Pattern application; deterministic |
| `red_team_audit` | `deepseek-reasoner` | CoT catches subtle bugs; extra cost justified |
| `generate_boilerplate` | `deepseek-chat` | Predictable output |

### Reasoner-mode constraints

The thinking-mode endpoint refuses several parameters with HTTP 400:

| Disallowed | Reason |
|---|---|
| `temperature`, `top_p` | Sampling baked into the reasoning policy |
| `presence_penalty`, `frequency_penalty` | Same |
| `logprobs`, `top_logprobs` | CoT internals not exposed via logprobs |
| `tools` (function-calling) | Reasoner doesn't support tool routing yet |
| FIM (Fill-In-Middle) | Reasoner doesn't expose suffix continuation |

**Multi-turn caveat:** when continuing a threaded reasoner conversation,
the caller must NOT include prior `reasoning_content` in the messages
array — only the visible `content` from each prior turn. Including the
CoT triggers HTTP 400 "reasoning_content not allowed in input". Our
`pkg/deepseek/client.go::threadAppend` already strips this correctly
(stores only `assistant`/`user` roles with `content`).

### Currently hardcoded

`defaultModel = "deepseek-chat"` (client.go:43). Per-action override is a
future enhancement; expose `Config.ModelOverrides map[string]string` and
sanitize the payload (strip `temperature` etc.) when the resolved model is
`deepseek-reasoner`.

---

## 5. Session modes — when ThreadID matters

```go
const (
    SessionModeEphemeral    // fire-and-forget, no state persisted
    SessionModeThreaded     // ThreadID persisted in BoltDB
)
```

Per-action defaults (`pkg/deepseek/session/router.go`):

| Action | Mode | ThreadID returned? | Caller saves? |
|---|---|---|---|
| `distill_payload` | Ephemeral | empty | no |
| `map_reduce_refactor` | Ephemeral | empty | no |
| `red_team_audit` | Threaded | `ds_thread_<8hex>` | yes — pass back next call to continue the audit |
| `generate_boilerplate` | Threaded (background) | `ds_thread_<8hex>` | yes — for status polling |

### Threaded flow (red_team_audit)

```
Call 1: action=red_team_audit, files=[...], target_prompt="audit auth flow"
  → response.thread_id = "ds_thread_a1b2c3d4"
  → BoltDB ~/.neo/db/deepseek.db bucket "deepseek_threads" persists:
      {id, history, file_deps, file_deps_key (sha256), status: active}

Call 2: action=red_team_audit, thread_id="ds_thread_a1b2c3d4",
        target_prompt="now check for race conditions in the same flow"
  → server reloads history, prepends to messages, posts new turn
  → if any file in file_deps changed (sha256 diff) → thread auto-expired,
    response signals "thread invalidated, start a new one"
```

### Cache hit between calls

Independent of thread mode — the DeepSeek API's prefix cache hits
whenever Block 1's SHA-256 matches a recent prior call. Two mechanisms
combined:

1. **API prefix cache (free, ~1h window):** automatic. No client code
   needed.
2. **Thread state (BoltDB, persistent):** explicit. Caller passes
   `ThreadID` to continue.

The cheapest workflow: keep the same file list across distillation calls
to maximize cache hits, then swap to threaded only when actually
auditing multi-turn.

---

## 6. Rate limiting + billing circuit breaker

Two protections layered (`pkg/deepseek/client.go`):

```
[131.B] Token bucket rate limiter
  refill_rate = Config.RateLimitTPM (default 60000 tokens/min)
  burst       = Config.RateLimitBurst (default 10000)
  blocks Call() until tokens available

[131.J] Per-session billing circuit breaker
  hard_cap = Config.MaxTokensPerSession (default 500000 tokens)
  rejects new calls once cumulative tokens exceed cap
  reset by removing the BoltDB bucket "deepseek_billing"
```

If a refactor batch is going to consume >500K tokens, raise the cap
explicitly via `neo.yaml::deepseek.max_tokens_per_session`. Default
catches runaway prompts.

---

## 7. Token accounting & cost estimation

Real numbers from our smoke test (2026-04-30):

```
Action:        distill_payload
Files:         pkg/state/daemon_certify.go (65 lines, ~2 KB)
Prompt:        "Summarize this file in 2 sentences..."
Chunks:        2 (ASTChunker split by function)
Total tokens:  1880  (1620 prompt + 260 completion)
cache_hit:     false (first call)
Cost:          1620 × $0.27/1M + 260 × $1.10/1M ≈ $0.000724

Repeating the same call within ~1h:
cache_hit:     true (same Block 1 fingerprint)
Cost:          1620 × $0.07/1M + 260 × $1.10/1M ≈ $0.000400
Savings:       45% on this call (mostly input was cached)
```

**Rule of thumb for `distill_payload` (V4-flash pricing):**

| Workload | Tokens (typical) | Cost (cache miss) | Cost (cache hit) |
|---|---|---|---|
| 1 KB source · 1 chunk | ~800-1500 total | ~$0.0002 | ~$0.00004 (5×↓) |
| 10 KB source · 5 chunks | ~5K-8K total | ~$0.001 | ~$0.0002 (5×↓) |
| 100 KB source · 50 chunks | ~50K-80K total | ~$0.012 | ~$0.0024 (5×↓) |

`map_reduce_refactor` is cheaper per call (refactor pattern small, output
is the diff). `red_team_audit` with reasoner is 3-5× the chat cost for the
same prompt because of CoT output volume.

**Cache-friendly batching pattern:** if you'll process the same 50-file
codebase with 10 different prompts, do all 10 calls in sequence
**without changing the `Files` slice between them**. Block 1 fingerprint
stays identical → 9/10 calls hit cache → 50× input cost reduction.

---

## 8. Common debugging steps

### Plugin not spawning

1. `curl -s http://127.0.0.1:9000/api/v1/plugins` — should list `deepseek`
   with `status: running`.
2. If `errors:{deepseek: "..."}`, read the error: usually `DEEPSEEK_API_KEY
   is required` (vault didn't bridge → check `neo login --provider deepseek`
   ran successfully).
3. Tail Nexus log: `tail -50 ~/.neo/logs/nexus-neoanvil-45913.log`.

### "rate limit" errors

`pkg/deepseek/client.go::callAPI` returns `deepseek: rate limit: ...` when
the local token bucket is exhausted. Either:
- Raise `Config.RateLimitTPM` (default 60K/min) in `neo.yaml::deepseek`.
- Wait — the bucket refills at the configured rate.

### Unexpected "session billing exceeded"

The 500K-token cap is per-process. Reset by either:
- Restart Nexus (`make rebuild-restart`) — wipes in-memory + BoltDB on
  fresh boot if the bucket was created mid-session.
- Or raise `max_tokens_per_session` in config.

### Cache always misses

Most common cause: `Files` list differs across calls (different paths or
different content). `CacheKeyTracker.Snapshot` re-hashes per-call. Verify
with `pkg/deepseek/cache/tracker.go::CacheKeyTracker.Get(path)`.

---

## 9. Future enhancements (open follow-ups, not yet épicas)

| Gap | Impact | Effort |
|---|---|---|
| Capture `prompt_cache_hit_tokens` from API response | Real cache visibility | 1 day |
| Per-action model overrides (chat vs reasoner) | Cost optimization for `red_team_audit` | 2 days |
| `temperature` / `top_p` exposed as call args | Output variance control | 1 day |
| Streaming responses (`stream: true`) | Live progress for long calls | 3 days |
| Tool-calling (DeepSeek function-calling) | Agent-style flows inside DeepSeek | 1 week |

These are intentionally NOT in PILAR XXVII — start with the simple
hardcoded chat model, validate the daemon iterative flow, then layer
optimizations once we have real usage data.
