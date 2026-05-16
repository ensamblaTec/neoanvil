# NeoAnvil — Pitch sheet

Reusable pitch in 6 lengths. Pick by venue.

---

## 1. Name & tagline

**NeoAnvil** — MCP Server & SRE Orchestrator for AI-Assisted Development.

---

## 2. One-liner (X/Twitter, badge)

> Local-first MCP server in pure Go — 14 tools, code-graph + RAG, every AI edit gates through AST + shadow-compile + certify.

(160 characters incl. spaces)

---

## 3. Two-liner (HN title, Reddit title)

> Show NeoAnvil: a pure-Go MCP server that gates every AI code edit through AST + shadow-compile + a time-limited cert seal.

---

## 4. Three lines (registry description, README hero)

NeoAnvil is a pure-Go Model Context Protocol server that wraps AI coding assistants (Claude, GPT, Cursor) in a disciplined SRE workflow. Every mutation passes through a transactional pipeline — AST validation, shadow-compile, optional test run, and a time-limited certification seal. Uncertified changes are rejected by a pre-commit hook.

---

## 5. ~100 words (Reddit body, HN text, Discord pinned)

NeoAnvil is a pure-Go MCP server I built to fix a problem I kept hitting with Claude Code / Cursor: they happily ship broken code if you let them. NeoAnvil sits in front of every code mutation the AI proposes and gates it through AST validation, shadow-compile, optional test impact, and a time-limited certification seal. Uncertified changes get rejected by a pre-commit hook.

On top of the cert pipeline it ships 14 MCP tools / 60+ operations: a Code Property Graph with SSA call graphs + PageRank, HNSW vector search with binary quantization, a 3-layer cache stack, multi-workspace dispatcher (Neo-Nexus), plugins for Jira / DeepSeek / GitHub, and lifecycle hooks that auto-enforce a BRIEFING → BLAST_RADIUS → Edit → certify loop. Local-first via Ollama; cross-compiles to linux/darwin × amd64/arm64.

MIT, no telemetry, no signup.

---

## 6. Long form (~300 words, Anthropic forum, dev.to)

I've been running AI coding agents (Claude Code, Cursor, sometimes GPT in CI) on a non-trivial Go codebase for the last six months, and a pattern kept repeating: the agent confidently ships code that compiles in its head but breaks in mine. Half the time the bug is one missed nil-check in a switch case it never read. The other half it's a refactor that breaks 5 callers it never grepped for.

**NeoAnvil** is the result of trying to solve this at the protocol layer instead of begging the model to be more careful.

It's a Model Context Protocol server in pure Go. The headline mechanic is what I call **certify-on-edit**: every mutation the AI proposes (an `Edit`, `Write`, `MultiEdit`) goes through a transactional pipeline before it's allowed near a commit:

- AST validation (`go/ast` for Go, tree-sitter for TS/Py/Rust)
- Shadow-compile (in-memory `go build ./...`)
- Optional test-impact narrowing (`go test -run` of affected pkgs)
- Time-limited certification seal (15 min in pair mode, 5 min in fast mode)
- Pre-commit hook rejects anything without a fresh seal

On top of that pipeline there are 14 MCP tools / 60+ operations:

- **23 radar intents** for code intelligence (BRIEFING, BLAST_RADIUS, SEMANTIC_CODE, AST_AUDIT, GRAPH_WALK, FILE_EXTRACT, COMPILE_AUDIT, …)
- **Code Property Graph** with SSA-based call graph, PageRank, and fast-boot persistence
- **HNSW vector index** with embedding cache and binary quantization
- **3-layer cache stack**: QueryCache (54ns hit), TextCache (33ns hit), EmbeddingCache
- **Multi-workspace dispatcher** (Neo-Nexus) — one SSE endpoint, N workers
- **Plugin subsystem** — Jira Cloud, DeepSeek, GitHub (each in its own subprocess with `__health__` zombie detection)
- **Lifecycle hooks** for Claude Code that auto-enforce BRIEFING → BLAST_RADIUS → Edit → certify
- **OpenTelemetry** W3C traceparent propagation
- **Slack + Discord webhooks** with allowlist filters and retry/backoff

Pure-Go native build (`make build`), Docker stage 3 enables CGO for tree-sitter. Cross-compiles to linux/darwin × amd64/arm64 with SIMD auto-vectorization via GOAMD64/GOARM64. Local-first via Ollama.

MIT licensed, no telemetry, no signup. Single-binary distribution.

Repo: <https://github.com/ensamblaTec/neoanvil>
