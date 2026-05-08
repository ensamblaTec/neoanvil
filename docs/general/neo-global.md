# NEO-GO GLOBAL RULES (Universal Base)

Reglas universales aplicables a cualquier proyecto orquestado por NeoAnvil.
Este fichero es una plantilla estable — NO depende del código interno del motor.
Copiar a `docs/neo-global.md` en proyectos nuevos junto con `CLAUDE-global.md`.

**Versión contrato: V6.5 — 13 tools MCP / 32 operations.**

---

## G1 — BOOT / BRIEFING (obligatorio)

La PRIMERA acción de CUALQUIER sesión es `neo_radar(intent: "BRIEFING")`.
Aplica también al reanudar desde resumen de contexto comprimido. El resumen es orientativo; el orquestador es autoritativo. Si el Master Plan está cerrado (`Open: 0`) usar `mode: "compact"`. Si retorna el plan completo o supera 8KB, ejecutar `neo_compress_context`. Si banner `⚠️ BINARY_STALE:Nm`: sugerir `make rebuild-restart` antes de certificar.

## G2 — CICLO OUROBOROS (obligatorio)

Por cada cambio de código: **1)** `BLAST_RADIUS` antes de editar, **2)** Edit/Write nativos, **3)** `neo_sre_certify_mutation` con paths absolutos + `complexity_intent`, **4)** `neo_chaos_drill` opcional tras cambios críticos. No editar sin investigar. No commit sin certificar.

## G3 — TOKEN BUDGET

Prohibido `Read` nativo en archivos ≥100 líneas — usar `READ_SLICE`. Prohibido >3 ediciones seguidas sin `neo_compress_context`. BRIEFING auto-compacta >8KB. **Read nativo con `offset`/`limit` NO es sustituto de READ_SLICE** — bypasa el tooling, no actualiza métricas IO, no aplica OOM-safe chunking.

## G4 — PRE-AUDIT ANTE BUGS

Para reportes de bug o código complejo, primero `AST_AUDIT` (descarta CC>15, bucles infinitos, shadowing). Solo si sale limpio, proceder con Read/Edit. `AST_AUDIT` acepta archivo / directorio / glob (`pkg/**/*.go`).

## G5 — BLAST-RADIUS OBLIGATORIO

Antes de modificar cualquier archivo: `neo_radar BLAST_RADIUS target: "ruta"`. No opcional, sin excepción por tamaño. Si `graph_status: not_indexed`, el fallback grep devuelve `confidence` (high/medium/low). `confidence: low` → resultado orientativo, certificar para reindexar. Soporta `targets []string` para análisis paralelo.

## G6 — ZERO-HARDCODING

PROHIBIDO IPs, localhost, puertos, endpoints o credenciales embebidos. Todo vía `neo.yaml`, `.neo/.env` (con expansión `${VAR}`) o el loader de config del stack. Resolución recursiva por árbol de directorios — el binario es agnóstico de CWD.

## G7 — AISLAMIENTO I/O EN MCP

NUNCA `fmt.Print`/`fmt.Println`/`os.Stdout` en código que hable JSON-RPC — destruye la conexión. Usar `log.Printf` (o el logger equivalente del stack). Con arquitectura headless (Épica 85) el riesgo es menor pero sigue siendo buena práctica. Mutaciones van por `neo_sre_certify_mutation`.

## G8 — ZERO-ALLOCATION (Hot-Paths)

En hot-paths: `sync.Pool`, slices reciclados con `[:0]`, structs por valor. PROHIBIDO `make()`/`new()` dentro de bucles críticos. PROHIBIDO `any`/`interface{}` innecesarios que debiliten el compilador. PROHIBIDO silenciar errores con `_ =`.

## G9 — CERTIFY SCHEMA

`neo_sre_certify_mutation` lee archivos del disco ya editados. NO inyecta código.
Schema: `mutated_files` (array de paths **absolutos**) + `complexity_intent` (`O(1)_OPTIMIZATION` / `O(LogN)_SEARCH` / `FEATURE_ADD` / `BUG_FIX`) + `rollback_mode` (`atomic` default / `granular` / `none`) + `dry_run` (opcional, solo AST + build checks sin seal).
**Trampa:** `O(1)_OPTIMIZATION` falla con nested loops aunque sean channels/pipelines — usar `FEATURE_ADD` si hay control flow.
Pair/Daemon: AST → Bouncer → `go test -short` → Index. Fast: AST → Index.
Al certificar exitosamente, el path se registra en `session_state` BoltDB y se expone en BRIEFING como `session_mutations`.

