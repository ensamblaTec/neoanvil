# r/ClaudeAI post

**Subreddit:** [r/ClaudeAI/submit](https://www.reddit.com/r/ClaudeAI/submit)
**Flair:** Coding (or Tools | Showcase)
**Best time:** Tue/Wed 9–11am ET

---

## Title (pick one)

1. **NeoAnvil — an MCP server that auto-enforces BRIEFING → BLAST_RADIUS → Edit → certify in every Claude Code session.** *(recommended)*
2. Built an MCP server that hooks into Claude Code's lifecycle so it can't ship uncertified code edits.
3. Stopping Claude from shipping broken refactors: a pure-Go MCP server with certify-on-edit + lifecycle hooks.

---

## Body

If you use Claude Code on a real codebase, you've probably hit this: Claude is confident, fast, and occasionally ships a refactor that breaks 5 callers it never grepped. The fix-it dance burns more tokens than the original task.

**NeoAnvil** is a pure-Go MCP server I built that sits between Claude Code and the codebase and enforces a disciplined SRE loop on every session.

### The loop

```
SessionStart  → BRIEFING (workspace state, RAG coverage, open tasks)
PreToolUse(Edit/Write) → BLAST_RADIUS (find callers, transitive deps)
                       → AST_AUDIT (complexity, shadow vars, hot-paths)
PostToolUse(Edit) → certify pipeline (AST + shadow-compile + test-impact)
                  → time-limited cert seal (15min pair / 5min fast)
Stop → pre-commit hook rejects anything without a fresh seal
```

The hooks ship as `.claude/hooks/*.sh` — 7 hooks, ~32KB of bash, bash 3.2 compatible. They register in `.claude/settings.json` and run inside Claude Code's native lifecycle (`SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop`).

### What you actually feel as a Claude Code user

- Open a session — BRIEFING auto-injects so you don't waste tokens asking "what's the state?"
- Type "refactor pkg/foo" — `pre-edit-blast.sh` runs BLAST_RADIUS first, surfaces callers Claude would otherwise miss
- Claude edits — `post-edit-cert-reminder.sh` tracks the file in `session_pending_cert.list`
- Try to commit without certifying — pre-commit hook rejects with a clear message
- Close the session — `stop-cert-gate.sh` final-checks pending certs

### Beyond hooks

NeoAnvil also exposes 14 MCP tools / 60+ operations to Claude as native tool calls: a Code Property Graph (SSA-based, with PageRank + GRAPH_WALK BFS), HNSW vector search with binary quantization for semantic code lookup, 3-layer cache stack, multi-workspace dispatcher so you can have several repos behind one MCP endpoint, and plugins for Jira Cloud, DeepSeek, and GitHub.

### Setup

```bash
git clone https://github.com/ensamblaTec/neoanvil && cd neoanvil
make docker-up                                    # Path A — Docker
# or
make build && neo setup my-workspace             # Path B — Native
```

Point Claude Code's MCP config at `http://127.0.0.1:9000/mcp/sse`. The hooks auto-detect workspace from CWD.

Pure Go, MIT, no telemetry, no signup. Runs entirely local (Ollama for embeddings; LLM choice is yours).

Repo: <https://github.com/ensamblaTec/neoanvil>
Migration guide for the hooks alone: [`docs/guide/neo-doctrine-migration-guide.md`](https://github.com/ensamblaTec/neoanvil/blob/main/docs/guide/neo-doctrine-migration-guide.md)

Curious if anyone else is doing protocol-layer enforcement vs in-context prompts for keeping Claude honest.

---

## Comment-reply hooks

**Q: "Is this Claude-specific?"**
> The MCP server is client-agnostic — any MCP client works. The lifecycle hooks specifically target Claude Code (`.claude/hooks/`), but the same pattern adapts to Cursor or any other MCP client with a hooks surface.

**Q: "How much overhead does certify add?"**
> Typical 1-file Go edit: ~400ms. Multi-package refactor: ~25s if test-impact narrowing kicks in. There's a `fast` mode that skips Bouncer + tests (just AST + index) — ~80ms.

**Q: "Does it work with cloud LLMs?"**
> Yes. The cert pipeline is purely local (your machine validates the edits), but the LLM can be Claude/GPT/Ollama/anything that speaks MCP. The Ollama integration is for the embeddings layer (RAG), which has to be local for latency.
