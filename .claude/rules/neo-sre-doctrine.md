# DOCTRINA SRE: TOOLS V10.6 (OUROBOROS)

Esquema y uso de las **15 herramientas MCP** que exponen 60+ operations. Inventario: 7 macro-tools + 8 specialists (incluyendo `neo_local_llm` ADR-013). **master_plan.md vacío (todo archivado en master_done.md, 72 entries) · 3 plugins MCP (jira, deepseek, github) + 1 testmock (echo) · 9 ADRs activos (005-013). Estado GREEN.**
Cualquier tool fuera de este set es deuda técnica erradicada.

**PROHIBIDO:** usar `Agent(subagent_type="Explore")` para auditar este repositorio — cuesta 31.5k tokens vs ~2k con neo_radar. Usar siempre AST_AUDIT/COMPILE_AUDIT/TECH_DEBT_MAP directamente.

---

## 1. `neo_radar` (Macro-Inteligencia) — 23 intents

Oráculo unificado de lectura e investigación. Solo lectura, nunca muta.

**Schema:** `{ "intent": string, "target"?: string, "targets"?: []string, "db_alias"?: string, "limit"?: int, "start_line"?: int, "mode"?: string, "max_depth"?: int, "edge_kind"?: string, "force_tier"?: string, "filter_package"?: string, "min_calls"?: int, "bypass_cache"?: bool, "force_grep"?: bool, "min_results"?: int, "include_unexported"?: bool, "target_workspace"?: string, "method"?: string, "validate_payload"?: string }`