## G10 — ROLLBACK MODES

`atomic` (default): un fallo revierte TODO el batch al snapshot pre-mutación. `granular`: revierte solo el archivo fallido. `none`: solo reporta sin revertir. Producción: usar `atomic` siempre.

## G11 — DAEMON RESTRINGIDO

`neo_daemon` usa campo `action` (NO `intent`). Acciones: PullTasks, PushTasks, Vacuum_Memory, SetStage, FLUSH_PMEM, QUARANTINE_IP. **PROHIBIDO en Pair-Mode y Fast-Mode.** Suspendido automáticamente cuando RAPL > 60W (modo STABILIZING). Override testing: `NEO_RAPL_OVERRIDE_WATTS`.

## G12 — CHAOS DRILL

`neo_chaos_drill` es síncrono con límite de 10 segundos. Schema: `target` (URL), `aggression_level` (1-10, goroutines = nivel × 1000), `inject_faults` (bool). Reporte Markdown con TPS, % Errors, Shedded, Heap RAM, GC Runs, Status.

## G13 — SEMANTIC vs GREP

`SEMANTIC_CODE` para búsquedas conceptuales/semánticas (devuelve snippets Markdown). `Grep` nativo para búsquedas exactas de símbolo o string literal. **Si SEMANTIC_CODE retorna 0 → Grep inmediato.** No reintentar con otra frase — el problema es cobertura del índice, no la query.

## G14 — DB READ-ONLY GUARD

`DB_SCHEMA` es solo SELECT. Guard rechaza DROP/DELETE/UPDATE/INSERT/TRUNCATE/ALTER/CREATE/REPLACE. Sin `SELECT *` en tablas >1M filas. Sin mutaciones sin WHERE determinístico. `EXPLAIN QUERY PLAN` antes de queries nuevas. Drivers soportados: PostgreSQL (lib/pq, pgx/v5/stdlib), SQLite. Configurar alias en `neo.yaml → databases:` con `driver`, `dsn` (usar `${VAR}` desde `.neo/.env`), `max_open_conns`.

## G15 — SEGURIDAD BASE

HTTP clients: `sre.SafeHTTPClient()` para URLs externas (SSRF guard completo), `sre.SafeInternalHTTPClient(sec)` para tráfico servidor→servidor (solo loopback). PROHIBIDO `http.Client` crudo. Sockets Unix con `os.Chmod(0600)` post-Listen. Phoenix Protocol requiere `SRE_PHOENIX_ARMED=true`. Sanitizar inputs antes de shell (strip `"`, `&`, `;`, `$`, backticks). Dashboard HUD restringido a `127.0.0.1`. **`//nolint:gosec` solo con categoría documentada** (ver `.claude/rules/neoanvilsec-audit.md`): G304-WORKSPACE-CANON, G304-DIR-WALK, G304-CLI-CONSENT, G204-LITERAL-BIN, G204-SHELL-WITH-VALIDATION, G107-WRAPPED-SAFE-CLIENT, G402-STRESS-TEST.

## G16 — PRE-COMMIT HOOK

Archivos staged `.go/.ts/.tsx/.js/.jsx/.css` sin sello de `neo_sre_certify_mutation` son rechazados. TTL vía `neo.yaml → sre.certify_ttl_minutes` (default 15 min pair, 5 min daemon/fast). Certificar justo antes del commit. Bypass de emergencia: `NEO_CERTIFY_BYPASS=1 git commit` se registra como `bypassed` ⚠️ en el heatmap.

## G17 — PERSISTIR APRENDIZAJE

Tras bug no obvio, auditoría arquitectónica compleja o patrón nuevo: `neo_memory(action: "commit", topic, scope, content)`. REM sleep (5 min idle) consolida el buffer al HNSW de largo plazo.

## G18 — DIRECTIVE CRUD

