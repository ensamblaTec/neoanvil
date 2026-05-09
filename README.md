<div align="center">
  <h1>NeoAnvil</h1>
  <p><strong>MCP Server & SRE Orchestrator for AI-Assisted Development</strong></p>
  <p>
    <img src="https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat-square&logo=go" alt="Go" />
    <img src="https://img.shields.io/badge/MCP-Model_Context_Protocol-8A2BE2?style=flat-square" alt="MCP" />
    <img src="https://img.shields.io/badge/Build-GREEN-3fb950?style=flat-square" alt="Status" />
    <img src="https://img.shields.io/badge/CGO-none-232F3E?style=flat-square" alt="Zero-CGO" />
    <img src="https://img.shields.io/badge/Staticcheck-0_findings-3fb950?style=flat-square" alt="Clean" />
  </p>
</div>

---

NeoAnvil is a **Model Context Protocol (MCP) server** written in pure Go that provides AI coding assistants (Claude, GPT, Ollama) with a disciplined development workflow. Every code mutation proposed by the AI goes through a transactional pipeline: AST validation, shadow-compile, test execution, and a time-limited certification seal. Uncertified changes are rejected by a pre-commit hook.

## Features

- **14 MCP tools / 60+ operations** — unified toolkit for code intelligence, mutation certification, chaos testing, memory, and caching (counted from live `GET /openapi.json::x-mcp-tools`)
- **23 radar intents** — BRIEFING, BLAST_RADIUS, SEMANTIC_CODE, AST_AUDIT, GRAPH_WALK, FILE_EXTRACT, and more
- **Multi-workspace dispatcher** (Neo-Nexus) — manages multiple workspaces behind a single SSE endpoint
- **4-tier federation** — workspace, project, org, and nexus-level configuration and knowledge stores
- **Plugin system** — subprocess MCP plugins (Jira Cloud, DeepSeek, GitHub) with per-tenant rate limiting and `__health__` zombie detection
- **OpenAPI surface** — `GET /openapi.json` returns the live MCP tool registry as OpenAPI 3.0; Swagger UI served at `/docs`
- **OpenTelemetry tracing** — W3C traceparent propagation Nexus → child neo-mcp via `X-Neo-Traceparent`; pluggable `pkg/otelx` tracer (noop by default, RecordingTracer for tests)
- **Webhook notifications** — `pkg/notify` Slack + Discord dispatcher fed by per-child SSE subscribers (allowlist-filtered events with retry+backoff)
- **Code Property Graph** — SSA-based call graph with PageRank, BFS walk, and fast-boot persistence
- **HNSW vector index** — semantic code search with embedding cache and binary quantization
- **3-layer cache stack** — QueryCache (54ns hit), TextCache (33ns hit), EmbeddingCache
- **Pure Go native build** — cross-compiles to linux/darwin × amd64/arm64 with SIMD auto-vectorization via GOAMD64/GOARM64. Docker stage 3 enables CGO for tree-sitter parsers (gcc + musl-dev)
- **Operator HUD** — real-time dashboard with SSE event bus (21 event types)

## Quick Start

NeoAnvil supports two parallel deployment paths. Pick one:

### Path A — Docker (recommended for new operators)

```bash
git clone https://github.com/ensamblatec/neoanvil && cd neoanvil

# 1. Build the multi-stage image (~3 min cold; UID/GID auto-detected
#    so bind-mounted host files stay editable from your IDE).
make docker-build

# 2. Bring up the stack (3 services: neoanvil + ollama + ollama-embed)
make docker-up

# 3. Pull the LLM + embed models on first up (one-time)
docker exec -it neoanvil-ollama       ollama pull llama3.2:3b
docker exec -it neoanvil-ollama-embed ollama pull nomic-embed-text

# 4. Verify
make docker-status
curl http://127.0.0.1:9000/status
```

The container auto-registers your repo as workspace `<basename>-<8hex>`.
Point your MCP client at `http://127.0.0.1:9000/mcp/sse`.

For side-by-side with a native install (defaults clash on
9000/8087/11434/11435), see [`docs/onboarding/docker.md`](./docs/onboarding/docker.md);
deeper architecture in [`docs/onboarding/docker-architecture.md`](./docs/onboarding/docker-architecture.md).

### Path B — Native (recommended for hot-reload development)

```bash
# Prerequisites: Go 1.26+, Ollama (optional, for embeddings)

# 1. Clone and build
git clone https://github.com/ensamblatec/neoanvil && cd neoanvil
go work sync
make build          # builds neo-mcp + neo-nexus + neo CLI

# 2. Scaffold a fresh workspace (or reuse existing one)
neo setup my-workspace      # generates neo.yaml + .mcp.json
cp .neo/.env.example .neo/.env    # add secrets here

# 3. Start the dispatcher
make rebuild-restart              # neo-nexus on :9000, workers on :91xx

# 4. Verify
curl http://127.0.0.1:9000/status
```

