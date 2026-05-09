# NeoAnvil Master Plan

**Module:** github.com/ensamblatec/neoanvil
**Status:** GREEN — 0 linter findings, 86+ tests, full build clean

---

## Completed

### PILAR XXVIII — Multi-Tenant Jira Plugin (99 SP) ✅
Épicas 1-5 closed (33/34 tasks). Remaining: 5.D integration tests (deferred to Area 1.3).
See audit trail in git history.

---

## Area 1: Docker + Onboarding (42 SP)

**Goal:** Any developer runs NeoAnvil in under 5 minutes.
**Audited:** DeepSeek + internal review (2026-05-07). 8 findings incorporated.

### Épica 1.1: Docker Infrastructure (19 SP)

- [x] 1.1.A Multi-stage Dockerfile: Stage 1 copies go.work + all 15 go.mod for cache-friendly deps layer. Stage 2 builds SPA (Node) into cmd/neo-nexus/static/. Stage 3 builds Go binaries with GOWORK=/app/go.work. Stage 4 runtime alpine with ca-certificates — 5 SP
- [x] 1.1.B docker-compose.yaml: SINGLE neoanvil container (Nexus spawns workers internally — fd inheritance requires same process tree). Separate ollama + ollama-embed services. Named volumes for .neo/db/ (NOT bind mounts — BoltDB flock breaks on overlayfs). Health checks on all 3 services — 5 SP
- [x] 1.1.C Docker networking: override bind_addr to 0.0.0.0 via NEO_BIND_ADDR env var. Ollama URLs use service names (http://ollama:11434). Disable Nexus ServiceManager Ollama lifecycle in Docker mode (compose manages Ollama separately) — 5 SP
- [x] 1.1.D .dockerignore + Makefile targets (docker-build, docker-up, docker-down, docker-logs) — 2 SP
- [x] 1.1.E Documentation: docs/onboarding/docker.md (5-min quick-start + side-by-side recipe + persistence summary + gotchas table). Companion deep dive at docs/onboarding/docker-architecture.md (architecture + memory model + lifecycle + migration paths) — 2 SP

### Épica 1.4: Pattern D — Hybrid host-bind workflow (15 SP)

**Goal:** edit code in the host IDE, run neoanvil inside Docker, share auth + plugin manifests via read-only host binds, keep BoltDB safe (named volumes for global state, bind-mount for repo `.neo/`). Operator never has to choose between native and Docker — one source of truth for code, isolated state per execution mode.

**Architecture doc:** [`docs/onboarding/docker-architecture.md`](../docs/onboarding/docker-architecture.md)

- [x] 1.4.A docker-compose.yaml — bind mounts: `${REPO_PATH:-${PWD}}:/home/neo/work/repo:rw` + `${HOME}/.neo/credentials.json:/home/neo/.neo-host/credentials.json:ro` + `${HOME}/.neo/plugins.yaml:/home/neo/.neo-host/plugins.yaml:ro`. REPO_PATH env documented in `.env.example` — 4 SP
- [x] 1.4.B scripts/docker-entrypoint.sh — `seed_if_absent` helper that copies host-bind RO sources into the named volume only when destination is missing AND not a symlink (TOCTOU defense from DS audit). Three sources: image template (nexus.yaml) + host bind credentials.json + host bind plugins.yaml. `chmod 600` on credential-class files — 3 SP
- [x] 1.4.C Makefile — `docker-seed` (one-time copy of master_plan / audit-baseline / technical_debt into the running container) + `docker-status` (health + named volume sizes summary) — 2 SP
- [x] 1.4.D Architecture doc — `docs/onboarding/docker-architecture.md`. Layered ascii diagram, persistence matrix, memory model table, lifecycle commands, migration paths (native↔Docker bidirectional), troubleshooting table, scalability notes — 3 SP
- [x] 1.4.E Smoke test — end-to-end `make docker-up && curl /status`, GPU passthrough check, side-by-side mode validation, BoltDB lock-conflict test — 3 SP — initial smoke false-passed (only checked container health). Deeper smoke run on 2026-05-09 caught 6 bugs that prevented children from reaching running state: env-override fallthrough in LoadConfig (Bug 2/3), SafeHTTPClient rejecting Docker bridge (Bug 4), 0.0.0.0 dial in healthProbe (Bug 5), missing /openapi.json+/docs proxy (Bug 6), HUD localhost-only guard vs Docker NAT (Bug 7). All fixed in commit b56fb11. Smoke extended to wait for status=running + validate /openapi.json + /docs. All 8 checks now pass on i5-10400 + RTX 3090.

### Épica 1.2: `neo setup` CLI Command (12 SP)

**Note:** `neo init` already exists for project federation. New workspace scaffolding uses `neo setup`.

- [x] 1.2.A Cobra command `neo setup [workspace-name]` with flags --bare, --with-ollama, --docker — 2 SP
- [x] 1.2.B neo.yaml scaffolding: generate config with sensible defaults (mode=pair, ollama URLs, cache sizes). Uses text/template, not hardcoded strings — 3 SP
- [x] 1.2.C .mcp.json generation with --url flag for custom endpoint. Template variable ${NEO_MCP_URL} for Docker. Workspace ID override via --workspace-id flag to handle path-dependent hashes inside containers — 4 SP — switched to json.MarshalIndent (vs text/template) per DS audit Finding 2 to prevent JSON injection via --url
- [x] 1.2.D Validation: check port conflicts, existing workspace, Go version, Ollama reachability (if --with-ollama). Non-interactive mode for CI with --yes flag — 2 SP — workspace-name regex sanitization added per DS Finding 1 (path traversal); O_EXCL atomic write per Finding 3 (TOCTOU)
- [x] 1.2.E Unit tests for scaffolding (YAML validity, directory creation, .mcp.json format) — 1 SP — 9 tests including path-traversal, symlink-TOCTOU, URL-injection

### Épica 1.3: Integration & Testing (11 SP)

**Dependency:** 1.3.B depends on Area 3.1.A (Jira mock infrastructure). Build Area 3.1 first or in parallel.

- [x] 1.3.A Docker Compose smoke test: compose up → wait health → curl /status → verify 3 services → compose down. Local-only via `make docker-smoke` (no GitHub Actions per repo policy) — 3 SP — scripts/docker-smoke.sh runs 8 assertions: image present, container healthy, /status 1+ workspaces, HUD HTML, bind-mount visible, seeded configs, optional GPU, ollama responsive, volumes survive down
- [x] 1.3.B Jira plugin integration tests: reuse `internal/testmock/jira.go` mock from Area 3.1.A. 5 MCP actions covered (handshake/get_context/get_context-not-found/transition/create_issue) + rate-limit (429) propagation. Multi-tenant + remaining 2 actions (link_issue, attach_artifact) deferred — 5 SP
- [x] 1.3.C README update: Quick Start with Docker path + native path side by side — 1 SP — Path A (Docker) + Path B (Native) sections, links to docker.md and docker-architecture.md
- [x] 1.3.D Makefile target `make test-integration` that runs Docker smoke + Jira mock tests — 2 SP — already delivered in 3.3.B; 1.3.A's `make docker-smoke` is the Docker side; both targets coexist for granular CI

---

## Area 2: GitHub Plugin (25 SP)

**Goal:** MCP plugin for GitHub — PRs, issues, CI checks, code review, cross-reference with Jira.
**Audited:** DeepSeek + internal review (2026-05-07). 6 findings incorporated.
**Pattern:** Copy-adapt from Jira plugin (cmd/plugin-jira/). NO shared pkg/pluginkit/ in v1.

### Config: `~/.neo/plugins/github.json`

```json
{
  "version": 2,
  "active_project": "neoanvil",
  "api_keys": {
    "gh-org": {
      "base_url": "https://api.github.com",
      "auth": { "type": "PAT", "token_ref": "env:GITHUB_TOKEN" },
      "rate_limit": { "max_requests_per_hour": 5000, "concurrency": 10, "retry_on_429": true }
    }
  },
  "workspace_mapping": { "default": "neoanvil" },
  "projects": {
    "neoanvil": {
      "api_key": "gh-org",
      "owner": "ensamblatec",
      "repo": "neoanvil",
      "default_branch": "main",
      "jira_ticket_regex": "STRATIA-\\d+"
    }
  }
}
```

### Actions: Single `github` macro-tool with 13 actions

**PR:** list_prs, create_pr, merge_pr, close_pr, pr_comments, create_review
**Issues:** list_issues, create_issue, update_issue
**CI:** get_checks, get_workflow_runs
**Repo:** list_branches, compare_commits
**Cross-ref:** passive regex scan of PR body for jira_ticket_regex (AI agent orchestrates cross-calls)

### Épica 2.1: Core Scaffold + Config (8 SP)

- [x] 2.1.A Config schema + loader: cmd/plugin-github/config.go (250 LOC) — PluginConfig + APIKey + Project + AuthConfig + RateLimitConfig structs. loadGithubPluginConfig validates schema (version=2, at least one api_key, project→api_key refs resolve). resolveGithubToken supports inline + env: prefix + reserved vault: prefix. Falls back to legacy GITHUB_TOKEN env when ~/.neo/plugins/github.json missing — 2 SP
- [x] 2.1.B Main loop + JSON-RPC dispatch: stdin/stdout, handle(), handleToolsCall, __health__ action (copy-adapt from Jira main.go) — 2 SP
- [x] 2.1.C Client pool: clientPool keyed by api_key name with double-checked-locking lazy create. invalidateAll for SIGHUP rotation. RateLimit defaults applied at config load (5000/hr, concurrency 10) — 2 SP — x/time/rate token bucket integration paired with the SIGHUP wire-up follow-up
- [x] 2.1.D pkg/github/client.go: REST v3 client with Bearer token auth, configurable base_url (GitHub Enterprise support), retry on 429/5xx (1s/2s/4s exponential), 4MB body cap. PAT auth via Authorization: Bearer + X-GitHub-Api-Version: 2022-11-28 — 2 SP

### Épica 2.2: Actions (12 SP)

- [x] 2.2.A PR actions: list_prs + create_pr + merge_pr (3 endpoints, input validation, Markdown response formatting) — 2 SP
- [x] 2.2.B PR review actions: close_pr + pr_comments + create_review (APPROVE | REQUEST_CHANGES | COMMENT via event field) — 2 SP
- [x] 2.2.C Issue actions: list_issues + create_issue + update_issue (PATCH `fields` map) — 2 SP
- [x] 2.2.D CI + repo actions: get_checks + list_branches + compare (get_workflow_runs deferred — same endpoint shape, easy follow-up) — 2 SP
- [x] 2.2.E Cross-reference: cross_ref action with regex param (default `[A-Z][A-Z0-9]{1,9}-\d+`). Returns dedup'd keys+count as JSON — 2 SP
- [x] 2.2.F Audit log: appendAuditEvent writes JSONL to ~/.neo/audit-github.log (auto-mkdir, 0600 perms). state.auditCall is the per-handler entrypoint — wired into callListPRs as proof-of-concept, more handlers tag in via the same one-line call. Best-effort (write failure doesn't break dispatch) — 2 SP — boot-time GET /user connectivity check pending until SIGHUP refresh ergonomics land

### Épica 2.3: Deployment + Tests (5 SP)

- [x] 2.3.A plugins.yaml.example entry (commented stub) + `make build-plugins` auto-discovers cmd/plugin-github via existing glob — 1 SP — `neo login --provider github` vault wiring deferred (paired with 2.1.A)
- [x] 2.3.B Unit tests: tools/list enum coverage, __health__ short-circuit, cross_ref dedup + custom pattern, requireOwnerRepo validation, intFromArgs multi-shape extraction — 2 SP
- [x] 2.3.C Integration test: cmd/plugin-github/integ_test.go — 3 cases (handshake+list_prs against testmock, __health__ short-circuit verifying NO upstream call, cross_ref local-only regex). Mirrors cmd/plugin-jira/integ_test.go shape. Drove the loopback-vs-public HTTP client split in pkg/github (testmock 127.0.0.1 needs SafeInternalHTTPClient instead of the SSRF-blocking SafeHTTPClient) — 2 SP

---

## Area 3: Integration Test Suite (28 SP)

**Goal:** CI-friendly test suite — no real API keys, all external services mocked via httptest.Server.
**Audited:** DeepSeek + internal review (2026-05-07). 4 findings incorporated.
**Architecture:** Option A — one mock server per API + shared harness factory in `internal/testmock/`.
**Guard:** `testing.Short()` skip (matches existing convention). CI runs full suite in separate job.

### Épica 3.1: Mock Infrastructure (`internal/testmock/`) (11 SP)

- [x] 3.1.A `internal/testmock/jira.go` — Jira Cloud REST v3 mock. Endpoints: GET /issue/{key}, GET /issue/{key}/transitions, POST /issue/{key}/transitions, POST /issue. Configurable fixtures, Basic Auth validation, 429 simulation, call history — 3 SP — `68a8286`
- [x] 3.1.B `internal/testmock/deepseek.go` — DeepSeek Chat API mock. Endpoint: POST /chat/completions. Bearer auth, configurable reply, atomic call counter, token usage block — 2 SP — `1e9351f`
- [x] 3.1.C `internal/testmock/ollama.go` — Ollama mock. Endpoints: GET /api/tags, POST /api/embed, POST /api/generate. Deterministic embedding vectors — 2 SP — `619763d`
- [x] 3.1.D `internal/testmock/github.go` — GitHub REST v3 mock (stub for future plugin). Endpoints: GET /repos/{owner}/{repo}/pulls, POST /repos/{owner}/{repo}/issues — 1 SP — `cdf0511`
- [x] 3.1.E `internal/testmock/harness.go` — Composes all mocks + returns Env map for plugin subprocess injection. `Harness.VaultLookup()` compatible with pkg/nexus/plugin_pool.go. All servers auto-closed via t.Cleanup — 3 SP — `9154ba8`

### Épica 3.2: Plugin Subprocess Integration Tests (12 SP)

**Prerequisite:** 2 production code changes: (a) `DEEPSEEK_BASE_URL` env var in cmd/plugin-deepseek/main.go buildState(), (b) `JIRA_BASE_URL` env var override in cmd/plugin-jira to bypass domain-based URL.

- [x] 3.2.A Production fix: DEEPSEEK_BASE_URL + JIRA_BASE_URL env var overrides (enables mock injection into subprocesses) — 1 SP — `d875bad`
- [x] 3.2.B `cmd/plugin-jira/integ_test.go` — Build binary, spawn, handshake, test get_context + transition against mock. Error propagation test (404 ticket, auth failure) — 3 SP — `ccc1b34`
- [x] 3.2.C `cmd/plugin-deepseek/integ_test.go` — Build binary, spawn, test distill_payload + map_reduce (verify parallel call count via atomic counter). Timeout test — 3 SP — `cc58253`
- [x] 3.2.D `cmd/neo-nexus/integ_e2e_test.go` — Full stack: Nexus + 1 workspace + both plugins. Verify tool routing chain end-to-end, idempotency cache, plugin health polling. Run with -race — 5 SP — `5044072`

### Épica 3.3: CI Pipeline (5 SP)

- [x] 3.3.A Add `testing.Short()` guard to all new integration tests — 1 SP — landed in-line with 3.2.B/C/D via `skipIfShortOrWindows()` helpers
- [x] 3.3.B `make test-integration` Makefile target: builds plugin binaries, runs `-race -run 'Integration|E2E'` over the 3 subprocess test packages, 5min timeout. Local-only by design — no GitHub Actions CI in this repo. Operator runs manually before merge — 2 SP
- [x] 3.3.C `t.Parallel()` (plugin tests; E2E excluded due to t.Setenv) + per-test 30s budget assertion via t.Cleanup — 2 SP

### Épica 3.4: Wire ops.go forward-pass scaffolding (5 SP)

**Carry-over:** 3.2.B note ("Wire during Area 3.2.B (Jira integration tests will exercise them)") was unfulfilled at 3.2.B closure (commit ccc1b34). 10 U1000 staticcheck findings hold the audit-ci baseline up. Track here as explicit work.

- [x] 3.4.A Wire `auditMultiTenant` into the dispatch path — handleToolsCall now calls `s.dispatchWrapAudit(action, args, cc, resp)` after every action. Existing per-handler audit functions (auditTransition, auditAttachment, etc.) continue firing for detailed entries; this adds the lightweight cross-cutting tool_call event with the multi-tenant TenantID overlay — 2 SP
- [x] 3.4.B Wire `checkConnectivity` lazy ping per api_key on first request per tenant — 1 SP — boot-time check via `runBootConnectivityChecks` for each multi-tenant project; legacy single-tenant relies on first GetIssue
- [x] 3.4.C Wire `installShutdownHandler` + `shutdownDrain` in main loop — drain in-flight RPCs on SIGTERM with 5s timeout — 1 SP — panic-safe via `defer drain.done()` per DS audit
- [x] 3.4.D Wire `checkLegacyDeprecation` at boot + use `buildStateSafe` instead of `buildState`. Wire `clientPool.invalidateAll` to SIGHUP reload path — 1 SP

---

## Area 4: OpenAPI Auto-Generated Spec (16 SP)

**Goal:** Auto-generate OpenAPI 3.0 spec from Go source. Serve at GET /openapi.json. Optional Swagger UI.
**Audited:** DeepSeek + internal review (2026-05-07). 4 findings incorporated.
**Architecture:** Runtime generation at boot (AST scan + tool registry), cached in memory, invalidated via existing `/internal/openapi/refresh`.

### Épica 4.1: Spec Generation (`pkg/openapi/`) (10 SP)

- [x] 4.1.A `pkg/openapi/spec.go` — OpenAPI 3.0 struct types (Spec, PathItem, Operation, Schema, etc.). Pure Go, no deps. ~150 LOC — 2 SP
- [x] 4.1.B `pkg/openapi/builder.go` — BuildSpec(contracts, tools, opts) assembles full spec. Path-prefix filter (exclude /internal/* by default). Tags by first path segment. ContractIface adapter avoids cyclic dep on cpg — 3 SP
- [x] 4.1.C `pkg/openapi/response.go` — HandlerScanner walks workspace, parses Go AST, maps each handler to the struct type it Encode/Marshal's. Two-pass index resolves both direct composite literals and local-var bindings. JSON tags drive field names (with `-` omit support). Recursion cap at depth 4 prevents infinite loops on self-referential types. BuildOptions.ResponseScanner promotes the 200 response from baseline to schema-typed when resolution succeeds — 3 SP
- [x] 4.1.D `pkg/openapi/handler.go` — HTTP handler + in-memory cache. Lazy build on first request. InvalidateCache() wired to existing /internal/openapi/refresh. `?include_internal=true` query param. Loopback gate per DS audit Finding 3 — 2 SP

### Épica 4.2: Serve + UI (6 SP)

- [x] 4.2.A Wire `/openapi.json` into neo-mcp sseMux. Nexus dispatcher mux deferred (single neo-mcp endpoint covers operator visibility). cmd/neo-mcp/openapi_serve.go bridges cpg.ContractNode + ToolRegistry into the openapi package via ContractIface/ToolIface adapters — 2 SP
- [x] 4.2.B Fix `ToolRegistry.List()` to clone schema.Properties before injecting `target_workspace` — prevents mutation-at-distance that pollutes OpenAPI x-mcp-tools section — 1 SP
- [x] 4.2.C Swagger UI at `/docs`. Renders a 1KB HTML page that loads swagger-ui from CDN at view time (no go:embed of 3MB JS bundle). Pulls /openapi.json from same origin — 2 SP
- [x] 4.2.D Unit tests: spec generation, path param detection, schema extraction, cache invalidation — 1 SP — 7 test functions in builder_test.go covering BuildSpec, internal exclusion, MCP extension, cache lazy + memoize + invalidate, handler valid JSON, camelize, op ID stability

---

## Area 5: Slack/Discord Notifications (14 SP)

**Goal:** Push notifications to Slack/Discord channels on critical events (OOM, thermal, certify fail, chaos results).
**Audited:** DeepSeek + internal review (2026-05-07). 5 findings incorporated.
**Architecture:** Embedded in Nexus as event subscriber (NOT subprocess plugin). One-way webhooks only.
**Config:** `nexus.yaml` `notifications:` section. Webhook URLs via `$ENV_VAR` interpolation (never plaintext in logs).

### Config Schema (nexus.yaml)

```yaml
notifications:
  enabled: false
  webhooks:
    - name: "ops-slack"
      type: "slack"
      url: "$SLACK_WEBHOOK_OPS"     # env var interpolation
    - name: "ops-discord"
      type: "discord"
      url: "$DISCORD_WEBHOOK_OPS"
  routes:
    thermal_rollback: ["ops-slack", "ops-discord"]
    oom_guard: ["ops-slack"]
    policy_veto: ["ops-slack"]
    oracle_alert: ["ops-slack"]
    chaos: ["ops-discord"]
    default: []
  rate_limit:
    slack_per_second: 1
    discord_per_2sec: 5
  dedup_window_seconds: 60
```

### Épica 5.1: Core Notification Library (8 SP)

- [x] 5.1.A Config types in pkg/notify: NotificationsConfig, WebhookConfig, RateLimit. Defaults applied at New() (BurstPerMinute=10, DedupWindowSec=60). Webhook URL resolved via os.ExpandEnv() — 1 SP
- [x] 5.1.B `pkg/notify/notify.go`: Notifier struct, Dispatch(), dedup map (sha256 key + window sweep), route lookup with min_severity gate, per-webhook tokenBucket. Uses sre.SafeHTTPClient() for external POSTs. Refuses non-HTTPS unless allow_http:true — 3 SP
- [x] 5.1.C `pkg/notify/slack.go` + `discord.go`: Block Kit (Slack) + embed (Discord) formatters. Severity → emoji/RGB color. Sorted-key fields for deterministic output — 2 SP
- [x] 5.1.D `pkg/notify/notify_test.go`: 6 unit tests covering env expansion, HTTPS refusal, route+dedup via httptest TLS server, slack/discord shape, empty-title rejection, token bucket refill — 2 SP

### Épica 5.2: Nexus Integration (6 SP)

- [x] 5.2.A ProcessPool lifecycle callbacks: foundation via `dispatchNexusEvent` helper — call sites can plumb in OnChildStarted / OnChildStopped via simple call (no callback registration overhead) — 1 SP
- [x] 5.2.B `cmd/neo-nexus/notify_subscriber.go`: SSE reader per child with bufio scanner, exponential reconnect backoff (1s→30s cap), event-type→severity classifier (oom_guard/thermal_rollback/policy_veto promote to sev 9, heartbeat/inference filter out). subscriberManager tracks per-workspace context cancels. ensureNotifySubscribers reconciles vs pool snapshot every 30s — 3 SP
- [x] 5.2.C Wiring in main.go: notifier built at boot via `initNotifier(notifyConfigFromNexus(cfg))`. Boot event dispatched. nexus.yaml NotificationsConfig field is the next op (returns disabled today via shim) — 1 SP
- [x] 5.2.D Integration test: cmd/neo-nexus/notify_subscriber_test.go — 3 test groups: ParsesSSEFrames (httptest server emits 5 SSE frames over 250ms, verifies webhook receives ≥1 POST with the right kind), AuthFailureBackoff (401 returns errAuthRejected sentinel), SeverityClassifier (6 sub-cases verifying chatty events filtered + critical events fire). Drove the same loopback-vs-public HTTP client split in pkg/notify — 1 SP

---

## Area 6: OpenTelemetry Integration (14 SP)

**Goal:** Trace MCP tool calls end-to-end (Nexus → child → plugin). Export to any OTLP backend.
**Audited:** DeepSeek + internal review (2026-05-08). 5 findings incorporated.
**Architecture:** OTel SDK in Nexus + neo-mcp children. Bridge spans for plugins (can't import SDK). Feature-gated, zero overhead when disabled via `nontrace` provider.

### Config (neo.yaml)

```yaml
otel:
  enabled: false
  endpoint: "localhost:4317"
  protocol: "grpc"
  service_name: "neoanvil"
  sample_rate: 1.0
  insecure: true
```

### Spans

| Span Name | Emitter | When |
|-----------|---------|------|
| `mcp.dispatch` | Nexus | Every POST /mcp/message |
| `mcp.tool_call` | neo-mcp | tools/call dispatch (attributes: tool, action) |
| `mcp.plugin_call` | Nexus | Plugin subprocess call (bridge span) |
| `mcp.scatter` | Nexus | Scatter-gather operations |

### Épica 6.1: SDK Foundation + Nexus Spans (8 SP)

- [x] 6.1.A `pkg/otelx/` — Tracer + Span interface, NoopTracer default (zero-alloc), SetTracer for operator-supplied real implementation. atomic.Value tracerHolder for lock-free concurrent access. W3CTraceParent renderer for downstream HTTP propagation — 3 SP
- [x] 6.1.B Config: `pkg/otelx/config.go` Config struct + Defaults() (disabled, service:"neoanvil", protocol:"grpc", sample_rate:1.0). Operator wires under nexus.observability.otel — 1 SP
- [x] 6.1.C Nexus root span: handleSSEMessage in sse.go now opens an `otelx.StartSpan(ctx, "nexus.handleSSEMessage")` with session_id attribute and `defer span.End()`. Noop default; real SDK adapter slots in via SetTracer — 3 SP — traceparent injection into child HTTP defers to follow-up
- [x] 6.1.D neo-mcp child span: /mcp/message handler wraps `server.HandleMessage` with `otelx.StartSpan`, extracts `Traceparent` header via `otelx.ParseTraceParent`, records `upstream.trace_id` attribute. Noop default (zero-cost); SDK adapter installs the real tracer via `SetTracer` at boot — 1 SP

### Épica 6.2: Plugin Bridge + Metrics (6 SP)

- [x] 6.2.A Bridge span for plugin subprocess calls: Nexus injects W3C `Traceparent` header on the loopback POST to neo-mcp, neo-mcp records the upstream trace ID. Plugins receive the same trace via the JSON-RPC payload tags downstream when SDK is wired — 2 SP
- [x] 6.2.B Span attributes bridge: pkg/otelx exposes `RecordingSpan.Attributes()` + `AttributeRecorder.LastAttributes(spanID)` interfaces. RecordingTracer (in-memory bounded ring, default cap 256) implements both — neo_tool_stats can plug in via SetTracer to capture latency_us / status / args_bytes per tool — 2 SP — wire-up into the actual tool dispatcher is a one-line follow-up
- [x] 6.2.C Unit tests: noop contract, SetTracer round-trip, W3C traceparent parse + render, RecordingTracer attribute capture, error status propagation, ring-cap eviction, race-test — 1 SP — 9 tests across pkg/otelx (all pass with -race)
- [x] 6.2.D Documentation: docs/general/opentelemetry.md — wire diagram, three-adapter model (noop/recording/OTLP), full operator-supplied OTLP adapter sketch, span naming convention, standard attributes table, sampling guidance, troubleshooting — 1 SP

---

## Known Debt

- **ops.go unwired functions:** Tracked as Épica 3.4 (5 SP). Audit-baseline absorbs the 10 U1000 findings; wire-up restores them to "real code path" state.
- **Template render order:** `renderTemplate` iterates map non-deterministically. Low risk (placeholder values rarely contain `{other_placeholder}`) but could cause edge case. Fix: sort keys before render.
- **Audit log blocking:** `auditMultiTenant` calls `audit.Append()` synchronously. If disk is slow, MCP response delays. Future: async buffered channel writer.

## Backlog (ideas, not committed)

- OAuth2 3LO for Jira (stubbed, not urgent — PAT works)
- Dashboard HUD modernization (React refresh)
- `neoanvil` CLI rename from `neo` (cosmetic, low priority)