`neo_memory(action: "learn")` acepta `action_type`: add (default), update, delete.
Update/delete requieren `directive_id` (1-based). ADD con `supersedes: [1,2]` auto-depreca. DELETE es soft (`~~OBSOLETO~~`). Sync dual-layer: BoltDB ↔ `.claude/rules/neo-synced-directives.md`.

## G19 — SELF-AUDIT DE CIERRE

Al finalizar cada bloque significativo: tabla de tools usadas con rating 1-10, peor tool identificada, propuesta concreta de mutación (campo del schema o formato de respuesta). Va DESPUÉS de `neo_memory(action: "commit")`, ANTES del cierre.

## G20 — INMUTABILIDAD FRONTEND

En TS/JS/React: no mutar estado directamente. Usar copias superficiales. Mantener componentes como funciones puras.

## G21 — NEXUS CONFIG SPLIT

Neo-Nexus (dispatcher multi-workspace) lee SU PROPIO archivo `~/.neo/nexus.yaml` — NO reutiliza el `neo.yaml` por-workspace. Resolución: `$NEO_NEXUS_CONFIG → ~/.neo/nexus.yaml → defaults`. Los children heredan `NEO_PORT`, `NEO_WORKSPACE_ID` y `cfg.Nexus.Child.ExtraEnv` pero su propio `neo.yaml` vive en cada workspace.

**Contrato stdin (crítico):** `cfg.Nexus.Child.StdinMode` debe ser `"devnull"` salvo para debug. `"inherit"` causa hang bajo cliente MCP con stdin-pipe.

**Transport flag:** campo `transport` en `WorkspaceEntry` (`"sse"` | `"stdio"` | `""`). Fijar al registrar: `POST /api/v1/workspaces {"path":"...","transport":"sse"}`. Nexus filtra en arranque.

**`managed_workspaces`:** lista fallback para entradas sin `transport` explícito. Vacío = arranca todas. El hijo SSE DEBE exponer `GET /health` para que `verifyBoot` lo marque `StatusRunning`.

**OAuth proxy (RFC 7591 + 9728):** Nexus reenvía `/.well-known/oauth-authorization-server`, `/.well-known/oauth-protected-resource` y `/oauth/*` al hijo activo. También strip-prefix `/workspaces/<id>` antes de proxyTo para que las rutas OAuth lleguen al child root.

**Watchdog:** siempre habilitado en producción. Eventos `[NEXUS-EVENT] child_*` son la fuente confiable de debug post-mortem.

**Hot-reload:** `kill -HUP $(pgrep neo-nexus)` recarga `~/.neo/nexus.yaml`. Evento: `[NEXUS-EVENT] config_reloaded`.

**URL routing:** `.mcp.json` puede apuntar a `http://<nexus-host>/workspaces/<id>/mcp/sse`. Nexus extrae el ID del path y proxea.

**Status endpoint:** `GET /status` retorna JSON array sin auth. Para dashboards externos.

**Ownership de `~/.neo/`:** Solo Nexus escribe `~/.neo/workspaces.json`. Los hijos neo-mcp leen (`NEO_NEXUS_CHILD=1` inyectado). Para registrar workspace: `POST /api/v1/workspaces`.

**`make rebuild-restart` mejorado (Épica 229.1):** SIGTERM gracioso (5s → SIGKILL) + verificación `/status` post-start hasta 30s antes de declarar éxito. Flush de cache snapshots garantizado.

## G22 — HOT-RELOAD SAFE LIST

Campos que se recargan automáticamente sin restart (fsnotify dir-watch, robusto a `sed -i`):

- `inference.*`, `governance.*`, `sentinel.*`
- `cognitive.strictness`
- `sre.safe_commands`, `sre.unsupervised_max_cycles`, `sre.kinetic_monitoring`, `sre.kinetic_threshold`, `sre.digital_twin_testing`, `sre.consensus_enabled`, `sre.consensus_quorum`
- `rag.query_cache_capacity`, `rag.embedding_cache_capacity` → `Resize()` inmediato
- `cpg.max_heap_mb` → re-evaluado en cada `Graph()` call

**Unsafe** (requieren `make rebuild-restart`): puertos, DB paths, certs, `ai.provider`, `rag.vector_quant`.

## G23 — CACHE STACK (3 capas)

