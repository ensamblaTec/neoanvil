# ADR-016 — Ouroboros lifecycle hooks: enforcement automático del flujo SRE

**Status:** Implemented.
**Date:** 2026-05-13.
**Driver:** session self-audit que reveló violaciones del [CICLO-OUROBOROS] en auto-mode.

## Context

El flujo Ouroboros (BRIEFING → BLAST_RADIUS → Edit/Write → certify) era hasta hoy **advisory** — documentado en `skills/sre-workflow/SKILL.md` y `CLAUDE.md`, pero sin mecanismo que lo enforcara. Una self-auditoría de sesión (2026-05-13) confirmó:

- 32 tool calls nativos (Bash grep, Read, Edit) vs 4 tools neo en una tarea explícita de "auditoría". Ratio neo:nativos = 12.5%.
- Edit en `cmd/neo-mcp/radar_folder_audit.go` **sin BLAST_RADIUS previo**.
- 5 invocaciones de `grep -rn` sin probar SEMANTIC_CODE primero, violando [SEMANTIC_CODE_FALLBACK].

**Causas raíz combinadas:**

1. **Auto-mode bias** — Claude 4.7 en auto-mode prioriza respuestas directas. Cuando la tarea parece "ligera", el modelo down-pondera overhead procedural.
2. **Refactor skill-first** — moví doctrina de `rules/neo-workflow.md` (siempre cargado) a `skills/sre-workflow/SKILL.md` (frontmatter visible, body solo on-trigger). Eso debilitó el enforcement signal.
3. **Tool friction perceptual** — `grep -rn` da feedback en ~1s; `neo_radar(SEMANTIC_CODE)` requiere formatear query + roundtrip MCP. Bajo presión cognitiva, el shortest path gana.
4. **No hay enforcement real-time** — solo existe `SessionStart` hook (briefing.sh). Sin PreToolUse / PostToolUse / Stop, la doctrina depende de la disciplina momentánea del agent.

## Decision

Implementar **3 nuevos lifecycle hooks** en `.claude/settings.json` que enforcen Ouroboros de forma automática, sin requerir invocación manual del agent (`/skill-name`).

```
SessionStart                            → briefing.sh (existente)
PreToolUse:Edit|Write|MultiEdit         → pre-edit-blast.sh (NUEVO)
PostToolUse:Edit|Write|MultiEdit        → post-edit-cert-reminder.sh (NUEVO)
Stop                                    → stop-cert-gate.sh (NUEVO)
```

### Diseño de cada hook

**`pre-edit-blast.sh`** (PreToolUse:Edit|Write|MultiEdit):

- Recibe JSON en stdin con `tool_input.file_path`.
- **Skip silencioso** para doc-only edits (`.md`, `.yaml`, `.json`, `.txt`, `.toml`, `.ini`, etc).
- **TTL cache 5min** en `.neo/blast_cache.json` (`{path: unix_ts}`). Evita re-correr BLAST_RADIUS en mismo file durante una sesión de edits seguidos.
- **Cache hit:** imprime _"BLAST_RADIUS cached (Ns ago)"_, exit 0.
- **Cache miss:** curl POST `neo_radar(BLAST_RADIUS, target=<file>)` con timeout 10s, imprime markdown a stdout → se inyecta al contexto del agent **antes** que Edit ejecute.
- **Fail-open** si Nexus offline — imprime warning, exit 0. NUNCA bloquea (exit 2 reservado para violaciones explícitas que decidamos enforcear más adelante).
- Eviction automático en cache (entries > 24h).

**`post-edit-cert-reminder.sh`** (PostToolUse:Edit|Write|MultiEdit):

- Recibe JSON con `file_path`.
- Skip si no es código productivo.
- **Append** path a `.neo/session_pending_cert.list` (dedupe).
- Imprime one-line reminder: `⏳ pending certify: <file> (TTL 15min in pair mode)`.
- TTL leído de `.neo/mode` o `NEO_SERVER_MODE` env.

**`stop-cert-gate.sh`** (Stop):

