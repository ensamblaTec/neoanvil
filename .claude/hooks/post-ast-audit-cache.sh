#!/bin/bash
# PostToolUse hook: cache successful AST_AUDIT runs so the hard gate at
# pre-edit-blast.sh:188 (BoltDB AST_AUDIT_BOLTDB gate) passes silently on
# subsequent Edits within TTL.
#
# Closes the loop introduced in 7f9f427 (hard gate). Previously the only
# way to silence the gate was NEO_BLAST_HOOK_AST_BYPASS=1 env var. Now:
# operator runs AST_AUDIT manually → this hook fires PostToolUse → cache
# populated → next Edit on same path passes silently.
#
# Triggered by Claude Code PostToolUse event matching mcp__neoanvil__neo_radar.
# Receives JSON in stdin with tool_input + tool_response. Filter chain:
#   1. tool_input.intent must be "AST_AUDIT" (skip other neo_radar intents)
#   2. tool_response.content[0].text MUST contain "No issues found"
#      (only cache CLEAN audits — per DS audit Finding 1.1, audits with
#      warnings should NOT be cached because operator must fix warnings
#      before editing)
#   3. extract tool_input.target → cache as path
#
# Cache: .neo/ast_audit_cache.json — same shape as blast_cache.json.
# TTL governed by pre-edit-blast.sh's NEO_BLAST_HOOK_TTL_SECONDS (default 300s).
#
# Spec: ADR-016 hooks design + DS-validated mitigations 2026-05-13.
# bash 3.2 safe: jq + python heredoc with env-var passing.

set -uo pipefail

[ "${NEO_AST_CACHE_HOOK_DISABLE:-0}" = "1" ] && exit 0

REPO_ROOT="${NEO_REPO_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null)}"
[ -z "$REPO_ROOT" ] && exit 0

INPUT="$(cat 2>/dev/null)"
[ -z "$INPUT" ] && exit 0

# Filter 1: intent must be AST_AUDIT. jq extracts safely even when fields
# are absent (returns "null" string which fails the equality check).
INTENT=$(printf '%s' "$INPUT" | jq -r '.tool_input.intent // ""' 2>/dev/null)
[ "$INTENT" != "AST_AUDIT" ] && exit 0

# Filter 2: target path required.
TARGET=$(printf '%s' "$INPUT" | jq -r '.tool_input.target // ""' 2>/dev/null)
[ -z "$TARGET" ] && exit 0

# Resolve target to absolute path (matches what pre-edit-blast hard-gate
# uses as cache key). neo_radar accepts both relative and absolute targets.
case "$TARGET" in
  /*) ABS_TARGET="$TARGET" ;;
  *)  ABS_TARGET="${REPO_ROOT}/${TARGET}" ;;
esac

# Filter 3: response MUST explicitly say "No issues found". Per DS audit
# Finding 1.1: caching warning-tolerated audits silently bypasses CC>15
# enforcement on subsequent edits. We require literal CLEAN match.
#
# Tolerant of multiple response shapes:
#   - tool_response.content[].text         (MCP envelope)
#   - tool_response.result.content[].text  (some intents)
#   - tool_response (raw string)           (fallback)
RESPONSE_TEXT=$(printf '%s' "$INPUT" | jq -r '
  ((.tool_response.content // .tool_response.result.content // []) | map(.text // "") | join("\n"))
  + ((.tool_response | if type == "string" then . else "" end))
' 2>/dev/null)

# CLEAN signal: AST_AUDIT response includes "No issues found" verbatim
# when the audit passed cleanly. Anything else (warnings, errors, empty)
# is rejected — don't cache.
case "$RESPONSE_TEXT" in
  *"No issues found"*) ;;
  *) exit 0 ;;
esac

# Cache write — JSON object {path: timestamp, ...}. Eviction of >24h
# entries keeps file bounded (same pattern as blast_cache.json).
CACHE_DIR="${REPO_ROOT}/.neo"
CACHE_FILE="${CACHE_DIR}/ast_audit_cache.json"
mkdir -p "$CACHE_DIR" 2>/dev/null
NOW=$(date +%s)

AST_CACHE_FILE="$CACHE_FILE" ABS_TARGET="$ABS_TARGET" NOW="$NOW" python3 <<'PYEOF' 2>/dev/null
import json, os
cache_file = os.environ.get("AST_CACHE_FILE","")
path = os.environ.get("ABS_TARGET","")
now = int(os.environ.get("NOW","0"))
try:
    with open(cache_file) as f:
        cache = json.load(f)
except (FileNotFoundError, json.JSONDecodeError, ValueError):
    cache = {}
cache[path] = now
# Evict entries older than 24h to keep file bounded.
cache = {k: v for k, v in cache.items() if isinstance(v, (int, float)) and now - v < 86400}
with open(cache_file, "w") as f:
    json.dump(cache, f)
PYEOF

# Emit confirmation to agent (via additionalContext) so it knows the cache
# was populated and subsequent Edits on this path will pass the hard gate.
jq -nc --arg path "$ABS_TARGET" '{
  hookSpecificOutput: {
    hookEventName: "PostToolUse",
    additionalContext: ("[ouroboros-hook · AST_AUDIT_CACHE] ✅ AST_AUDIT CLEAN cached for " + $path + ". The pre-edit-blast hard gate ([AST_AUDIT_BOLTDB]) will now allow Edits on this path silently for the next 300s.")
  }
}'

exit 0
