---
name: sre-troubleshooting
description: Recovery patterns + tool degradation rules + boot diagnosis para neoanvil. Use cuando una tool falle, un workspace no arranque, MCP esté offline, BLAST_RADIUS retorne not_indexed, SEMANTIC_CODE retorne 0, GRAPH_WALK retorne No reachable nodes, o SSE se desconecte.
---

# SRE Troubleshooting — degradation + recovery patterns

> Migrado de `.claude/rules/neo-synced-directives.md` (split temático
> 2026-04-28). Patterns de fallo detectados en producción + sus
> workarounds.

---

## Tool degradation rules

### `BLAST_RADIUS` retorna `graph_status: not_indexed`

NO bloquear edición. Continuar con `confidence: low`.
- Usar `Grep` para callers manualmente
- Certify después para reindex
- Si `rag_index_coverage < 80%`: mencionar al operador

Campos de respuesta indican calidad:
- `fallback_used` (none/grep/ast)
- `confidence` (high/medium/low/none)
- `index_coverage` (% del workspace)

`confidence: low` = solo scan de imports, no PageRank — orientativo.

### `BLAST_RADIUS` falla por SSRF (`[SRE-SSRF FATAL]`)

Causa raíz: binary stale de neo-nexus sin `SafeInternalHTTPClient`.

**Fix:** `make rebuild-restart` (detecta stale automáticamente).
**Mientras tanto:** Grep + COMPILE_AUDIT como sustitución.

### `SEMANTIC_CODE` retorna 0

NO reintentar con otra frase. El problema es cobertura del índice,
no la query. Cambiar INMEDIATAMENTE a `Grep`. SEMANTIC_CODE solo
para queries verdaderamente abstractas/conceptuales.

Para cualquier búsqueda con nombre de función, símbolo o string
específico: usar Grep directamente sin pasar por SEMANTIC_CODE.

### `GRAPH_WALK` retorna `No reachable nodes` (con CPG activo)

Limitación SSA documentada — common en receiver methods + funciones
con solo stdlib calls. NO es un bug — es inherente a cómo SSA lower
las llamadas.

**Workaround:** `BLAST_RADIUS target=<file.go>` para callers reversos.

### `INCIDENT_SEARCH` HNSW tier devuelve 0

Ollama probablemente offline al boot → `IndexIncidents()` no corrió.
Default cascade ya hace fallback a text_search. Si aún 0, verificar
que hay `INC-*.md` en `.neo/incidents/`.

**Forzar BM25:** `force_tier: "bm25"` (funciona sin Ollama).

---

## MCP / SSE recovery

### MCP offline / SSE desconectado

```bash
# Verificar
curl -s http://127.0.0.1:9142/health     # child neo-mcp
curl -s http://127.0.0.1:9000/status     # todos los children

# Bypass via curl directo (sin SSE transport)
curl -s -X POST http://127.0.0.1:9142/mcp/message \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{...}}'

# Pre-commit bypass (registrado como ⚠️ en heatmap)
NEO_CERTIFY_BYPASS=1 git commit -m "..."
```

### SSE Transport — dos canales

NeoAnvil expone:
- **stdio** primario (Claude Code lo invoca via `.mcp.json type:stdio`)
- **SSE** via Nexus dispatcher (`:9000/mcp/sse → hijo en puerto dinámico`)

Producción: Claude Code → Nexus :9000/mcp/sse → child neo-mcp.
Un proceso neo-mcp = un workspace fijo. Nexus gestiona pool con
`managed_workspaces` filter.

OAuth proxy en Nexus: `/.well-known/` + `/oauth/` se reenvían al
hijo activo.

Acceso directo (sin Nexus): `http://127.0.0.1:9142/mcp/sse`.

---

## Boot Diagnosis — workspace no arranca

### Síntomas
- `hnsw.db: timeout` o `EWOULDBLOCK` después de `make rebuild-restart`
- `verifyBoot timeout` en nexus log
- BoltDB lock zombie

### Procedimiento
```bash
# 1. Encontrar PID zombie
lsof +D .neo/db/ | grep -v COMMAND

# 2. Kill el proceso
kill <PID>

# 3. Restart via API
curl -X POST http://127.0.0.1:9000/api/v1/workspaces/start/<id>
```

### Doble-Nexus diagnosis
Si `pgrep -la neo-nexus` muestra dos procesos → matar el de PID más
bajo primero.

**Root cause:** `verifyBoot()` en Nexus no mata el proceso hijo
cuando el health-check expira. El procedimiento manual es
obligatorio (Épica 267 lo resolverá).

---

## RESUME-BRIEFING-GUARD