- Lee `.neo/session_pending_cert.list` vs `.neo/db/certified_state.lock`.
- Si hay pending paths sin sello → imprime banner con la lista de uncertified files + reminder de `neo_sre_certify_mutation`.
- **Soft warn** (exit 0, NO bloquea) — el pre-commit hook ya bloquea en `git commit` time. Este hook solo da visibilidad si el operador no llega al commit.
- Limpia la lista pending al final (lock file es authoritative going forward).

### Env overrides comunes

| Var | Default | Propósito |
|-----|---------|-----------|
| `NEO_NEXUS_URL` | `http://127.0.0.1:9000` | Dispatcher base URL |
| `NEO_WORKSPACE_ID` | `neoanvil-9b272` | Workspace target |
| `NEO_BLAST_HOOK_DISABLE` | `0` | Set `1` para skip total (debug) |
| `NEO_CERT_HOOK_DISABLE` | `0` | Set `1` para skip post-edit + stop |
| `NEO_BLAST_HOOK_TTL_SECONDS` | `300` | TTL cache override |
| `NEO_REPO_ROOT` | `git rev-parse --show-toplevel` | Repo path |

## Consequences

**Positive**

- **El flujo es ahora estructural, no documentación.** El agent VE el BLAST_RADIUS injectado antes de poder ejecutar Edit. No puede "olvidar" — el hook ya lo hizo.
- **Cubre cualquier sub-agent, cualquier modo (pair/fast/daemon)** sin reescribir doctrina.
- **Skip filter elimina overhead en doc edits** (.md/.yaml). El 80% de los edits en una sesión típica son docs.
- **TTL cache** elimina BLAST_RADIUS duplicados en edits seguidos al mismo file.
- **Stop hook + Pre-commit hook + Post-edit hook** forman un trip-wire de 3 capas. Si una falla, las otras 2 atrapan.
- **No requiere user invocation** — totalmente automático.

**Negative**

- **Latencia ~50-200ms por Edit** en código (BLAST_RADIUS roundtrip MCP, cuando cache miss).
- **Si Nexus está offline**, los hooks fail-open con warning — los edits proceden sin BLAST_RADIUS. Mitigación: el operador debe ejecutar BRIEFING manual y verificar peer/Nexus health antes de sesiones críticas.
- **Riesgo de hook roto** — un script con bug puede generar ruido en cada Edit. Mitigación: tests manuales con JSON simulado antes de commit (incluidos en este commit).
- **State files** (`blast_cache.json`, `session_pending_cert.list`) en `.neo/` — gitignored. No commitear nunca.

**Trade-offs intencionales**

- **Stop hook es soft-warn, no blocking.** El pre-commit hook ya bloquea en commit time. Stop blocking sería invasivo para sesiones experimentales.
- **PreToolUse fail-open.** Bloquear el Edit cuando Nexus está down paralizaría la sesión. Aceptamos el riesgo y dejamos que la disciplina del operador + pre-commit gate cierre el agujero.

## Implementation notes

- **Tooling:** bash + curl + python3 (jq no instalado en el environment; python3 disponible siempre).
- **Lectura JSON stdin:** mismo patrón que `briefing.sh` (python3 -c con tolerancia a errores).
- **Cache file format:** JSON simple `{"/path/to/file.go": 1747095123}`. Eviction de entries > 24h al actualizar.
- **Test manual:** cada hook acepta JSON simulado vía stdin. Ver tests en `scripts/hook_smoke_test.sh` (TODO follow-up).

## Revision 2026-05-13 — CRITICAL bug: hook output must be JSON, not markdown

### Empirical observation

Después del commit inicial 6524e0a, validación end-to-end reveló:
- Hooks **SÍ ejecutan** en respuesta a Edit (state files .neo/blast_cache.json y .neo/session_pending_cert.list se populan correctamente).
- **PERO** el markdown stdout NO aparece en el contexto del agent (la inyección visible no ocurre).

### Root cause (via WebFetch oficial docs)

Claude Code parsea el stdout del hook como JSON. Si el JSON tiene `hookSpecificOutput.additionalContext`, ese string se inyecta al contexto del modelo. **Plain markdown a stdout se silencia** — el parser falla y el hook se trata como side-effect-only.

