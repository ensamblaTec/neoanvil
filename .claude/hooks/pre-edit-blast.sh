#!/bin/bash
# PreToolUse hook: auto BLAST_RADIUS antes de Edit/Write/MultiEdit en código productivo.
# Implementa enforcement automático de [CICLO-OUROBOROS] (BLAST_RADIUS antes de edit)
# sin requerir invocación manual del agent.
#
# Triggered por Claude Code PreToolUse:Edit|Write|MultiEdit. Recibe JSON en stdin
# con tool_input.file_path. Imprime JSON con `hookSpecificOutput.additionalContext`
# (no markdown raw) — el formato oficial de Claude Code para inyectar al contexto
# del modelo. Ver docs/adr/ADR-016 + https://code.claude.com/docs/en/hooks.
#
# Bug raíz descubierto 2026-05-13: imprimir markdown raw a stdout NO inyecta al
# contexto del agent. Claude Code parsea stdout como JSON y solo inyecta el
# string en `hookSpecificOutput.additionalContext`. Plain markdown se silencia.
#
# Comportamiento:
#   - Skip silente para doc-only edits (.md, .yaml, .json, etc) — exit 0 sin output.
#   - TTL cache 5min en .neo/blast_cache.json — evita re-correr BLAST_RADIUS
#     en mismo file durante una sesión de edits seguidos. Cache hit: emite
#     additionalContext minimal recordando el cache, NO re-llama Nexus.
#   - Cache miss: curl POST BLAST_RADIUS, emite JSON con markdown en
#     additionalContext, exit 0 con permissionDecision=allow.
#   - Fail-open si Nexus offline — emite warning en additionalContext, exit 0.
#     NUNCA bloquea (exit 2 reservado para violaciones explícitas que decidamos
#     enforcear en una iteración futura del ADR).
#
# Env overrides:
#   NEO_NEXUS_URL              base URL del dispatcher (default 127.0.0.1:9000)
#   NEO_WORKSPACE_ID           workspace target (default neoanvil-9b272)
#   NEO_BLAST_HOOK_DISABLE     set a 1 para skip total (debug)
#   NEO_BLAST_HOOK_TTL_SECONDS override TTL cache (default 300)
#   NEO_REPO_ROOT              repo path (default git rev-parse --show-toplevel)
#
# Spec: ADR-016 (revision 2026-05-13: JSON output format).

set -uo pipefail

[ "${NEO_BLAST_HOOK_DISABLE:-0}" = "1" ] && exit 0

NEXUS_URL="${NEO_NEXUS_URL:-http://127.0.0.1:9000}"
WORKSPACE_ID="${NEO_WORKSPACE_ID:-neoanvil-9b272}"
TTL_SECONDS="${NEO_BLAST_HOOK_TTL_SECONDS:-300}"
REPO_ROOT="${NEO_REPO_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null)}"

# emit_json: wraps a context string in the Claude Code hookSpecificOutput envelope.
# All output to stdout MUST be valid JSON (any non-JSON stdout makes Claude Code
# ignore the hook output silently — that was the bug in v1).
emit_json() {
  local ctx="$1"
  python3 -c "
import json, sys
ctx = sys.argv[1]
payload = {
  'hookSpecificOutput': {
    'hookEventName': 'PreToolUse',
    'permissionDecision': 'allow',
    'additionalContext': ctx,
  }
}
print(json.dumps(payload))
" "$ctx"
}

# Read JSON from stdin — defensive parsing.
INPUT="$(cat 2>/dev/null)"
[ -z "$INPUT" ] && exit 0

FILE_PATH=$(printf '%s' "$INPUT" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
    p = d.get("tool_input", {}).get("file_path", "")
    print(p, end="")
except Exception:
    pass
' 2>/dev/null)

[ -z "$FILE_PATH" ] && exit 0

# Filter: only fire on productive code files. Doc/config edits skip silently.
case "${FILE_PATH,,}" in
  *.go|*.ts|*.tsx|*.js|*.jsx|*.py|*.rs|*.css) ;;
  *) exit 0 ;;
esac

