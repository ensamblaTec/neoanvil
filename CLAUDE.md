# NeoAnvil MCP Orchestrator

Motor SRE de orquestación MCP escrito en Go. **Ouroboros V10.6 · master_plan.md vacío (todo archivado en `master_done.md`) · 14 tools MCP / 60+ operations · 3 plugins MCP (Jira, DeepSeek, GitHub) · Native build = Pure Go; Docker stage 3 = CGO enabled (gcc + musl-dev) para tree-sitter parsers**. Estado: GREEN (0 linter findings, 0 CC>15, audit-ci clean). **4 tiers** activos: `workspace` → `project` → `org` → `nexus` (global). **Áreas cerradas en sesión 2026-05-09**: Area 1 Docker + Pattern D híbrido (host bind-mount source + named volumes para state, scoped per-file binds tras DS audit Finding 1), Area 2 GitHub plugin (11 actions MCP, multi-tenant), Area 3 Integration tests (jira mock + docker smoke), Area 4 OpenAPI (`GET /openapi.json` + Swagger UI), Area 5 pkg/notify (Slack/Discord webhooks), Area 6 pkg/otelx (W3C traceparent + RecordingTracer), Phase R multi-tenant (`~/.neo/credentials.json` + audit-{jira,github}.log JSONL hash-chain), Phase S RecordingTracer + AttributeRecorder. **PILAR XXIII–XXVII** entregados previamente: Jira plugin ecosystem, DeepSeek Fan-Out Engine, Daemon V2, PILAR XXVII Daemon iterativo MCP-driven con Trust scoring + Pair feedback loop (`pkg/state/daemon_audit.go` + `daemon_trust.go` + `daemon_results.go` + `pair_audit_events.go`; 5 actions: `execute_next`, `approve`, `reject`, `trust_status`, `pair_audit_emit`). **PILAR XXVI Omni-Mesh**: Brain Portable (`pkg/brain/` — crypto ChaCha20-Poly1305, R2 store, local FS driver, TsnetStore/TsnetServer via Tailscale), Android scaffold. **Lazy lifecycle**: `lazy_prewarm_seconds` + predictive topology wake `wakeProjectSiblings` (ÉPICA 150.M/N).

> **Base universal reusable:** el ciclo operativo, las leyes de calidad y los contratos de las macro-tools están en [`CLAUDE-global.md`](./CLAUDE-global.md) y [`docs/neo-global.md`](./docs/general/neo-global.md). Este fichero solo contiene lo **específico de NeoAnvil** como proyecto.

---

## Stack del proyecto

| Componente | Detalle |
|------------|---------|
| **Build MCP** | `go build -o bin/neo-mcp ./cmd/neo-mcp` o `make build-mcp` |
| **Build CLI** | `go build -o bin/neo ./cmd/neo` |
| **Build TUI** | `make build-tui` (Bubbletea dashboard, submódulo `go.work`) |
| **Build migrate** | `make build-migrate-quant` (offline int8/binary reporter) |
| **Tests** | `go test ./...` o `go test -short ./pkg/...` |
| **Audit** | `make audit` (staticcheck + ineffassign + modernize + coverage) |
| **Audit CI** | `make audit-ci` (fail-on-new vs `.neo/audit-baseline.txt`) |
| **Config** | `neo.yaml` (resolución recursiva, Zero-Hardcoding) |
| **Frontend** | React + Vite en `web/` |
| **Dashboard HUD** | `http://127.0.0.1:8087/` — servido por Nexus (SPA embebida), datos proxeados al hijo activo |
| **MCP Transport** | Via Nexus `http://127.0.0.1:9000/workspaces/<id>/mcp/sse` → hijo en puerto dinámico (9100-9299). neo-mcp es headless |
| **Workspace Registry** | `~/.neo/workspaces.json` (global, fuera del repo) |
| **Memoria long-term** | `.neo/db/hnsw.db` + `.neo/db/brain.db` (NO commitear) |

---

## 14 tools MCP (60+ operations) — Épica 239 + PILAR LXVI + PILAR LXVII (org tier)

**7 Macro-Tools** (ver contrato en `CLAUDE-global.md §5` + detalle en `.claude/rules/neo-sre-doctrine.md`):

