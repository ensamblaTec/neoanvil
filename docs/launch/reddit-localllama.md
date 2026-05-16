# r/LocalLLaMA post

**Subreddit:** [r/LocalLLaMA/submit](https://www.reddit.com/r/LocalLLaMA/submit)
**Flair:** Resources (or Tutorial | Tools)
**Best time:** Tue/Wed 9–11am ET

---

## Title (pick one)

1. **NeoAnvil — pure-Go MCP server that gates every AI code edit through AST + shadow-compile + cert seal. Local-first via Ollama.** *(recommended)*
2. NeoAnvil: local-first MCP server in Go — code-graph + RAG + cert pipeline so your AI can't ship broken refactors.
3. Built an MCP server in Go that runs entirely local (Ollama), wraps Claude/GPT in a certify-on-edit pipeline, and ships a code-property-graph.

---

## Body

I've been running local Ollama models + Claude Code on a non-trivial Go codebase for ~6 months and kept hitting the same failure mode: the model confidently ships code that compiles in its head but breaks 5 callers it never grepped for.

**NeoAnvil** is my attempt to fix this at the protocol layer.

It's a Model Context Protocol server in **pure Go** (single binary, no CGO on the native build) that wraps any MCP-compatible AI client (Claude Code, Cursor, GPT-via-bridge, Ollama-via-bridge) in what I call a **certify-on-edit** pipeline:

- AST validation (`go/ast` for Go, tree-sitter for TS/Py/Rust)
- Shadow-compile (in-memory `go build ./...`)
- Optional test-impact narrowing (only run tests for affected pkgs)
- Time-limited certification seal (15 min in pair mode, 5 min in fast mode)
- Pre-commit hook rejects anything without a fresh seal

On top of the cert pipeline:

- **14 MCP tools / 60+ operations** — BRIEFING, BLAST_RADIUS, SEMANTIC_CODE, AST_AUDIT, GRAPH_WALK, FILE_EXTRACT, COMPILE_AUDIT, TECH_DEBT_MAP, …
- **Code Property Graph** with SSA-based call graph + PageRank + fast-boot persistence (BoltDB)
- **HNSW vector index** with embedding cache + binary quantization (recall 1.000 in 3 prod workspaces)
- **3-layer cache stack** — QueryCache (54ns hit), TextCache (33ns hit), EmbeddingCache
- **Multi-workspace dispatcher** (Neo-Nexus) — one SSE endpoint, N worker children, OAuth proxy
- **Plugin subsystem** — subprocess MCP plugins (Jira Cloud, DeepSeek, GitHub) with per-tenant rate limiting + `__health__` zombie detection
- **Lifecycle hooks** that auto-enforce a BRIEFING → BLAST_RADIUS → Edit → certify loop in Claude Code

**Local-first stack** — Ollama for both LLM and embeddings; tested with `qwen2.5-coder:7b` for refactor work and `nomic-embed-text` for the RAG index. Routing matrix decides which prompts go local (refactor / boilerplate / yes-no) vs DeepSeek (SEV≥9, crypto, architectural).

**Why pure Go?** Cross-compiles to linux/darwin × amd64/arm64 with SIMD auto-vectorization via GOAMD64/GOARM64. CGO only kicks in on the Docker stage-3 build for tree-sitter parsers.

MIT, no telemetry, no signup. The whole stack runs offline.

Repo: <https://github.com/ensamblaTec/neoanvil>

Happy to answer questions — especially curious if anyone has tried similar protocol-layer enforcement vs in-context prompting for code safety.

---

## Comment-reply hooks

Pre-write 3 short replies for the predictable questions:

**Q: "Why MCP not just an LSP?"**
> MCP gives the AI structured tool calls + bidirectional state. LSP is editor-side and language-specific. MCP also lets me expose chaos drills, OpenTelemetry traces, and the Code Property Graph as first-class tools the model can invoke during planning, not just at edit time.

**Q: "How fast is it really?"**
> Cold: ~3s for a 100k-LOC Go workspace (includes CPG load + HNSW snapshot). Warm cache: 54ns for query hits, 33ns for text. Cert pipeline runs AST + shadow-compile in ~400ms for typical 1-file edits, ~25s for multi-package sweeps. RAPL-based thermal guard throttles above 60W.

**Q: "What's the lock-in?"**
> None. It speaks plain MCP over SSE — your client points at `127.0.0.1:9000/mcp/sse` and disconnects whenever. The certify seal is just a BoltDB row + a pre-commit hook. Both are removable in one command.
