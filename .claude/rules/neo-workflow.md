# PROTOCOLO OPERATIVO OBLIGATORIO (Ouroboros V6.5)

Este protocolo aplica a TODA sesión y TODA interacción con el codebase. Sin excepciones.

---

## 0. BOOT — al inicio de CUALQUIER sesión

**OBLIGATORIO. No hacer nada más antes de esto.**

```
neo_radar(intent: "BRIEFING")
```

Devuelve: modo, fase activa, tasks pendientes (Open/Closed), RAM, IO de sesión, session_mutations, CPG heap vs limit, binary_stale marker, INC-IDX, cache hit-rates.

### Casos especiales que TAMBIÉN requieren BRIEFING

| Situación | Acción |
|-----------|--------|
| Sesión nueva | `BRIEFING` antes de cualquier cosa |
| Claude Code reanuda desde resumen de contexto comprimido | `BRIEFING` obligatorio — el resumen NO reemplaza la sincronización con el orquestador |
| El usuario dice "continua", "sigue", "resume" | `BRIEFING` primero igualmente |
| Cambio de tarea dentro de la misma sesión | `BRIEFING` para verificar estado actual |

Si el BRIEFING retorna el Master Plan completo: ejecutar `neo_compress_context` antes de continuar.
Si Open: 0 y Closed estable, usar `BRIEFING mode: compact` en arranques futuros para reducir IO.
Si `rag_index_coverage < 80%`: advertir al usuario antes de ejecutar BLAST_RADIUS o SEMANTIC_CODE.
Si banner `⚠️ BINARY_STALE:Nm`: recomendar `make rebuild-restart` antes de certificar nuevas features.
Si banner `⚠️ NEXUS-DEBT:N P0:M`: ejecutar `neo_debt(action:"affecting_me")` para ver detalles de los eventos que bloquean este workspace antes de planificar trabajo. Seguir el `Recommended` de cada entry (típicamente lsof/kill para BoltDB locks, restart via `POST /api/v1/workspaces/start/<id>`). Tras resolver, `neo_debt(action:"resolve", scope:"nexus", id:"...", resolution:"...")` para marcarlo cerrado.

**4-tier debt (PILAR LXVII):** además de `scope:"nexus"` y `scope:"project"`, ahora `neo_debt(scope:"org")` lee/escribe `.neo-org/DEBT.md` para issues que afectan múltiples projects del org (ej. "Go 1.26 upgrade across all projects", "TLS cert rotation"). `record` acepta `affected_projects []string`. Al escribir debt que afecta >1 project, usar scope:"org". Al escribir debt que afecta >1 workspace del mismo project, usar scope:"project". Jerarquía hacia afuera: workspace → project → org → nexus.

**Org directives (PILAR LXVII):** `neo_memory(action:"learn", scope:"org", directive:"...")` persiste reglas arquitectónicas org-wide en `.neo-org/DIRECTIVES.md`. Auto-sync 355.B mirrorea a `.claude/rules/org-*.md` de cada workspace miembro al próximo boot — el agente las carga junto con las rules locales. Usar `supersedes: [N, M]` para deprecar reglas obsoletas de una. Requiere workspace bajo `.neo-org/`.

---

## 1. INVESTIGAR — antes de cada edición

**Prohibido editar sin investigar primero.**

### 1.A Selección de herramienta según contexto