- `neo_radar` — **19 intents**: BRIEFING, BLAST_RADIUS, SEMANTIC_CODE, DB_SCHEMA, TECH_DEBT_MAP, READ_MASTER_PLAN, SEMANTIC_AST, READ_SLICE, AST_AUDIT, HUD_STATE, FRONTEND_ERRORS, WIRING_AUDIT, COMPILE_AUDIT, GRAPH_WALK, PROJECT_DIGEST, INCIDENT_SEARCH, PATTERN_AUDIT, CONTRACT_QUERY, FILE_EXTRACT
- `neo_sre_certify_mutation` — ACID Guardian (single op): AST + Bouncer + tests + seal TTL
- `neo_daemon` — **12 actions**: 6 originales (PullTasks, PushTasks, Vacuum_Memory, SetStage, FLUSH_PMEM, QUARANTINE_IP — prohibidas en Pair/Fast) + MARK_DONE (read-only, exempt) + 5 PILAR XXVII (`execute_next`, `approve`, `reject` — daemon mode only · `trust_status`, `pair_audit_emit` — pair-exempt). Ver [`docs/pilar-xxvii-daemon-mcp.md`](./docs/general/briefing-format.md) y [`docs/pair-trust-feedback.md`](./docs/general/readme-reference.md).
- `neo_chaos_drill` — Load test síncrono 10s (single op)
- `neo_cache` — **6 actions**: stats, flush, resize, warmup, persist, inspect (consolidó 6 tools)
- `neo_command` — **3 actions**: run, approve, kill (consolidó 3 tools)
- `neo_memory` — **9 actions**: commit, learn, rem_sleep, load_snapshot + store, fetch, list, drop, search (Knowledge Store — PILAR XXXIX). `learn` acepta `action_type: add|update|delete`

**7 Specialist Tools:**

- `neo_compress_context` — squash de outputs largos
- `neo_apply_migration` — SQL via `dba.Analyzer` con guardrails ACID
- `neo_forge_tool` — hot-compile Go→WASM en runtime
- `neo_download_model` — stream `.wasm`/`.onnx`/`.gguf` a `.neo/models/`
- `neo_log_analyzer` — análisis semántico de logs + correlación HNSW con INC corpus
- `neo_tool_stats` — p50/p95/p99 + error count por tool MCP (JSON/CSV). El output incluye campo `plugin_metrics` con p50/p95/p99/calls/errors/rejections/cache_hits por (plugin, tool) — fetched desde Nexus `GET /api/v1/plugin_metrics` vía `SafeInternalHTTPClient`; `null` cuando Nexus no está alcanzable. [154.F]
- `neo_debt` — **5 actions** (PILAR LXVI): list, record, resolve, affecting_me, fetch. 4-tier debt: workspace (technical_debt.md), project (SHARED_DEBT.md), nexus (HTTP a dispatcher), org (reservado PILAR LXVII). Usar `affecting_me` al inicio de sesión para ver issues detectados por Nexus que afectan este workspace.

**Cache stack** (ver detalle en `CLAUDE-global.md §5` y README § "Cache stack"):
- `QueryCache` (SEMANTIC_CODE node IDs, 54 ns hit)
- `TextCache` (BLAST_RADIUS/PROJECT_DIGEST/GRAPH_WALK markdown, 33 ns hit)
- `EmbeddingCache` ([]float32 vectors, skip 30 ms Ollama)
- Generation invalidation via `Graph.Gen` atomic bump en cada `InsertBatch`
- Autopersist on SIGTERM, auto-load at boot, hot-reloadable via `rag.query_cache_capacity`/`rag.embedding_cache_capacity`
- `CachedComputePageRank` en `pkg/cpg` — memoise por graph pointer, 29× speedup medido

**Tools deprecated/eliminadas** (NO invocar — ya no existen en el registry):
- `neo_cache_stats`/`_flush`/`_resize`/`_warmup`/`_persist`/`_inspect` → `neo_cache(action: X)`
- `neo_run_command`/`_approve_command`/`_kill_command` → `neo_command(action: X)`
- `neo_memory_commit`/`_learn_directive`/`_rem_sleep`/`_load_snapshot` → `neo_memory(action: X)`
- `neo_apply_patch`, `neo_dependency_graph`, `neo_pipeline` → reemplazadas por `neo_sre_certify_mutation` + `BLAST_RADIUS`
- `neo_inspect_dom` → `FRONTEND_ERRORS` intent
- `neo_inspect_matrix` → `HUD_STATE` intent
- `neo_inject_fault` → `neo_chaos_drill(inject_faults: true)`

