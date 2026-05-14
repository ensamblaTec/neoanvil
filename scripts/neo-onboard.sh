#!/usr/bin/env bash
# scripts/neo-onboard.sh — port the neo *enforcement + fluency* layer into
# another workspace so its Claude Code agent actually follows the Ouroboros
# flow AND reaches for the right neo tools unprompted.
#
# WHY THIS EXISTS
#   A directive in CLAUDE.md is a soft request the model can skip. The reason
#   neoanvil's agent reliably runs BRIEFING → BLAST_RADIUS → certify — and
#   knows that a perf question means `neo_tool_stats sort_by:p99` — is NOT the
#   directives. It is three layers the harness/context provide, not the
#   model's goodwill. Other workspaces carry the directive *text* but not
#   these layers, so the flow and the tool fluency are both ignored.
#
# WHAT THIS SCRIPT PORTS
#   Layer 0 — MCP wired:  PREFLIGHT CHECK only. Without the neo MCP connected
#             the agent has zero neo tools; hooks + skills are then inert.
#   Layer 1 — enforcement: .claude/settings.json hooks + .claude/hooks/*.sh.
#             The harness runs these — auto-BRIEFING, pre-edit BLAST gate,
#             cert gate. This is what the model cannot skip.
#   Layer 2 — fluency:     .claude/skills/* (curated — sre-db is omitted, it is
#             path-scoped to neoanvil's pkg layout). The granular "for X reach
#             for Y" doctrine.
#   Layer 3 — directives:  a *seed* copy of neo-synced-directives.md, left at
#             .claude/neo-directives-seed.md (NOT auto-active) for the operator
#             to curate — many directives are neoanvil-implementation-specific.
#
#   NOT ported: the git pre-commit cert gate — neo-mcp self-installs it at
#   every boot (workspace_utils.go::installPreCommitHook).
#
# USAGE
#   scripts/neo-onboard.sh <target-workspace-path> [--dry-run] [--force] [--no-skills]
#     --dry-run    print what would change, write nothing
#     --force      re-apply even if the target already has neo hooks
#     --no-skills  port hooks + directive seed only — skip layer 2. Use when the
#                  target already carries the doctrine (e.g. older .claude/rules/
#                  files) and copying skills would just duplicate it.
#
# REQUIRES: jq; target is a git repo; ideally the target is already registered
# in ~/.neo/workspaces.json (the script warns, does not fail, if not).
set -euo pipefail

SRC_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DRY_RUN=0
FORCE=0
NO_SKILLS=0
TARGET=""

# Skills that are NOT portable verbatim — sre-db's auto-load path globs
# (pkg/dba/, pkg/rag/, migrations/) are neoanvil-specific and would never
# match another project's layout.
SKILL_EXCLUDE="sre-db"

for arg in "$@"; do
	case "$arg" in
		--dry-run)   DRY_RUN=1 ;;
		--force)     FORCE=1 ;;
		--no-skills) NO_SKILLS=1 ;;
		-*)          echo "neo-onboard: unknown flag: $arg" >&2; exit 2 ;;
		*)           TARGET="$arg" ;;
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
SRC_SKILLS="$SRC_ROOT/.claude/skills"
SRC_DIRECTIVES="$SRC_ROOT/.claude/rules/neo-synced-directives.md"
[ -f "$SRC_SETTINGS" ] || die "source settings not found: $SRC_SETTINGS"
[ -d "$SRC_HOOKS" ]    || die "source hooks dir not found: $SRC_HOOKS"

TGT_CLAUDE="$TARGET/.claude"
TGT_SETTINGS="$TGT_CLAUDE/settings.json"
TGT_HOOKS="$TGT_CLAUDE/hooks"
TGT_SKILLS="$TGT_CLAUDE/skills"
TGT_DIR_SEED="$TGT_CLAUDE/neo-directives-seed.md"

