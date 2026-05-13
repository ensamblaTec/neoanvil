# neo-doctrine migration guide

Adopt the Ouroboros V10.6 doctrine + lifecycle hooks + directives durability flow in a new repo/workspace. This is the same structure that neoanvil ships natively and that has been mirrored to `/develop/other/` (strategos + strategosia_frontend umbrella).

**Source of truth:** `/Users/manufactura/go/src/github.com/ensamblatec/neoanvil/.claude/` and `/Users/manufactura/go/src/github.com/ensamblatec/neoanvil/CLAUDE.md`. This guide describes how to replicate the relevant subset in a target project.

---

## 1. What gets migrated

| Artifact | Path in target | Purpose |
|---|---|---|
| 7 lifecycle hooks | `.claude/hooks/*.sh` | Auto-enforce Ouroboros flow (BRIEFING → BLAST_RADIUS → Edit → certify) |
| Hook registration | `.claude/settings.json` | Wire hooks into Claude Code lifecycle events |
| (Optional) Skills | `.claude/skills/*/SKILL.md` | Task-mode + path-scoped doctrine, on-demand |
| (Optional) Rules dir | `.claude/rules/` | Synced directives (mirrors BoltDB via `neo_memory(learn)`) |
| (Optional) `CLAUDE.md` | Project root | Inline contract — auto-loaded by Claude Code at session start |

**Minimum viable migration:** hooks + settings.json. The rest are nice-to-have.

---

## 2. Hook inventory + sizing

| Hook | Event matcher | Bytes | Purpose |
|---|---|---|---|
| `briefing.sh` | SessionStart | 2,642 | Inject BRIEFING at session boot (auto-detects workspace ID from CWD) |
| `userprompt-ds-premortem.sh` | UserPromptSubmit | 6,053 | Detect multi-file/concurrency/hot-path prompts → remind of 4-layer red-team flow |
| `pre-edit-blast.sh` | PreToolUse: Edit\|Write\|MultiEdit | 10,580 | Auto-BLAST_RADIUS before Edit + AST_AUDIT_BOLTDB hard gate for transactional code |
| `pre-memory-supersedes.sh` | PreToolUse: `mcp__*__neo_memory` | 4,167 | Warn before adding directive with already-existing `[TAG]` |
| `post-edit-cert-reminder.sh` | PostToolUse: Edit\|Write\|MultiEdit | 2,525 | Track pending-cert files in session_state, warn before commit |
| `post-ast-audit-cache.sh` | PostToolUse: `mcp__*__neo_radar` | 4,401 | Cache CLEAN AST_AUDIT runs so subsequent Edits don't re-gate |
| `stop-cert-gate.sh` | Stop | 2,197 | Final cert-pending check at session close |

**Total:** ~32 KB of shell, 7 hooks, 818 lines. All bash 3.2 compatible (macOS-safe).

---

## 3. Migration steps

### Step 1 — copy the hook files

```bash
TARGET=<path-to-target-repo>
SRC=/Users/manufactura/go/src/github.com/ensamblatec/neoanvil

mkdir -p "$TARGET/.claude/hooks"
cp "$SRC/.claude/hooks/"*.sh "$TARGET/.claude/hooks/"
chmod +x "$TARGET/.claude/hooks/"*.sh
```

### Step 2 — patch workspace detection in 2 hooks

`briefing.sh` and `pre-edit-blast.sh` auto-detect workspace ID from CWD. The default fallback (`neoanvil-35694`) won't match your repo. Patch the `case "$PWD" in` block in both files:

```bash
# briefing.sh / pre-edit-blast.sh
case "$PWD" in
  *<your-project-substring>*)  DEFAULT_WS="<your-workspace-id>" ;;
  *)                            DEFAULT_WS="<your-workspace-id>" ;;
esac
```

Workspace IDs live in `~/.neo/workspaces.json`. Spawn one via:

```bash
curl -X POST http://127.0.0.1:9000/api/v1/workspaces/start/<workspace-name>
```

If `/develop/other/` (strategos umbrella) is your pattern, the existing fallback already handles strategos/strategosia.

### Step 3 — copy + adapt settings.json

```bash
cp "$SRC/.claude/settings.json" "$TARGET/.claude/settings.json"
```

**Federation matcher expansion:** if the target workspace will route through Nexus with multiple MCP server prefixes (e.g. `mcp__neoanvil__neo_memory` + `mcp__neo-yourproject__neo_memory`), expand the `PreToolUse`/`PostToolUse` matchers to alternate them:

```json
{
  "matcher": "mcp__neoanvil__neo_memory|mcp__neo-yourproject__neo_memory|mcp__neo-yourumbrella__neo_memory",
  "hooks": [...]
}
```

The `/develop/other/.claude/settings.json` is the canonical reference for federation-aware matchers.

### Step 4 — (optional) install CLAUDE.md

If the target project doesn't already have a CLAUDE.md, copy the neoanvil one as a starting template and trim:

```bash
cp "$SRC/CLAUDE.md" "$TARGET/CLAUDE.md"
```

Target ≤60 lines (see [`docs/general/directives-durability.md`](../general/directives-durability.md) for the upfront-context-budget discipline).

### Step 5 — (optional) install skills + rules

```bash
mkdir -p "$TARGET/.claude/skills" "$TARGET/.claude/rules"
cp -r "$SRC/.claude/skills/"* "$TARGET/.claude/skills/"
# Rules directory starts empty — `neo_memory(action:learn)` populates it via dual-layer sync.
```

