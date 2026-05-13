#!/bin/bash
# UserPromptSubmit hook: detect multi-file / multi-feature / complex-scope
# prompts and inject a reminder to invoke DeepSeek red_team_audit BEFORE the
# agent starts editing. Enforces directive [DS-PREMORTEM-MULTI-FEATURE].
#
# Why this exists (lesson 2026-05-13):
#   Agent (Claude) shipped commit d8e62c2 with an incomplete SHUTDOWN HNSW
#   fix because no premortem caught the racing os.Exit(0). Fix was completed
#   in 2d75c03 — cost 1 extra commit + 1 extra restart cycle.
#
# Filter strategy:
#   - Prompt ≥ 80 chars (skip "ok", "procede", "go")
#   - Match ≥ 1 trigger keyword (Spanish + English variants)
#   - Skip if prompt already mentions "deepseek" / "premortem" (agent aware)
#
# Output JSON with hookSpecificOutput.additionalContext when triggers fire,
# or empty exit 0 when prompt is short / no triggers.
#
# bash 3.2 safe: no ${VAR,,}, no heredoc-vs-stdin conflict. jq for JSON I/O.

set -uo pipefail

[ "${NEO_DS_PREMORTEM_DISABLE:-0}" = "1" ] && exit 0

INPUT="$(cat 2>/dev/null)"
[ -z "$INPUT" ] && exit 0

# Extract prompt via jq (stdin-safe, no python heredoc conflict).
# Empty string when missing/null — falls through to PROMPT_LEN check.
PROMPT=$(printf '%s' "$INPUT" | jq -r '.prompt // ""' 2>/dev/null)
[ -z "$PROMPT" ] && exit 0

# Filter 1: prompt must be substantial. Short prompts are usually
# follow-ups / acks ("procede", "yes", "make realizado") — no plan
# to premortem.
PROMPT_LEN=${#PROMPT}
[ "$PROMPT_LEN" -lt 80 ] && exit 0

# Lowercase via tr (bash 3.2 safe; ${VAR,,} requires bash 4+).
PROMPT_LC=$(printf '%s' "$PROMPT" | tr '[:upper:]' '[:lower:]')

# Filter 2: skip when prompt mentions DeepSeek or premortem — the agent
# has already been told. Avoids noise on second turn.
case "$PROMPT_LC" in
  *deepseek*|*premortem*|*pre-mortem*|*red_team*|*red-team*)
    exit 0 ;;
esac

# Filter 3: trigger keywords. ≥1 match → fire reminder.
TRIGGER_HITS=0
for kw in \
  "refactor" \
  "refactori" \
  "subsystem" \
  "subsistem" \
  "sharding" \
  "shard" \
  "race condition" \
  "concurren" \
  "race-condition" \
  "lazy wal" \
  "lazy-wal" \
  "boot loader" \
  "shutdown" \
  "sigterm" \
  "multi-file" \
  "varios archivos" \
  "multiple files" \
  "feature work" \
  "tier 1" \
  "tier 2" \
  "tier 1d" \
  "tier 2e" \
  "tier 2f" \
  "epica" \
  "épica" \
  "redesign" \
  "rediseñ" \
  "architecture" \
  "arquitectur" \
  "hot-path" \
  "hot path" \
  "implementar" \
  "implementemos"; do
  case "$PROMPT_LC" in
    *"$kw"*) TRIGGER_HITS=$((TRIGGER_HITS + 1)) ;;
  esac
done

[ "$TRIGGER_HITS" -lt 1 ] && exit 0

# Build the reminder context. Árbol de decisión multi-layer red_team:
# neo expone 4 capas adversariales complementarias — no son redundantes,
# operan a niveles distintos (AST / runtime / OS-level / semantic LLM).
# El hook recomienda la capa correcta según pista de prompt.
CTX="[ouroboros-hook · RED-TEAM-LAYERING] Prompt sugiere scope multi-file / complejo (${TRIGGER_HITS} trigger(s), ${PROMPT_LEN} chars). neo tiene **4 capas adversariales** — no usarlas como sinónimo:

  ┌─────────────────────┬──────────────────────────────────────┬─────────────────────┐
  │ Capa                │ Cuándo                                │ Cómo invocar        │
  ├─────────────────────┼──────────────────────────────────────┼─────────────────────┤
  │ 1. Bouncer (AST)    │ AUTO en cada certify_mutation        │ (nothing — auto)    │
  │ 2. DreamCycle       │ AUTO en REM sleep (5 min idle)       │ (nothing — auto)    │
  │ 3. neo_chaos_drill  │ Mutación a surface HTTP / endpoint   │ MANUAL — tool call  │
  │ 4. DS red_team_audit│ Nuevo subsistema / concurrencia /    │ MANUAL — tool call  │
  │                     │ SIGTERM / boot / hot-path semántico  │                     │
  └─────────────────────┴──────────────────────────────────────┴─────────────────────┘

PRE-EDIT (capa 4 — frontier reasoning):
  mcp__neoanvil__deepseek_call(
    action: \"red_team_audit\",
    thread_id: \"ds_thread_e28c2310246d72ed\",  ← reuses cache, 50× cheaper
    target_prompt: \"<plan: files, design constraints, expected commits>\",
    reasoning_effort: \"high\",
    model: \"deepseek-v4-flash\"
  )

POST-DEPLOY de mutaciones a HTTP surface (capa 3 — runtime siege):
  mcp__neoanvil__neo_chaos_drill(
    target: \"http://127.0.0.1:<port>/<endpoint>\",
    aggression_level: 5,    ← 5000 goroutines × 10s
    inject_faults: true     ← packet loss / latency / OOM simulation
  )

DS audit retorna GO / DEFER + hidden complexity. chaos_drill retorna p99 latency + crash signatures. Si ambos pasan → ship. Si DS=DEFER → reducir scope. Si chaos_drill=crash → fix antes de merge.

SKIP capa 4 si: bug-fix 1 archivo, doc-only, refactor cosmético. APLICAR si: ≥3 archivos, ≥2 commits, hot-path, SIGTERM/boot/recovery, supply-chain, crypto.
SKIP capa 3 si: edit no toca HTTP/SSE/gRPC/socket surface. APLICAR si: cambia handler, middleware, router, plugin dispatch, Nexus proxy.

Lección 2026-05-13: NO invocar DS premortem costó 1 commit incompleto (d8e62c2 → superseded por 2d75c03) + 1 restart. NO invocar chaos_drill tras mutar surface = riesgo p99 silente hasta producción. Bouncer + DreamCycle son automáticos — confiables. Las 2 manuales son tu responsabilidad."

# Emit JSON envelope via jq (avoids quote-escape hell of inline python).
jq -nc --arg ctx "$CTX" '{
  hookSpecificOutput: {
    hookEventName: "UserPromptSubmit",
    additionalContext: $ctx
  }
}'

exit 0