- **`QueryCache`** — SEMANTIC_CODE node IDs. Hit ~54 ns, miss ~6 µs. Capacity: `rag.query_cache_capacity` (default 256).
- **`TextCache`** — BLAST_RADIUS / PROJECT_DIGEST / GRAPH_WALK markdown bodies. Hit ~33 ns.
- **`EmbeddingCache`** — `[]float32` 768-d vectors. Hit ~5 ns, miss ~30 ms (Ollama). Capacity: `rag.embedding_cache_capacity` (default 128).

**Invalidación:** `Graph.Gen.Add(1)` en cada `InsertBatch` → todas las entradas caducan lazy. Per-call: `bypass_cache: true` fuerza re-compute.

**Observabilidad:** `neo_cache(action: "stats")` JSON. BRIEFING compact muestra `Qcache`/`Tcache`/`Ecache` segments + ⚠️ si `evict_rate > 30%`.

**Persistencia:** auto-persist on SIGTERM, auto-load at boot. Snapshots en `.neo/db/*.snapshot.json` (versioned, fail-open).

## G24 — CPG + GRAPH_WALK

`cpg.max_heap_mb` default 512. Hot-reloadable — raise inmediato re-habilita serving sin rebuild. El graph se preserva en memoria cuando el guard tripa.

CPG build es lazy: primera llamada a `PROJECT_DIGEST`/`GRAPH_WALK` espera hasta 200ms; si no listo, degrada a heatmap-only.

**Limitación SSA documentada:** `GRAPH_WALK` sobre receiver methods (ej: `certifyOneFile` de `*CertifyMutationTool`) retorna "No reachable nodes". El SSA pass no emite call edges desde métodos. Workaround: `BLAST_RADIUS` sobre el archivo host para callers reversos.

## G25 — INCIDENT INTELLIGENCE

INC corpus vive en `.neo/incidents/INC-*.md`. Para `PATTERN_AUDIT`, el INC debe tener header `**Affected Services:**`.

`INCIDENT_SEARCH` es tri-tier: default cascade **BM25** (Ollama-free, <5ms) → **HNSW** (semantic, requiere embedder) → **text_search** (last-resort grep). Opcional `force_tier: bm25|hnsw|text` para ejercitar un path específico.

`neo_log_analyzer` es un standalone tool (no intent de neo_radar). Schema: `content|log_path` + `max_lines` (default 1000). Retorna: level counts, gaps >1s, top error components, correlación HNSW con corpus INC.

## G26 — AUDIT CI

`make audit-baseline` captura el estado limpio actual en `.neo/audit-baseline.txt` (commit a git). `make audit-ci` compara run actual vs baseline y falla si aparece cualquier NEW finding. Usar como CI gate obligatorio.

Los 3 linters gateados: staticcheck (U1000 unused), ineffassign (ineffectual assignments), modernize (Go 1.22+ idioms). `make audit` corre los 3 + coverage por paquete en <3s.

## G27 — TIER OWNERSHIP (354.Z-redesign · 2026-04-23)

Tres tiers de KnowledgeStore con dueños deterministas (bbolt no soporta mixed RW+RO):

| Tier | Backing file | Dueño | No-dueños acceden vía |
|------|--------------|-------|------------------------|
| `workspace` | `<ws>/.neo/db/knowledge.db` (standalone) o project knowledge.db (federation) | Local child / coordinator | Proxy al coord |
| `project` | `.neo-project/db/knowledge.db` | **Coordinator workspace** (`coordinator_workspace:` en `.neo-project/neo.yaml`) | Proxy vía Nexus MCP routing al coord |
| `nexus` | `~/.neo/shared/db/global.db` | **Nexus dispatcher (singleton)** | Proxy HTTP REST `/api/v1/shared/nexus/*` |

Operaciones: `neo_memory(action:"store|fetch|list|drop|search", tier:"...", namespace, key, ...)`. BRIEFING compact muestra `tier:project=leader|proxy:X|legacy` inline.

**Reserved namespaces** del tier `nexus` (seeded al boot): `improvements`, `lessons`, `operator`, `upgrades`, `patterns`. Usar para cross-project lessons, operator preferences, meta-patterns.

Guía completa: `neoanvil/docs/tier-ownership.md`.
