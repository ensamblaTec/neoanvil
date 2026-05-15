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

## Performance migrations

Measured against the running Ollama embed instance on this workstation
(`nomic-embed-text` dim=768, RTX 3090). Re-runnable any time via:

```bash
go test -tags ollama_live -v -count=1 ./pkg/rag/ -run TestBenchLive_ -timeout 5m
```

The bench file lives in `pkg/rag/embed_bench_live_test.go` and is gated
behind a build tag, so CI stays offline-clean.

### Embed pipeline scaling — sequential vs `/api/embed` (plural)

This is the underlying lever. Hot-paths below all benefit by amortising
HTTP round-trips into a single Ollama call.

| Batch | Pre (sequential) | Post (batched) | Speedup |
|------:|:----------------:|:--------------:|:-------:|
| 1     | 13 ms            | 17 ms          | 0.80×   |
| 4     | 58 ms            | 30 ms          | 1.90×   |
| 8     | 119 ms           | 46 ms          | 2.60×   |
| 16    | 239 ms           | 72 ms          | **3.32×** |
| 32    | 495 ms           | 133 ms         | **3.72×** |
| 64    | 962 ms           | 289 ms         | 3.34×   |

Batch=1 is intentionally slower — the implementation short-circuits
single-text calls to `/api/embeddings` (singular). Sweet spot is
batch=16-32 on this hardware; beyond that, Ollama's runner saturates.

### Migrated hot-paths

Each row below is one production code site that was switched from
N sequential `Embed()` calls to a single `rag.EmbedMany()` call.
Numbers are end-to-end (embed + downstream HNSW work) on this hardware.

| Site | Pattern | Pre | Post | Speedup |
|------|---------|----:|-----:|:-------:|
| `cmd/neo-mcp/macro_tools.go` post-certify hook (8 chunks) | embed → `graph.Insert` per chunk | 212 ms | 135 ms | **1.57×** |
| `cmd/neo-mcp/radar_semantic.go::embedAndInsert` (8 chunks) | embed → `InsertBatch` | 130 ms | 53 ms | **2.45×** |
| `cmd/neo-mcp/rem_cycle.go::consolidateMemexToHNSW` (25 entries) | per-entry embed → per-entry Insert | 648 ms | 382 ms | **1.70×** |
| `cmd/neo-mcp/workspace_utils.go` per-file ingest (16 chunks) | adaptive batch + retry-fallback | 240 ms | 72 ms | **3.33×** |

Notes:

- `workspace_utils.go` uses an **adaptive** strategy: try the batch
  first, and on **any** error fall back to the per-chunk retry loop with
  the existing crash/busy/transient backoff ladder. Best-case fast,
  worst-case identical to the pre-migration baseline.
- `radar_semantic.go::embedAndInsert` already used `InsertBatch` for the
  HNSW write; only the embed half changed, but the speedup is highest
  here because the loop overhead was dominated by HTTP latency.
- `rem_cycle.go` keeps a `consolidateMemexPerEntry` fallback for when
  the batch embed fails — REM consolidation never regresses.

### Cold-boot impact

The migrated `workspace_utils.go` is also the path the cold HNSW
rebuild takes when `.neo/db/hnsw.bin` is missing or schema-stale.
Before the migration this was the canonical "5-6 min" path documented
in `CLAUDE.md`. With per-file embed at 3.3× and the adaptive-batch
strategy preserving all retry semantics, the same workload now
finishes in roughly **~1.5-2 minutes** for a typical workspace. Most
boots still take the fast snapshot path (`<5s`); cold rebuild only
fires after schema bumps or when a workspace is first registered.

### Regression guard — HNSW Search latency

The migration touched zero search code paths. The numbers below are
captured fresh after each commit so we can spot accidental regressions.

| Metric | Value |
|--------|------:|
| Search median (`k=10`, corpus=200 nodes) | **4 µs** |
| Search p95                               | **4 µs** |

### Speed-First Initiative — agent tool-latency reduction (2026-05-15)