If you don't run neo-mcp from this target workspace, skills/rules won't auto-inject from BoltDB — operator must use Claude Code's native `/skill-name` invocation.

---

## 4. Validation checklist

After migration, validate from inside the target repo:

```bash
# Hook syntax
for h in .claude/hooks/*.sh; do bash -n "$h" && echo "$h OK"; done

# Hook smoke test — should emit valid JSON with no errors
echo '{"prompt":"refactor del HTTP plugin dispatcher con streaming SSE"}' | \
  bash .claude/hooks/userprompt-ds-premortem.sh | python3 -m json.tool | head

# Workspace detection
echo '{"tool_name":"Edit","tool_input":{"file_path":"'"$PWD"'/example.go"}}' | \
  bash .claude/hooks/pre-edit-blast.sh 2>&1 | grep -E "workspace|BLAST"

# Settings.json shape
python3 -c "import json; d=json.load(open('.claude/settings.json')); print('hooks events:', list(d['hooks'].keys()))"
```

Expected: 7 events listed (SessionStart, UserPromptSubmit, PreToolUse×2, PostToolUse×2, Stop), each hook bash-syntax-clean, workspace detection returns the target ID.

---

## 5. Workspace-specific adaptations matrix

| Hook | Adaptable? | What to patch |
|---|---|---|
| `briefing.sh` | yes | `case "$PWD" in` block → workspace ID |
| `pre-edit-blast.sh` | yes | `case "$PWD" in` block + (optional) BoltDB path patterns if your project uses different `pkg/` layout |
| `post-edit-cert-reminder.sh` | no | Workspace-agnostic |
| `post-ast-audit-cache.sh` | no | Workspace-agnostic — processes response shape only |
| `pre-memory-supersedes.sh` | no | Reads tool_input + greps local file |
| `userprompt-ds-premortem.sh` | no | Reads prompt + keyword match |
| `stop-cert-gate.sh` | no | Workspace-agnostic |

Only 2 of 7 hooks need workspace-specific patches. The rest are portable verbatim.

---

## 6. Federation context (if applicable)

If the target project is part of a federation (multiple workspaces sharing a project directory like `/develop/other/`), check that:

1. `~/.neo/workspaces.json` has the target workspace registered (`type: workspace`).
2. `~/.neo/nexus.yaml` lists it under `managed_workspaces` (or leave empty = manage all).
3. `.neo-project/neo.yaml` (if multi-workspace project) declares `coordinator_workspace`.
4. Hook matchers in `settings.json` cover all MCP prefixes that route through Nexus:
   - `mcp__neoanvil__*` (cross-routed via Nexus default)
   - `mcp__neo-<your-workspace-name>__*` (direct child neo-mcp)

See [`docs/guide/neo-project-federation-guide.md`](./neo-project-federation-guide.md) for full federation setup.

---

## 7. Directives durability inheritance

The migration brings the directives durability story automatically — it lives entirely in the neo-mcp binary (`pkg/rag/wal.go`), not in hooks. As long as the target workspace points at a recent neo-mcp build (≥ commit `549dde9`), it inherits:

- 2-tier corruption guards (`abs` + `rel-loss > 20%`) at boot
- Pre-destructive snapshot before `CompactDirectives`
- `neo_memory(action_type:restore)` action for recovery

No additional migration work needed. See [`docs/general/directives-durability.md`](../general/directives-durability.md) + [`docs/adr/ADR-017-directives-durability.md`](../adr/ADR-017-directives-durability.md).

---

## 8. Metrics — what migration delivers

| Capability | Numbers |
|---|---|
| Hook count | 7 |
| Hook total LOC | 818 |
| Hook total size | 32,565 bytes |
| Lifecycle events covered | 5 (SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, Stop) |
| Tool matchers | 2 wildcards (Edit\|Write\|MultiEdit + neo_memory + neo_radar) |
| Average hook timeout | 6.7s (sum: 47s, max: 20s briefing) |
| Bash version required | 3.2 (macOS-safe) |
| External deps | jq, python3, curl, git |
| Workspace adaptations needed | 2 of 7 hooks |
| Federation-aware matchers | 4 of 6 PreToolUse/PostToolUse entries |

---

## 9. Rollback

If the hooks become problematic during a session:

```bash
# Per-hook disable (env var)
NEO_DS_PREMORTEM_DISABLE=1
NEO_MEMORY_HOOK_DISABLE=1
NEO_AST_CACHE_HOOK_DISABLE=1
NEO_BLAST_HOOK_AST_BYPASS=1   # only bypasses BoltDB hard-gate, not full BLAST_RADIUS

# Full rollback
rm .claude/settings.json   # or rename to .claude/settings.json.disabled
```

Hooks fail-open by default — if any hook errors or times out, the underlying tool call still proceeds (Claude Code lifecycle continues). Hard gates are the only exception (they return `permissionDecision:"ask"`, not `"deny"`).

---

## See also

- [`docs/general/neo-global.md`](../general/neo-global.md) — universal portable operational laws (G1-G19)
- [`docs/general/directives-durability.md`](../general/directives-durability.md) — recovery flow operator guide
- [`docs/adr/ADR-016-ouroboros-lifecycle-hooks.md`](../adr/ADR-016-ouroboros-lifecycle-hooks.md) — hook framework design rationale
- [`docs/adr/ADR-017-directives-durability.md`](../adr/ADR-017-directives-durability.md) — durability hardening rationale
- `/Users/manufactura/develop/other/.claude/` — canonical migrated example (strategos umbrella)