| Intent | Uso | Campos requeridos |
|--------|-----|-------------------|
| `BRIEFING` | **OBLIGATORIO al inicio de sesión Y al reanudar desde contexto comprimido.** Retorna: modo, fase, Open/Closed, RAM, IO, session_mutations, CPG heap vs limit, INC-IDX, binary_stale marker. Acepta `mode: "compact"` (una línea). Auto-compacta si supera 8KB. Prefix `⚠️ RESUME \|` si agente trabajó sin BRIEFING previo | `mode` opcional |
| `BLAST_RADIUS` | Radio de impacto antes de **cualquier** edición. Acepta `targets []string` para análisis paralelo. Fallback automático a grep si `graph_status: not_indexed`. Opcional `force_grep: true` para saltarse CPG | `target` o `targets` |
| `SEMANTIC_CODE` | Búsqueda semántica con snippets en Markdown. Solo para queries abstractas. Si retorna 0 → Grep inmediato, no reintentar. **Footer schema (ÉPICA 153):** `_match_summary: dense=N bm25=M grep=P [crossWS=Q]_` + `_embed_status: healthy\|down_` (Ollama embedder health) + `_result_quality: ok\|undershoot\|empty_` (retrieval quality, independiente de embedder) + `_fallback_used: none\|grep\|grep_no_match\|bm25_only\|down_grep\|down_grep_no_match_`. Quality=undershoot+bm25>0 promueve BM25 antes de dense en el render. Markdown content de archivos sanitiza fences via ZWJ insertion (ÉPICA 153.H, defense vs file-content fence injection). | `target` (descripción conceptual), `min_results` opcional |
| `DB_SCHEMA` | Inspección protegida de BD (solo SELECT, nunca muta). Soporta PostgreSQL/SQLite via `neo.yaml → databases:` | `target` (query SQL), `db_alias` |
| `TECH_DEBT_MAP` | Mapa de calor de hotspots de mutación + CodeRank. Si vacío: certify más archivos para popular el heatmap | `limit` (default 10) |
| `READ_MASTER_PLAN` | Lee la fase activa del master plan | — |
| `SEMANTIC_AST` | Chunking semántico de un archivo por bloques de significado | `target` (filepath) |
| `READ_SLICE` | Lectura OOM-safe de archivos **≥ 100 líneas**. **Obligatorio** para archivos grandes. `Read` nativo con offset/limit NO es sustituto | `target` (filepath), `start_line`, `limit` |
| `AST_AUDIT` | Análisis estático: CC > 15, bucles infinitos, variables shadow. Acepta directorio (batch) o glob pattern (`pkg/**/*.go`). Cuando CPG activo usa SSA-exact CC (McCabe E-N+2) | `target` (filepath, dir/, o glob) |
| `HUD_STATE` | Estado interno: MCTS nodes, RAM, color de salud, bus SSE event counts | — |
| `FRONTEND_ERRORS` | Errores del frontend React/Vite en tiempo real | — |
| `WIRING_AUDIT` | Detecta paquetes importados pero no instanciados en main.go. Ejecutar tras añadir import | — |
| `COMPILE_AUDIT` | Build + `symbol_map` JSON de símbolos exportados con línea inicial + stale cert list. Usar para Read con offset quirúrgico. Opcional `include_unexported: true`. Opcional `filter_symbol: string` — filtro case-insensitive sobre claves del symbol_map; retorna solo las entradas que contienen el substring (ej: `filter_symbol:"handleContract"` → 1-2 líneas). Sin efecto sobre build/cert checks. | `target` (package path o filepath) |
| `GRAPH_WALK` | BFS sobre el CPG desde un símbolo. Útil cuando BLAST_RADIUS identifica un nodo central. Output: lista numerada BFS `reachable_count: N` + por símbolo: `name pkg=... file:line`. Requiere CPG activo (`cpg.max_heap_mb` ≥ heap real). Edge kinds: `call` (default), `cfg`, `contain`, `all`. **Limitación SSA documentada:** receiver methods pueden retornar "No reachable nodes" — usar BLAST_RADIUS como alternativa | `target` (nombre de símbolo exacto), `max_depth` (default 2), `edge_kind` |
| `PROJECT_DIGEST` | Resumen ejecutivo: hotspots + CodeRank top functions + package coupling + HNSW coverage. Opcional `min_calls` (default 0) y `filter_package` para surface low-volume-but-significant edges. Usar al inicio de sesión para calibrar prioridades | — |
| `INCIDENT_SEARCH` | Búsqueda tri-tier sobre corpus `.neo/incidents/*.md`. Default cascade BM25→HNSW. Opcional `force_tier: bm25\|hnsw\|text` para exercise específico (Épica 229.5) | `target` (query), `force_tier` opcional |
| `PATTERN_AUDIT` | Lee `.neo/incidents/` directamente (sin HNSW), parsea INC con `ParseIncidentMeta`, detecta patrones recurrentes por `AffectedServices`. Solo funciona en INC post-Épica 153 (con header `**Affected Services:**`) | — |
| `CONTRACT_QUERY` | Consulta quirúrgica de un endpoint HTTP por path fragment. Filtra por `method` (GET/POST/…), extrae request schema (Go struct fields + tipos + required), valida `validate_payload` JSON opcional. Cacheable en TextCache (key `CONTRACT_QUERY:<target>:<method>`). Usa OpenAPI + Go AST + Python routes + TS callers. | `target` (path fragment), `method` opcional, `validate_payload` opcional |
| `FILE_EXTRACT` | Extracción quirúrgica de un símbolo o substring de un archivo. `context_lines:0` retorna el cuerpo completo del símbolo via `ast.Node.End()`. Cacheado via symbol_map (mtime-based). Hasta 3 hits en substring scan, windows merged. Preferir sobre READ_SLICE cuando se conoce el nombre del símbolo. | `target` (filepath), `query` (símbolo o substring), `context_lines` (default 5; 0 = full body) |
| `CONTRACT_GAP` | Detecta endpoints sin test, sin schema documentado, o con drift entre OpenAPI spec y handler real. Usa AST scan + route extraction. Retorna lista de gaps con severidad. | — |
| `INBOX` | Bandeja de mensajes cross-workspace. Parámetros: `filter: unread\|all\|urgent` + `key` para fetch de mensaje específico (marca como leído). Útil para coordinación federation sin polling activo. | `filter` opcional, `key` opcional |
| `PLUGIN_STATUS` | Estado de todos los plugins MCP activos: uptime, tools_registered, health, last_dispatch. Incluye métricas p50/p95/p99/calls/errors. Equivalente a `GET /api/v1/plugins` + plugin_metrics. | — |
| `CLAUDE_FOLDER_AUDIT` | Audita el directorio `.claude/` buscando ficheros > 40k chars, referencias stale a tools deprecadas, intents desconocidos, y skills sin SKILL.md. Retorna tabla con hallazgos y recomendaciones. | — |

**Regla de oro:** Si el archivo tiene ≥ 100 líneas, usa `READ_SLICE`. Nunca `Read` nativo en archivos grandes.

