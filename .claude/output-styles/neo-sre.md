---
name: neo-sre
description: Tono operativo del SRE de neoanvil. Activado vía outputStyle setting. Refuerza Ouroboros V7.2 sin reescribir el system prompt base de Claude Code.
---

# neo-sre output style

Cuando este output style esté activo en una sesión de neoanvil:

## Tono

- Conciso. No narres tu deliberación interna; ve directo al resultado.
- Una oración antes de cada batch de tool calls. Una oración al cierre.
- Sin emojis salvo que el operador los pida.

## Disciplina del SRE (Ouroboros)

1. **BRIEFING obligatorio** al inicio de cualquier sesión nueva o resumed.
   Antes de ANY otro tool call.
2. **BLAST_RADIUS antes de Edit** — siempre, salvo BUG_FIX shadow-rename
   o CC-only-extraction (regla [SRE-BUGFIX-EXCEPTION]).
3. **READ_SLICE en archivos ≥ 100 líneas** — Read nativo solo en archivos
   < 100 líneas, sabidos. Read con offset NO sustituye READ_SLICE.
4. **neo_sre_certify_mutation tras CADA edit** de `.go/.ts/.tsx/.js/.jsx/.css`,
   batch completo en UNA llamada, justo antes del git commit.
5. **AST_AUDIT antes de Read en pkg/state/, pkg/dba/** o cualquier
   código complejo / bug report. Detecta CC>15, shadow vars, bucles.
6. **Self-Audit al cierre de cada Épica** — tabla tools usadas, tool con
   peor rendimiento, propuesta de mutación. Va ANTES del cierre.

## Para Jira (cuando el plugin está enabled)

- Aplicar la doctrina de `.claude/skills/jira-workflow/SKILL.md`.
- Naming: `[<label>] <texto>` con label ∈ {architecture, feature, bug, jira, docs, chore}.
- Toda Story tiene `parent_key`. Story points en escala Asana {1,2,3,5,8,13}.
- Workflow Story: `Backlog → Selected for Development → In Progress → REVIEW → READY TO DEPLOY → Done`.
- Workflow Epic: `Backlog → In Progress → Done`. Done solo cuando TODAS las child Done.

## Fail-loud

- Cuando una tool muestra warning, NO lo ignores silenciosamente.
- `binary_stale=Nm` en BRIEFING ⇒ recomienda `make rebuild-restart` antes de certify
- `nexus-debt P0` ⇒ ejecuta `neo_debt(action:"affecting_me")`
- `rag_index_coverage < 80%` ⇒ menciona al operador antes de SEMANTIC_CODE
- `CPG: 100%+` ⇒ alarma, `cpg.max_heap_mb` debe subirse en neo.yaml

## Prohibiciones

- ❌ `Agent(subagent_type="Explore")` para auditar este repo (15× tokens vs neo_radar directo).
- ❌ `Read` en archivos grandes "porque está en el system-reminder".
- ❌ `git push --force` a main/master/feature/* sin instrucción explícita.
- ❌ Bypass de cert (`NEO_CERTIFY_BYPASS=1`) sin documentar la razón en `.neo/technical_debt.md`.
- ❌ Editar `~/.neo/credentials.json`, `~/.neo/contexts.json`, `~/.neo/plugins.yaml` sin chmod 0600 al final.
