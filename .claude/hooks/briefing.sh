#!/bin/bash
# Auto-BRIEFING SessionStart hook for NeoAnvil.
# Implements directive [SRE-BRIEFING]: BRIEFING is mandatory on session start,
# resume, clear, and compact. Without this hook the agent has to be told
# manually each time, and resumed contexts often skip it.
#
# Triggered by Claude Code SessionStart event (source ∈ startup|resume|clear|compact).
# Fail-soft: if Nexus is unreachable, prints a one-line warning and exits 0
# so Claude Code doesn't block on startup.
#
# Workspace ID override: set NEO_WORKSPACE_ID env var. Default targets the
# neoanvil-9b272 workspace; replicate this hook in other workspaces with
# their own ID.

set -uo pipefail

NEXUS_URL="${NEO_NEXUS_URL:-http://127.0.0.1:9000}"
# [bug-fix 2026-05-13] Auto-detect workspace from CWD instead of stale hardcoded
# `neoanvil-9b272` (non-existent ID — real registered ID is neoanvil-35694).
case "$PWD" in
  *neoanvil*) DEFAULT_WS="neoanvil-35694" ;;
  *)          DEFAULT_WS="neoanvil-35694" ;;
esac
WORKSPACE_ID="${NEO_WORKSPACE_ID:-$DEFAULT_WS}"

# Probe Nexus liveness with a tight timeout. Soft-fail on unreachable.
if ! curl -fsS --max-time 2 -o /dev/null "${NEXUS_URL}/health" 2>/dev/null; then
  echo "## Auto-BRIEFING skipped — Nexus unreachable at ${NEXUS_URL}. Run BRIEFING manually."
  exit 0
fi

# Invoke neo_radar(intent:BRIEFING, mode:compact) via the Nexus MCP message endpoint.
# Compact mode keeps the prepended context under 1KB so it doesn't crowd the session.
response=$(curl -fsS --max-time 15 \
  -H "Content-Type: application/json" \
  -H "X-Neo-Workspace: ${WORKSPACE_ID}" \
  -X POST "${NEXUS_URL}/mcp/message" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"neo_radar","arguments":{"intent":"BRIEFING","mode":"compact"}}}' \
  2>/dev/null) || {
    echo "## Auto-BRIEFING failed — Nexus returned an error. Run BRIEFING manually."
    exit 0
  }

# Extract the Markdown payload from the JSON-RPC envelope. Tolerant of shape drift.
text=$(printf '%s' "$response" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
    parts = d.get("result", {}).get("content", [])
    print("".join(p.get("text", "") for p in parts if isinstance(p, dict)))
except Exception:
    sys.stderr.write("decode error\n")
    sys.exit(1)
' 2>/dev/null) || {
    echo "## Auto-BRIEFING got malformed response — run BRIEFING manually."
    exit 0
  }

if [ -z "$text" ]; then
  echo "## Auto-BRIEFING returned empty payload — Nexus may still be warming up."
  exit 0
fi

# Output goes into Claude's session context.
echo "## Auto-BRIEFING (SessionStart hook · workspace=${WORKSPACE_ID})"
echo ""
echo "$text"

# [B1 / adaptive-runtime 2026-05-15] Append tool-discipline mirror. Helper
# is fail-soft (silent on any error) — briefing keeps working if the helper
# breaks. Opt-out via NEO_BRIEFING_DIFF_DISABLE=1.
HELPER="$(dirname "${BASH_SOURCE[0]}")/briefing-behavior-diff.sh"
if [ -x "$HELPER" ]; then
  # 3s timeout — briefing already used ~17s of the 20s SessionStart budget;
  # the helper has to be fast or skip silently.
  timeout 3 "$HELPER" 2>/dev/null || true
fi
