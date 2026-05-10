# CLAUDE Global Base (NeoAnvil Universal Template)

**Plantilla reusable.** Copia este archivo y [`docs/neo-global.md`](./docs/neo-global.md) a cualquier proyecto orquestado por NeoAnvil. Junto con el binario `neo-mcp` constituyen la base mínima para programar con el ciclo Ouroboros en cualquier stack.

Este archivo NO depende del código interno de NeoAnvil. Solo describe el contrato operativo con el orquestador.

**Versión contrato: V10.6 — 15 tools MCP / 60+ operations / 23 intents radar.**

---

## 1. Directiva de arranque (OBLIGATORIO)

**Al iniciar CUALQUIER sesión, antes de cualquier otra acción:**

```
neo_radar(intent: "BRIEFING")
```

Aplica también cuando:
- La sesión continúa desde un resumen de contexto comprimido
- El usuario dice "continúa", "sigue", "resume"
- Parece que el contexto anterior es suficiente

Si el Master Plan está cerrado (`Open: 0`), usar `mode: "compact"` para reducir IO.
Si BRIEFING devuelve el Master Plan completo o supera 8KB, ejecutar `neo_compress_context`.
Si BRIEFING muestra `⚠️ BINARY_STALE:Nm`, sugerir `make rebuild-restart` antes de certificar.

---

## 2. Ciclo Ouroboros por cada cambio

```
INVESTIGAR → EDITAR → CERTIFICAR → (OPCIONAL) CHAOS
neo_radar    Edit/Write   neo_sre_certify_mutation   neo_chaos_drill
```

1. **Investigar** (obligatorio, sin excepción) — `neo_radar BLAST_RADIUS` antes de cualquier edición. Si el archivo es reporte de bug → `AST_AUDIT` primero.
2. **Editar** con herramientas nativas (Read, Edit, Write) en Pair/Fast.
3. **Certificar** con `neo_sre_certify_mutation` usando **paths absolutos** + `complexity_intent` + `rollback_mode` opcional.
4. **Chaos drill** opcional tras cambios críticos en hot-paths.
5. **Persistir aprendizaje** con `neo_memory(action: "commit")` tras bugs no obvios o auditorías arquitectónicas.

**No editar sin investigar. No commit sin certificar.**

---

## 3. Tabla: cuándo usar Neo vs nativo

| Situación | Herramienta | Regla crítica |
|-----------|-------------|---------------|
| Primer contacto con un paquete | `COMPILE_AUDIT` primero | Retorna `symbol_map` con línea exacta → luego `READ_SLICE` con offset quirúrgico. Nunca READ_SLICE desde línea 1 |
| Búsqueda conceptual/abstracta | `SEMANTIC_CODE` | **Si retorna 0 resultados → Grep inmediato. No reintentar.** El problema es cobertura de índice, no la query |
| Búsqueda exacta de símbolo o string | `Grep` nativo | Más confiable que SEMANTIC_CODE para nombres exactos |
| Leer archivo `< 100` líneas | `Read` nativo | — |
| Leer archivo `≥ 100` líneas | `READ_SLICE` con offset de COMPILE_AUDIT | Prohibido READ_SLICE desde línea 1 si COMPILE_AUDIT ya tiene el offset |
| Impacto antes de editar | `BLAST_RADIUS` (obligatorio) | **Si retorna `not_indexed` → Grep fallback, continuar con warning. No bloquear.** |
| Bug / código complejo | `AST_AUDIT` primero | Descartar CC>15, bucles infinitos, variables shadow. Acepta globs (`pkg/**/*.go`) |
| Editar pkg/state/, pkg/dba/ o BoltDB | `AST_AUDIT` obligatorio | Verifica cursor iteration y transaction leaks |
| Agregar import a main.go | `WIRING_AUDIT` post-edit | Detecta imports sin instanciar — ejecutar justo después de la edición |
| Hotspots de deuda técnica | `TECH_DEBT_MAP` | Antes de refactors con >3 archivos |
| Subgrafo de calls de un símbolo | `GRAPH_WALK` | BFS hacia abajo. Si "No reachable nodes" → SSA limitation, usar BLAST_RADIUS |
| Estado del proyecto | `BRIEFING` | — |
| Schema de BD (read-only) | `DB_SCHEMA` | — |
| Paquetes huérfanos (solo Go) | `WIRING_AUDIT` | — |
| Build + symbol map (solo Go) | `COMPILE_AUDIT` | — |
| Archivos por patrón | `Glob` nativo | — |
| Comandos shell | `Bash` o `neo_command(action: "run")` | `neo_command` si requiere aprobación humana |
| Inspeccionar caches RAG | `neo_cache(action: "stats")` | JSON con hits/misses por capa |
| Latencia por tool MCP | `neo_tool_stats` | p50/p95/p99 + error rate |
| Certificar edición | `neo_sre_certify_mutation` | Batch completo en UNA llamada, justo antes del git commit |
| Guardar lección aprendida | `neo_memory(action: "commit")` | Bugs no obvios, patrones, workarounds |
| Persistir regla arquitectónica | `neo_memory(action: "learn")` | Dual-layer: BoltDB + `.claude/rules/` |
| Comprimir contexto | `neo_compress_context` | Tras 3+ ediciones o IO >500KB |
| Analizar logs/INC | `neo_log_analyzer` | Schema: `content\|log_path` + `max_lines` |

