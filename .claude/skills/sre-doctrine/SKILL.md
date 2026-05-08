---
name: sre-doctrine
description: Doctrina SRE Ouroboros V7.2 para neoanvil — flujo operativo (BRIEFING → BLAST_RADIUS → Edit → certify), modos pair/fast/daemon, leyes universales (zero-hardcoding, aislamiento MCP, atomic rollback). Use when starting a session, before editing code, or troubleshooting workflow gaps. Auto-loaded.
---

# Doctrina SRE — Ouroboros V7.2

> Reglas operativas obligatorias para todo agente operando neoanvil.
> Migrado de `.claude/rules/neo-synced-directives.md` (split temático
> 2026-04-28).

---

## CICLO OUROBOROS — flujo obligatorio por cambio

```
1. BRIEFING al inicio de sesión (obligatorio, también en resume)
2. BLAST_RADIUS antes de editar cualquier archivo
3. Edit/Write nativos (NO neo_apply_patch — deprecado)
4. neo_sre_certify_mutation con paths absolutos + complexity_intent
5. neo_chaos_drill opcional tras cambios críticos
```

**No editar sin investigar. No commit sin certificar.**

### BRIEFING obligatorio

La PRIMERA acción de cualquier sesión es `neo_radar(intent:
"BRIEFING")`. Esto aplica también cuando Claude Code reanuda desde un
resumen de contexto comprimido — el resumen NO reemplaza la
sincronización con el orquestador. Modo `compact` cuando Master Plan
esté cerrado para reducir IO.

### Resume Briefing Guard

Al reanudar desde contexto comprimido, primer tool call DEBE ser
BRIEFING. Sin excepciones. La ilusión de "ya sé del contexto" es la
causa raíz del fallo — el orquestador tiene estado que el resumen no
captura.

### Modos de operación

| Modo | NEO_SERVER_MODE | Edición | Certificación | neo_daemon | TTL seal |
|------|-----------------|---------|---------------|------------|----------|
| Pair | `pair` | Nativa | AST + Bouncer + Tests | PROHIBIDO | 15 min |
| Fast | `fast` | Nativa | Solo AST + Index | PROHIBIDO | 5 min |
| Daemon | `daemon` | Vía neo_daemon | Completa | Habilitado | 5 min |

`sre.certify_ttl_minutes` en neo.yaml para override.

---

## LEYES UNIVERSALES

### Zero-Hardcoding
PROHIBIDO IPs, localhost, puertos fijos en código. Todo viene de
`neo.yaml` o env vars. Resolución por búsqueda recursiva. Secretos en
`.neo/.env` (gitignored) + referencias `${VAR_NAME}` en yaml.

### Aislamiento MCP
NUNCA `fmt.Print` u `os.Stdout` en código MCP — destruye conexión
JSON-RPC. Usar exclusivamente `log.Printf`. Mutaciones van por
`neo_sre_certify_mutation`.

### Zero-Allocation
PROHIBIDO `make()` o `new()` en Hot-Paths (RAG, MCTS, HNSW).
`sync.Pool` y memoria plana. Slices reciclados con `[:0]`. Sin
`any`/`interface{}` innecesarios. No silenciar errores con `_ =`.

### Cyclomatic Complexity Cap = 15
`AST_AUDIT` enforce con SSA-exact (McCabe E-N+2) cuando CPG activo.
Falsos positivos del regex (AST CC>15 pero SSA ≤15) descartados
silenciosamente.

### Rollback Modes
`neo_sre_certify_mutation` acepta `rollback_mode`:
- `atomic` (default) — revierte TODOS los archivos del batch si falla
  cualquiera
- `granular` — revierte solo el archivo que falló
- `none` — solo reporta el error sin revertir

---

## TOKEN BUDGET

- Read nativo PROHIBIDO en archivos ≥ 100 líneas — usar
  `neo_radar READ_SLICE`. Read con offset/limit **NO es sustituto**
- 3+ ediciones seguidas sin `neo_compress_context` PROHIBIDO
- `BRIEFING mode:compact` cuando Master Plan cerrado
- `BRIEFING mode:delta` entre BRIEFINGs de la misma sesión (solo
  campos cambiados)
- Top token offenders medidos: BRIEFING ~185K en sesión larga; Read
  ≥200 líneas ~42K. FILE_EXTRACT context_lines:0 ~375 tokens vs 42K

---

## Pre-Audit ante bug

Ante reportes de bug o análisis complejo: PRIMERO
`neo_radar(intent:"AST_AUDIT", target:"ruta/archivo.go")`. Solo si
retorna sin issues, proceder con Read/Edit. Detecta CC>15, bucles
infinitos, variables shadow.

### AST_AUDIT obligatorio en BoltDB

Antes de editar `pkg/state/`, `pkg/dba/`, o cualquier código con
transacciones BoltDB/SQLite. Verifica cursor iteration correcta
(misuse común: `b.Cursor().Next()` en lugar de `c.Next()`),
transaction leaks, CC>15 en callbacks.

---

## Self-Audit obligatorio

Al finalizar cada Épica o bloque significativo: tabla tools usadas
con calificación 1-10, tool con peor rendimiento, propuesta concreta
de mutación. Va ANTES del cierre de sesión, DESPUÉS de
`neo_memory(action:"commit")`.

---

## Cuando algo se rompe

### MCP offline / SSE desconectado

```bash
curl -s -X POST http://127.0.0.1:9142/mcp/message \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{...}}'

# Pre-commit bypass cuando neo-mcp offline (registrado como bypass ⚠️)
NEO_CERTIFY_BYPASS=1 git commit -m "..."
```

### Binary stale

`BRIEFING` muestra `binary_stale=Nm` o `binary_age=Nm`. Si > 30 min,
recomendar `make rebuild-restart` antes de certify (TTL del seal va
al binary running).

### Workspace zombie / BoltDB lock

Síntoma: workspace falla al arrancar con "hnsw.db: timeout" o
"EWOULDBLOCK".

```bash
lsof +D .neo/db/ | grep -v COMMAND   # encontrar PID zombie
kill <PID>
curl -X POST http://127.0.0.1:9000/api/v1/workspaces/start/<id>
```

---

## See also

- [`skills/sre-tools/SKILL.md`](../sre-tools/SKILL.md) — referencia de las 14+ tools MCP
- [`skills/sre-quality/SKILL.md`](../sre-quality/SKILL.md) — leyes de calidad de código
- [`skills/jira-workflow/SKILL.md`](../jira-workflow/SKILL.md) — doctrina Jira
- [`output-styles/neo-sre.md`](../../output-styles/neo-sre.md) — tono SRE activable
- [`rules/neo-synced-directives.md`](../../rules/neo-synced-directives.md) — archivo legacy completo (incluye obsoletas)