`neo setup` (Area 1.2) generates `neo.yaml` + `.mcp.json` with sensible
defaults; flags include `--bare`, `--with-ollama`, `--docker`, `--yes`
(non-interactive CI).

## Architecture

```
┌─────────────────────────────────────────────────┐
│  AI Assistant (Claude Code / GPT / Cursor)      │
│  Connects via SSE to Neo-Nexus                  │
└──────────────────┬──────────────────────────────┘
                   │ MCP (JSON-RPC over SSE)
┌──────────────────▼──────────────────────────────┐
│  Neo-Nexus (cmd/neo-nexus)         Port :9000   │
│  Multi-workspace dispatcher                     │
│  ┌─────────┐ ┌──────────┐ ┌──────────────────┐ │
│  │ SSE/OAuth│ │ Watchdog │ │ Plugin Pool      │ │
│  │ Proxy    │ │ Health   │ │ (Jira, DeepSeek) │ │
│  └─────────┘ └──────────┘ └──────────────────┘ │
└──┬──────────────┬──────────────┬────────────────┘
   │              │              │
┌──▼───┐    ┌─────▼────┐   ┌────▼─────┐
│neo-mcp│    │ neo-mcp  │   │ neo-mcp  │    Workers
│:9100  │    │ :9101    │   │ :9102    │    (one per workspace)
└───────┘    └──────────┘   └──────────┘
```

**Key binaries:**

| Binary | Purpose |
|--------|---------|
| `neo-mcp` | MCP worker — handles tool calls, manages RAG index, CPG, and caches |
| `neo-nexus` | Dispatcher — routes requests, manages worker pool, serves HUD dashboard |
| `neo` | CLI — workspace management, auth, config |

## Project Structure

```
cmd/
  neo-mcp/          MCP server (main worker)
  neo-nexus/        Multi-workspace dispatcher
  neo/              CLI tool
  plugin-jira/      Jira Cloud MCP plugin (multi-tenant)
  plugin-deepseek/  DeepSeek API plugin
  neo-tui/          Terminal dashboard (Bubbletea)
  sandbox/          Industrial ingestion server (mTLS)
pkg/
  rag/              HNSW vector index, embedding, cache stack
  cpg/              Code Property Graph (SSA, PageRank, BFS)
  config/           Recursive config loader (neo.yaml)
  jira/             Jira Cloud REST client
  deepseek/         DeepSeek API client
  auth/             Credential store, audit log (hash-chain)
  sre/              HTTP clients (anti-SSRF), oracle, healer
  state/            BoltDB state management, daemon, trust scoring
  workspace/        Workspace registry
  memx/             Episodic memory buffer, WAL sanitizer
  incidents/        Incident intelligence (BM25 + HNSW)
  consensus/        Multi-agent debate engine
  brain/            Portable encrypted storage (ChaCha20-Poly1305)
  pubsub/           SSE event bus
web/                React + Vite operator dashboard
.claude/            Claude Code rules, skills, hooks
```

## Configuration

NeoAnvil uses `neo.yaml` for per-workspace configuration. Secrets go in `.neo/.env` (gitignored) and are referenced as `${VAR_NAME}` in the YAML.

```yaml
# neo.yaml (minimal)
server:
  mode: pair          # pair | fast | daemon
ai:
  provider: ollama
  base_url: http://localhost:11434
rag:
  query_cache_capacity: 256
  embedding_cache_capacity: 128
cpg:
  max_heap_mb: 512
```

Full configuration reference: [docs/guide/neo-yaml-guide.md](./docs/guide/neo-yaml-guide.md)

### Multi-workspace setup (Neo-Nexus)

```yaml
# ~/.neo/nexus.yaml
dispatcher:
  bind_addr: 127.0.0.1
  port: 9000
children:
  - id: my-project
    path: /path/to/project
    lifecycle: eager   # eager | lazy
plugins:
  enabled: true
```

## MCP Tools

### Macro Tools (4)

| Tool | Operations | Purpose |
|------|-----------|---------|
| `neo_radar` | 23 intents | Code intelligence — BRIEFING, BLAST_RADIUS, SEMANTIC_CODE, AST_AUDIT, COMPILE_AUDIT, GRAPH_WALK, FILE_EXTRACT, CONTRACT_QUERY, and more |
| `neo_sre_certify_mutation` | 1 | ACID certification pipeline — AST + compile + test + seal |
| `neo_daemon` | 12 actions | Task queue, memory vacuum, cognitive stages |
| `neo_chaos_drill` | 1 | Synchronous 10-second load test |