| Situación | Herramienta | Notas |
|-----------|-------------|-------|
| Explorar un paquete desconocido | `COMPILE_AUDIT` primero | Retorna `symbol_map` con línea exacta de cada símbolo. Luego `READ_SLICE` con offset quirúrgico. NUNCA leer desde línea 1 a ciegas |
| Voy a editar cualquier archivo | `BLAST_RADIUS` | Siempre. Ver regla de degradación abajo |
| Busco algo conceptual/arquitectónico | `SEMANTIC_CODE` | Solo para queries abstractas ("patrón de retry con backoff exponencial"). Ver regla de fallback abajo |
| Busco un símbolo, función o string exacto | `Grep` nativo | Más rápido y confiable que SEMANTIC_CODE para búsquedas exactas |
| Archivo ≥ 100 líneas | `READ_SLICE` con `start_line` + `limit` | Prohibido Read nativo. Usar `COMPILE_AUDIT` primero para saber offset exacto |
| Archivo < 100 líneas | `Read` nativo | Única excepción al uso de Read |
| Bug o código complejo | `AST_AUDIT` primero | Descartar CC>15, bucles infinitos, shadows ANTES de leer el código |
| Editar `pkg/state/`, `pkg/dba/` o cualquier BoltDB | `AST_AUDIT` obligatorio | Verificar cursor iteration correcta y ausencia de transaction leaks |
| Refactor amplio (>3 archivos) | `TECH_DEBT_MAP` | Identificar hotspots antes de decidir orden de cambios |
| Recién agregué un import a `main.go` | `WIRING_AUDIT` post-edit | Verificar que el nuevo paquete está instanciado, no solo importado |
| Explorar subgrafo de calls de un símbolo | `GRAPH_WALK` | Cuando BLAST_RADIUS identifica un nodo central. Schema: `target` (nombre símbolo), `max_depth` (default 2), `edge_kind` (call/cfg/contain). Retorna árbol BFS hacia abajo |
| Auditar repo completo (sweep) | `AST_AUDIT` + `COMPILE_AUDIT` por directorio | **PROHIBIDO usar `Agent(subagent_type="Explore")` — cuesta 15× más tokens sin añadir calidad** |
| Buscar incidentes similares | `INCIDENT_SEARCH` | Default cascade BM25→HNSW. Opcional `force_tier` para exercise específico. Si retorna 0 → `PATTERN_AUDIT` como fallback |
| Patrones recurrentes en corpus INC | `PATTERN_AUDIT` | Solo funciona en INC post-Épica 153 (con `**Affected Services:**` header). Lee `.neo/incidents/` directamente |
| Análisis de log o INC file | `neo_log_analyzer` | Standalone tool (NO intent de neo_radar). Schema: `content\|log_path` + `max_lines`. Retorna: level counts, gaps, top error components, correlación HNSW |

### 1.B Reglas de degradación (cuando una tool falla)

**BLAST_RADIUS retorna `graph_status: not_indexed`:**
- No bloquear la edición. Continuar con `confidence: low`.
- Usar `Grep` para buscar callers del archivo target manualmente.
- Certificar el archivo editado para que se re-indexe al grafo.
- Si `rag_index_coverage < 80%`: mencionar al usuario que BLAST_RADIUS será poco confiable hasta re-indexar.

**SEMANTIC_CODE retorna 0 resultados:**
- NO reintentar con otra frase. El problema es de cobertura del índice, no de la query.
- Cambiar INMEDIATAMENTE a `Grep` con el símbolo o patrón más específico posible.
- Reportar al usuario que SEMANTIC_CODE tuvo 0 resultados (permite diagnosticar cobertura).

**BLAST_RADIUS falla por SSRF (`[SRE-SSRF FATAL]`):**
- Causa raíz: binary stale de neo-nexus sin `SafeInternalHTTPClient`.
- Fix: `make rebuild-restart` (detecta stale automáticamente).
- Mientras tanto: usar `Grep` + `COMPILE_AUDIT` como sustitución.

**GRAPH_WALK retorna `No reachable nodes` (con CPG activo):**
- Limitación SSA documentada — common en receiver methods + funciones con solo stdlib calls.
- Workaround: `BLAST_RADIUS target=<file.go>` para callers reversos.
- NO es un bug — es inherente a cómo SSA lower las llamadas.

**INCIDENT_SEARCH HNSW tier devuelve 0:**
- Ollama probablemente offline al boot → `IndexIncidents()` no corrió.
- Default cascade ya hace fallback a text_search. Si aún 0, verificar que hay `INC-*.md` en `.neo/incidents/`.
- Forzar BM25: `force_tier: "bm25"` (funciona sin Ollama).

### 1.C Árbol de decisión rápido