### Árbol de decisión de tools de lectura

```
¿Primer contacto con el paquete?
  → COMPILE_AUDIT → READ_SLICE con offset exacto del symbol_map

¿Busco algo específico?
  ¿Tiene nombre concreto (función, struct, string)? → Grep
  ¿Es concepto abstracto sin nombre exacto?         → SEMANTIC_CODE
    ¿Retornó 0 resultados?                           → Grep inmediato

¿Voy a editar?
  → BLAST_RADIUS siempre
    ¿not_indexed?   → Grep fallback + advertencia, NO bloquear
    ¿SSRF en scatter? → make rebuild-restart, usar Grep mientras

¿Es BoltDB/state/dba? → AST_AUDIT obligatorio antes de editar
¿Añado import a main.go? → WIRING_AUDIT después de editar
¿Quiero callees de un símbolo? → GRAPH_WALK (si "No reachable" → SSA method, usar BLAST_RADIUS)
```

---

## 4. Modos de operación

| Modo | `NEO_SERVER_MODE` | Certificación | `neo_daemon` | TTL seal |
|------|-------------------|---------------|--------------|----------|
| **Pair** | `pair` | AST + Bouncer + Tests | PROHIBIDO | 15 min |
| **Fast** | `fast` | Solo AST + Index | PROHIBIDO | 5 min |
| **Daemon** | `daemon` | Completa (orquestada) | Habilitado | 5 min |

TTL configurable vía `neo.yaml → sre.certify_ttl_minutes`. El pre-commit hook rechaza archivos sin sello vigente.

Daemon mode se **suspende automáticamente** cuando RAPL > 60W (`STABILIZING`). Override: `NEO_RAPL_OVERRIDE_WATTS=N`.

---

## 5. Las 15 tools MCP (contrato V10.6)

### 7 Macro-Tools

| Tool | Rol | Ops | Campo clave |
|------|-----|-----|-------------|
| `neo_radar` | Oráculo read-only | **23 intents** | `intent` |
| `neo_sre_certify_mutation` | Guardian ACID post-edición | 1 | `mutated_files[]`, `complexity_intent`, `rollback_mode` |
| `neo_daemon` | Admin asíncrono | **12 actions** | `action` (NO `intent`). Prohibido en Pair/Fast (excepto MARK_DONE/trust_status/pair_audit_emit) |
| `neo_chaos_drill` | Estrés síncrono 10s | 1 | `target`, `aggression_level` (1-10), `inject_faults` |
| `neo_cache` | Cache obs + control | **6 actions** | `action: stats\|flush\|resize\|warmup\|persist\|inspect` |
| `neo_command` | Shell dispatcher | **3 actions** | `action: run\|approve\|kill` |
| `neo_memory` | Brain-state + Knowledge Store | **9 actions** | `action: commit\|learn\|rem_sleep\|load_snapshot\|store\|fetch\|list\|drop\|search` |

### 7 Specialist Tools

| Tool | Rol |
|------|-----|
| `neo_compress_context` | Squash de outputs largos |
| `neo_apply_migration` | SQL raw via `dba.Analyzer` ACID |
| `neo_forge_tool` | Hot-compile Go→WASM en runtime (⚠️ scaffold roto, ver technical_debt.md) |
| `neo_download_model` | Stream .wasm/.onnx/.gguf a `.neo/models/` |
| `neo_log_analyzer` | Logs + correlación HNSW con INC |
| `neo_tool_stats` | p50/p95/p99 + errors por tool MCP + plugin_metrics |
| `neo_debt` | 4-tier debt registry (workspace/project/org/nexus, PILAR LXVI/LXVII) |

### Enums clave

