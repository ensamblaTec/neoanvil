---
name: sre-tools
description: Inventario de las 15 tools MCP de neoanvil (neo_radar 23 intents, neo_sre_certify_mutation, neo_chaos_drill, neo_command, neo_cache, neo_daemon, neo_memory + 8 specialists incluyendo neo_local_llm ADR-013 y los plugins Jira/DeepSeek/GitHub). Use when picking a tool, troubleshooting tool degradation, or learning about a specific intent.
---

# SRE Tools — inventario operativo

> Las 15 tools MCP que expone neoanvil (más specialists). Para detalle
> completo de schemas ver `.claude/rules/neo-sre-doctrine.md` —
> esta skill es la guía de selección rápida.

---

## Specialist tools (8)

| Tool | Use case |
|------|----------|
| `neo_compress_context` | Squash large outputs / logs / 3+ edits since last BRIEFING |
| `neo_apply_migration` | SQL raw via dba.Analyzer (internal SQLite brain.db) |
| `neo_forge_tool` | Hot-compile Go→WASM (⚠️ scaffold-broken, see technical_debt.md) |
| `neo_download_model` | Stream `.wasm`/`.onnx`/`.gguf` to `.neo/models/` |
| `neo_log_analyzer` | Semantic log analysis + INC corpus HNSW correlation |
| `neo_tool_stats` | p50/p95/p99 + errors per tool MCP, includes plugin metrics |
| `neo_debt` | 4-tier debt registry (workspace/project/org/nexus) — PILAR LXVI/LXVII |
| `neo_local_llm` | **ADR-013**: prompt → local Ollama (Qwen 7B default). $0/call, ~5-30s/audit, 407 ms warm-cache trivial reply. Default model from `cfg.AI.LocalModel` |

---

## Macro-tools (7)

### `neo_radar` — 23 intents

| Intent | Cuándo usar |
|--------|-------------|
| `BRIEFING` | OBLIGATORIO inicio sesión / resume |
| `BLAST_RADIUS` | Antes de cualquier edit |
| `SEMANTIC_CODE` | Búsqueda conceptual; si 0 resultados → Grep |
| `DB_SCHEMA` | Inspección protegida BD (read-only) |
| `TECH_DEBT_MAP` | Hotspots + CodeRank antes de refactors amplios |
| `READ_MASTER_PLAN` | Lee fase activa del master plan |
| `SEMANTIC_AST` | Chunking semántico de archivo |
| `READ_SLICE` | OOM-safe en archivos ≥100 líneas |
| `AST_AUDIT` | CC>15, shadows, infinite loops; obligatorio en BoltDB code |
| `HUD_STATE` | Estado interno: MCTS, RAM, salud |
| `FRONTEND_ERRORS` | Errores React/Vite |
| `WIRING_AUDIT` | Tras añadir import a main.go |
| `COMPILE_AUDIT` | Build + symbol_map (offset quirúrgico para READ_SLICE) |
| `GRAPH_WALK` | BFS desde símbolo en CPG |
| `PROJECT_DIGEST` | Snapshot estructural |
| `INCIDENT_SEARCH` | Tri-tier search sobre `.neo/incidents/` |
| `PATTERN_AUDIT` | Patrones recurrentes (INC post-153.C) |
| `CONTRACT_QUERY` | Endpoint HTTP por path fragment |
| `FILE_EXTRACT` | Surgical extraction por símbolo |
| `CONTRACT_GAP` | Diff TS fetch vs Go routes |
| `INBOX` | Agent-to-agent inbox cross-workspace |
| `PLUGIN_STATUS` | Plugin pool runtime state (PILAR XXIII) |
| `CLAUDE_FOLDER_AUDIT` | Drift detection .claude/skills/ vs CLAUDE.md (128.1) |

### `neo_sre_certify_mutation` — Guardian ACID

```
mutated_files (paths absolutos), complexity_intent
  ∈ {O(1)_OPTIMIZATION, O(LogN)_SEARCH, FEATURE_ADD, BUG_FIX}
rollback_mode: atomic (default) | granular | none
dry_run: bool
```

Trampa: `O(1)_OPTIMIZATION` falla con nested loops aunque sean
pipeline/channel. Usar `FEATURE_ADD` para feature con control flow.

### `neo_daemon` — 12 actions

**6 originales** (PROHIBIDAS en Pair/Fast): `PullTasks`, `PushTasks`,
`Vacuum_Memory`, `SetStage`, `FLUSH_PMEM`, `QUARANTINE_IP`.
**+ MARK_DONE** (read-only, exempt en cualquier modo).
**+ 5 PILAR XXVII**: `execute_next` / `approve` / `reject` (daemon-mode only)
+ `trust_status` / `pair_audit_emit` (pair-exempt para feedback loop).
Suspendido cuando RAPL > 60W (modo STABILIZING). Ver
[ADR-009 daemon-trust-scoring](../../../docs/adr/ADR-009-daemon-trust-scoring.md).

