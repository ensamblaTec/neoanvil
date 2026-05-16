# Hacker News — Show HN

**URL:** [news.ycombinator.com/submit](https://news.ycombinator.com/submit)
**Best time:** Mon–Thu 6:30–8:30am Pacific (avoid Friday/weekend)
**Strategy:** post once, no resubmit. Reply to every top-level comment in first 4h.

---

## Title (max 80 chars)

**Show HN: NeoAnvil – Pure-Go MCP server that gates every AI code edit through AST+compile**

(78 chars, includes hyphen substitution per HN style)

## Alternative titles

- Show HN: A pure-Go MCP server with code-graph, RAG, and certify-on-edit
- Show HN: NeoAnvil – Multi-workspace MCP server with SSA call graph and HNSW search

---

## URL field

`https://github.com/ensamblaTec/neoanvil`

---

## Text (optional but recommended for Show HN)

NeoAnvil is a Model Context Protocol server I built in pure Go (no CGO on the native build) after burning too many tokens fixing AI-generated refactors that compiled in the model's head but broke 5 callers it never grepped for.

The core mechanic is **certify-on-edit**: every code mutation an AI client proposes goes through a transactional pipeline before it's allowed near a commit — AST validation, in-memory shadow-compile, optional test-impact narrowing, and a time-limited certification seal. A pre-commit hook rejects anything without a fresh seal.

On top of the cert pipeline, the server exposes 14 MCP tools / 60+ operations:

- 23 radar intents for code intelligence (BRIEFING, BLAST_RADIUS, SEMANTIC_CODE, AST_AUDIT, GRAPH_WALK, FILE_EXTRACT, …)
- A Code Property Graph built on `golang.org/x/tools/go/ssa` with PageRank + BFS walk + fast-boot persistence (~1.6s for 1.25M nodes)
- HNSW vector index with embedding cache and binary quantization (recall 1.000 in 3 production workspaces)
- 3-layer cache stack (QueryCache 54ns hit, TextCache 33ns hit, EmbeddingCache)
- Multi-workspace dispatcher (Neo-Nexus) — one SSE endpoint, N workspace workers, OAuth proxy
- Subprocess plugin subsystem (Jira Cloud, DeepSeek, GitHub) with `__health__` zombie detection
- OpenAPI 3.0 surface — the MCP tool registry serializes to `/openapi.json`, Swagger UI at `/docs`
- OpenTelemetry W3C traceparent propagation across the Nexus → child boundary
- Slack + Discord webhook dispatcher with allowlist filters + retry/backoff

Engineering bits that might be interesting:

- Pure-Go native build cross-compiles to linux/darwin × amd64/arm64 with SIMD auto-vec via GOAMD64/GOARM64. CGO only kicks in on the Docker stage-3 build for tree-sitter parsers.
- HNSW distance kernels have hand-written ARM64 assembly with the quirks the Go assembler docs don't surface (FMLA needs V-prefix, FADDP doesn't exist for float, etc.).
- Hot paths (RAG query, MCTS, cert batch) forbid `make()`/`new()` — buffers via `sync.Pool`, slices reset with `[:0]`. Custom zero-alloc JSON marshaller.
- BoltDB single-leader across the 4-tier config (workspace/project/org/nexus) because bbolt doesn't allow mixed RW+RO across processes.

MIT, no telemetry, no signup. Runs entirely local with Ollama (qwen2.5-coder + nomic-embed-text by default) — the LLM choice is yours though.

Docs: <https://github.com/ensamblaTec/neoanvil/blob/main/README.md>

Happy to answer questions. The piece I'd most welcome critique on is the OpenAPI/MCP bridge — exposing the MCP tool registry as OpenAPI lets you wire MCP servers into observability stacks that don't speak MCP natively, but I haven't seen anyone else doing it.

---

## First-comment reply (post yourself, 30s after submission)

> A few things that aren't in the README yet but came up in early testing:
>
> 1. The cert seal TTL (15min pair / 5min fast) is configurable in `neo.yaml` under `sre.certify_ttl_minutes` — picked those defaults empirically from how long a typical AI edit-test-fix cycle takes.
> 2. The `__health__` action on plugin subprocesses uses lock-free atomics so it can't be blocked by an in-flight API call. Pattern is in `cmd/plugin-deepseek/main.go::handleHealth` if anyone wants to crib it.
> 3. There's a separate MCP Registry submission in flight under `io.github.ensamblaTec/neoanvil`. The registry is in preview (launched Sep 2025), so don't be surprised if direct namespace lookups don't resolve until the publisher CLI run completes.
