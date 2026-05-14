# Neo Enforcement — onboarding other workspaces

## The problem: a directive is not enforcement

Telling an agent "follow the Ouroboros flow" in `CLAUDE.md` is a **soft request**.
The model can — and routinely does — skip it. The reason the **neoanvil** agent
reliably runs `BRIEFING → BLAST_RADIUS → Edit → certify` is **not** the
directives. It is the **Claude Code hooks**: the harness executes them
unconditionally, before/after tool calls, with no model discretion involved.

Other workspaces (strategos, strategosia_frontend, neo-go) carry the same
directive *text* but **not the hook layer** — so their agents ignore the flow.
This is the same principle as the `feedback_skill_gate_placement` lesson: *the
gate must sit at a layer the agent cannot skip.*

| Layer | Enforced by | Skippable by the model? |
|---|---|---|
| `CLAUDE.md` directive | nobody — it's prose | **yes** |
| `.claude/skills/*` | model decides to invoke | **yes** |
| `.claude/settings.json` hooks | the Claude Code harness | **no** |
| git pre-commit hook | git | **no** |

## What the enforcement layer is

Seven Claude Code hooks (`.claude/settings.json` + `.claude/hooks/*.sh`):

| Hook event | Script | Effect |
|---|---|---|
| `SessionStart` | `briefing.sh` | auto-runs `neo_radar(BRIEFING)` — no "forgot to brief" |
| `UserPromptSubmit` | `userprompt-ds-premortem.sh` | RED-TEAM-LAYERING reminder on multi-file prompts |
| `PreToolUse` (Edit/Write) | `pre-edit-blast.sh` | auto-BLAST_RADIUS + AST_AUDIT gate before every edit |
| `PreToolUse` (neo_memory) | `pre-memory-supersedes.sh` | directive-dedup guard |
| `PostToolUse` (Edit/Write) | `post-edit-cert-reminder.sh` | tracks pending-certify files |
| `PostToolUse` (neo_radar) | `post-ast-audit-cache.sh` | caches AST_AUDIT passes (TTL window) |
| `Stop` | `stop-cert-gate.sh` | blocks ending a turn with uncertified `.go/.ts/...` |

Plus the **git pre-commit cert gate** — but that one installs *itself*: neo-mcp
writes it to `.git/hooks/pre-commit` on every boot
(`cmd/neo-mcp/workspace_utils.go::installPreCommitHook`). Any workspace that
runs neo-mcp already has it; the onboarding script does **not** touch it.

## Onboarding a workspace

```bash
scripts/neo-onboard.sh <target-workspace-path> [--dry-run] [--force]
```

What it does:
1. Resolves the target's workspace ID from `~/.neo/workspaces.json`.
2. Copies `.claude/hooks/*.sh` into the target's `.claude/hooks/`.
3. **Merges** the neo `hooks` block into the target's `.claude/settings.json` —
   target-owned hooks and other keys (`permissions`, `env`, …) are preserved;
   neo's matcher-groups are *appended*, not substituted. A timestamped backup
   is written first.
4. Injects `env.NEO_WORKSPACE_ID` so `briefing.sh` / `pre-edit-blast.sh` target
   the right workspace instead of neoanvil's hardcoded fallback.

`--dry-run` prints the merged `settings.json` and writes nothing. `--force`
re-applies even when the target already carries neo hooks (idempotency is keyed
on `briefing.sh` being present in the target settings).

What it deliberately does **not** do:
- It does not install the git pre-commit hook — neo-mcp self-installs that.
- It does not register the workspace in `~/.neo/workspaces.json` — boot the
  target's neo-mcp once for that (it auto-registers). If the workspace is not
  yet registered, the script still copies the hooks but leaves
  `NEO_WORKSPACE_ID` unset and tells you to re-run with `--force` afterwards.

## Post-onboarding verification

1. Boot the target's neo-mcp once (auto-registers the workspace + installs the
   git pre-commit gate).
2. Re-run `scripts/neo-onboard.sh <target> --force` if the first run could not
   resolve `NEO_WORKSPACE_ID`.
3. Restart the target's Claude Code session — `SessionStart` should fire
   `briefing.sh` and the session should open with the auto-BRIEFING block.
4. Make a trivial edit in the target — `pre-edit-blast.sh` should print a
   BLAST_RADIUS impact assessment before the edit lands.

## Known caveats

- **`briefing.sh` CWD fallback** — the script's `case "$PWD"` only knows
  `*neoanvil*`; for any other workspace it relies on `NEO_WORKSPACE_ID` (which
  this onboarding script sets in `settings.json` env). If that env is missing
  the hook briefs the wrong workspace.
- **Display strings** — `post-edit-cert-reminder.sh` hardcodes
  `mcp__neoanvil__neo_sre_certify_mutation` in its operator-facing message. The
  *gate* still works (it's path/state based, not MCP-name based), but the hint
  text names neoanvil's MCP server. A fully generic kit would template the MCP
  server name; for now it is a cosmetic mismatch, noted as a follow-up.
- The hook scripts invoke neo via `curl` to the Nexus dispatcher
  (`NEO_NEXUS_URL`, default `http://127.0.0.1:9000`) — the target must be a
  Nexus-managed workspace for the hooks to reach a live MCP.