**Cuándo usar GRAPH_WALK vs BLAST_RADIUS:**
- `BLAST_RADIUS`: quién depende del archivo que voy a editar (callers / impacto hacia arriba)
- `GRAPH_WALK`: qué llama un símbolo hacia abajo (callees / subgrafo de calls)
- Usar juntos: BLAST_RADIUS primero para decidir si editar, GRAPH_WALK después para entender el subgrafo
- Limitación: target debe ser el nombre exacto del símbolo (ej: `handleTechDebtMap`, no `RadarTool.handleTechDebtMap`). Si retorna `No reachable nodes` con CPG activo → método hoja o SSA no registró sus calls (receiver methods común).

---

## 2. `neo_daemon` (Burócrata Administrativo) — 6 actions

Cola de tareas y etapas cognitivas. **PROHIBIDO en Pair-Mode y Fast-Mode.**
**SUSPENDIDO automáticamente cuando RAPL > 60W (modo STABILIZING).**

**Schema:** `{ "action": string, "tasks"?: array, "stage"?: int, "target_ip"?: string }`

| Action | Uso | Campos requeridos |
|--------|-----|-------------------|
| `PullTasks` | Obtiene la siguiente tarea O(1) de la cola BoltDB | — |
| `PushTasks` | Encola nuevas tareas en BoltDB | `tasks` (array de `{description, target_file}`) |
| `Vacuum_Memory` | Defragmentación WAL en background + purga session_state > 24h. Ejecutar al cerrar sesión | — |
| `SetStage` | Transición de fases cognitivas (1-6) | `stage` |
| `FLUSH_PMEM` | Vaciar caches PMEM via control socket | — |
| `QUARANTINE_IP` | Aislar IP via eBPF | `target_ip` |

**IMPORTANTE:** El campo se llama `action`, NO `intent`.

---

## 3. `neo_sre_certify_mutation` (Guardian ACID)

Certifica mutaciones ya realizadas por la IA. NO inyecta código.
**OBLIGATORIO después de editar `.go/.ts/.tsx/.js/.jsx/.css`.**

**Schema:** `{ "mutated_files": string[], "complexity_intent": string, "rollback_mode"?: string, "dry_run"?: bool }`

| Campo | Tipo | Valores |
|-------|------|---------|
| `mutated_files` | Array de strings | Paths **absolutos** de archivos editados |
| `complexity_intent` | Enum | `O(1)_OPTIMIZATION`, `O(LogN)_SEARCH`, `FEATURE_ADD`, `BUG_FIX` |
| `rollback_mode` | Enum (opcional) | `atomic` (default), `granular`, `none` |
| `dry_run` | bool (opcional) | Solo AST + build checks, no escribe seal ni indexa al RAG |

**Flujo según modo:**
- **Pair/Daemon:** AST → Bouncer termodinámico → `go test -short` → Index/Rollback
- **Fast:** AST → Index (sin bouncer ni tests)

**Rollback:** atomic = revierte batch completo si falla cualquier archivo. granular = solo revierte el archivo que falló. none = solo reporte, sin revertir.

**TRAMPA:** `O(1)_OPTIMIZATION` falla con nested loops (aunque sean pipeline/channel). Usar `FEATURE_ADD` para cualquier feature con control flow.

Éxito: sello en `.neo/db/certified_state.lock` + path registrado en `session_state` BoltDB + indexado RAG + evento al HUD.
Fallo: rollback según `rollback_mode` + mensaje de error.

---

## 4. `neo_chaos_drill` (Dron de Caos Síncrono)

Asedio termodinámico de 10 segundos. Opcional pero recomendado tras cambios críticos.
También disponible desde el Operator HUD en `http://127.0.0.1:8087/` vía botón Chaos Drill.

**Schema:** `{ "target": string, "aggression_level"?: int, "inject_faults"?: bool }`

| Campo | Tipo | Descripción |
|-------|------|-------------|
| `target` | string | URL del hijo (`http://127.0.0.1:9142/health` via Nexus, o directo) |
| `aggression_level` | int 1-10 | Goroutines = nivel × 1000 |
| `inject_faults` | bool | Simula timeouts SQL y NOAUTH Redis |

**Reporte:** Markdown con TPS, % Errores, Events Shedded, Max Heap RAM, GC Runs, Status.

---