---

## Paquetes clave (internos)

| Package | Rol |
|---------|-----|
| `cmd/neo-mcp/` | Entrypoint MCP + handlers macro/micro |
| `cmd/neo-mcp/radar_handlers.go` | 19 intents radar (~3800 LOC — post-Épica 300) |
| `cmd/neo-mcp/tool_cache.go` | Dispatcher unificado (Épica 239) |
| `cmd/neo-mcp/tool_command.go` | Dispatcher unificado (Épica 239) |
| `cmd/neo-mcp/tool_memory.go` | Dispatcher unificado (Épica 239) |
| `cmd/neo-mcp/ghost_interceptor.go` | Modo fantasma autónomo + divergence guard |
| `cmd/neo-mcp/config_watcher.go` | Hot-reload `neo.yaml` (dir-watch fsnotify — Épica 229.4c) |
| `cmd/neo-migrate-quant/` | CLI offline — reporte de quantización int8/binary (Épica 170.D) |
| `pkg/incidents/` | Incident Intelligence: indexer HNSW+BM25, pattern extractor, CPG correlator, TriageEngine |
| `pkg/cpg/` | Code Property Graph: nodos, aristas, PageRank, BFS walk, SSA CC (hot-reloadable via `MaxHeapMBFn`). `persist.go` — Gob serialization + schema-versioned fast-boot (PILAR XXXII). `bridge.go` — CPG→federation scatter. |
| `pkg/rag/` | Grafo HNSW, embedding, ObservablePool, cache stack (QueryCache + TextCache + EmbeddingCache generic) |
| `pkg/rag/quantize.go` | Int8/binary primitives (4-way unrolled), `PopulateInt8`, `PopulateBinary`, `SearchInt8`, `SearchBinary` |
| `pkg/memx/` | Buffer episódico + REM sleep + WAL sanitizer |
| `pkg/consensus/` | Motor de debate multi-agente (Auditor/Optimizer/Architect) |
| `pkg/sre/oracle.go` | Predicción de fallos vía regresión lineal |
| `pkg/sre/healer.go` | Self-Healer: Reaper, Thermal Rollback, OOM Guard |
| `pkg/sre/allocs.go` | `ZeroAllocJSONMarshal` (usar este, NO `pkg/utils/`) |
| `pkg/dba/` | `Analyzer` con buffers pre-alocados, ACID transaccional |
| `pkg/workspace/` | `LoadRegistry()` auto-crea `~/.neo/workspaces.json`. `Type` field detecta workspaces de proyecto (`.neo-project/neo.yaml`) (PILAR XXXI) |
| `pkg/config/` | Loader recursivo de `neo.yaml` (Zero-Hardcoding obligatorio) + write-back backfill + `.neo/.env` expansion. `project.go` — `LoadProjectConfig` + `WriteProjectConfig`. `merge.go` — `MergeConfigs` 3-tier (workspace > project > global) (PILAR XXXI) |
| `pkg/auth/` | `keystore.go` — `Credentials` store en `~/.neo/credentials.json` (0600). `Load/Save/AddEntry/GetByProvider`. TenantID inyectado al boot en `cfg.Auth.TenantID` + `state.SetActiveTenant()` (PILAR XXXIII) |
| `cmd/neo-nexus/` | Dispatcher multi-workspace: proxy SSE+OAuth (RFC 7591/9728), pool de hijos, watchdog |
| `pkg/nexus/` | Config loader (`~/.neo/nexus.yaml`), ProcessPool, PortAllocator, WatchDog, ServiceManager (Ollama deps) |

---

## Convenciones del proyecto