### `neo_chaos_drill` — Asedio síncrono 10s

```
target (URL), aggression_level (1-10, goroutines = nivel × 1000),
inject_faults (bool)
```

### `neo_cache` — 6 actions

`stats` | `flush` | `resize` | `warmup` | `persist` | `inspect`

### `neo_command` — 3 actions

`run` (stages comando con `risk_score` 1-10 + `blast_radius_analysis`,
retorna `ticket_id` para approval) | `approve` | `kill`. Use `// turbo`
en el comando para auto-approve.

### `neo_memory` — 9 actions

| Action | Use |
|--------|-----|
| `commit` | Lección episódica (BoltDB memex_buffer) |
| `learn` | Directiva permanente (dual-layer: BoltDB + .claude/rules/) |
| `rem_sleep` | Forzar consolidación |
| `load_snapshot` | Restaurar Gob |
| `store/fetch/list/drop/search` | Knowledge Store cross-workspace |

Tier ownership: `workspace` | `project` (coord workspace) |
`org` | `nexus` (singleton dispatcher).

---

## Specialist tools (7+)

| Tool | Use |
|------|-----|
| `neo_compress_context` | Tras 3+ ediciones o IO >500KB |
| `neo_apply_migration` | SQL via `dba.Analyzer` con guardrails |
| `neo_forge_tool` | Hot-compile Go→WASM en runtime |
| `neo_download_model` | Stream `.wasm`/`.onnx`/`.gguf` a `.neo/models/` |
| `neo_log_analyzer` | Análisis logs / INC files |
| `neo_tool_stats` | p50/p95/p99 + errors por tool MCP |
| `neo_debt` | 5 actions; 4-tier (workspace/project/nexus/org) |
| `mcp__neoanvil__jira_jira` | **Plugin Jira (PILAR XXIII):** action ∈ {get_context, transition, create_issue, link_issue, attach_artifact} |

---

## Reglas de degradación

### BLAST_RADIUS retorna `graph_status:not_indexed`

No bloquear edición. Continuar con `confidence:low`. Usar Grep para
callers manualmente. Certify después para reindex.

### SEMANTIC_CODE retorna 0

NO reintentar con otra frase — el problema es cobertura del índice,
no la query. Cambiar INMEDIATAMENTE a `Grep`. SEMANTIC_CODE solo
para queries verdaderamente abstractas.

### GRAPH_WALK retorna `No reachable nodes` (con CPG activo)

Limitación SSA documentada — common en receiver methods. Workaround:
`BLAST_RADIUS target=<file.go>` para callers reversos.

### MCP offline (sesión perdida)

```bash
curl -s -X POST http://127.0.0.1:9142/mcp/message ...
NEO_CERTIFY_BYPASS=1 git commit -m "..."
```

---

## Tools deprecated (NO invocar)

- `neo_apply_patch` → Edit/Write nativo + `neo_sre_certify_mutation`
- `neo_dependency_graph` → `neo_radar(intent:"BLAST_RADIUS")`
- `neo_pipeline` → `neo_sre_certify_mutation`
- `neo_inspect_dom` → `neo_radar(intent:"FRONTEND_ERRORS")`
- `neo_inspect_matrix` → `neo_radar(intent:"HUD_STATE")`
- `neo_inject_fault` → `neo_chaos_drill(inject_faults:true)`
- `neo_run_command/approve_command/kill_command` → `neo_command(action:...)`
- `neo_cache_stats/flush/resize/warmup/persist/inspect` → `neo_cache(action:...)`
- `neo_memory_commit/learn_directive/rem_sleep/load_snapshot` → `neo_memory(action:...)`

---

## Tool selection patterns

### Primer contacto con paquete desconocido
1. `COMPILE_AUDIT` (symbol_map + líneas exactas)
2. `READ_SLICE` con `start_line` del symbol_map
3. NUNCA leer desde línea 1 a ciegas

### Búsqueda
- Símbolo/string exacto → Grep
- Concepto → SEMANTIC_CODE (si 0 → Grep)
- Incidente similar → INCIDENT_SEARCH
- Patrón en INC corpus → PATTERN_AUDIT
- Endpoint contract → CONTRACT_QUERY

### Audit batch repo

PROHIBIDO `Agent(subagent_type="Explore")` para auditar este repo
(15× tokens vs neo_radar directo). Usar:
- `AST_AUDIT pkg/X/` (batch glob)
- `COMPILE_AUDIT pkg/X` (cert status + symbol_map)
- `TECH_DEBT_MAP` (hotspots)
- `WIRING_AUDIT` post-import
- `neo_log_analyzer` para INC files

---

## See also

- [`sre-doctrine`](../sre-doctrine/SKILL.md) — flujo Ouroboros
- [`sre-quality`](../sre-quality/SKILL.md) — Leyes de calidad
- [`jira-workflow`](../jira-workflow/SKILL.md) — Plugin Jira specifically