## 5. `neo_cache` (Observabilidad + control de caches) — 6 actions [Épica 239]

Consolidó `neo_cache_stats/flush/resize/warmup/persist/inspect` (6 tools separadas) en un único dispatcher.

**Schema:** `{ "action": string, "include"?: []string, "top_n"?: int, "scope"?: string, "capacity"?: int, "targets"?: []string, "handlers"?: []string, "from_recent"?: bool, "query_top_n"?: int, "text_top_n"?: int, "embedding_top_n"?: int, "target"?: string, "top_k"?: int }`

| Action | Uso | Args extra |
|--------|-----|------------|
| `stats` | JSON live con hits/misses/hit_ratio (lifetime + 5min windowed), top_5 entries, warmup_suggested (recent misses), search_paths counters, tool_latency p50/p95/p99 | `include`, `top_n` |
| `flush` | Bump `Graph.Gen` → invalida TODAS las entries O(1). Usar tras edit manual sin certify | — |
| `resize` | Runtime capacity retune per-layer. Hot-reloadable via `neo.yaml` (fsnotify) — esta action es para rebinding inmediato | `scope: query\|text\|both`, `capacity` |
| `warmup` | Pre-llena caches en paralelo (sem=4). Schema: `targets []string` o `from_recent: true` (auto-source desde `RecentMissTargets` rings). `handlers` opcional: SEMANTIC_CODE/BLAST_RADIUS | `targets`, `handlers`, `from_recent` |
| `persist` | Escribe snapshots al disco: scope query/text/embedding/all + per-cache top_n. Auto-ejecutado en SIGTERM | `scope`, `query_top_n`, `text_top_n`, `embedding_top_n` |
| `inspect` | Per-target debug cross-layer. Usa `Peek()` (Épica 227) — no contamina contadores | `target`, `top_k` |

**Invariantes clave:**
- `Graph.Gen` bumpea en cada `InsertBatch` → caches invalidan lazy
- Per-call args: `bypass_cache: true` fuerza re-compute + refresh del entry
- BRIEFING compact muestra `Qcache`/`Tcache`/`Ecache` segments + ⚠️ warning si `evict_rate > 30%`
- Config fields: `rag.query_cache_capacity` (256), `rag.embedding_cache_capacity` (128) — hot-reloadables vía `neo.yaml` sin restart
- Snapshots en `.neo/db/{query,text,embedding}_cache.snapshot.json` — esquema versionado, fail-open al boot

---

## 6. `neo_command` (Shell dispatcher) — 3 actions [Épica 239]

Consolidó `neo_run_command` + `neo_approve_command` + `neo_kill_command`.

**Schema:** `{ "action": string, "command"?: string, "risk_score"?: int, "blast_radius_analysis"?: string, "ticket_id"?: string, "pid"?: int }`

| Action | Uso | Args extra |
|--------|-----|------------|
| `run` | STAGES un comando shell para autorización humana. **Requiere** `risk_score` (1-10) + `blast_radius_analysis`. Retorna `ticket_id`. Usar `// turbo` en comando para auto-approve | `command`, `risk_score`, `blast_radius_analysis` |
| `approve` | Ejecuta un ticket previamente staged | `ticket_id` |
| `kill` | Termina un background process iniciado con `run` (comando terminado en `&`) | `pid` |

---

## 7. `neo_memory` (Brain-state + Knowledge Store) — 9 actions [Épica 239 + 354.Z-redesign]

Consolidó brain-state (`commit/learn/rem_sleep/load_snapshot`) + Knowledge Store (`store/fetch/list/drop/search`).

**Schema:** `{ "action": string, "tier"?: "workspace"|"project"|"nexus", "topic"?: string, "scope"?: string, "content"?: string, "directive"?: string, "action_type"?: string, "directive_id"?: int, "supersedes"?: []int, "namespace"?: string, "key"?: string, "tags"?: []string, "hot"?: bool, "query"?: string, "k"?: int, "snapshot_path"?: string }`