- **Commits:** `feat(sre): <descripción>` para épicas del master plan. Scopes alternativos: `fix`, `docs`, `refactor`, `test`, `chore`, `plan`, `style`, `security`.
- **Rollback:** siempre `atomic` en producción; `granular`/`none` solo con justificación explícita.
- **Config extension pattern** (`pkg/config/config.go`): 1) campo con `yaml` tag en `RAGConfig`/`SREConfig`/etc., 2) default en `defaultNeoConfig()`, 3) backfill en `LoadConfig()` con `if cfg.X == 0 { cfg.X = default }`, 4) actualizar `neo.yaml` + `neo.yaml.example`, 5) si es hot-reloadable, añadir en `config_watcher.go WatchConfig`. PROHIBIDO hardcoding numérico en `pkg/rag/`, `pkg/memx/`, `cmd/`.
- **Cierre de sesión:** `neo_daemon(action: "Vacuum_Memory")` (solo si mode=daemon) para defragmentar WAL.
- **Master plan:** fuente autoritativa es `master_plan.md` — BRIEFING lo lee directo y cuenta `- [ ]` vs `- [x]`. Ya cerrado: `master_done.md` (141 épicas archivadas con `<!-- archived: YYYY-MM-DD -->`).
- **Workspace migración:** copiar `~/.neo/workspaces.json` + `.neo/db/brain.db` + `.neo/db/hnsw.db` + `.neo/db/hnsw.bin` (HNSW fast-boot snapshot, opcional — si existe ahorra cold-rebuild en la nueva máquina, ÉPICA 149) + `.neo/db/cpg.bin` (CPG snapshot) + `~/.neo/credentials.json` a la nueva máquina. Sin esos ficheros el server arranca limpio (PKI se regenera solo, CPG hace cold-build, HNSW hace cold-rebuild + saves nuevo snapshot async). **hnsw.db es authoritative; hnsw.bin es derivado** — borrar hnsw.bin fuerza cold rebuild sin pérdida de datos (`rm .neo/db/hnsw.bin`). Si se incrementa `rag.HNSWSnapshotSchemaVersion` en el código, el snapshot stale es auto-detectado al boot y se hace cold-rebuild automáticamente.
- **Project Federation:** crear `.neo-project/neo.yaml` en el directorio raíz del proyecto con `project_name`, `member_workspaces`, `dominant_lang`. `LoadConfig` lo detecta vía walk-up y aplica `MergeConfigs` 3-tier automáticamente.
- **Tier ownership (354.Z + PILAR LXVII · 2026-04-24):** cuatro tiers de knowledge store con dueños deterministas.
  - `tier:"workspace"` — local `<ws>/.neo/db/knowledge.db` o alias de project.
  - `tier:"project"` — `coordinator_workspace` (declarado en `.neo-project/neo.yaml`) posee el flock de `.neo-project/db/shared.db`; non-coords boot con `ks=nil` y proxean via Nexus MCP routing.
  - `tier:"org"` — [PILAR LXVII] `coordinator_project` (declarado en `.neo-org/neo.yaml`) posee `.neo-org/db/org.db`; non-coord projects reciben `ErrOrgStoreReadOnly` (HTTP proxy pendiente). Namespaces reservados: `directives`, `memory`, `debt`, `context`.
  - `tier:"nexus"` — Nexus dispatcher (singleton) posee `~/.neo/shared/db/global.db`; children proxy via HTTP REST `/api/v1/shared/nexus/*`.
  - bbolt NO soporta mixed RW+RO. Ver [`docs/tier-ownership.md`](./docs/general/tier-ownership.md).
- **Federation files por scope** (355.B auto-sync):
  - Workspace: `.claude/rules/*.md` (local, incluye `org-*.md` auto-mirrorado).
  - Project: `.neo-project/{neo.yaml, SHARED_DEBT.md, CONTRACT_PROPOSALS.md, PROJECT_TASKS.md, knowledge/}`.
  - Org: `.neo-org/{neo.yaml, DEBT.md, DIRECTIVES.md, knowledge/directives/}`.
- **Hot-reload:** campos safe (inference, governance, sentinel, cache capacity, cpg.max_heap_mb, consensus, kinetic) se recargan automáticamente al editar `neo.yaml`. Los unsafe (puertos, DB paths, provider) requieren `make rebuild-restart`.

---

## Modos de operación

| Modo | `NEO_SERVER_MODE` | Comportamiento | TTL seal |
|------|-------------------|----------------|----------|
| **Pair** | `pair` | Edición nativa + certificación completa (AST + bouncer + tests). `neo_daemon` PROHIBIDO. Build frontend bypass | 15 min |
| **Fast** | `fast` | Edición nativa + certificación ligera (AST + index, sin bouncer ni tests). `neo_daemon` PROHIBIDO | 5 min |
| **Daemon** | `daemon` | Autónomo nocturno. `neo_daemon` habilitado. Certificación estricta. Suspendido si RAPL >60W | 5 min |

Override TTL via `sre.certify_ttl_minutes` en `neo.yaml`.

---

## Neo-Nexus (multi-workspace dispatcher)

