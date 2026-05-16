# r/mcp post

**Subreddit:** [r/mcp/submit](https://www.reddit.com/r/mcp/submit)
**Flair:** Project | Server
**Best time:** check the subreddit's posting guidelines + recent top-post timestamps before submitting

---

## Title

**NeoAnvil — multi-workspace MCP server in pure Go: 14 tools, SSE dispatcher, plugin subprocess pool, Code Property Graph, HNSW RAG.**

---

## Body

Sharing a pure-Go MCP server I've been building for the last few months. Focus is *server-side capabilities* rather than thin wrappers around a single API.

### Architecture

```
AI client (Claude Code / Cursor / GPT)
       │ MCP (JSON-RPC over SSE)
       ▼
Neo-Nexus :9000   ── dispatcher, OAuth proxy, watchdog, plugin pool
  ├─ neo-mcp :9100   ── workspace worker A
  ├─ neo-mcp :9101   ── workspace worker B
  └─ plugin pool     ── jira, deepseek, github (subprocess each)
```

### Tool registry

14 tools / 60+ operations exposed via `GET /openapi.json::x-mcp-tools`:

- **`neo_radar`** — meta-tool with 23 intents (BRIEFING, BLAST_RADIUS, SEMANTIC_CODE, AST_AUDIT, GRAPH_WALK, FILE_EXTRACT, COMPILE_AUDIT, TECH_DEBT_MAP, PROJECT_DIGEST, INCIDENT_SEARCH, CONTRACT_QUERY, …)
- **`neo_sre_certify_mutation`** — AST + shadow-compile + test-impact pipeline with time-limited cert seal
- **`neo_daemon`** — 12 governance actions (pull/push tasks, vacuum memory, FLUSH_PMEM, quarantine_IP, …)
- **`neo_chaos_drill`** — synchronous chaos injection (1-10 aggression, configurable goroutines)
- **`neo_cache`** — 3-layer cache (Query 54ns, Text 33ns, Embedding) + stats + invalidation
- **`neo_command`** — sandboxed shell exec with allowlist + timeout
- **`neo_memory`** — durability-hardened directives + lessons (BoltDB + WAL + corruption guards + snapshot/restore)
- **`neo_compress_context`** — token-aware context summarization
- **`neo_apply_migration`** — schema migrations
- **`neo_download_model`** — Ollama model lifecycle
- **`neo_log_analyzer`** — structured log triage
- **`neo_tool_stats`** — per-tool cost + invocation telemetry
- **`neo_debt`** — technical debt store with priority + resolution lifecycle
- **`neo_local_llm`** — local Ollama LLM invocation (qwen2.5-coder default)

Plus 3 plugin servers (Jira/DeepSeek/GitHub), each in its own subprocess with a `__health__` action so Nexus can detect zombies and respawn.

### Interesting bits

- **OpenAPI surface** — the MCP tool registry serializes to OpenAPI 3.0 (`/openapi.json`), Swagger UI at `/docs`. Lets you wire MCP into observability + API clients that don't speak MCP natively.
- **OpenTelemetry traceparent propagation** — W3C `traceparent` flows from your client through Nexus into the child worker via `X-Neo-Traceparent`. Pluggable `pkg/otelx` tracer (noop default; recording for tests).
- **Webhook subsystem** — `pkg/notify` ships Slack + Discord dispatcher fed by per-child SSE subscribers with event allowlist filters + retry/backoff. Lets you wire MCP server events into team channels without polling.
- **HUD dashboard** — 21 SSE event types, 8 dedicated handlers + 3 critical banners + 10 ops-log channels. Served at `127.0.0.1:8087/`.
- **Federation tiers** — 4-tier config + knowledge store (workspace → project → org → nexus). bbolt doesn't support mixed RW+RO so each tier has a single-leader rule.

### Build / distribution

- Pure-Go native build (zero CGO)
- Cross-compiles to linux/darwin × amd64/arm64 with SIMD auto-vec via GOAMD64/GOARM64
- Docker stage-3 enables CGO for tree-sitter (gcc + musl-dev)
- Single binary deploys: `neo-mcp` + `neo-nexus` + `neo` CLI
- MIT, no telemetry

### Status

- 100% staticcheck clean
- audit-baseline tracked in repo (`.neo/audit-baseline.txt`)
- About to publish to the official MCP Registry under `io.github.ensamblatec/neoanvil`

Repo: <https://github.com/ensamblaTec/neoanvil>

Feedback welcome — especially on the OpenAPI/MCP bridge approach. I'd love to see other servers exposing their tool registry as OpenAPI; happy to extract that into a small library if there's interest.