Phase 2 closed and shipped. Originally six surgical wins targeting
"agent tool latency × call frequency" — the real bottleneck for any
workspace running neo-mcp. The follow-up session on 2026-05-15 closed
the entire Phase 2 epic (`-run` narrowing wired into certify), surfaced
+ fixed three latent infrastructure bugs (SSRF IPv6 dual-stack, polyglot
RAG coverage, CPG metric mislabeling), and cleared the strategosia RAG
0% false alarm. The full plan + audit findings live in
[`.neo/master_plan.md`](./.neo/master_plan.md). The binary ships in
every workspace, so each saving propagates.

| Win | What |
|-----|------|
| **`symbolMapCache` persisted across restart** (Phase 0.D) | `cmd/neo-mcp/radar_compile.go::symbolMapCache` (already mtime-keyed) now snapshots to `.neo/db/symbol_map.snapshot.json` on shutdown and rehydrates at boot via `setupCaches`. First `COMPILE_AUDIT` after `make rebuild-restart` skips the ~50 ms `go/ast` parse and returns from the persisted map in µs |
| **Auto-warmup at boot + persisted miss-ring** (Phase 0.A) | `QueryCache`/`TextCache` snapshots now include `recent_misses` (`omitempty`); after boot, an async goroutine fires `neo_cache_warmup{from_recent:true}` on the rehydrated targets. Live: post-restart **`Tcache: 100% (6H/0M)`** vs `0%` before — the most-recently-missed BLAST_RADIUS targets are warm on the first agent call instead of paying the cold-path tax |
| **File-mtime cache gate for BLAST_RADIUS** (Phase 1 MV) | New `TextCache.PutWithMtime` + `GetWithMtimeFallback`. An entry stays valid while EITHER `graph.Gen` matches (legacy) OR the target file's `os.Stat` mtime matches. Editing `pkg/foo/x.go` bumps `graph.Gen` but no longer invalidates the BLAST_RADIUS cache for `pkg/bar/y.go` (mtime unchanged) — saves the CPG PageRank walk on every unrelated edit |
| **`testsImpactedBy` + certify response surface** (Phase 2 MV+) | Helper computes the set of `_test.go` files affected by a mutation: same-package siblings via `os.ReadDir` + cross-package transitive importers via depth-5 reverse-BFS over `GRAPH_EDGES`. Single inverted-index build per batch. Result surfaces both in the certify response (`[CERTIFY-TEST-IMPACT] N test file(s) ...`) and in the log |
| **Per-batch `go list`/`go test` dedup** (Speed-First 2026-05-15) | `certifyLocalBatch` now passes a `testedPkgs` map through `certifyOneFile → runFileChecks → runGoBouncer`. First file in pkg/foo runs `go test pkg/foo`; siblings in the same batch + pkg skip the redundant invocation (Go compiles the whole package, so file 1's run already validated cumulative state). DS pre-mortem audited — safe |
| **Phase 2.2 v1 `-run` narrowing** (gated, opt-in) | `runGoBouncer` now emits `go test -short -run "^(TestA|TestB|...)$" pkgPath` when `sre.test_impact_enabled: true`. Same-pkg impacted tests + integration build-tag escape hatch + operator allowlist (`sre.test_impact_always_run`) all unioned. Empty impacted set → safe fallback to full pkg test (logged + JSON-surfaced as `test_impact.fallback=true`). DS Finding 1 mitigation built in (no `^()$` regex possible) |
| **Per-action `tool_stats` aggregates** (Phase 0.B) | `pkg/observability/store.go::persistCall` dual-writes both `neo_radar` AND `neo_radar/<intent>` rows into `bucketToolAggregate`. `neo_tool_stats sort_by:p99` now surfaces `neo_radar/BLAST_RADIUS` p99 separately from `neo_radar/AST_AUDIT` — which intent owns the latency tail is no longer hidden by the lumped average |
| **`vector_quant: hybrid` as the new template default** (Phase 0.C) | `neo.yaml.example` flipped from `float32` → `hybrid` (ADR-014 already established recall=1.000 across 3 production workspaces with ~2× speedup at +3% RAM). `applyRAGDefaults` binary-level default unchanged so existing yaml-less workspaces aren't silently flipped on a binary upgrade |

Phase 2 close-out (2026-05-15) added 4 master_plan checkboxes:

- **2.3 — Safe fallback on graph staleness.** Empty impacted set →
  `[CERTIFY-TEST-IMPACT-FALLBACK]` log + `test_impact.fallback=true`
  JSON. "Never silently drop coverage" guarantee.
- **2.4 — Always-run escape hatch.** Test files with `//go:build
  integration` (new + legacy syntax) auto-include all their tests in
  `-run` regex. Operator allowlist via `sre.test_impact_always_run`
  unions with the dep-graph set. Both EXPAND-only; bare-token guard
  rejects `integrationdev` false positives.
- **2.5 — Surface in certify response.** JSON gains `test_impact`
  sub-object with `selected_count` + `selected_names` + `skipped_via_
  dep_graph` (narrowing fired) or `fallback: true` (graph stale).
- **2.6 — Regression tests.** `test_impact_e2e_test.go` synthesizes
  the `tool_memory.go` 1-line-change spec in a temp workspace and
  asserts `TestWithRemSleepDefaults` selected, cross-pkg leaf
  excluded, fallback path triggered when graph empty, allowlist
  rescues from fallback.

Plus four reliability + correctness fixes the close-out shook out:

- **SSRF IPv6-first dual-stack drift fixed** — `pkg/sre/ssrf.go::
  SafeOperatorHTTPClient` and `SafeHTTPClient` previously dialed
  `ips[0]` from `net.LookupIP`. On macOS, `localhost` resolves to
  `[::1, 127.0.0.1]`; Ollama binds 127.0.0.1 only, so the dial of
  `::1:11434` returned RST and Go's stdlib didn't fall through to
  IPv4. `neo_local_llm` reported 100% error rate. Helper rewritten to
  `dialFirstReachable` (iterate all resolved IPs). Plus
  defense-in-depth: `pkg/config/config.go` defaults + backfills
  normalized to `http://127.0.0.1:11434`, and runtime `neo.yaml`
  patched to match. Cross-workspace `localhost:1143[45]` audit: zero
  occurrences left in yaml/go.
- **Polyglot RAG coverage (lang-aware)** — `pkg/rag/graph.go::
  IndexCoverage` hardcoded a `.go` non-test filter. Strategosia
  (Next.js, 0 `.go` files, 897 `.ts/.tsx`, 4 GB populated
  `hnsw.bin`) reported a permanent `RAG: 0% ⚠️ low_rag_coverage` false
  alarm. New `IndexCoverageWithLang(g, workspace, dominantLang)` maps
  the workspace's `dominant_lang` to source extensions (go / js / ts /
  py / rs); briefing uses `cfg.Workspace.DominantLang`. Latent path
  filter bug fixed in passing: top-level `vendor/` /
  `node_modules/` / `.next/` are now also excluded (legacy filter
  matched nested only).