```
¿Primer contacto con un paquete?
  → COMPILE_AUDIT (symbol_map + líneas) → READ_SLICE con offset exacto

¿Búsqueda de algo?
  ¿Símbolo/string exacto?         → Grep
  ¿Concepto arquitectónico?       → SEMANTIC_CODE (si retorna 0 → Grep inmediato)
  ¿Incidente o log?               → neo_log_analyzer (no es intent de neo_radar)
  ¿Patrones en corpus INC?        → PATTERN_AUDIT (INC post-153 con Affected Services)
  ¿Incidente semánticamente?      → INCIDENT_SEARCH (default cascade, o force_tier)

¿Antes de editar?
  → BLAST_RADIUS siempre
    ¿not_indexed? → Grep fallback, continuar con warning
    ¿SSRF fatal?  → make rebuild-restart, usar Grep mientras tanto

¿Archivo a leer?
  ≥ 100 líneas → READ_SLICE (con offset de COMPILE_AUDIT si disponible)
  < 100 líneas → Read nativo

¿Bug o transacción BoltDB? → AST_AUDIT obligatorio antes de editar

¿Quiero explorar llamadas de un símbolo?
  → GRAPH_WALK target=<nombre_simbolo> max_depth=2
    ¿Quiero impacto inverso (quién llama a X)?  → BLAST_RADIUS es más apropiado
    ¿No reachable nodes? → documentado: SSA limitation, usar BLAST_RADIUS

¿Necesito auditar varios paquetes?
  → AST_AUDIT pkg/rag/ → AST_AUDIT pkg/sre/ → COMPILE_AUDIT cmd/neo-mcp (batch)
  PROHIBIDO: Agent(subagent_type="Explore") — 15× más costoso en tokens
```

---

## 2. EDITAR — herramientas nativas

Usar Read, Edit, Write directamente. El modo determina la agresividad:

- **Pair:** Edición normal, certificación completa (AST + bouncer + tests)
- **Fast:** Iteración rápida, certificación AST-only (sin tests)
- **Daemon:** Orquestado via neo_daemon (PullTask → edit → Vacuum)

**Post-edit obligatorio:**
- Si agregaste un nuevo `import` a `main.go` → ejecutar `WIRING_AUDIT` inmediatamente.
- Si editaste `pkg/state/`, `pkg/dba/` o cualquier cursor BoltDB → verificar con `go build` antes de certificar.

**Límite:** No más de 3 ediciones seguidas sin `neo_compress_context`.

---

## 3. CERTIFICAR — después de cada edición

**OBLIGATORIO después de modificar archivos `.go`, `.ts`, `.tsx`, `.js`, `.jsx`, `.css`.**

```json
neo_sre_certify_mutation({
  "mutated_files": ["/ruta/absoluta/archivo1.go", "/ruta/absoluta/archivo2.go"],
  "complexity_intent": "FEATURE_ADD"
})
```

Intents válidos: `O(1)_OPTIMIZATION`, `O(LogN)_SEARCH`, `FEATURE_ADD`, `BUG_FIX`.

**Reglas de certificación:**
- Certificar TODOS los archivos del batch en UNA sola llamada — no en rondas separadas.
- Para commits batch (>3 archivos): certificar INMEDIATAMENTE antes del `git commit` (no 10 minutos antes). El TTL de 15min se agota rápido en sesiones largas.
- Si el pre-commit hook rechaza por TTL expirado: re-certificar y hacer el commit en la misma secuencia sin demora.
- Si el lock file escribe a un subdirectorio (síntoma de binary stale): `make rebuild-restart` y copiar el stamp manualmente al root lock file.

**Intents — trampa conocida:**
- `O(1)_OPTIMIZATION` falla si el archivo tiene nested loops (aunque sean channels). Usar `FEATURE_ADD` para cualquier feature con control flow.

**Rollback:**
- `atomic` (default): revierte todos los archivos del batch si cualquiera falla.
- `granular`: revierte solo el archivo que falló.
- `none`: solo reporta el error.

**Dry run:**
- `dry_run: true` corre AST + build checks sin escribir el seal ni indexar al RAG. Safe pre-flight check para archivos aún en edición.

---

## 4. VALIDAR RESILIENCIA — opcional, recomendado tras cambios críticos

```json
neo_chaos_drill({
  "target": "http://127.0.0.1:9142/health",
  "aggression_level": 5,
  "inject_faults": true
})
```

Si Status = UNSTABLE: investigar con `AST_AUDIT` y `BLAST_RADIUS` antes de cerrar.