Schema oficial PreToolUse hook output:
```json
{
  "hookSpecificOutput": {
    "hookEventName": "PreToolUse",
    "permissionDecision": "allow" | "deny" | "ask" | "defer",
    "permissionDecisionReason": "user-only string (NOT visible to Claude)",
    "additionalContext": "string injected into Claude's context",
    "updatedInput": { /* modified tool input */ }
  }
}
```

**Distinción crítica:**
- `permissionDecisionReason` → visible **al usuario solamente** (permission dialog/notification)
- `additionalContext` → visible **al modelo (Claude) solamente** (inyectado al context window)

Exit code semantics:
- `exit 0` + JSON stdout → Claude Code parsea, aplica decision + injection
- `exit 0` sin output → no-op (proceed silently)
- `exit 2` → BLOCKING. stderr se inyecta al modelo como error message. PreToolUse cancela el tool call.
- Cualquier otro exit → non-blocking error, log only.

### Fix aplicado

Los 3 hooks reescritos para emitir JSON en lugar de markdown plain:

- `pre-edit-blast.sh`: wraps el BLAST_RADIUS markdown en `hookSpecificOutput.additionalContext` con `permissionDecision: "allow"`. Doc-only edits siguen siendo skip silente (exit 0 sin output, no JSON, no-op).
- `post-edit-cert-reminder.sh`: wraps el reminder en `hookSpecificOutput.additionalContext` con `hookEventName: "PostToolUse"`.
- `stop-cert-gate.sh`: wraps el banner uncertified en `hookSpecificOutput.additionalContext` con `hookEventName: "Stop"`.

### Lección estructural

El feature request anthropics/claude-code#12623 ("Non-blocking PreToolUse hooks that inject context") aún está abierto en la community, lo que sugiere que el patrón de `additionalContext` para inyección no-bloqueante es el path canónico documentado pero todavía evolucionando. La diferencia `permissionDecisionReason` vs `additionalContext` no es obvia desde fuera — el "Claude visibility" del segundo es lo que permite enforcement real.

## Verification (post-revision)

Tests manuales con JSON simulado vía stdin:
- `pre-edit-blast.sh` con `.md` → exit 0 sin output (skip silente correcto)
- `pre-edit-blast.sh` con `cmd/neo-mcp/main.go` → JSON válido con `additionalContext` conteniendo BLAST_RADIUS scatter-gather de 3 workspaces + CPG analysis ✓
- `post-edit-cert-reminder.sh` → JSON con `additionalContext` reminder ✓
- `stop-cert-gate.sh` → JSON con `additionalContext` banner uncertified ✓

End-to-end validation pendiente: requiere otro Edit en .go productivo post-revision para confirmar que el contexto inyectado SÍ aparece al modelo.

## Open questions / follow-ups

1. **Cache invalidation:** ¿debe `neo_sre_certify_mutation` invalidar el blast_cache entry del file que certificó? Hoy depende del TTL (5min). Probablemente sí, post-MVP.
2. **PostToolUse certify auto:** ¿debería el post-edit hook auto-disparar `neo_sre_certify_mutation` batched al final de la sesión? Riesgo: cert prematura si el operador iba a editar más. Defer.
3. **UserPromptSubmit hook:** podría inyectar un warning si el último N de edits no se certificaron. Probablemente over-engineering — el Stop hook ya cubre el caso.
4. **Hard enforcement vs soft inject:** si el modelo aún ignora el `additionalContext` por bias auto-mode, escalar a `permissionDecision: "deny"` cuando BLAST_RADIUS detecta `impacted_count > N`. Trade-off: friction al user (tool call falla, requiere retry). Defer hasta ver evidencia de no-compliance post-revision.
5. **Empirical compliance test:** ¿el agent realmente cambia comportamiento al ver el contexto inyectado? Necesita medir N edits con/sin hook activo, contar cuántos llevaron a manual investigation antes del edit. Post-MVP.

## References

- `[CICLO-OUROBOROS]` directive en `.claude/rules/neo-synced-directives.md`
- `[SRE-BRIEFING]`, `[SRE-CERTIFY]`, `[LEY-PAIR-MODE]`
- `[OUROBOROS-NO-GREP-SHORTCUT]` directive (añadida 2026-05-13)
- Self-audit transcript de la sesión 2026-05-13 (commits 821bb4c→b31783b)
- Claude Code hooks docs: PreToolUse, PostToolUse, Stop