| Action | Uso | Args extra |
|--------|-----|------------|
| `commit` | Entrada episódica en memex_buffer (BoltDB). Consolidado al HNSW durante REM sleep (5 min idle) | `topic`, `scope`, `content` |
| `learn` | Directiva arquitectónica permanente. **Dual-layer sync:** BoltDB + `.claude/rules/neo-synced-directives.md`. `action_type: add (default), update, delete`. `supersedes: [1, 2]` auto-depreca | `directive`, `action_type`, `directive_id`, `supersedes` |
| `rem_sleep` | Fuerza consolidación del memex_buffer a HNSW | — |
| `load_snapshot` | Restaura estado neuronal desde Gob (erasure-coded shards) | `snapshot_path` |
| `store` | Escribe entrada en Knowledge Store del tier | `tier`, `namespace`, `key`, `content`, `tags`, `hot` |
| `fetch` | Lee entrada por key | `tier`, `namespace`, `key` |
| `list` | Lista entries de un namespace (`*` = all namespaces) | `tier`, `namespace`, `tag` |
| `drop` | Hard-delete | `tier`, `namespace`, `key` |
| `search` | Substring search sobre key+content | `tier`, `namespace`, `query`, `k` |

**Tier ownership (354.Z + PILAR LXVII):**

| Tier | Backing | Dueño | Cómo escriben los no-dueños |
|------|---------|-------|------------------------------|
| `workspace` (default) | `<ws>/.neo/db/knowledge.db` o project knowledge.db | Local child (o coord en federation) | Proxy al coord |
| `project` | `.neo-project/db/knowledge.db` | **Coordinator workspace** (`coordinator_workspace` en `.neo-project/neo.yaml`) | Proxy vía Nexus MCP routing al coord |
| `org` [LXVII] | `.neo-org/db/org.db` | **Coordinator project** (`coordinator_project` en `.neo-org/neo.yaml`) | Non-coord projects reciben `ErrOrgStoreReadOnly` — HTTP proxy pendiente (follow-up) |
| `nexus` | `~/.neo/shared/db/global.db` | **Nexus dispatcher (singleton)** | Proxy HTTP a `/api/v1/shared/nexus/*` |

**Namespaces reservados org-tier:** `directives` (autosincs a `.claude/rules/org-*.md` via 355.B), `memory`, `debt` (ver también `.neo-org/DEBT.md`), `context`.

bbolt no soporta mixed RW+RO — cada tier necesita un leader único. Ver `docs/tier-ownership.md` para el detalle completo. BRIEFING compact muestra `tier:project=leader|proxy:X|legacy` para que el agente vea el rol.

**learn scope:"org":** `neo_memory(action:"learn", scope:"org", directive:"...")` persiste a `.neo-org/DIRECTIVES.md` con ID monotónico. Supersedes auto-depreca IDs listados. Auto-sync 355.B mirrorea al próximo boot de cada workspace miembro.

---

## 8. Specialist Tools (8 tools)

| Tool | Cuándo usarlo |
|------|--------------|
| `neo_compress_context` | Cuando BRIEFING retorna Master Plan completo, IO > 500KB, o tras 3+ ediciones seguidas. Alternativa: `BRIEFING mode: compact` cuando el plan esté cerrado |
| `neo_apply_migration` | Ejecuta SQL raw via `dba.Analyzer` con guardrails ACID |
| `neo_forge_tool` | Hot-compile Go → WASM de tools custom en runtime (⚠️ scaffold roto, ver technical_debt.md) |
| `neo_download_model` | Stream de `.wasm`/`.onnx`/`.gguf` a `.neo/models/` |
| `neo_log_analyzer` | Schema: `content\|log_path` + `max_lines` (default 1000). Análisis semántico de logs: level counts, gaps >1s, top-5 error components, correlación HNSW con corpus INC. Usar para analizar INC-*.md directamente o logs de producción |
| `neo_tool_stats` | JSON con p50/p95/p99/errors/calls por tool MCP. Schema: `sort_by: p99\|p95\|p50\|errors\|calls` + `top N` + `format: json\|csv` |
| `neo_debt` | **5 actions** (PILAR LXVI + LXVII): list, record, resolve, affecting_me, fetch. **4-tier debt access**: `workspace` (`.neo/technical_debt.md` — kanban.AppendTechDebt), `project` (`.neo-project/SHARED_DEBT.md` — federation.ParseSharedDebt), `org` (`.neo-org/DEBT.md` — AppendOrgDebt/ResolveOrgDebt, record accepts `affected_projects []string`), `nexus` (HTTP `/internal/nexus/debt` — Nexus dispatcher). `affecting_me` es shortcut para ver issues Nexus contra el workspace actual — recomendado al inicio de sesión cuando BRIEFING muestra `⚠️ NEXUS-DEBT:N P0:M` |
| `neo_local_llm` | **ADR-013**: routes prompts a Ollama local (default `qwen2.5-coder:7b` por `cfg.AI.LocalModel`). $0/call, ~5-30s/audit en RTX 3090, **407 ms warm-cache** post-load. Schema: `prompt` + opcional `model`/`system`/`max_tokens`/`temperature`. Use para refactor sketches, mechanical fan-out, daemon-mode triage. SEV ≥ 9 audits + decisiones arquitectónicas siguen yendo a `deepseek_call` (frontier quality). Routing local-vs-DS lo decide el agent prompt, no el server |

