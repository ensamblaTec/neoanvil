#!/bin/bash
# B1 auto-snapshot hook — captures the tool-discipline state for the
# adaptive-runtime A/B measurement. Invoked twice per Claude Code session:
#
#   · SessionStart  → snapshot with note "auto-pre <source>"
#                     (called in background from briefing.sh so the 20s
#                     SessionStart budget stays for the briefing itself)
#   · Stop          → snapshot with note "auto-post"
#                     (called sync from settings.json Stop matcher)
#
# The arm label (baseline / treatment) is derived from the
# NEO_BRIEFING_DIFF_DISABLE env var by scripts/b1-measurement.sh itself,
# so operator only needs to flip that one variable between A/B rounds.
#
# Fail-soft on everything: no neo-mcp, no Python, no log dir → silent exit 0.
# Designed to add zero friction to operator workflow.
#
# Operator opt-out: NEO_B1_SNAPSHOT_DISABLE=1 (independent of the mirror
# opt-out — you can disable snapshotting without disabling B1 itself, or
# vice versa).
#
# Spec: docs/general/b1-measurement-protocol.md.
# Branch: feature/adaptive-briefing-diff.

set -uo pipefail

[ "${NEO_B1_SNAPSHOT_DISABLE:-0}" = "1" ] && exit 0

# Determine which phase based on the first argument: pre / post.
PHASE="${1:-pre}"

# Resolve the workspace repo root from this hook's path so the snapshot
# script is found whether the hook is invoked via the standard hook chain
# or directly for testing.
HOOK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd)"
REPO_ROOT="$(cd "$HOOK_DIR/../.." 2>/dev/null && pwd)"
SCRIPT="$REPO_ROOT/scripts/b1-measurement.sh"

[ -x "$SCRIPT" ] || exit 0

# Build the note. Use the SessionStart source if Claude Code provides it
# via stdin JSON ({"source":"startup|resume|clear|compact"}); otherwise
# fall back to just the phase tag.
NOTE_TAIL=""
if [ "$PHASE" = "pre" ]; then
  INPUT="$(cat 2>/dev/null)"
  if [ -n "$INPUT" ]; then
    SOURCE=$(printf '%s' "$INPUT" | jq -r '.source // ""' 2>/dev/null)
    [ -n "$SOURCE" ] && NOTE_TAIL=" $SOURCE"
  fi
fi
NOTE="auto-${PHASE}${NOTE_TAIL}"

# Portable timeout: macOS lacks the `timeout` binary by default, so we
# emulate it with background + kill. Internal curl --max-time caps in the
# snapshot script already bound it to ~7s; this is belt-and-suspenders for
# stuck DNS or python crashes.
NEO_B1_NOTE="$NOTE" "$SCRIPT" snapshot >/dev/null 2>&1 &
PID=$!
(
  sleep 8
  kill -9 "$PID" 2>/dev/null
) >/dev/null 2>&1 &
WATCHDOG=$!
wait "$PID" 2>/dev/null || true
kill "$WATCHDOG" 2>/dev/null || true

exit 0
