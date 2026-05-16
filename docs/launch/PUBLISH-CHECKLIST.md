# NeoAnvil — Launch Publish Checklist

One-page master with every URL, command, and copy-paste destination needed to take NeoAnvil public.

Generated 2026-05-16. All drafts live next to this file in `docs/launch/`.

---

## 0. Pre-flight (already done locally)

- [x] Personal paths sanitized in 2 public docs
- [x] Secrets audit clean (no API keys, JWTs, env values committed)
- [x] `feature/adaptive-briefing-diff` pushed to origin
- [x] `feature/adaptive-briefing-diff` merged into `develop` locally (12 commits ahead, **not pushed yet**)
- [x] `gh` CLI installed (`/opt/homebrew/bin/gh`, version verified)

Local state: on branch `develop`, working tree clean, **nothing on remote yet for develop**.

---

## 1. GitHub — push + open PRs

### 1.1 Authenticate `gh` (one-time, interactive)

```bash
! gh auth login
# Choose: GitHub.com → SSH → existing key → browser auth
# Account: ensamblaTec  (email net.git.ketza@gmail.com)
```

### 1.2 Push develop (brings the 12 merged commits to origin)

```bash
git push origin develop
```

### 1.3 Open PR: `develop` → `main`

**Option A — gh CLI (one command):**

```bash
gh pr create \
  --base main \
  --head develop \
  --title "Release: B1 Adaptive Runtime + launch-prep" \
  --body-file docs/launch/PR-DEVELOP-TO-MAIN.md
```

**Option B — browser:**

