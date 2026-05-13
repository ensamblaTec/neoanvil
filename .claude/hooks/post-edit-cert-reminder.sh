#!/bin/bash
# PostToolUse hook: registra Edit/Write/MultiEdit completados en una lista de
# "pending certify" para que el agent (y el Stop hook) sepan qué archivos
# están sin sellar por neo_sre_certify_mutation.
#
# Triggered por Claude Code PostToolUse:Edit|Write|MultiEdit. Recibe JSON en
# stdin con tool_input.file_path. Imprime un recordatorio one-line al stdout
# → se inyecta al contexto post-edit del agent.
#
# Comportamiento:
#   - Skip silencioso si file_path no es código productivo (.go, .ts, etc).
#   - Append a .neo/session_pending_cert.list (dedupe).
#   - Imprime: "⏳ pending certify: <file> (TTL <minutes>min)".
#   - Fail-silent: si /.neo/ no escribible, exit 0 sin error.
#
# Spec: ADR-016.

set -uo pipefail

[ "${NEO_CERT_HOOK_DISABLE:-0}" = "1" ] && exit 0

REPO_ROOT="${NEO_REPO_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null)}"
[ -z "$REPO_ROOT" ] && exit 0

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

# Filter: only track productive code files.
case "${FILE_PATH,,}" in
  *.go|*.ts|*.tsx|*.js|*.jsx|*.css) ;;
  *) exit 0 ;;
esac

# Determine TTL based on mode (read from .neo/mode written by neo-mcp).
NEO_MODE="${NEO_SERVER_MODE:-$(cat "$REPO_ROOT/.neo/mode" 2>/dev/null || echo pair)}"
if [ "$NEO_MODE" = "pair" ]; then
  TTL_MIN=15
else
  TTL_MIN=5
fi

# Append to pending list with dedupe.
PENDING_FILE="$REPO_ROOT/.neo/session_pending_cert.list"
mkdir -p "$REPO_ROOT/.neo" 2>/dev/null
{
  if [ -f "$PENDING_FILE" ]; then
    grep -v "^${FILE_PATH}$" "$PENDING_FILE" 2>/dev/null
  fi
  echo "$FILE_PATH"
} > "${PENDING_FILE}.tmp" 2>/dev/null && mv "${PENDING_FILE}.tmp" "$PENDING_FILE" 2>/dev/null

echo "_⏳ [ouroboros-hook] pending certify: \`$(basename "$FILE_PATH")\` (TTL ${TTL_MIN}min in $NEO_MODE mode). Llama \`neo_sre_certify_mutation\` antes del git commit._"
exit 0
