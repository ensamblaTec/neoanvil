# dev.to article (long-form)

**URL:** [dev.to/new](https://dev.to/new)
**Tags:** `mcp`, `claude`, `ai`, `go`
**Canonical URL:** if you also post to your own blog later, set canonical there.

---

## Title

**Stopping AI Coding Agents From Shipping Broken Refactors — A Protocol-Layer Approach**

## Cover image suggestion

Architecture diagram from the README (sections "Architecture" → ASCII box). Render it cleanly with carbon.now.sh or a similar tool, save as 1000×420 PNG, upload.

---

## Article body

> *TL;DR — I built NeoAnvil, a pure-Go Model Context Protocol server that gates every code mutation an AI agent proposes through AST validation, shadow-compile, and a time-limited certification seal. A pre-commit hook rejects anything without a fresh seal. Repo: <https://github.com/ensamblaTec/neoanvil>.*

### The failure mode

You give Claude Code (or Cursor, or GPT-via-Continue) a non-trivial refactor task. It reads three files, plans, writes a confident-looking patch, and ships. Build fails. Five callers it never opened are broken because the signature changed.

You ask it to fix. It reads the five callers. Two of *those* callers have their own callers. The patch sprawls. By the time it lands, you've burned more tokens fixing the refactor than the refactor was worth.

This isn't a model intelligence problem. The model knows how to grep callers. It just didn't, because in the moment it was confident enough to skip the step. Adding "please grep callers before editing" to the system prompt helps for ~3 sessions, then drifts.

The fix has to be **structural**, not prompted.

### Protocol-layer enforcement

The Model Context Protocol (MCP) is the obvious surface to enforce at. The AI client calls MCP tools to read files, search code, run shell commands. If you put a server in front of those tool calls that's *opinionated about what the agent must do*, you get hard enforcement without prompt engineering.

That's what NeoAnvil is. A pure-Go MCP server I wrote over the last few months that wraps any MCP-compatible client (Claude Code, Cursor, GPT-via-bridge) in a disciplined SRE loop:

```
SessionStart  → BRIEFING auto-injected (workspace state, RAG coverage)
UserPromptSubmit → multi-file/hot-path heuristic → DS pre-mortem hint
PreToolUse(Edit) → BLAST_RADIUS (find all callers, transitive deps)
                 → AST_AUDIT (CC, shadow vars, hot-path detection)
PostToolUse(Edit) → file enters session_pending_cert.list
                  → cert pipeline (AST + shadow-compile + test-impact)
Stop → final cert-pending check
Pre-commit → rejects anything without a fresh cert seal
```

The "cert seal" is the meat. Every `Edit`, `Write`, `MultiEdit` the agent makes triggers:

1. **AST validation** — `go/ast` for Go, tree-sitter for TS/Py/Rust. Confirms the change parses and matches expected structure.
2. **Shadow-compile** — in-memory `go build ./...` (or `tsc --noEmit` for TS). Proves the change type-checks across the package graph, not just locally.
3. **Optional test-impact** — `go test -run` narrowed to the packages affected by the change. Skips full-suite latency.
4. **Time-limited seal** — a row in BoltDB with a TTL (15 min pair mode, 5 min fast mode).
5. **Pre-commit hook** — rejects any file in the commit that doesn't have a fresh seal.

In practice: the agent edits a function signature. The cert pipeline shadow-compiles. Five callers break. Cert fails with the caller list. The agent now has structured feedback — five concrete files to fix — *before* a single commit is attempted. The fix-it loop happens in seconds against the cert pipeline, not in minutes against `git push` failures.

### Beyond the cert pipeline

While I was building the cert mechanic, the same architecture grew into a fuller MCP-server toolkit. The headline numbers:

- **14 MCP tools / 60+ operations** — `neo_radar` (the meta-tool with 23 intents), `neo_sre_certify_mutation`, `neo_daemon` (governance actions), `neo_chaos_drill` (synchronous chaos injection), `neo_cache` (3-layer cache stack), `neo_memory` (durability-hardened directives), and more.
- **23 radar intents** — `BRIEFING`, `BLAST_RADIUS`, `SEMANTIC_CODE`, `AST_AUDIT`, `GRAPH_WALK`, `FILE_EXTRACT`, `COMPILE_AUDIT`, `TECH_DEBT_MAP`, `PROJECT_DIGEST`, `INCIDENT_SEARCH`, `CONTRACT_QUERY`, and more.
- **Multi-workspace dispatcher** (Neo-Nexus) — one SSE endpoint, N workspace workers, OAuth proxy, plugin pool.
- **Code Property Graph** built on `golang.org/x/tools/go/ssa` — SSA-exact call graph with PageRank, BFS walk, fast-boot persistence (1.6s for 1.25M nodes).
- **HNSW vector index** with binary quantization. Recall 1.000 in 3 production workspaces using `nomic-embed-text` embeddings.
- **3-layer cache stack** — QueryCache (54ns hit), TextCache (33ns hit), EmbeddingCache.
- **Plugin subsystem** — Jira Cloud, DeepSeek, GitHub. Each in its own subprocess with a `__health__` action for zombie detection.
- **OpenAPI 3.0 surface** — the MCP tool registry serializes to `/openapi.json`, Swagger UI at `/docs`. Lets you wire MCP into observability stacks.
- **OpenTelemetry W3C traceparent propagation** across the Nexus → child worker boundary.
- **Slack + Discord webhooks** with allowlist filters + retry/backoff.

### Engineering choices worth highlighting

**Pure Go, native build, no CGO.** Cross-compiles cleanly to linux/darwin × amd64/arm64 with SIMD auto-vec via `GOAMD64=v3` / `GOARM64`. CGO only turns on in Docker stage 3 for tree-sitter parsers. The forcing function has been valuable — no surprise libc, no dynamic linking, single-binary ops.

**SIMD assembly for HNSW kernels (ARM64).** The Go ARM64 assembler has quirks the docs don't surface — `FMLA` needs a `V` prefix, `FADDP` doesn't exist for float (use `VEXT` rotation), `Fn` and `Vn.S[0]` are the same physical register. Hand-rolled kernels for the float32×4 dot product made the difference between 280ms and 4ms per query on M1.

**Zero-allocation hot paths.** RAG query path, MCTS search path, and cert batch processor all forbid `make()`/`new()`. Buffers via `sync.Pool`, slices reset with `[:0]`. Custom `ZeroAllocJSONMarshal` in `pkg/sre/allocs.go`.

**SafeHTTPClient.** Internal anti-SSRF dialer that iterates resolved IPs and rejects RFC1918/link-local/loopback for external clients. Caught an IPv6-first dual-stack drift bug I shipped, then fixed via this layer.

### Try it

```bash
git clone https://github.com/ensamblaTec/neoanvil && cd neoanvil

# Path A — Docker (fastest)
make docker-build && make docker-up
docker exec -it neoanvil-ollama       ollama pull llama3.2:3b
docker exec -it neoanvil-ollama-embed ollama pull nomic-embed-text

# Path B — Native (best for hot-reload dev)
make build
neo setup my-workspace
make rebuild-restart

# Point your MCP client at:
# http://127.0.0.1:9000/mcp/sse
```

MIT, no telemetry, no signup. Runs offline via Ollama; LLM choice is yours.

### What I'd love feedback on

1. The **OpenAPI / MCP bridge** — exposing the MCP tool registry as OpenAPI 3.0 lets you wire MCP servers into observability + API tools that don't speak MCP. I haven't seen anyone else doing this; happy to extract it as a small library if there's interest.
2. The **lifecycle hooks pattern** for Claude Code. The 7 hooks I shipped cover BRIEFING/BLAST_RADIUS/certify, but there's enormous headroom for community-contributed hooks (CI integration, design-doc enforcement, security review gates). The migration guide explains how to adopt the hooks alone if you don't want the MCP server.
3. The **cert seal TTL.** I picked 15 min (pair) / 5 min (fast) empirically. If you've thought about how long a typical AI edit-test-fix cycle takes — I'd love to hear what you'd pick and why.

### Repo + docs

- **Code:** <https://github.com/ensamblaTec/neoanvil>
- **README:** <https://github.com/ensamblaTec/neoanvil/blob/main/README.md>
- **Hooks-only migration guide:** <https://github.com/ensamblaTec/neoanvil/blob/main/docs/guide/neo-doctrine-migration-guide.md>

Thanks for reading. If this saved you token spend, drop a star and tell me what hook you'd want next.