---

## 5. CIERRE DE SESIÓN

1. Certificar todos los archivos pendientes via `neo_sre_certify_mutation` (batch completo, una llamada).
2. Guardar lecciones aprendidas via `neo_memory(action: "commit")` (bugs no obvios, patrones arquitectónicos).
3. `neo_daemon(action: "Vacuum_Memory")` para defragmentar WAL (solo si mode=daemon).
4. Commit con formato: `feat(sre): descripción`.
5. Marcar tasks completadas en `master_plan.md`. El Kanban archivará automáticamente en REM sleep.
6. Si se identificaron herramientas con tasa de fallo >50%: agregar Épica de mejora al master_plan.

---

## Tabla completa: Neo vs herramientas nativas

| Tarea | Herramienta | Condición / Notas |
|-------|------------|-------------------|
| Explorar paquete desconocido | `COMPILE_AUDIT` | Siempre primero — da symbol_map + líneas |
| Búsqueda conceptual/abstracta | `SEMANTIC_CODE` | Si 0 resultados → Grep inmediato, no reintentar |
| Búsqueda exacta (símbolo, string) | `Grep` | Más confiable que SEMANTIC_CODE para exactas |
| Leer archivo < 100 líneas | `Read` nativo | — |
| Leer archivo ≥ 100 líneas | `READ_SLICE` | Con `start_line` del symbol_map de COMPILE_AUDIT |
| Impacto de un cambio | `BLAST_RADIUS` | Si `not_indexed` → Grep fallback, continuar |
| Auditoría calidad de código | `AST_AUDIT` | Obligatorio en BoltDB y código complejo. Acepta globs (`pkg/**/*.go`) |
| Verificar imports instanciados | `WIRING_AUDIT` | Después de agregar imports a `main.go` |
| Deuda técnica | `TECH_DEBT_MAP` | Antes de refactors amplios |
| Estado del proyecto | `BRIEFING` | — |
| Leer schema BD | `DB_SCHEMA` | — |
| Editar código | `Edit` / `Write` | — |
| Validar edición | `neo_sre_certify_mutation` | Batch completo, una llamada, justo antes del commit |
| Guardar lección | `neo_memory(action: "commit")` | Bugs no obvios, patrones, workarounds |
| Persistir regla arquitectónica | `neo_memory(action: "learn", action_type: "add")` | Dual-layer: BoltDB + `.claude/rules/` |
| Comprimir contexto | `neo_compress_context` | Tras 3+ ediciones o IO >500KB |
| Inspeccionar caches | `neo_cache(action: "stats")` | JSON con hits/misses/hit_ratio + top entries |
| Invalidar caches | `neo_cache(action: "flush")` | Tras edit manual sin certify |
| Warmup caches | `neo_cache(action: "warmup", from_recent: true)` | Pre-populate desde recent misses rings |
| Latencia por tool | `neo_tool_stats` | p50/p95/p99 + errors + calls. JSON o CSV |
| Buscar archivos por patrón | `Glob` | — |
| Ejecutar comandos | `Bash` o `neo_command(action: "run")` | `neo_command` si requiere aprobación humana |
| Chaos drill post-release | `neo_chaos_drill` | aggression 1-10 + inject_faults opcional |

---

## 6. Auditoría periódica (pre-commit / pre-release)

Después de cerrar un pilar o antes de una release, correr:

```bash
make audit       # staticcheck + ineffassign + modernize + coverage (25 pkgs)
make audit-ci    # fail-on-new vs .neo/audit-baseline.txt (CI gate)
```

Si hay NEW findings: resolver antes de merge. Si son intencionales: regenerar baseline con `make audit-baseline` tras cerrar la épica asociada.

---

## 7. Auto-Audit al final de cada épica

Al finalizar cada Épica o bloque de trabajo significativo, generar una sección 'Self-Audit' con:
1. Tabla de tools usadas con calificación 1-10 de utilidad real.
2. Identificación de la tool con peor rendimiento.
3. Propuesta concreta de mutación para esa tool (campo del schema o formato de respuesta).

El Self-Audit va ANTES del cierre de sesión, DESPUÉS de `neo_memory(action: "commit")`.
