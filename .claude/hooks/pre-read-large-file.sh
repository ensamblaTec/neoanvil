#!/bin/bash
# PreToolUse:Read hook — warn (NOT block) when Claude tries to Read a file
# ≥500 lines. Suggests READ_SLICE or FILE_EXTRACT alternatives.
#
# Triggered by Claude Code PreToolUse:Read. Receives JSON on stdin with
# tool_input.file_path. Output: JSON with `additionalContext` so the
# warning lands as agent-visible context (not a block).
#
# Rationale: [CONTEXT-EFFICIENCY] (directive 23) prohibits Read native on
# files ≥100L, [TOOL-DISCIPLINE-CHECKLIST] (directive 58) reinforces.
# Read of macro_tools.go (2300L) = ~42K tokens vs FILE_EXTRACT = ~375
# tokens. This hook makes the cost visible at the call site instead of
# discovering it at session-close audit.
#
# Threshold 500L (not 100L) to avoid noise on routine doc/config edits
# while still catching the heavy Go source files. Operator can disable
# via NEO_READ_HOOK_DISABLE=1.
#
# Spec: session 2026-05-15 (tool-discipline initiative).

set -uo pipefail

[ "${NEO_READ_HOOK_DISABLE:-0}" = "1" ] && exit 0

emit_json() {
  local ctx="$1"
  python3 -c "
import json, sys
print(json.dumps({
  'hookSpecificOutput': {
    'hookEventName': 'PreToolUse',
    'additionalContext': sys.argv[1],
  }
}))
" "$ctx"
}

INPUT="$(cat 2>/dev/null)"
[ -z "$INPUT" ] && exit 0

FILE_PATH=$(printf '%s' "$INPUT" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
    p = d.get("tool_input", {}).get("file_path", "")
    print(p)
except Exception:
    print("")
' 2>/dev/null)

# Empty path or non-existent file → no warning (Read will error itself).
[ -z "$FILE_PATH" ] && exit 0
[ ! -f "$FILE_PATH" ] && exit 0

# Skip warning for: read of small offset/limit slice (already efficient),
# binary files (we can't usefully suggest READ_SLICE), and files outside
# the workspace (Claude reading its own state).
case "$FILE_PATH" in
  /private/tmp/*|/tmp/*) exit 0 ;;
  *.bin|*.db|*.snapshot.json|*.lock) exit 0 ;;
esac

# Check if the Read call has offset+limit already set — if so, the agent
# is being surgical and we don't need to nag.
HAS_OFFSET=$(printf '%s' "$INPUT" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
    ti = d.get("tool_input", {})
    print("1" if "offset" in ti else "0")
except Exception:
    print("0")
' 2>/dev/null)
[ "$HAS_OFFSET" = "1" ] && exit 0

LINE_COUNT=$(wc -l < "$FILE_PATH" 2>/dev/null | tr -d ' ')
[ -z "$LINE_COUNT" ] && exit 0

# Threshold: 500 lines. Below = silent. Above = warn + suggest alternatives.
if [ "$LINE_COUNT" -lt 500 ]; then
  exit 0
fi

# File extension determines best alternative.
ext="${FILE_PATH##*.}"
case "$ext" in
  go|ts|tsx|js|jsx|py|rs)
    SUGGEST="**[TOOL-DISCIPLINE]** \`Read\` on $FILE_PATH ($LINE_COUNT lines) — prefer surgical tools. For known symbol: \`neo_radar(intent:\\\"FILE_EXTRACT\\\", target:\\\"$FILE_PATH\\\", query:\\\"<symbol_name>\\\", context_lines:0)\`. For unknown package: \`neo_radar(intent:\\\"COMPILE_AUDIT\\\", target:\\\"$FILE_PATH\\\")\` first to get the symbol_map, then FILE_EXTRACT with the line offset. Read native ≈ ${LINE_COUNT}L × ~18tok/L vs FILE_EXTRACT ≈ ~400 tokens for a single symbol body. Directive 23 (CONTEXT-EFFICIENCY) + 58 (TOOL-DISCIPLINE-CHECKLIST)."
    ;;
  md|yaml|yml|json|toml)
    # Markdown/yaml/json: less critical, just remind about offset/limit.
    SUGGEST="**[TOOL-DISCIPLINE]** \`Read\` on $FILE_PATH ($LINE_COUNT lines) — consider \`offset\` + \`limit\` if you only need a section, or \`neo_radar(intent:\\\"READ_SLICE\\\", target:\\\"$FILE_PATH\\\", start_line:N, limit:M)\` for IO-metric-tracked surgical reads."
    ;;
  *)
    exit 0  # unknown extension → no warning
    ;;
esac

emit_json "$SUGGEST"