- `complexity_intent`: `O(1)_OPTIMIZATION`, `O(LogN)_SEARCH`, `FEATURE_ADD`, `BUG_FIX`
- `rollback_mode`: `atomic` (default), `granular`, `none`
- `neo_radar.intent`: BRIEFING, BLAST_RADIUS, SEMANTIC_CODE, DB_SCHEMA, TECH_DEBT_MAP, READ_MASTER_PLAN, SEMANTIC_AST, READ_SLICE, AST_AUDIT, HUD_STATE, FRONTEND_ERRORS, WIRING_AUDIT, COMPILE_AUDIT, GRAPH_WALK, PROJECT_DIGEST, INCIDENT_SEARCH, PATTERN_AUDIT
- `neo_daemon.action`: PullTasks, PushTasks, Vacuum_Memory, SetStage, FLUSH_PMEM, QUARANTINE_IP
- `neo_cache.action`: stats, flush, resize, warmup, persist, inspect
- `neo_command.action`: run, approve, kill
- `neo_memory.action`: commit, learn, rem_sleep, load_snapshot. Sub-action para `learn`: `action_type: add\|update\|delete`

**Trampa clásica:** `O(1)_OPTIMIZATION` falla ante nested loops (aunque sean pipeline/channel). Usar `FEATURE_ADD` cuando hay control flow.

### Tools que NO existen más (si las ves en documentación vieja, IGNORAR)

- `neo_cache_stats/_flush/_resize/_warmup/_persist/_inspect` → `neo_cache(action: X)`
- `neo_run_command/_approve_command/_kill_command` → `neo_command(action: X)`
- `neo_memory_commit/_learn_directive/_rem_sleep/_load_snapshot` → `neo_memory(action: X)`
- `neo_apply_patch`, `neo_dependency_graph`, `neo_pipeline` → reemplazadas por `neo_sre_certify_mutation` + `BLAST_RADIUS`

---

## 6. Leyes universales de calidad

Ver `docs/neo-global.md` para el detalle operativo (auto-cargadas). Resumen:

1. **Zero-Hardcoding** — IPs, puertos, endpoints vienen de config (`neo.yaml` o env), nunca embebidos. Secretos en `.neo/.env` con expansión `${VAR}`.
2. **Aislamiento I/O en MCP** — nunca `fmt.Print`/`stdout` en código MCP; solo `log.Printf`.
3. **Zero-Allocation en hot-paths** — `sync.Pool`, slices con `[:0]`, nada de `make()`/`new()` dentro de bucles críticos.
4. **Seguridad** — Dos clientes HTTP: `sre.SafeHTTPClient()` para URLs externas/configuradas por usuario (SSRF guard completo), `sre.SafeInternalHTTPClient(sec)` para llamadas servidor→servidor a URLs controladas por el proceso. PROHIBIDO `http.Client` crudo. `//nolint:gosec` solo con categoría documentada (ver `.claude/rules/neoanvilsec-audit.md`).
5. **DB Read-Only** — guard rechaza DROP/DELETE/UPDATE/INSERT/TRUNCATE/ALTER/CREATE/REPLACE. Sin `SELECT *` en tablas >1M filas. `EXPLAIN` antes de queries nuevas.
6. **Token Budget** — Read nativo prohibido en archivos ≥100 líneas (usar `READ_SLICE`). Máx 3 ediciones seguidas sin `neo_compress_context`.
7. **Rollback atómico por defecto** — un fallo en el batch revierte TODO el batch. `granular`/`none` solo si hay razón explícita.
8. **CC cap 15** — `AST_AUDIT` enforce con SSA-exact (McCabe E-N+2) cuando CPG activo.
9. **Audit CI** — `make audit-ci` es el gate. 0 NEW findings vs `.neo/audit-baseline.txt` es obligatorio.

---

## 7. Session state (automático)

- Cada path certificado se escribe al bucket `session_state` de BoltDB (key: `workspace|boot_unix`).
- `BRIEFING` expone el listado como `session_mutations` al final de la respuesta.
- Sobrevive a `neo_compress_context`. `Vacuum_Memory` purga sesiones >24h.
- No requiere acción manual.

---

## 8. Cierre de sesión

1. Certificar archivos pendientes (batch único, una llamada).
2. `neo_memory(action: "commit")` con lecciones del bloque.
3. `neo_daemon(action: "Vacuum_Memory")` (solo en daemon mode).
4. Commit con formato `<tipo>(<scope>): <mensaje>`.
5. Self-audit breve (tools usadas + rating 1-10 + propuesta de mutación para la peor).

---

## 9. Invariantes de observabilidad

Al ejecutar `BRIEFING`, segmentos esperados:

- **`Mode`** — pair/fast/daemon
- **`Phase`** — fase actual del master_plan
- **`Open` / `Closed`** — conteo de subtareas `- [ ]` vs `- [x]`
- **`Next`** — próxima subtarea abierta (si hay)
- **`RAM`**, **`IO`** — runtime stats
- **`RAG`** — `IndexCoverage` % del workspace
- **`Muts`** — session_mutations count
- **`INC-IDX`** — total INCs / indexed + BM25 indexed
- **`Qcache`/`Tcache`/`Ecache`** — hit rate por capa (solo si ejercitado)
- **`CPG`** — heap/limit MB (%). ⚠️ si > 80%

