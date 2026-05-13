#!/bin/bash
# Stop hook: warn (soft) si hay edits pendientes de certify al cerrar la sesión.
# No bloquea — el pre-commit hook ya bloquea en git commit time. Este hook
# solo da visibilidad para que el operador no se vaya con edits sin sellar.
#
# Triggered por Claude Code Stop event (al final de la conversación).
# Output: JSON con `systemMessage` top-level. **El schema de Claude Code NO
# incluye `Stop` en `hookSpecificOutput`** — solo PreToolUse/UserPromptSubmit/
# PostToolUse/PostToolBatch. Para Stop usar systemMessage/reason/decision al
# nivel raíz. Bug previo: emitía hookSpecificOutput.hookEventName="Stop" y
# Claude Code rechazaba con "Hook JSON output validation failed".
#
# Spec: ADR-016 (revision 2026-05-13: JSON output format; 2026-05-13bis: Stop fix).

set -uo pipefail

[ "${NEO_CERT_HOOK_DISABLE:-0}" = "1" ] && exit 0

REPO_ROOT="${NEO_REPO_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null)}"
[ -z "$REPO_ROOT" ] && exit 0

PENDING_FILE="$REPO_ROOT/.neo/session_pending_cert.list"
LOCK_FILE="$REPO_ROOT/.neo/db/certified_state.lock"

[ ! -f "$PENDING_FILE" ] && exit 0

# Build list of uncertified files: in pending but not in lock.
UNCERTIFIED=()
while IFS= read -r path; do
  [ -z "$path" ] && continue
  if [ -f "$LOCK_FILE" ]; then
    if ! grep -q "^${path}|" "$LOCK_FILE" 2>/dev/null; then
      UNCERTIFIED+=("$path")
    fi
  else
    UNCERTIFIED+=("$path")
  fi
done < "$PENDING_FILE"

if [ ${#UNCERTIFIED[@]} -gt 0 ]; then
  # Build the context message.
  CTX="[ouroboros-hook] ⚠️ Session ending with ${#UNCERTIFIED[@]} uncertified edit(s):"
  for f in "${UNCERTIFIED[@]}"; do
    rel=$(echo "$f" | sed "s|^$REPO_ROOT/||")
    CTX="${CTX}
  - ${rel}"
  done
  CTX="${CTX}

El pre-commit hook bloqueará \`git commit\` hasta que se ejecute \`mcp__neoanvil__neo_sre_certify_mutation\` con la lista de archivos arriba.
Bypass de emergencia: \`NEO_CERTIFY_BYPASS=1 git commit\` (queda registrado como ⚠️ en TECH_DEBT_MAP)."

  python3 -c "
import json, sys
print(json.dumps({
  'systemMessage': sys.argv[1],
}))
" "$CTX"
fi

# Reset pending list for next session (the lock file is authoritative going forward).
> "$PENDING_FILE" 2>/dev/null
exit 0