- **Polyglot project override** — `pkg/config/merge.go::applyProject
  Overrides` had `dst.Workspace.DominantLang = project.DominantLang`
  unconditional. For `strategos-project` (Go backend + Next.js
  frontend under one project), the project's `dominant_lang: go`
  silently overrode strategosia's explicit `typescript` → the
  lang-aware coverage fix still saw "go" → still 0%. Inverted to
  default-provider semantic: workspace explicit wins, project fills
  in only when workspace empty. Companion test ships in
  `pkg/config/project_test.go`.
- **CPG metric clarified as process-wide** — `pkg/cpg/manager.go::
  CurrentHeapMB` returns `runtime.MemStats.HeapAlloc` (whole-process,
  not CPG-only). Comment block now states this loudly; the
  `cpg.max_heap_mb` config tag in fact controls the
  whole-process OOM threshold. Long-term renaming or real CPG-only
  tracking deferred to a debt entry.

Reliability fixes that also rode the wave:

- **`batchMap` BoltDB-persisted** (Phase 4.B) — `cmd/neo-nexus/plugin_async.go` gained `SaveBatchMapping`/`GetBatchMapping` so async batch polls survive Nexus restart. Latent today (batch_files not in schema) but closes a sharp edge waiting for batch to go live.
- **`jira/jira` error_rate 1.0 explained** (Phase 4.C) — `docs/plugins/jira-integration-guide.md` gained a Troubleshooting section so operators seeing 100% errors on a `running` plugin diagnose the credentials path instead of reporting a code bug.

