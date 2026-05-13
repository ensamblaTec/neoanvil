#!/bin/bash
# PreToolUse hook: SUPERSEDES check before neo_memory(learn, action_type:add).
# Enforces directive [SRE-DIRECTIVE-SUPERSEDES-AUDIT]: search existing
# directives for the same bracket-tag before adding a new one — if a
# matching tag already exists, the operator should action_type:update
# with directive_id + supersedes:[N] instead of duplicating.
#
# Triggered by Claude Code PreToolUse matching mcp__neoanvil__neo_memory.
# Reads tool_input JSON, filters to learn+add intent, extracts the
# proposed directive's first bracket tag, greps the on-disk synced
# directives file for that tag. If a match exists → injects reminder
# via additionalContext (NO hard gate — operator may legitimately add a
# variant directive; the reminder lets them choose update vs add).
#
# Spec: ADR-016 hooks design + DS-validated GO 2026-05-13.
# bash 3.2 safe: jq for JSON, native bash regex for tag extraction.

set -uo pipefail

[ "${NEO_MEMORY_HOOK_DISABLE:-0}" = "1" ] && exit 0

REPO_ROOT="${NEO_REPO_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null)}"
[ -z "$REPO_ROOT" ] && exit 0

INPUT="$(cat 2>/dev/null)"
[ -z "$INPUT" ] && exit 0

# Filter 1: action must be "learn" with action_type "add". Other actions
# (delete, compact, store, fetch, etc.) don't add new directives.
ACTION=$(printf '%s' "$INPUT" | jq -r '.tool_input.action // ""' 2>/dev/null)
ACTION_TYPE=$(printf '%s' "$INPUT" | jq -r '.tool_input.action_type // ""' 2>/dev/null)
if [ "$ACTION" != "learn" ] || [ "$ACTION_TYPE" != "add" ]; then
  exit 0
fi

# Filter 2: directive content required.
DIRECTIVE=$(printf '%s' "$INPUT" | jq -r '.tool_input.directive // ""' 2>/dev/null)
[ -z "$DIRECTIVE" ] && exit 0

# Extract the first bracket-tag from the directive. Doctrine convention is
# `[TAG-NAME] description...`. We use sed to grab the bracketed token.
# Match [WORD] where WORD = uppercase letters, digits, hyphens, underscores.
# Falls back to empty when no bracket tag present (e.g. operator wrote
# directive without standard prefix — those are legitimate but we can't
# auto-check supersedes without the tag).
TAG=$(printf '%s' "$DIRECTIVE" | sed -nE 's/^\[([A-Z0-9_-]+)\].*/\1/p' | head -1)
if [ -z "$TAG" ]; then
  # No bracket tag — emit informational nudge but allow.
  jq -nc '{
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "allow",
      additionalContext: "[ouroboros-hook · SUPERSEDES] directive lacks the [TAG-NAME] prefix convention. Consider adding one (e.g. [SRE-XXX]) so [SRE-DIRECTIVE-SUPERSEDES-AUDIT] can audit duplicates on future adds."
    }
  }'
  exit 0
fi

# Filter 3: search the on-disk synced directives file for that tag.
DIRECTIVES_FILE="${REPO_ROOT}/.claude/rules/neo-synced-directives.md"
if [ ! -f "$DIRECTIVES_FILE" ]; then
  # File missing — first add, allow silently.
  exit 0
fi

# Grep for the exact bracket tag. Use fixed-string to avoid regex metachar
# issues. Match line that contains "[TAG]" anywhere.
MATCH_LINES=$(grep -n -F "[${TAG}]" "$DIRECTIVES_FILE" 2>/dev/null | head -3)

if [ -z "$MATCH_LINES" ]; then
  # No existing directive with this tag — clean add, allow silently.
  exit 0
fi

# Match found — emit reminder. NOT a hard gate (operator may legitimately
# want to add a variant tag like [SRE-X-V2]); just a nudge to consider
# action_type:update if this is meant to supersede.
PREVIEW=$(printf '%s' "$MATCH_LINES" | head -2 | sed 's/^/  /')
CTX="[ouroboros-hook · SUPERSEDES] ⚠️ Tag \`[${TAG}]\` already exists in neo-synced-directives.md:

${PREVIEW}

Per [SRE-DIRECTIVE-SUPERSEDES-AUDIT]: if this learn(add) is meant to REPLACE the existing directive, use:
  mcp__neoanvil__neo_memory(
    action: \"learn\",
    action_type: \"update\",
    directive_id: <N from the line above>,
    directive: \"<new text>\",
    supersedes: [<N>]
  )

If this is genuinely a NEW directive (different scope despite shared tag prefix), proceed with the add — but consider renaming the tag to avoid future grep collisions."

jq -nc --arg ctx "$CTX" '{
  hookSpecificOutput: {
    hookEventName: "PreToolUse",
    permissionDecision: "allow",
    additionalContext: $ctx
  }
}'

exit 0
