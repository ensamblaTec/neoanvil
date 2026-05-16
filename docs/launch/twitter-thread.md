# X / Twitter thread

8 tweets, ~280 chars each. Numbered (1/8 … 8/8) so they thread cleanly.

---

**1/8**

I shipped NeoAnvil — a pure-Go MCP server that gates every AI code edit through AST + shadow-compile + a time-limited cert seal.

If your AI agent has ever shipped a refactor that broke 5 callers it never grepped — this is the protocol-layer fix.

→ github.com/ensamblaTec/neoanvil 🧵

---

**2/8**

The headline mechanic: **certify-on-edit**.

Every Edit/Write the AI proposes goes through:
• AST validation (`go/ast` + tree-sitter)
• Shadow-compile (in-memory `go build ./...`)
• Test-impact narrowing
• Time-limited cert seal (15min pair / 5min fast)

Pre-commit hook rejects uncertified files.

---

**3/8**

Beyond the cert pipeline: 14 MCP tools / 60+ ops including a Code Property Graph (SSA-based call graph + PageRank), HNSW vector search with binary quantization, and a 3-layer cache stack (54ns query hits, 33ns text hits).

Local-first via Ollama. No cloud lock-in.

---

**4/8**

Multi-workspace dispatcher (Neo-Nexus) — one SSE endpoint, N workspace workers, OAuth proxy.

You can run several repos behind one MCP endpoint. Each worker has its own RAG index, CPG, and cache. Federation is 4-tier: workspace → project → org → nexus.

---

**5/8**

Subprocess plugin pool: Jira Cloud, DeepSeek, GitHub. Each plugin runs in its own process with a `__health__` action so Nexus can detect zombies and respawn.

Per-tenant rate limiting + audit hash-chain. Multi-tenant credentials via `~/.neo/credentials.json` (0600).

---

**6/8**

Lifecycle hooks for Claude Code auto-enforce BRIEFING → BLAST_RADIUS → Edit → certify.

7 bash scripts, ~32KB total, bash 3.2 compatible. Registers in `.claude/settings.json`. You can adopt the hooks alone without the MCP server.

Migration guide in the repo.

---

**7/8**

Engineering: pure-Go native build (zero CGO). Cross-compiles linux/darwin × amd64/arm64. SIMD auto-vec via GOAMD64/GOARM64. Hand-written ARM64 assembly for the HNSW distance kernels.

100% staticcheck clean. Audit baseline tracked in-repo.

---

**8/8**

MIT, no telemetry, no signup. Single binary, cross-platform.

Try it: `make docker-up` or `make build && neo setup my-workspace`.

→ github.com/ensamblaTec/neoanvil

(About to publish to the official MCP Registry under `io.github.ensamblaTec/neoanvil` — keep an eye on registry.modelcontextprotocol.io)