Remaining: Phase 1 callsites (SEMANTIC_CODE / GRAPH_WALK /
PROJECT_DIGEST) still need distinct invalidation primitives — explicit
follow-up epic in `.neo/master_plan.md` (1.1–1.7). End-to-end
wall-clock benchmark for Phase 2.6 deferred (env-dependent
measurement, instrumented via `[CERTIFY-TEST-IMPACT-RUN]` log).

### Local LLM tool — `neo_local_llm` (ADR-013)

15th MCP tool ships a $0/call complement to the DeepSeek plugin. Routes
prompts to the operator's existing Ollama instance running `qwen2.5-coder:7b`
on the local GPU. Routing local-vs-remote stays in the agent prompt — the
tool is just the local-side dispatch surface.

Live measurements on this workstation (RTX 3090 24GB, Ollama 11434):

| Workload | Qwen 7B (local) | DeepSeek API (estimated) |
|----------|----------------:|-------------------------:|
| Trivial prompt cold (~2 token reply, model loading) | 0.28 s | ~3-5 s + $0.001 |
| **Trivial prompt warm-cache** (~15 char reply) | **407 ms** | ~3-5 s + $0.001 |
| Realistic audit (~500 token reply, 16 tok/s sustained) | 25-32 s | ~5-15 s + $0.005 |
| Daemon mode 100 audits/night | **~$0** | $3-15 |
| Quality on 1-shot race-condition audit | found bug correctly | found bug correctly |

After the model is warm in VRAM (held by Ollama keep-alive), trivial
classification calls drop to sub-500ms — competitive with the API for
yes/no triage decisions while staying free.

Tradeoff: ~2× slower per audit cold, ~equal warm, free and offline-capable.
Default model picked for portability (4.5 GB fits any 8 GB+ GPU + 16 GB+
system RAM); `qwen2.5-coder:32b` would be higher quality but requires
64 GB+ system RAM to load via Ollama. Operators set the default once via
`cfg.AI.LocalModel` in `neo.yaml`; per-call override via `args["model"]`
still works.

Recommended routing rule (codified in ADR-013, not enforced server-side):

| Use the local model for                | Keep DeepSeek for                  |
|----------------------------------------|------------------------------------|
| Boilerplate, refactor sketches         | New crypto/auth/storage primitives |
| Mechanical fan-out (rename, migrate)   | SEV ≥ 9 security audits            |
| Daemon-mode triage / yes-no questions  | Architectural decisions             |
| Translation, summarisation             | Anything that becomes ground truth  |

### HNSW hybrid quantization — `vector_quant: hybrid` (ADR-014)

PILAR XXV/170 shipped int8/binary/hybrid HNSW search primitives years
ago, with a `cfg.RAG.VectorQuant` config field and a boot hook that
calls `populateQuantCompanion()`. During the 2026-05-10 audit we found
the **search dispatch was never wired** — the four production
`Graph.Search()` call sites always used the float32 path regardless of
the quant config. The companion arrays were getting populated at boot
but never queried.

ADR-014 ships:

1. New `Graph.SearchAuto(ctx, q, topK, cpu, quant)` dispatcher in
   `pkg/rag/hsnw.go` that routes to `SearchHybridBinary` /
   `SearchBinary` / `SearchInt8` based on quant + populated state.
2. The 4 production call sites (radar_semantic, radar_briefing, main×2)
   now use `SearchAuto` with `cfg.RAG.VectorQuant`.
3. `populateQuantCompanion` extended with the `hybrid` case (populates
   binary companion since hybrid uses binary candidate filter +
   float32 rerank).
