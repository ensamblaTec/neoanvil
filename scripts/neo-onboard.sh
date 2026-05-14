#!/usr/bin/env bash
# scripts/neo-onboard.sh — port the neo *enforcement* layer into another
# workspace so its Claude Code agent actually follows the Ouroboros flow.
#
# WHY THIS EXISTS
#   A directive in CLAUDE.md is a soft request the model can skip. The reason
#   neoanvil's agent reliably runs BRIEFING → BLAST_RADIUS → certify is the
#   Claude Code *hooks* in .claude/settings.json — the harness executes them,
#   not the model's goodwill. Other workspaces (strategos, etc.) carry the
#   directives but not the hooks, so the flow is ignored. This script copies
#   the hook layer over. See docs/onboarding/neo-enforcement.md.
#
# SCOPE
#   This ports ONLY the Claude Code harness hooks (.claude/settings.json +
#   .claude/hooks/*.sh). The git pre-commit cert gate is NOT handled here:
#   neo-mcp self-installs it at every boot (workspace_utils.go::
#   installPreCommitHook), so a target that runs neo-mcp already has it.
#
# USAGE
#   scripts/neo-onboard.sh <target-workspace-path> [--dry-run] [--force]
#     --dry-run  print what would change, write nothing
#     --force    re-apply even if the target already has neo hooks
#
# REQUIRES: jq; target is a git repo; ideally the target is already registered
# in ~/.neo/workspaces.json (the script warns, does not fail, if not).
set -euo pipefail

SRC_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DRY_RUN=0
FORCE=0
TARGET=""

for arg in "$@"; do
	case "$arg" in
		--dry-run) DRY_RUN=1 ;;
		--force)   FORCE=1 ;;
		-*)        echo "neo-onboard: unknown flag: $arg" >&2; exit 2 ;;
		*)         TARGET="$arg" ;;
	esac
done

die()  { echo "neo-onboard: $*" >&2; exit 1; }
note() { echo "[neo-onboard] $*"; }

[ -n "$TARGET" ]    || die "usage: scripts/neo-onboard.sh <target-workspace-path> [--dry-run] [--force]"
command -v jq >/dev/null 2>&1 || die "jq is required"
[ -d "$TARGET" ]    || die "target is not a directory: $TARGET"
TARGET="$(cd "$TARGET" && pwd)"
[ -d "$TARGET/.git" ] || die "target is not a git repo: $TARGET"
[ "$TARGET" != "$SRC_ROOT" ] || die "target is neoanvil itself — nothing to onboard"

SRC_SETTINGS="$SRC_ROOT/.claude/settings.json"
SRC_HOOKS="$SRC_ROOT/.claude/hooks"
[ -f "$SRC_SETTINGS" ] || die "source settings not found: $SRC_SETTINGS"
[ -d "$SRC_HOOKS" ]    || die "source hooks dir not found: $SRC_HOOKS"

TGT_CLAUDE="$TARGET/.claude"
TGT_SETTINGS="$TGT_CLAUDE/settings.json"
TGT_HOOKS="$TGT_CLAUDE/hooks"

# --- resolve the target's workspace ID from ~/.neo/workspaces.json -----------
# The hooks honour NEO_WORKSPACE_ID; we inject it into the target's settings
# env so briefing.sh / pre-edit-blast.sh target the right workspace instead of
# neoanvil's hardcoded fallback.
REGISTRY="$HOME/.neo/workspaces.json"
WS_ID=""
if [ -f "$REGISTRY" ]; then
	WS_ID="$(jq -r --arg p "$TARGET" '
		(.workspaces // [])[] | select((.path // "") == $p) | .id // empty
	' "$REGISTRY" 2>/dev/null | head -n1 || true)"
fi
if [ -z "$WS_ID" ]; then
	note "⚠️  target not found in $REGISTRY — NEO_WORKSPACE_ID will be left for the"
	note "    operator to set. Register the workspace first (boot its neo-mcp once),"
	note "    then re-run with --force, or edit .claude/settings.json env manually."
else
	note "resolved workspace id: $WS_ID"
fi

# --- idempotency -------------------------------------------------------------
if [ -f "$TGT_SETTINGS" ] && grep -q '\.claude/hooks/briefing\.sh' "$TGT_SETTINGS" && [ "$FORCE" -eq 0 ]; then
	note "target already has neo hooks (briefing.sh present in settings.json)."
	note "nothing to do. Pass --force to re-apply."
	exit 0
fi

# --- compute the merged settings.json ---------------------------------------
# Deep-merge: for each neo hook type, APPEND neo's matcher groups to whatever
# the target already has (target's own hooks are preserved). Inject
# env.NEO_WORKSPACE_ID when resolved.
EXISTING="{}"
[ -f "$TGT_SETTINGS" ] && EXISTING="$(cat "$TGT_SETTINGS")"

MERGED="$(printf '%s' "$EXISTING" | jq \
	--slurpfile neo "$SRC_SETTINGS" \
	--arg wsid "$WS_ID" '
	. as $tgt
	| reduce ($neo[0].hooks | keys[]) as $k ($tgt;
		.hooks[$k] = ((.hooks[$k] // []) + ($neo[0].hooks[$k])))
	| if $wsid != "" then .env = ((.env // {}) + {"NEO_WORKSPACE_ID": $wsid}) else . end
')"

# --- report ------------------------------------------------------------------
note "source : $SRC_ROOT"
note "target : $TARGET"
note "hooks  : $(ls "$SRC_HOOKS"/*.sh 2>/dev/null | wc -l | tr -d ' ') scripts → $TGT_HOOKS/"
note "settings: merge neo hooks block into $TGT_SETTINGS"

if [ "$DRY_RUN" -eq 1 ]; then
	note "--- DRY RUN — nothing written. Merged settings.json would be: ---"
	printf '%s\n' "$MERGED"
	exit 0
fi

# --- apply -------------------------------------------------------------------
mkdir -p "$TGT_HOOKS"
cp "$SRC_HOOKS"/*.sh "$TGT_HOOKS"/
chmod +x "$TGT_HOOKS"/*.sh
note "copied hook scripts → $TGT_HOOKS/"

if [ -f "$TGT_SETTINGS" ]; then
	BACKUP="$TGT_SETTINGS.bak.$(date +%s)"
	cp "$TGT_SETTINGS" "$BACKUP"
	note "backed up existing settings → $BACKUP"
fi
printf '%s\n' "$MERGED" > "$TGT_SETTINGS"
note "wrote merged settings → $TGT_SETTINGS"

# --- post-checks -------------------------------------------------------------
echo
note "✅ hook layer installed. Remaining (operator):"
note "  1. The git pre-commit cert gate installs itself when the target's"
note "     neo-mcp next boots — no action needed if it runs neo-mcp."
if [ -z "$WS_ID" ]; then
	note "  2. Register the workspace + set NEO_WORKSPACE_ID in .claude/settings.json"
	note "     env (boot its neo-mcp once to auto-register, then re-run --force)."
fi
note "  3. Restart the target's Claude Code session so SessionStart fires briefing.sh."
note "  4. Verify: a new session should auto-print the BRIEFING block."