Binario separado `cmd/neo-nexus` que orquesta múltiples instancias de `neo-mcp` (una por workspace). Enruta `/mcp/sse` + `/mcp/message` al hijo correcto por workspace ID en path o header `X-Neo-Workspace`.

- **Config:** `~/.neo/nexus.yaml` (NO mezclar con `neo.yaml` por-workspace). Plantilla en `nexus.yaml.example`. Resolución: `$NEO_NEXUS_CONFIG → ~/.neo/nexus.yaml → defaults`.
- **Dispatcher HTTP:** `cfg.Nexus.dispatcher_port` (default `9000`) + `bind_addr` (default `127.0.0.1`).
- **Children:** rango `port_range_base..+port_range_size` (default `9100-9299`). Un puerto determinístico por hash del workspace path.
- **Operator HUD Dashboard:** Nexus sirve la SPA en `dashboard_port` (default `8087`) y proxea las APIs de datos al hijo activo. neo-mcp ya NO sirve HTML ni tiene `//go:embed`. [Épica 85]
- **Arquitectura headless (Épica 85):** neo-mcp es un worker RPC puro — sin stdio, sin SSE broadcast. Expone: `POST /mcp/message` (JSON-RPC), `GET /health`, APIs de datos internos + `/mcp/sse` opcional. Nexus es el entry point primario MCP.
- **Ruteo por contrato:** `target_workspace` inyectado en todos los tool schemas. Precedencia: `params.arguments.target_workspace` > URL path > `X-Neo-Workspace` header > active workspace.
- **Boot de hijos:** `stdin_mode: "devnull"` — neo-mcp no lee de stdin (headless). `"inherit"` solo para debug.
- **Logs de hijos:** `logs.mode: "file"` dirige stdout/stderr a `~/.neo/logs/nexus-<workspace-id>.log` con rotación manual (`rotate_mb`, `keep_files`).
- **Watchdog:** polling HTTP `/health` con `check_interval_seconds`, streak de `failure_threshold`, restart con `max_restarts_per_hour` como circuit breaker. Estado `Quarantined` cuando se excede el rate limit.
- **SIGTERM gracioso (Épica 229.1):** `make rebuild-restart` ahora envía SIGTERM a neo-mcp children + espera 5s antes de escalar a SIGKILL. Flush de cache snapshots garantizado.
- **Health verification post-start (Épica 229.1):** `make rebuild-restart` verifica `status=running` en `/status` (hasta 30s) antes de declarar éxito.
- **OAuth proxy (Épica 229.2/3):** `/.well-known/oauth-authorization-server` + `oauth-protected-resource` (RFC 9728) + `/oauth/*` reenvía al hijo activo. El dispatcher también STRIPS el prefix `/workspaces/<id>` antes del `proxyTo()` para que las rutas OAuth llegen al child root.
- **API mgmt:** `/api/v1/workspaces` (GET/POST), `/api/v1/workspaces/start/<id>`, `/api/v1/workspaces/stop/<id>`, `/api/v1/workspaces/wake/<id>` (lazy MVP, ÉPICA 150.D), `/api/v1/workspaces/active` (PUT). Token opcional via `cfg.API.auth_token` + header `X-Nexus-Token`. `/wake` accepta ID o Name (mirror /start), singleflight-coalesces concurrent callers.
- **Status endpoint:** `GET /status` devuelve JSON array con todos los hijos: `{id, name, path, port, status, pid, restarts, uptime_seconds, last_ping_ago_seconds, boot_phase, boot_pct, lifecycle}`. Boot fields populated durante `status=starting` (ÉPICA 148.E); `lifecycle: "lazy"` para workspaces registrados con lazy lifecycle. Sin auth requerido.
- **Boot progress (ÉPICA 148):** child neo-mcp expone `GET /boot_progress` retornando `{phase, hnsw_bytes_total, hnsw_bytes_read, hnsw_pct, started_at_unix, elapsed_seconds}`. Nexus polea durante verifyBoot y propaga a `/status`. BRIEFING peers section muestra `(strategosia hnsw_load=67%)` cuando peer en starting.
- **HNSW fast-boot (ÉPICA 149):** child guarda snapshot binario de `Graph.Nodes/Edges/Vectors` en `.neo/db/hnsw.bin`. Stale guard via per-bucket KeyN (NOT mtime — Vacuum_Memory bumps mtime sin cambio semántico). Schema v2: `magic[4]+schema_version+canonical_dim+counts+wal_file_size+node/edge/vector_key_n+blake2b[32]`. Boot from snapshot en <5s vs 5-6 min cold rebuild. Save async post-cold-load + periodic (default 30 min) + SIGTERM hook. Operator copia `hnsw.bin` al migrar máquinas para preservar fast-boot. **hnsw.db es authoritative; hnsw.bin es derivado.** Forzar cold rebuild: `rm .neo/db/hnsw.bin`. Schema bump: incrementar `rag.HNSWSnapshotSchemaVersion` const → snapshot schema mismatch → auto cold-rebuild sin perder datos (hnsw.db intacto).
- **Lazy lifecycle (ÉPICA 150 MVP):** `~/.neo/nexus.yaml::children.lifecycle: eager (default) | lazy`. Lazy: skip spawn at boot, register status=cold; first /wake (or future SSE-handler integration) triggers singleflight-coalesced spawn. Sin idle reaper aún (defer 150.C); operadores deben /stop manual. `lazy_boot_timeout_seconds` default 600 acomoda HNSW WAL load. Combinable con 149 fast-boot para boots <15s. **Lazy pre-warm (ÉPICA 150.M):** `lazy_prewarm_seconds: N` en `child:` — `time.AfterFunc(N)` post-`RegisterCold` arranca el workspace en background sin esperar el primer request; timer se cancela si el workspace alcanza `StatusRunning` vía otra ruta (`prewarmTimers sync.Map`). **Topology wake (ÉPICA 150.N):** cuando `EnsureRunning(X)` tiene éxito (spawn ok), `wakeProjectSiblings(X)` dispara en goroutine y arranca todos los workspaces del mismo `ProjectID` con `StatusCold + Lifecycle=="lazy"`. Cascade termina naturalmente: los ya-running no disparan nuevas olas.
- **Plugin lifecycle observability (ÉPICA 152):** `cmd/neo-nexus/plugin_boot.go::startAndHandshake` emite `[PLUGIN-LIFECYCLE]` events explícitos por stage (spawn_start, spawn_ok, handshake_start, handshake_ok, plugin_tools_received, plugin_dispatcher_ready). Background goroutine polea `__health__` MCP action en cada plugin cada 30s y populates `pluginRuntime.health` map. `/api/v1/plugins` embeds health snapshot per plugin. BRIEFING `plugins:` segment marca zombies cuando: PollErr != "" OR tools_registered=[] OR PolledAtUnix > 90s atrás.
- **Plugin __health__ contract (ÉPICA 152.H):** todos los plugins MCP DEBEN exponer action `__health__` que retorna `{plugin_alive, tools_registered, uptime_seconds, last_dispatch_unix, error_count, api_key_present}`. **Local-only**: NO invoca el upstream API — instantáneo (<10ms target). Sin esto, neo-mcp no puede distinguir "process alive + dispatcher dead" zombies.
- **Plugin call metrics (ÉPICA 154):** `/api/v1/plugin_metrics` retorna p50/p95/p99/calls/errors/rejections/cache_hits per (plugin, tool). Atomic counters en process-wide sync.Map (sobrevive SIGHUP reload). Cache hits + ACL/policy rejections excluded de latency ring (no skewing).
- **Internal endpoint auth (PILAR LXXI):** Rutas `/internal/certify/*` y `/internal/chaos/*` protegidas por `X-Neo-Internal-Token` — ephemeral boot token generado al arrancar cada hijo neo-mcp. Nexus lo inyecta automáticamente en las llamadas internas. `evictPortHolder` aplica guard de nombre de proceso: no hace SIGKILL a menos que el proceso ocupante sea `neo-mcp` (150.O guard). Previene kill accidental de procesos de sistema que reusen un puerto del pool.
- **Hot-reload (SIGHUP):** `kill -HUP $(pgrep neo-nexus)` recarga `~/.neo/nexus.yaml` sin reiniciar el dispatcher. Reconcilia el pool.
- **URL routing:** `.mcp.json` puede apuntar directamente a un workspace con `http://127.0.0.1:9000/workspaces/<id>/mcp/sse`. Nexus extrae el ID del path, verifica `StatusRunning` y proxea.

