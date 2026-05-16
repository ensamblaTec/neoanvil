# awesome-mcp-servers — submission

## Entry (alphabetical insertion)

Category: **🔗 Aggregators** (multi-workspace + plugin dispatcher fits best).
Insert under that heading in alphabetical order. The leading `e` in `ensamblaTec` places it among `e`-prefixed entries.

```markdown
- [ensamblaTec/neoanvil](https://github.com/ensamblaTec/neoanvil) 🏎️ 🏠 🍎 🪟 🐧 - MCP server & SRE orchestrator for AI coding. Multi-workspace dispatcher (Neo-Nexus), Code Property Graph + HNSW RAG, lifecycle hooks (BRIEFING → BLAST_RADIUS → certify), 14 MCP tools, plugins for Jira/DeepSeek/GitHub.
```

Legend symbols used:
- 🏎️ Go codebase
- 🏠 Local service (no cloud dependency)
- 🍎 🪟 🐧 Runs on macOS / Windows / Linux

## Alternative category (if Aggregators rejected)

**🤖 Coding Agents** — fits if maintainer prefers framing by the certify-on-edit pipeline rather than the dispatcher.

## Fork + PR flow (gh)

```bash
gh repo fork punkpeye/awesome-mcp-servers --clone --remote
cd awesome-mcp-servers
git checkout -b add-neoanvil

# Edit README.md — insert the entry above under "## 🔗 Aggregators"

git add README.md
git commit -m "Add ensamblaTec/neoanvil under Aggregators"
git push -u origin add-neoanvil

gh pr create --base main --head ensamblaTec:add-neoanvil \
  --title "Add ensamblaTec/neoanvil (Aggregators)" \
  --body "$(cat <<'EOF'
**NeoAnvil** — pure-Go MCP server & SRE orchestrator for AI-assisted coding.

- Multi-workspace dispatcher (Neo-Nexus) — one SSE endpoint, N workers
- Code Property Graph (SSA-based) + HNSW vector search + 3-layer cache
- Lifecycle hooks auto-enforce BRIEFING → BLAST_RADIUS → Edit → certify
- 14 MCP tools / 60+ operations; plugins for Jira, DeepSeek, GitHub
- Pure-Go native build, runs on linux/darwin × amd64/arm64
- MIT, no telemetry, no signup

Repo: https://github.com/ensamblaTec/neoanvil
EOF
)"
```

## After merge

The list is monitored by Smithery and Glama for auto-indexing — typically picked up within 48h. No further action needed.
