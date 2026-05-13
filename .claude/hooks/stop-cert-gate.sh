#!/bin/bash
# Stop hook: warn (soft) si hay edits pendientes de certify al cerrar la sesión.
# No bloquea — el pre-commit hook ya bloquea en git commit time. Este hook
# solo da visibilidad para que el operador no se vaya con edits sin sellar.
#
# Triggered por Claude Code Stop event (al final de la conversación).
# Lee .neo/session_pending_cert.list vs .neo/db/certified_state.lock.
# Imprime banner si hay diff. Limpia la lista pending al final.
#
# Spec: ADR-016.

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
  echo ""
  echo "## ⚠️ [ouroboros-hook] Session ended with ${#UNCERTIFIED[@]} uncertified edit(s)"
  echo ""
  echo "Los siguientes archivos fueron editados pero no certificados:"
  for f in "${UNCERTIFIED[@]}"; do
    echo "  - \`$(echo "$f" | sed "s|^$REPO_ROOT/||")\`"
  done
  echo ""
  echo "El pre-commit hook **bloqueará** \`git commit\` hasta que se ejecute \`neo_sre_certify_mutation\`."
  echo "Bypass de emergencia: \`NEO_CERTIFY_BYPASS=1 git commit\` (queda registrado como ⚠️)."
fi

# Reset pending list for next session (the lock file is authoritative going forward).
> "$PENDING_FILE" 2>/dev/null
exit 0