# --- layer-0 preflight: is the neo MCP server even wired into the target? -----
# Without an MCP connection the agent has ZERO neo tools — every hook and skill
# this script installs is then inert decoration. Check the project-level
# .mcp.json for a neo server (Nexus SSE url or a neo-mcp command). A missing
# project .mcp.json is a warning, not a hard fail: the server may live in the
# operator's global ~/.claude.json instead.
TGT_MCP="$TARGET/.mcp.json"
TGT_MCP_NAME=""
if [ -f "$TGT_MCP" ]; then
	# The MCP server's NAME (the mcpServers key) — the agent's neo tools are
	# exposed as mcp__<name>__neo_radar etc. We need it to rewrite the two hook
	# matchers that neoanvil hardcodes as mcp__neoanvil__* (a matcher that
	# doesn't match = a dead hook on the target).
	TGT_MCP_NAME="$(jq -r '
		(.mcpServers // {}) | to_entries[]
		| select(((.value.url // "") | test("/mcp/sse|127\\.0\\.0\\.1:9000"))
		         or ((.value.command // "") | test("neo-mcp")))
		| .key' "$TGT_MCP" 2>/dev/null | head -n1 || true)"
fi
if [ -n "$TGT_MCP_NAME" ]; then
	note "layer-0 OK: $TGT_MCP wires a neo MCP server (\"$TGT_MCP_NAME\")"
else
	note "🛑 LAYER-0 WARNING: no neo MCP server found in $TGT_MCP"
	note "   Without the MCP connected the agent has NO neo tools — the hooks +"
	note "   skills below are inert until it is wired. Confirm the neo MCP is in"
	note "   the target's Claude Code config (.mcp.json or global ~/.claude.json)."
	note "   (mcp__neoanvil__* hook matchers can't be retargeted without it.)"
fi

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
# the target already has (target's own hooks are preserved). Then:
#   - retarget the mcp__neoanvil__* hook matchers to the target's MCP server
#     name (else those two hooks would never fire on the target);
#   - inject env.NEO_WORKSPACE_ID when resolved.
# The gsub is a no-op on the target's own matchers — only mcp__neoanvil__*
# strings are rewritten.
EXISTING="{}"
[ -f "$TGT_SETTINGS" ] && EXISTING="$(cat "$TGT_SETTINGS")"

MERGED="$(printf '%s' "$EXISTING" | jq \
	--slurpfile neo "$SRC_SETTINGS" \
	--arg wsid "$WS_ID" \
	--arg tgtmcp "$TGT_MCP_NAME" '
	. as $tgt
	| reduce ($neo[0].hooks | keys[]) as $k ($tgt;
		.hooks[$k] = ((.hooks[$k] // []) + ($neo[0].hooks[$k])))
	| if ($tgtmcp != "" and $tgtmcp != "neoanvil")
	  then .hooks |= map_values(map(
		if has("matcher")
		then .matcher |= gsub("mcp__neoanvil__"; "mcp__" + $tgtmcp + "__")
		else . end))
	  else . end
	| if $wsid != "" then .env = ((.env // {}) + {"NEO_WORKSPACE_ID": $wsid}) else . end
')"

# Curated skill list — everything under .claude/skills/ except SKILL_EXCLUDE.
PORT_SKILLS=()
if [ -d "$SRC_SKILLS" ]; then
	for d in "$SRC_SKILLS"/*/; do
		[ -d "$d" ] || continue
		name="$(basename "$d")"
		case " $SKILL_EXCLUDE " in *" $name "*) continue ;; esac
		PORT_SKILLS+=("$name")
	done
fi

# --- report ------------------------------------------------------------------
note "source : $SRC_ROOT"
note "target : $TARGET"
note "hooks  : $(ls "$SRC_HOOKS"/*.sh 2>/dev/null | wc -l | tr -d ' ') scripts → $TGT_HOOKS/"
note "settings: merge neo hooks block into $TGT_SETTINGS"
if [ -n "$TGT_MCP_NAME" ] && [ "$TGT_MCP_NAME" != "neoanvil" ]; then
	note "         (retargeting mcp__neoanvil__* matchers → mcp__${TGT_MCP_NAME}__*)"
fi
if [ "$NO_SKILLS" -eq 1 ]; then
	note "skills : SKIPPED (--no-skills)"
else
	note "skills : ${#PORT_SKILLS[@]} → $TGT_SKILLS/  (excluded: $SKILL_EXCLUDE; pre-existing target skills are backed up, not clobbered)"
fi
note "directives: seed copy → $TGT_DIR_SEED  (NOT auto-active — operator curates)"

if [ "$DRY_RUN" -eq 1 ]; then
	note "--- DRY RUN — nothing written. ---"
	if [ "$NO_SKILLS" -eq 1 ]; then
		note "skills: SKIPPED (--no-skills)"
	else
		note "skills that would be copied: ${PORT_SKILLS[*]}"
	fi
	note "merged settings.json would be:"
	printf '%s\n' "$MERGED"
	exit 0
fi

# --- apply: layer 1 (hooks + settings) --------------------------------------
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

# --- apply: layer 2 (skills) -------------------------------------------------
# Non-destructive: a pre-existing target skill that DIFFERS is moved to
# <name>.bak.<ts> before being replaced (identical ones are left untouched) —
# the target may carry its own customised version. --no-skills skips the layer
# entirely, for targets that already hold the doctrine as .claude/rules/ files.
if [ "$NO_SKILLS" -eq 1 ]; then
	note "skills: skipped (--no-skills)"
else
	mkdir -p "$TGT_SKILLS"
	skills_new=0
	skills_replaced=0
	for name in "${PORT_SKILLS[@]}"; do
		dst="$TGT_SKILLS/$name"
		if [ -e "$dst" ]; then
			if diff -rq "$SRC_SKILLS/$name" "$dst" >/dev/null 2>&1; then
				continue  # identical — leave it
			fi
			mv "$dst" "$dst.bak.$(date +%s)"
			skills_replaced=$((skills_replaced + 1))
		else
			skills_new=$((skills_new + 1))
		fi
		cp -R "$SRC_SKILLS/$name" "$dst"
	done
	note "skills: $skills_new new, $skills_replaced replaced (prior version → .bak.*) → $TGT_SKILLS/"
fi

# --- apply: layer 3 (directive seed) ----------------------------------------
# Copied to a NON-active path on purpose: many directives are neoanvil-
# implementation-specific, and dropping a full set into .claude/rules/ could
# trip neo-mcp's LoadDirectivesFromDisk destructive sweep against the target's
# own BoltDB directives. The operator reviews, prunes, and either renames the
# curated subset into .claude/rules/neo-synced-directives.md or re-adds them
# via neo_learn_directive.
if [ -f "$SRC_DIRECTIVES" ]; then
	cp "$SRC_DIRECTIVES" "$TGT_DIR_SEED"
	note "seeded directives → $TGT_DIR_SEED (review + curate before activating)"
fi

# --- post-checks -------------------------------------------------------------
echo
note "✅ enforcement + fluency layers installed. Remaining (operator):"
note "  1. Layer 0 — confirm the neo MCP server is wired into the target's"
note "     Claude Code config. Without it, everything above is inert."
note "  2. The git pre-commit cert gate installs itself when the target's"
note "     neo-mcp next boots — no action needed if it runs neo-mcp."
if [ -z "$WS_ID" ]; then
	note "  3. Register the workspace + set NEO_WORKSPACE_ID in .claude/settings.json"
	note "     env (boot its neo-mcp once to auto-register, then re-run --force)."
fi
note "  4. Curate $TGT_DIR_SEED — prune neoanvil-specific directives, then"
note "     activate the keepers (rename into .claude/rules/ or neo_learn_directive)."
note "  5. Restart the target's Claude Code session so SessionStart fires briefing.sh."
note "  6. Verify: a new session should auto-print the BRIEFING block, and a perf"
note "     question should make the agent reach for neo_tool_stats unprompted."