---

## Referencia rápida

- Contrato operativo base: [`CLAUDE-global.md`](./CLAUDE-global.md)
- Leyes universales (template para nuevos proyectos): [`docs/neo-global.md`](./docs/general/neo-global.md)
- Workflow paso a paso: [`.claude/rules/neo-workflow.md`](./.claude/rules/neo-workflow.md)
- Doctrina macro-tools (13 tools / 32 ops): [`.claude/rules/neo-sre-doctrine.md`](./.claude/rules/neo-sre-doctrine.md)
- Leyes de código específicas Go/MCP: [`.claude/rules/neo-code-quality.md`](./.claude/rules/neo-code-quality.md)
- Doctrina DB/RAG/migraciones (scoped a pkg/dba, pkg/rag, migrations): [`.claude/rules/neo-db.md`](./.claude/rules/neo-db.md)
- Directivas activas (auto-gen, autoritativas): [`.claude/rules/neo-synced-directives.md`](./.claude/rules/neo-synced-directives.md)
- Política de `//nolint:gosec` (Épica 234): [`.claude/rules/neo-gosec-audit.md`](./.claude/rules/neo-gosec-audit.md)
- Política de deadcode (Épica 235): [`.claude/rules/neo-deadcode-triage.md`](./.claude/rules/neo-deadcode-triage.md)
- README completo: [`README.md`](./README.md)
- **`.claude/` reference completo:** [`docs/claude-folder-inventory.md`](./docs/plugins/claude-folder-inventory.md)

