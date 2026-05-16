# Anthropic Community Forum post

**URL:** [community.anthropic.com](https://community.anthropic.com/)
**Category:** Show & Tell (or `claude-code` tag if posting in the dev section)

---

## Title

**NeoAnvil — MCP server that auto-enforces a SRE workflow (BRIEFING → BLAST_RADIUS → Edit → certify) in every Claude Code session**

---

## Body

Hi all — long-time Claude Code user, sharing a project that started as my own scratch-itch and turned into something that's saved me a lot of token spend.

### The problem

When Claude Code edits non-trivial Go/TS code, two failure modes recur:

1. **Missed callers.** Claude refactors a function signature, finds the immediate caller, misses the 4 transitive ones. You discover during `go build`. The fix-it round-trip costs more than the refactor.
2. **Silent context drift.** Across a session, Claude loses track of which workspace state it's operating on — what's already shipped, what's pending cert, what's blocked. You spend tokens re-explaining.

### What NeoAnvil does

It's a pure-Go MCP server that wraps every Claude Code session in a disciplined loop, enforced by `.claude/hooks/*.sh` that register into Claude Code's native lifecycle events:

```
SessionStart  → BRIEFING auto-injected (workspace state, RAG coverage, open tasks)
UserPromptSubmit → DS pre-mortem hint for multi-file / hot-path prompts
PreToolUse(Edit) → BLAST_RADIUS (find all callers, transitive deps)
                 → AST_AUDIT hard gate for transactional code
PostToolUse(Edit) → track file in session_pending_cert.list
                  → cert pipeline (AST + shadow-compile + test-impact)
Stop → final cert-pending check
Pre-commit → rejects anything without a fresh cert seal
```

The hooks are 7 small bash scripts, ~32KB total, bash 3.2 compatible (macOS-safe). They register in `.claude/settings.json`. You can install just the hooks (without the MCP server) and get most of the discipline; the MCP server is what makes BLAST_RADIUS / AST_AUDIT / certify actually *do* something rather than just remind.

### What Claude Code feels like with it on

- Sessions open with a BRIEFING line: `Mode: pair | Phase: X | Open: 14 | Closed: 22 | RAG: 100% | CPG: 18%`. No more "what was I doing?" prompts.
- Type "refactor `pkg/foo`": the hook auto-runs BLAST_RADIUS first. Claude sees the caller graph before it edits. Less "I forgot file X" rework.
- Type "fix bug in handler": for HTTP/middleware code, the hook suggests chaos_drill. You decline or accept.
- Try to commit half-finished work: pre-commit hook lists the pending-cert files and blocks until you `neo_sre_certify_mutation` them or `NEO_CERTIFY_BYPASS=1` to override.
- Close the session: stop hook surfaces anything left pending so you don't ghost a half-shipped change.

### MCP-side capabilities

If you want more than the hooks: NeoAnvil exposes 14 MCP tools / 60+ operations to Claude as native tool calls. The most useful ones for Claude Code work:

- **`neo_radar`** — meta-tool with 23 intents. `BRIEFING`, `BLAST_RADIUS`, `SEMANTIC_CODE`, `AST_AUDIT`, `GRAPH_WALK`, `FILE_EXTRACT` (surgical symbol extraction → ~375 tokens vs ~42k for a full Read of a 200-line file), `COMPILE_AUDIT`, `TECH_DEBT_MAP`.
- **`neo_sre_certify_mutation`** — the cert pipeline.
- **`neo_memory`** — durability-hardened lessons + directives.
- **`neo_compress_context`** — token-aware context summarization for long sessions.
- **`neo_local_llm`** — local Ollama LLM for refactor/boilerplate/yes-no prompts. Routes the heavy lifting away from Claude when local is good enough.

### Setup

```bash
git clone https://github.com/ensamblaTec/neoanvil && cd neoanvil
make docker-up        # or `make build && neo setup my-workspace` for native
```

Point Claude Code's MCP config at `http://127.0.0.1:9000/mcp/sse`. The hooks auto-detect workspace from CWD.

### Pure Go, MIT

Single binary. Runs offline with Ollama for embeddings; LLM choice is yours. Cross-compiles to linux/darwin × amd64/arm64. No telemetry, no signup.

Repo: <https://github.com/ensamblaTec/neoanvil>
Migration guide for hooks-only adoption: <https://github.com/ensamblaTec/neoanvil/blob/main/docs/guide/neo-doctrine-migration-guide.md>

Would love to hear from anyone else doing protocol-layer enforcement vs in-context prompting for Claude Code. The hooks pattern feels under-explored — there's a lot of headroom for community-contributed hooks beyond what I shipped.