---

## 9. HTTP fallback (MCP offline)

Cuando Claude Code pierde la conexión SSE al Nexus pero neo-mcp sigue corriendo:

```bash
# Verificar estado
curl -s http://127.0.0.1:9142/health     # o puerto dinámico del child
curl -s http://127.0.0.1:9000/status     # todos los children

# Invocar tool directamente (sin MCP transport)
curl -s -X POST http://127.0.0.1:9142/mcp/message \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"TOOL","arguments":{...}}}' \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['result']['content'][0]['text'])"

# Pre-commit bypass cuando neo-mcp está offline (se registra en heatmap como bypassed ⚠️)
NEO_CERTIFY_BYPASS=1 git commit -m "..."
```

---

## 10. Nexus — Health y routing

Neo-Nexus (`:9000`) gestiona el pool de hijos. Reglas críticas:
- El hijo neo-mcp DEBE exponer `GET /health` en su mux — sin esto `verifyBoot` falla y el hijo queda en `status=error`
- `managed_workspaces` en `~/.neo/nexus.yaml` lista los workspaces SSE activos (vacío = todos)
- **OAuth proxy (Épica 229.2/3):** Nexus reenvía `.well-known/oauth-authorization-server` + `.well-known/oauth-protected-resource` (RFC 9728) + `/oauth/*` al hijo activo. También strip-prefix `/workspaces/<id>` antes de proxyTo para que el child reciba las rutas OAuth en su root.
- **`make rebuild-restart` mejorado (Épica 229.1):** SIGTERM gracioso (5s → SIGKILL) + verificación `/status` post-start hasta 30s antes de declarar éxito. Previene la ventana donde el hijo tracker queda desincronizado.
- Routing: `target_workspace` en args > URL path > header `X-Neo-Workspace` > active workspace fallback

---

## 11. Tools eliminadas / deprecated (NO invocar)

| Tool antiguo | Reemplazo |
|--------------|-----------|
| `neo_cache_stats` | `neo_cache(action: "stats")` |
| `neo_cache_flush` | `neo_cache(action: "flush")` |
| `neo_cache_resize` | `neo_cache(action: "resize", scope, capacity)` |
| `neo_cache_warmup` | `neo_cache(action: "warmup", targets OR from_recent)` |
| `neo_cache_persist` | `neo_cache(action: "persist", scope)` |
| `neo_cache_inspect` | `neo_cache(action: "inspect", target)` |
| `neo_run_command` | `neo_command(action: "run", command, risk_score, blast_radius_analysis)` |
| `neo_approve_command` | `neo_command(action: "approve", ticket_id)` |
| `neo_kill_command` | `neo_command(action: "kill", pid)` |
| `neo_memory_commit` | `neo_memory(action: "commit", topic, scope, content)` |
| `neo_learn_directive` | `neo_memory(action: "learn", directive, action_type)` |
| `neo_rem_sleep` | `neo_memory(action: "rem_sleep")` |
| `neo_load_snapshot` | `neo_memory(action: "load_snapshot", snapshot_path)` |
| `neo_apply_patch` | Edit/Write nativo + `neo_sre_certify_mutation` |
| `neo_dependency_graph` | `neo_radar(intent: "BLAST_RADIUS", target)` |
| `neo_pipeline` | `neo_sre_certify_mutation` |
| `neo_inspect_dom` | `neo_radar(intent: "FRONTEND_ERRORS")` |
| `neo_inspect_matrix` | `neo_radar(intent: "HUD_STATE")` |
| `neo_inject_fault` | `neo_chaos_drill(inject_faults: true)` |