**Skills disponibles** (15 total — `skills/` → auto-load si `trigger:` match, task-mode vía `/nombre`):

| Skill | Invocación | Uso |
|-------|-----------|-----|
| `sre-doctrine` | auto-load | Flujo Ouroboros, modos pair/fast/daemon, leyes |
| `jira-workflow` | auto-load | Ciclo vida Jira, naming, transitions, doc-pack |
| `github-workflow` | auto-load | 20 actions plugin-github, multi-tenant PAT, cross-ref Jira ↔ GitHub |
| `deepseek-workflow` | auto-load | 4 actions plugin-deepseek, cache 50× discipline, threaded mode, triage rules |
| `sre-tools` | auto-load | Inventario 14 tools MCP, 23 intents, degradación |
| `sre-quality` | auto-load | Zero-Alloc, SafeHTTP, certify TTL, gosec, deadcode |
| `sre-federation` | auto-load | Tier ownership, federation walk-up, CPG, auth, debt |
| `sre-troubleshooting` | auto-load | Recovery: MCP offline, boot fail, BLAST_RADIUS stale |
| `jira-create-pilar` | `/jira-create-pilar <PILAR>` | Mass-create Epics+Stories para un PILAR |
| `neo-doc-pack` | `/neo-doc-pack <KEY>` | Genera + adjunta doc-pack a ticket Jira |
| `jira-id` | `/jira-id <epic_id>` | Resuelve master_plan ID → MCPI ticket Jira |
| `jira-doc-from-commit` | `/jira-doc-from-commit` | Doc automático desde commit hash |
| `daemon-flow` | `/daemon-flow` | UI operativo daemon iterativo PILAR XXVII |
| `daemon-trust` | `/daemon-trust [pattern]` | Dashboard trust scores + tier history |
| `brain-doctor` | `/brain-doctor` | Diagnóstico Brain Portable: canonical_id, push status |

## PILAR XXIII — Plugin Jira ecosystem

PILAR XXIII (cerrado 2026-04-28) entrega plugin Jira completo + skills doctrinales. Ver:

- **Doctrina + workflow:** [`.claude/skills/jira-workflow/SKILL.md`](./.claude/skills/jira-workflow/SKILL.md) (auto-load)
- **Mass-create skill:** [`.claude/skills/jira-create-pilar/SKILL.md`](./.claude/skills/jira-create-pilar/SKILL.md) (`/jira-create-pilar <PILAR>`)
- **Doc pack skill:** [`.claude/skills/neo-doc-pack/SKILL.md`](./.claude/skills/neo-doc-pack/SKILL.md) (`/neo-doc-pack <KEY>`)
- **Jira ID resolver:** [`.claude/skills/jira-id/SKILL.md`](./.claude/skills/jira-id/SKILL.md) (`/jira-id <epic_id>`)
- **Doc from commit:** [`.claude/skills/jira-doc-from-commit/SKILL.md`](./.claude/skills/jira-doc-from-commit/SKILL.md) (`/jira-doc-from-commit`)
- **Output style SRE:** [`.claude/output-styles/neo-sre.md`](./.claude/output-styles/neo-sre.md)
- **Guía operativa:** [`docs/jira-integration-guide.md`](./docs/plugins/jira-integration-guide.md)
- **`.claude/` reference completo:** [`docs/claude-folder-inventory.md`](./docs/plugins/claude-folder-inventory.md)
- **ADRs:** [`docs/adr/ADR-005-plugin-architecture.md`](./docs/adr/ADR-005-plugin-architecture.md), [ADR-006](./docs/adr/ADR-006-jira-auth-flow.md), [ADR-007](./docs/adr/ADR-007-bidirectional-webhooks.md)