### Specialist Tools (7)

| Tool | Purpose |
|------|---------|
| `neo_cache` | Cache observability and control (6 actions) |
| `neo_command` | Shell command staging with approval flow |
| `neo_memory` | Knowledge store + episodic memory (9 actions, 4-tier) |
| `neo_debt` | Technical debt registry (4-tier: workspace/project/org/nexus) |
| `neo_compress_context` | Context window management |
| `neo_tool_stats` | Per-tool latency percentiles (p50/p95/p99) |
| `neo_log_analyzer` | Log analysis with HNSW incident correlation |

### Plugins (3)

| Plugin | Purpose |
|--------|---------|
| `jira` | Jira Cloud integration — multi-tenant, per-project workflows, naming enforcement (7 actions) |
| `deepseek` | DeepSeek API fan-out — distill, refactor, red-team audit, boilerplate generation (4 actions) |
| `github` | GitHub integration — PRs, issues, files, commits, repos, search, branches, releases (11 actions, multi-tenant) |

## Operating Modes

| Mode | Editing | Certification | `neo_daemon` | Seal TTL |
|------|---------|--------------|-------------|----------|
| **pair** | Native (Edit/Write) | Full (AST + bouncer + tests) | Disabled | 15 min |
| **fast** | Native | AST only (no tests) | Disabled | 5 min |
| **daemon** | Via neo_daemon | Full + strict | Enabled | 5 min |

## The Ouroboros Cycle

Every code change follows a mandatory workflow:

```
BRIEFING → BLAST_RADIUS → Edit → neo_sre_certify_mutation → (optional) neo_chaos_drill
```

1. **BRIEFING** — Sync with the orchestrator (open tasks, RAM, IO, CPG status)
2. **BLAST_RADIUS** — Analyze impact before editing (callers, dependents, PageRank)
3. **Edit** — Make changes using native tools
4. **Certify** — AST validation + shadow compile + tests + seal
5. **Chaos** — Optional load test for critical paths

The pre-commit hook rejects any `.go/.ts/.tsx/.js/.css` file without a valid certification seal.

## Building

```bash
make build          # Default: GOAMD64=v3 (AVX2) or GOARM64=v8.2
make build-fast     # GOAMD64=v4 (AVX-512)
make build-generic  # GOAMD64=v1 (portable, no SIMD)
make build-all      # Cross-compile matrix (linux+darwin x amd64+arm64)
make build-tui      # Terminal dashboard

make test           # go test ./...
make audit          # staticcheck + ineffassign + modernize + coverage
make audit-ci       # Fail-on-new vs .neo/audit-baseline.txt
```

## Security Model

- HTTP clients use `sre.SafeHTTPClient()` (anti-SSRF) for external URLs
- Internal traffic uses `sre.SafeInternalHTTPClient()` (loopback-only)
- Dashboard restricted to `127.0.0.1`
- Credentials stored in `~/.neo/credentials.json` (0600 permissions)
- Plugin auth via Personal Access Tokens (OAuth2 interface stubbed for future)
- Audit log with cryptographic hash chain (`~/.neo/audit-jira.log`)
- Pre-commit hook enforces certification seals with TTL

## Dependencies

Core dependencies (no CGO required):

- `go.etcd.io/bbolt` — BoltDB for state persistence
- `golang.org/x/time/rate` — Per-tenant rate limiting
- `github.com/fsnotify/fsnotify` — Config hot-reload
- `github.com/charmbracelet/bubbletea` — TUI dashboard (optional)

Full list in `go.mod`.

## Documentation

| Document | Description |
|----------|-------------|
| [neo-yaml-guide.md](./docs/guide/neo-yaml-guide.md) | Full configuration reference |
| [neo-project-federation-guide.md](./docs/guide/neo-project-federation-guide.md) | Multi-workspace federation setup |
| [jira-integration-guide.md](./docs/plugins/jira-integration-guide.md) | Jira plugin setup and workflow |
| [deepseek-api-reference.md](./docs/plugins/deepseek-api-reference.md) | DeepSeek plugin API reference |
| [plugin-author-guide.md](./docs/plugins/plugin-author-guide.md) | Writing custom MCP plugins |
| [neo-global.md](./docs/general/neo-global.md) | Universal operational laws |
| [ADR-005](./docs/adr/ADR-005-plugin-architecture.md) | Plugin subprocess architecture |

## Contributing

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/my-feature`)
3. Follow the Ouroboros cycle: BRIEFING → BLAST_RADIUS → Edit → Certify
4. Run `make audit` before committing
5. Submit a pull request

## License

MIT License. See [LICENSE](./LICENSE) for details.