4. Lazy re-populate (commit 338b945) — `ensureBinaryPopulated()` /
   `ensureInt8Populated()` re-run the populate when Insert post-boot
   invalidated the companion. Without this fix, the first Insert after
   boot caused silent fallback to float32 — invisible regression.

Empirical recall measurement on **3 production workspaces** (the bench
harness lives in `pkg/rag/recall_measure_live_test.go` behind the
`hnsw_live` build tag, so CI stays offline-clean):

| Workspace | Lang | Nodes | hybrid recall | hybrid lat | RAM extra |
|-----------|------|------:|:-------------:|----------:|----------:|
| neoanvil | Go (mixed) | 25,406 | **1.000** | 5 µs | 2.3 MB (3.1%) |
| code project (mid) | Go monolith | 64,939 | **1.000** | 5 µs | 6.1 MB (3.1%) |
| **code project (large)** | TypeScript | **132,866** | **1.000** | 5 µs | 12.5 MB (3.1%) |

Latency stays at ~5 µs across the 5× scale jump (25k → 132k), exactly
the O(log N) profile HNSW promises. RAM overhead holds at 3.1% across
all sizes. **Large TS code project confirmed at 132k vectors — the
"platillo fuerte" — with zero recall loss.**

Reproduce any time:

```bash
HNSW_BIN_PATH=/path/to/.neo/db/hnsw.bin \
  go test -tags hnsw_live -v ./pkg/rag/ -run TestRecall_Live -timeout 5m
```

#### Lazy re-populate cost model

When workspace ingest grows the graph post-boot, the binary companion
goes stale. `SearchAuto` detects this and re-populates inline on the
next search — paid ONCE per ingest cycle, not per query.

| Workspace | First query (cold, includes re-populate) | Warm queries |
|-----------|----------------------------------------:|-------------:|
| code project (65k LOC) | 192 ms | 20-31 ms |
| code project (133k LOC) | **525 ms** | 125-129 ms |

For 100 daemon-mode queries with 1 ingest in between:
`1 × 425 ms + 99 × 5 µs ≈ 425.5 ms total` — still beats the steady-state
even on the largest workspace.