**Plugin tool exposed:** `mcp__neoanvil__jira_jira` con 7 actions: `get_context | transition | create_issue | update_issue | link_issue | attach_artifact | prepare_doc_pack (commit_hash auto-derive)`. `update_issue` permite PATCH parcial sobre tickets ya creados (backfill description/summary/labels/dates/assignee) — útil para corregir checkbox hygiene retroactivo o sincronizar body al cierre Done. Ver Regla #13 en jira-workflow SKILL.md.

**Zero-token automation:** `make install-git-hooks` instala `scripts/git-hooks/post-commit` que detecta `MCPI-N` en cada commit message y auto-dispara `prepare_doc_pack` via Nexus REST. Sin invocar Claude. Override flags: `NEO_HOOK_DISABLE`, `NEO_HOOK_QUIET`, `NEO_NEXUS_URL`, `NEO_REPO_ROOT`.

**Auth + context locales** (todos `0600`): `~/.neo/credentials.json`, `~/.neo/contexts.json`, `~/.neo/plugins.yaml`, `~/.neo/audit-jira.log`. Operador config via `neo login` + `neo space use`.

**Naming canónico:** Issues `[<label>] <texto>`; doc-pack files snake_case 2-4 palabras (concepto, NO path); zip `<KEY>.zip` con root folder `<KEY>/` dentro.

**Workflow MCPI:** `Backlog → Selected for Development → In Progress → REVIEW → READY TO DEPLOY → Done`. Epic más corto: `Backlog → In Progress → Done` (cuando todas las child Done).

**Master plan IDs ≠ Jira ticket IDs:** `134.A.1` (master_plan checkbox) y `MCPI-52` (Jira issue) son espacios diferentes. `[EPIC-FINAL MCPI-N]` SIEMPRE referencia el ticket Jira real, nunca el ID del master plan. Helper: `pkg/jira.ResolveMasterPlanID(epicID)` o skill `/jira-id <epic_id>`. Detalle en [`.claude/skills/jira-workflow/SKILL.md` Regla #8](./.claude/skills/jira-workflow/SKILL.md).

**Hooks parsean solo subject:** los git hooks (`scripts/git-hooks/post-commit`, `commit-msg`) extraen IDs Jira únicamente del subject (primera línea) del commit message + validan vía `jira/get_context` antes de fire. Body es free-form prose donde refs como `ADR-009` o `Z0-9` o `MCPI-9999` no disparan `prepare_doc_pack`. Implementación en `scripts/git-hooks/lib-jira-tickets.sh` (Épica 139). Ver Regla #12 en jira-workflow SKILL.md.

## Sesión 2026-05-09 — nuevas docs

- **GitHub plugin (Area 2):** [`docs/plugins/github-integration-guide.md`](./docs/plugins/github-integration-guide.md) — **20 actions** (PRs:7, Issues:4, Repo:4, Code:3 con `list_files/get_file/search_code` para review remoto sin clone, Helpers:2), multi-tenant, cross-ref Jira ↔ GitHub patrón, audit hash-chain. Directiva canónica `[GITHUB-PLUGIN-WORKFLOW]` en `.claude/rules/neo-synced-directives.md`. ADR de diseño: [`docs/adr/ADR-011-github-plugin-design.md`](./docs/adr/ADR-011-github-plugin-design.md). Skill operacional: `/github-workflow` ([`.claude/skills/github-workflow/SKILL.md`](./.claude/skills/github-workflow/SKILL.md)).
- **Observability pipeline (Areas 4 + 5 + 6):** [`docs/general/observability.md`](./docs/general/observability.md) — `pkg/notify` (Slack/Discord webhook dispatcher) + `pkg/otelx` (W3C traceparent + noopTracer + RecordingTracer) + `pkg/openapi` (auto-generated `/openapi.json` + `/docs` Swagger UI). ASCII pipeline diagram, threat model, test patterns.