Al reanudar desde contexto comprimido ("This session is being
continued..."), el primer tool call DEBE ser `neo_radar BRIEFING`
— sin excepciones.

Si el contexto ya contiene info sobre el estado del proyecto, esa
información es orientativa pero NO reemplaza la sincronización.
Check concreto: antes de cualquier Read/Edit/Grep/Bash, verificar
internamente si BRIEFING ya corrió en esta sesión. Si no → BRIEFING
primero.

**Why:** la ilusión de "ya sé suficiente del contexto" es la causa
raíz del fallo — el orquestador tiene estado que el resumen no
captura: `binary_stale`, RAM real, `session_mutations`, INC-IDX.

**How to apply:** Primera acción de cualquier sesión o reanudación
= BRIEFING. Sin excepción. Incluye sesiones que parecen
"continuación obvia" de un trabajo previo.

---

## READ-SIZE-CHECK

Antes de usar Read nativo, verificar tamaño del archivo:

- ≥ 100 líneas → `READ_SLICE` con `start_line` del symbol_map
- < 100 líneas → Read nativo OK (única excepción)
- Si desconocido → `COMPILE_AUDIT` primero (retorna symbol_map +
  confirma que el paquete compila)

El hecho de que el contenido ya esté en contexto (system-reminder,
session summary) NO justifica usar Read nativo — el hábito correcto
debe mantenerse para que sea consistente en sesiones donde el
archivo NO esté en contexto.

**Why:** una violación pasada (sesión 2026-04-18) usó Read nativo
en `audit.go` (384 líneas) justificándolo porque el contenido
estaba en system-reminder. Aceptable ocasional pero forma hábito
incorrecto.

---

## CONTEXT-EFFICIENCY

Para archivos grandes (>200 líneas): preferir `FILE_EXTRACT` sobre
`READ_SLICE` cuando se quiere leer un símbolo específico. Flujo
óptimo:

1. `COMPILE_AUDIT` para symbol_map con línea exacta
2. `FILE_EXTRACT(target, query:simbolo, context_lines:0)` para
   cuerpo completo del símbolo
3. `FILE_EXTRACT(context_lines:5)` para snippets con contexto

`READ_SLICE` queda para casos donde NO hay un símbolo nombrado
(leer un bloque de inicialización, un import block, etc.).

Para archivos como `radar_handlers.go (~4000 líneas)`: NUNCA leer
el archivo completo — targetear el símbolo via COMPILE_AUDIT +
FILE_EXTRACT. Si ya fue leído en la sesión, NO hacer Read nuevo —
el contenido ya está en contexto.

---

## TOOL-SELECTION patterns

### Primer contacto con paquete desconocido
1. `COMPILE_AUDIT` (retorna symbol_map con línea exacta)
2. `READ_SLICE` con `start_line` del symbol_map — lectura
   quirúrgica O(1)
3. PROHIBIDO `READ_SLICE` desde línea 1 a ciegas

### COMPILE_AUDIT_FIRST
Cuando se va a leer un paquete desconocido o buscar un símbolo en
archivo grande: ejecutar COMPILE_AUDIT primero. Más eficiente que
READ_SLICE a ciegas y más rápido que SEMANTIC_AST para exploración
inicial.

### TOOL-COST-AUDIT
PROHIBIDO usar el subagente Explore de Claude Code para auditar
este repositorio. El subagente cuesta ~31.5k tokens en un solo
sweep vs ~2k usando neo_radar/neo_log_analyzer directamente.

Flujo correcto para audit:
1. `AST_AUDIT` en batch por directorio (eg: `pkg/sre/`, `pkg/rag/`)
2. `COMPILE_AUDIT` para cert status + symbol_map
3. `TECH_DEBT_MAP` para hotspots
4. `WIRING_AUDIT` post-import
5. `neo_log_analyzer` para INC files

NUNCA delegar audit a `Agent(subagent_type=Explore)` en neoanvil.

---

## CERTIFY-DX patterns

### CERTIFY_BATCH_TIMING
Para commits con múltiples archivos:
1. Certificar TODOS en una sola llamada (no en rondas)
2. Ejecutar la certificación INMEDIATAMENTE antes del git commit
   (TTL 15min se agota en sesiones largas)
3. Si pre-commit rechaza por TTL: re-certify y commit en la misma
   secuencia sin pausa

Si el lock file escribe a subdirectorio (síntoma de binary stale):
reconstruir binario y copiar stamps manualmente al root lock file
hasta que el binario fresco esté disponible.

### SRE-BUGFIX-EXCEPTION
Para cambios clasificados como `BUG_FIX` de tipo shadow-var-rename
o CC-only-extraction: OMITIR `BLAST_RADIUS` previo. Aplica solo
cuando:
- No hay cambio de firma pública
- Solo renombrado de variable interna o extracción a helper privado

NO aplica si:
- Afecta flujo compartido (green path certify, boot, función con
  múltiples callers)
- Se añaden nuevos parámetros aunque sea internos
- El archivo tiene >5 callers directos

Test rápido: ¿el cambio ejecuta código nuevo en runtime para TODOS
los usuarios? Si sí → BLAST_RADIUS obligatorio.

---

## See also

- `skills/sre-doctrine/SKILL.md` — flujo Ouroboros (BRIEFING obligatorio)
- `skills/sre-tools/SKILL.md` — referencia tool por tool
- `skills/sre-federation/SKILL.md` — workspace zombies, federation issues
- `skills/sre-quality/SKILL.md` — leyes que cuando se violan disparan veto