Validation rule (codified as Directive #55 [HNSW-QUANT-WIRING]): boot
logs proving "hybrid companion populated" are NOT sufficient evidence
the dispatch works. Always verify via the runtime counter:

```bash
neo_cache stats include:["search_paths"] → must show hybrid_count > 0
```

### Investigated and rejected — CPG SSA walk parallelization

A "parallelize the per-package CPG walk for 4-8× cold-boot" hypothesis
was tested with a phase-instrumented benchmark against the production
scope (`cfg.CPG.PackagePath = "./cmd/neo-mcp"`). Result: the
sequential walk only contributes **6 ms out of a 405 ms total**
(`packages.Load` dominates at 81.6%, already parallel internally).
Parallelizing the walk yields a few ms in absolute terms. Not shipped.

The phase bench lives in `pkg/cpg/builder_phases_test.go` behind the
`cpg_phases` build tag — re-run any time to revalidate the cost model:

```bash
go test -tags cpg_phases -v ./pkg/cpg/ -run TestPhases -timeout 2m
```

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
| [neo-doctrine-migration-guide.md](./docs/guide/neo-doctrine-migration-guide.md) | **Adopt this doctrine in a new repo** — hooks, settings, durability inheritance |
| [neo-enforcement.md](./docs/onboarding/neo-enforcement.md) | Port the enforcement layer to another workspace — the `scripts/neo-onboard.sh` kit + the discoverability/enforcement/fluency model |
| [jira-integration-guide.md](./docs/plugins/jira-integration-guide.md) | Jira plugin setup and workflow |
| [deepseek-api-reference.md](./docs/plugins/deepseek-api-reference.md) | DeepSeek plugin API reference |
| [plugin-author-guide.md](./docs/plugins/plugin-author-guide.md) | Writing custom MCP plugins |
| [neo-global.md](./docs/general/neo-global.md) | Universal operational laws |
| [directives-durability.md](./docs/general/directives-durability.md) | Corruption guards + snapshot/restore (ADR-017) |
| [ADR-005](./docs/adr/ADR-005-plugin-architecture.md) | Plugin subprocess architecture |
| [ADR-013](./docs/adr/ADR-013-local-llm-tool.md) | Local LLM tool (`neo_local_llm`) |
| [ADR-014](./docs/adr/ADR-014-hnsw-hybrid-quant.md) | HNSW hybrid quantization |
| [ADR-016](./docs/adr/ADR-016-ouroboros-lifecycle-hooks.md) | Ouroboros lifecycle hooks (PreToolUse/PostToolUse/Stop) |
| [ADR-017](./docs/adr/ADR-017-directives-durability.md) | Directives durability hardening (this session) |

## Doctrine snapshot — current numbers

What the neoanvil doctrine ships, measured 2026-05-13:

| Layer | Metric |
|---|---|
| **Tools MCP** | 15 tools / 60+ operations / 23 `neo_radar` intents / 3 plugins (Jira, DeepSeek, GitHub) |
| **Lifecycle hooks** | 7 hooks (briefing + 2× PreToolUse + 2× PostToolUse + UserPromptSubmit + Stop) · ~32 KB shell / 818 LOC · bash 3.2 safe |
| **Skills** | 17 (11 auto-loaded by context / 6 task-mode invocable via `/skill-name`) |
| **Directives** | 57 / 60 capacity · 500-char/directive limit · 0 outliers · 0 tag duplicates |
| **Upfront context budget** | ~5,113 tokens (CLAUDE.md 1,015 + rules 4,098) · target was ≤20k · 4× margin |
| **Directive corruption guards** | 2-tier: absolute (disk<5 AND BoltDB>50) + relative (BoltDB≥10 AND loss>20%) |
| **Pre-destructive snapshot** | `.neo/db/directives_snapshot.json` written before every `CompactDirectives` |
| **Restore loop** | `neo_memory(action_type:restore[, snapshot_path:"..."])` — fills gaps, conservative |
| **Test coverage (durability)** | 10 tests in `pkg/rag/wal_directives_sync_test.go` |
| **ADRs active** | 11 (ADR-005 → ADR-017, excluding withdrawn/superseded) |
| **Federation tier** | workspace → project (coord) → org → nexus (singleton) |

## Adopting this doctrine in your own repo

The enforcement layer is portable, and `scripts/neo-onboard.sh <target-workspace>`
automates it:

- copies the 7 hook scripts into `<target>/.claude/hooks/`;
- merges the neo `hooks` block into the target's `.claude/settings.json`
  **non-destructively** — target-owned hooks and keys (`permissions`, `env`, …)
  are preserved, the `mcp__neoanvil__*` matchers are retargeted to the target's
  MCP server name, `NEO_WORKSPACE_ID` is injected;
- copies the curated skill set (`--no-skills` skips it when the target already
  carries the doctrine as older `.claude/rules/` files);
- seeds `.claude/neo-directives-seed.md` for the operator to curate;
- preflight-checks that the target actually has the neo MCP wired — without it
  (layer 0) the hooks and skills are inert.

`--dry-run` prints the full plan and writes nothing. The git pre-commit cert
gate is **not** handled — neo-mcp self-installs it at boot.

Why a script and not just a `CLAUDE.md` directive: a directive is a soft
request the model can skip; the hooks are harness-enforced. See
[`neo-enforcement.md`](./docs/onboarding/neo-enforcement.md) for the
discoverability/enforcement/fluency model, and
[`neo-doctrine-migration-guide.md`](./docs/guide/neo-doctrine-migration-guide.md)
for the deeper federation story.

## Contributing

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/my-feature`)
3. Follow the Ouroboros cycle: BRIEFING → BLAST_RADIUS → Edit → Certify
4. Run `make audit` before committing
5. Submit a pull request

## License

MIT License. See [LICENSE](./LICENSE) for details.