Banners:
- `⚠️ BINARY_STALE:Nm` — binario más viejo que último commit a cmd/pkg
- `⚠️ RESUME` — sesión reanudó con mutations pero sin BRIEFING previo

---

## 10. Federation file scoping (4-tier: workspace → project → org → nexus)

Ouroboros soporta 4 tiers de federación con scope determinista y coordinator
pattern en cada nivel. Los archivos de convención viven en directorios
específicos y el agente los discovers via walk-up al boot.

| Path | Scope | Contenido | Edita cuándo |
|------|-------|-----------|--------------|
| `.claude/rules/*.md` | **workspace** | Rules específicas del workspace (incluye auto-mirror `org-*.md` de 355.B + `neo-project-federation.md` si aplica) | Siempre local; rules `org-*` son read-only (source en `.neo-org/`) |
| `.neo-project/neo.yaml` | project | `member_workspaces`, `coordinator_workspace`, `dominant_lang`, `knowledge_path`, `ignore_dirs_add`, `llm_overrides` | Al añadir/quitar workspaces, cambiar coordinator, mover artefactos |
| `.neo-project/FEDERATION.md` | project | README con IDs Nexus, puertos, roles, shared artifacts | Mismo trigger que `neo.yaml` |
| `.neo-project/SHARED_DEBT.md` | project | Deuda técnica cross-workspace (P0–P3) | Al detectar bug que afecta >1 workspace |
| `.neo-project/CONTRACT_PROPOSALS.md` | project | Two-phase approval queue [343.A] — breaking HTTP contracts pendientes | Auto-escrito por `checkContractDrift` |
| `.neo-project/PROJECT_TASKS.md` | project | Federated task queue [349.A] — `neo_daemon PushTasks scope:"project"` | Auto via `neo_daemon` |
| `.neo-org/neo.yaml` | **org** (LXVII) | `projects [...paths]`, `coordinator_project`, `llm_overrides`, `knowledge_path` | Al añadir/quitar projects del org, rotar coordinator |
| `.neo-org/DEBT.md` | org | Deuda cross-project (P0–P3) — `neo_debt scope:"org"` | Al detectar issue que afecta >1 project |
| `.neo-org/DIRECTIVES.md` | org | Reglas arquitectónicas org-wide — `neo_memory(learn, scope:"org")` | Al estandarizar una regla en todos los projects |
| `.neo-org/knowledge/directives/*.md` | org | Source de 355.B auto-sync a `.claude/rules/org-*.md` | Mismo trigger que DIRECTIVES.md |
| `~/.neo/nexus.yaml` | nexus (singleton) | `managed_workspaces`, `services.ollama_*`, `debt`, `port_range` | Al cambiar deployment del dispatcher |
| `~/.neo/nexus_debt.md` | nexus | Eventos auto-detectados por Nexus (verifyBoot timeouts, watchdog trips) | Auto via Nexus |

**Principios:**
- Cada tier tiene un **coordinator único** (singleton en nexus, `coordinator_workspace` en project, `coordinator_project` en org). Solo el coordinator abre su BoltDB en RW.
- Reglas fluyen **hacia adentro**: org → project → workspace (355.B mirror + walk-up discovery).
- Deuda fluye **hacia afuera**: workspace escala a project si afecta callers; project escala a org si afecta múltiples projects; Nexus captura issues cross-project automáticamente.
- LLMOverrides tiene **precedencia inversa** (org fills gap cuando project+workspace vacíos) para garantizar consistency cross-workspace sin over-specification.
- `.claude/rules/` es siempre **workspace-scoped**. Los `org-*.md` son copias read-only; editar el source en `.neo-org/knowledge/directives/`.

---

## Adaptación a otros stacks

- **Python/JS/Rust**: todas las reglas globales aplican. `COMPILE_AUDIT` y `WIRING_AUDIT` son Go-only — ignorarlas. `AST_AUDIT` funciona para Go/Python/TypeScript/Rust vía `astx.DefaultRouter` (ver `pkg/astx/`).
- **Zero-Hardcoding** se aplica con el loader de config del stack (`pydantic-settings`, `env.ts`, `config-rs`).
- El contrato de `neo_sre_certify_mutation` es stack-agnostic — acepta cualquier archivo, pero el flujo AST + build + tests asume el build system del proyecto (detectado vía `neo.yaml → workspace.modules: subdir → build_cmd`).