# TTL cache lookup. Format: one JSON object {"<path>": <unix_ts>, ...}.
CACHE_DIR="${REPO_ROOT}/.neo"
CACHE_FILE="${CACHE_DIR}/blast_cache.json"
mkdir -p "$CACHE_DIR" 2>/dev/null

NOW=$(date +%s)
CACHED=$(python3 -c "
import json, os, sys
cache_file = '$CACHE_FILE'
path = '$FILE_PATH'
ttl = $TTL_SECONDS
now = $NOW
try:
    with open(cache_file) as f:
        cache = json.load(f)
except (FileNotFoundError, json.JSONDecodeError):
    cache = {}
ts = cache.get(path, 0)
if isinstance(ts, (int, float)) and (now - ts) < ttl:
    print('HIT', int(now - ts))
else:
    print('MISS')
" 2>/dev/null)

BASENAME=$(basename "$FILE_PATH")

if [[ "$CACHED" == HIT* ]]; then
  AGE=${CACHED#HIT }
  emit_json "[ouroboros-hook] BLAST_RADIUS cached (${AGE}s ago, TTL=${TTL_SECONDS}s) for \`${BASENAME}\`. Proceeding with Edit — agent should recall the prior impact assessment from this session."
  exit 0
fi

# Probe Nexus liveness — tight timeout, fail-open.
if ! curl -fsS --max-time 2 -o /dev/null "${NEXUS_URL}/health" 2>/dev/null; then
  emit_json "[ouroboros-hook] ⚠️ Nexus unreachable — BLAST_RADIUS SKIPPED for \`${BASENAME}\`. Agent: investigate impact MANUALLY (Grep callers, check imports) if the change is non-trivial. This is a degraded session."
  exit 0
fi

# Run BLAST_RADIUS via MCP message endpoint.
RESP=$(curl -fsS --max-time 10 \
  -H "Content-Type: application/json" \
  -H "X-Neo-Workspace: ${WORKSPACE_ID}" \
  -X POST "${NEXUS_URL}/mcp/message" \
  -d "$(python3 -c "
import json
print(json.dumps({
    'jsonrpc': '2.0',
    'id': 1,
    'method': 'tools/call',
    'params': {
        'name': 'neo_radar',
        'arguments': {'intent': 'BLAST_RADIUS', 'target': '$FILE_PATH'},
    },
}))
")" 2>/dev/null) || {
  emit_json "[ouroboros-hook] BLAST_RADIUS request failed for \`${BASENAME}\` (curl error). Fail-open: edit proceeds. Agent: assess impact manually."
  exit 0
}

# Extract the Markdown payload from the JSON-RPC envelope. Tolerant of shape drift.
PAYLOAD=$(printf '%s' "$RESP" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
    parts = d.get("result", {}).get("content", [])
    print("".join(p.get("text", "") for p in parts if isinstance(p, dict)))
except Exception:
    sys.exit(1)
' 2>/dev/null) || {
  emit_json "[ouroboros-hook] BLAST_RADIUS response malformed for \`${BASENAME}\`. Fail-open: edit proceeds. Agent: assess impact manually."
  exit 0
}

# Update cache (best-effort, fail-silent).
python3 -c "
import json, os
cache_file = '$CACHE_FILE'
path = '$FILE_PATH'
now = $NOW
try:
    with open(cache_file) as f:
        cache = json.load(f)
except (FileNotFoundError, json.JSONDecodeError):
    cache = {}
cache[path] = now
# Evict entries >24h old to keep file bounded.
cache = {k: v for k, v in cache.items() if now - v < 86400}
with open(cache_file, 'w') as f:
    json.dump(cache, f)
" 2>/dev/null

# Build the context with a header marker so the agent recognizes this as
# hook-injected (not their own action). The full BLAST_RADIUS markdown
# follows so the agent has the impact map BEFORE deciding the edit.
HEADER="[ouroboros-hook] Auto-BLAST_RADIUS for \`${BASENAME}\` (TTL ${TTL_SECONDS}s). Review impact below BEFORE applying the Edit. If callers in another package would break, abort and reconsider."
FULL_CTX="${HEADER}

${PAYLOAD}"

emit_json "$FULL_CTX"
exit 0
