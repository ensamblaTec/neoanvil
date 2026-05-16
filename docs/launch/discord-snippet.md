# Discord / Slack snippets

Short copy-paste blocks for chat-based channels. Long-form should link to the repo, not paste the README.

---

## MCP Discord (modelcontextprotocol)

**Channel suggestion:** `#showcase` or `#servers`

```
🛠️ Just shipped NeoAnvil v0.10.6 — pure-Go MCP server with 14 tools (BRIEFING, BLAST_RADIUS, SEMANTIC_CODE, AST_AUDIT, GRAPH_WALK, …), multi-workspace SSE dispatcher, Code Property Graph + HNSW RAG, and a certify-on-edit pipeline that gates every AI mutation through AST + shadow-compile + test-impact before it can touch a commit.

Plugins for Jira / DeepSeek / GitHub. OpenAPI surface at /openapi.json. OTel traceparent propagation across the Nexus → worker boundary.

MIT, no telemetry, single binary, cross-compiles linux/darwin × amd64/arm64.

→ https://github.com/ensamblaTec/neoanvil
```

---

## Claude Code Discord / community Slack

```
Sharing NeoAnvil — an MCP server I built that hooks into Claude Code's lifecycle (SessionStart, PreToolUse, PostToolUse, Stop) to auto-enforce a BRIEFING → BLAST_RADIUS → Edit → certify loop. The pre-commit hook rejects edits without a fresh cert seal so Claude can't ship broken refactors.

If you've been burned by Claude shipping code that breaks 5 callers it never grepped — this is the protocol-layer fix.

Pure Go, MIT, runs local. → https://github.com/ensamblaTec/neoanvil
```

---

## Ollama Discord

**Channel suggestion:** `#integrations` or `#community-projects`

```
NeoAnvil — pure-Go MCP server that routes coding prompts between Claude/GPT and local Ollama based on a decision matrix (refactor/boilerplate/yes-no → local; SEV≥9/crypto/architectural → cloud).

Tested defaults: qwen2.5-coder:7b for code, nomic-embed-text for the RAG index.

100 audits/night = $3-15 with cloud DS vs $0 with local. Routing matrix in `pkg/local_llm/`.

→ https://github.com/ensamblaTec/neoanvil
```

---

## Go Discord (Gophers Slack)

**Channel suggestion:** `#show-and-tell`

```
Pure-Go MCP server (no CGO native build, CGO only in Docker stage-3 for tree-sitter). 14 tools, SSE multi-workspace dispatcher, SSA-based Code Property Graph with PageRank + BFS walk, HNSW vector index with hand-written ARM64 SIMD kernels.

Forced single-binary discipline → cross-compiles to linux/darwin × amd64/arm64 with GOAMD64/GOARM64 auto-vec.

100% staticcheck clean, audit-baseline tracked. MIT. → https://github.com/ensamblaTec/neoanvil
```