[https://github.com/ensamblaTec/neoanvil/compare/main...develop](https://github.com/ensamblaTec/neoanvil/compare/main...develop)

Body draft: [`PR-DEVELOP-TO-MAIN.md`](./PR-DEVELOP-TO-MAIN.md)

### 1.4 Merge PR after CI green

```bash
gh pr merge --merge   # or --squash / --rebase per your preference
git checkout main && git pull origin main
```

---

## 2. MCP Registry (official) — `registry.modelcontextprotocol.io`

**Note:** NOT a PR. It's a CLI publishing flow using a GitHub-namespace `server.json`.

### 2.1 Install publisher CLI

```bash
brew install mcp-publisher
# verify:
mcp-publisher --help
```

### 2.2 Place `server.json` at repo root

The manifest is drafted at [`server.json`](./server.json). Copy it to repo root:

```bash
cp docs/launch/server.json ./server.json
git add server.json && git commit -m "feat(launch): MCP Registry manifest (server.json)"
git push origin main
```

### 2.3 Login + publish

```bash
mcp-publisher login github
# Visit URL, paste code in browser, authorize the registry app on
# the ensamblaTec GitHub account.

mcp-publisher publish
# Reads ./server.json, validates schema, publishes under
# namespace io.github.ensamblaTec/neoanvil
```

### 2.4 Verify

[https://registry.modelcontextprotocol.io/v0/servers/io.github.ensamblaTec/neoanvil](https://registry.modelcontextprotocol.io/v0/servers/io.github.ensamblaTec/neoanvil)

---

## 3. awesome-mcp-servers — `punkpeye/awesome-mcp-servers` (87k stars)

Fork + PR. Entry drafted at [`awesome-mcp-entry.md`](./awesome-mcp-entry.md).

### 3.1 Fork + clone

```bash
gh repo fork punkpeye/awesome-mcp-servers --clone --remote
cd awesome-mcp-servers
git checkout -b add-neoanvil
```

### 3.2 Add entry under `## 🔗 Aggregators` (alphabetical)

Open `README.md`, find the `## 🔗 Aggregators` section, insert this line in alphabetical order:

```markdown
- [ensamblaTec/neoanvil](https://github.com/ensamblaTec/neoanvil) 🏎️ 🏠 🍎 🪟 🐧 - MCP server & SRE orchestrator for AI coding. Multi-workspace dispatcher (Neo-Nexus), Code Property Graph + HNSW RAG, lifecycle hooks (BRIEFING → BLAST_RADIUS → certify), 14 MCP tools, plugins for Jira/DeepSeek/GitHub.
```

### 3.3 Commit + push fork + PR

```bash
git add README.md
git commit -m "Add ensamblaTec/neoanvil under Aggregators"
git push -u origin add-neoanvil
gh pr create --base main --head ensamblaTec:add-neoanvil \
  --title "Add ensamblaTec/neoanvil (Aggregators)" \
  --body "MCP server & SRE orchestrator for AI coding. Multi-workspace dispatcher, code-graph + RAG, lifecycle hooks, 14 MCP tools. Pure Go, MIT licensed.

Repo: https://github.com/ensamblaTec/neoanvil"
```

---

## 4. Community forums — paste drafts manually

I cannot post to these. Each draft is ready in `docs/launch/`. Login on each platform and paste.

| Platform | URL | Draft file |
|---|---|---|
| **r/LocalLLaMA** | [reddit.com/r/LocalLLaMA/submit](https://www.reddit.com/r/LocalLLaMA/submit) | [`reddit-localllama.md`](./reddit-localllama.md) |
| **r/ClaudeAI** | [reddit.com/r/ClaudeAI/submit](https://www.reddit.com/r/ClaudeAI/submit) | [`reddit-claudeai.md`](./reddit-claudeai.md) |
| **r/mcp** | [reddit.com/r/mcp/submit](https://www.reddit.com/r/mcp/submit) | [`reddit-mcp.md`](./reddit-mcp.md) |
| **r/golang** (optional) | [reddit.com/r/golang/submit](https://www.reddit.com/r/golang/submit) | [`reddit-golang.md`](./reddit-golang.md) |
| **Hacker News (Show HN)** | [news.ycombinator.com/submit](https://news.ycombinator.com/submit) | [`hn-show.md`](./hn-show.md) |
| **Anthropic Community Forum** | [community.anthropic.com](https://community.anthropic.com/) | [`anthropic-forum.md`](./anthropic-forum.md) |
| **MCP Discord** | [discord.gg/mcp](https://discord.gg/modelcontextprotocol) | [`discord-snippet.md`](./discord-snippet.md) |
| **X/Twitter** | [twitter.com/compose](https://twitter.com/compose/post) | [`twitter-thread.md`](./twitter-thread.md) |
| **LinkedIn** (optional) | linkedin.com/post | [`linkedin-post.md`](./linkedin-post.md) |
| **Dev.to** (optional) | [dev.to/new](https://dev.to/new) | [`devto-article.md`](./devto-article.md) |

### Secondary directories (lower priority, also community)

| Directory | URL | Notes |
|---|---|---|
| **MCPServers.com** | [mcpservers.com/submit](https://mcpservers.com/submit) | Form-based submission |
| **Smithery** | [smithery.ai](https://smithery.ai/) | Auto-indexes from awesome-mcp-servers + registry |
| **Glama** | [glama.ai/mcp/servers](https://glama.ai/mcp/servers) | Auto-indexes |
| **mcp.so** | [mcp.so](https://mcp.so/) | Form-based |

Smithery + Glama typically pick up automatically once you're listed in awesome-mcp-servers OR the official registry — no manual action needed.

---

## 5. Order of operations (recommended)

1. `gh auth login` (one-time)
2. Push develop → origin
3. Open + merge PR develop→main
4. Add `server.json` to main, push
5. `mcp-publisher publish` → registers under `io.github.ensamblaTec/neoanvil`
6. Fork awesome-mcp-servers → PR (waits for review, can take days)
7. **Wait 24h** before community posts — gives Smithery/Glama time to auto-index from registry, so posts include working "list me on…" badges
8. Reddit posts on a Tuesday/Wednesday 9am-11am ET (best engagement window per r/LocalLLaMA mods)
9. HN Show on a weekday morning (Pacific) — single post, no resubmit
10. Discord + Twitter same day as Reddit

---

## 6. Anti-spam discipline

- Don't crosspost the same wording across all subs in 1 hour — Reddit's anti-spam flags it
- Each draft is **subtly customized** for its audience (LocalLLaMA → local-first emphasis; ClaudeAI → Claude integration; mcp → protocol mechanics)
- Reply to comments in first 4h — top engagement window
- Don't link-drop in unrelated subs (r/programming, etc.) — it backfires

---

## 7. Tracking (suggested)

Create a private spreadsheet:

| Platform | Posted | URL | Upvotes 24h | Notable comment |
|---|---|---|---|---|
| Registry | | | | |
| awesome-mcp-servers | | | | |
| r/LocalLLaMA | | | | |
| ... | | | | |

Helps you decide where to invest time in v2 launch.
