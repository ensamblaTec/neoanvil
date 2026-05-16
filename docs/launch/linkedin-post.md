# LinkedIn post

LinkedIn's algorithm penalizes external links in the first hour. Post the link as a comment ~10 minutes after publishing, not in the post body.

---

## Post body

Just shipped NeoAnvil — an open-source MCP server I built to fix a problem I kept hitting with AI coding agents: they happily ship refactors that break callers they never grepped for.

The mechanic that fixed it for me is **certify-on-edit**. Every mutation an AI agent proposes (Claude Code, Cursor, GPT) goes through a transactional pipeline before it can touch a commit:

→ AST validation across Go, TypeScript, Python, Rust
→ Shadow-compile (in-memory build) so the change is proven to type-check
→ Optional test-impact narrowing — only the affected packages run
→ Time-limited certification seal (15-min in pair mode, 5-min in fast mode)
→ A pre-commit hook rejects anything without a fresh seal

The protocol-layer enforcement turned out to be the right level. In-context "please be careful" prompts don't stop the failure; AST + shadow-compile does.

NeoAnvil also ships 14 MCP tools / 60+ operations: a Code Property Graph (SSA-based call graph + PageRank), HNSW vector search with binary quantization, multi-workspace dispatcher, plugin subsystem for Jira / DeepSeek / GitHub, and lifecycle hooks for Claude Code that auto-enforce a BRIEFING → BLAST_RADIUS → Edit → certify loop.

Pure Go. Cross-compiles to linux/darwin × amd64/arm64. Runs entirely local with Ollama. MIT licensed, no telemetry, no signup.

If you've been burned by AI-generated code that shipped broken — or if you build AI dev tooling — I'd genuinely value your feedback.

#AICoding #DeveloperTools #OpenSource #Go #ModelContextProtocol

---

## First comment (post ~10 min later)

Repo: https://github.com/ensamblaTec/neoanvil

Migration guide for adopting just the lifecycle hooks into your existing project (no MCP server needed): https://github.com/ensamblaTec/neoanvil/blob/main/docs/guide/neo-doctrine-migration-guide.md
