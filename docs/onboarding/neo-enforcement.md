# Neo Enforcement — onboarding other workspaces

## The problem: three things that aren't the same

Telling an agent "follow the Ouroboros flow" in `CLAUDE.md` is a **soft request**
the model routinely skips. But "make strategos use neo" is not one problem —
it's three, and they need different fixes:

| | What it is | How the agent gets it |
|---|---|---|
| **Discoverability** | `neo_tool_stats` is in the agent's tool list, with a self-describing schema | the neo MCP being *connected* — nothing else |
| **Enforcement** | the *flow* the agent **cannot skip** (BRIEFING, BLAST_RADIUS gate, cert gate) | Claude Code hooks — the harness runs them |
| **Fluency** | the *reflex* — "perf question → `neo_tool_stats sort_by:p99`", "refactor → `AST_AUDIT` batch" | the skills + directives loaded into context |

A workspace can have full *discoverability* (every neo tool in its list) and
still never use them — because nothing **primes** the agent to reach for them.
That priming is the auto-BRIEFING (a hook) plus the skills. This is the
`feedback_skill_gate_placement` lesson generalised: *the behaviour you want
must live at a layer the agent cannot skip or skim past.*

## The four layers

| Layer | What | Skippable? | Ported by `neo-onboard.sh`? |
|---|---|---|---|
| **0 — MCP wired** | the neo MCP server in the workspace's Claude Code config | n/a — without it, zero neo tools | **preflight check only** |
| **1 — Enforcement** | `.claude/settings.json` hooks + `.claude/hooks/*.sh` | **no** — harness runs them | **yes** |
| **2 — Fluency** | `.claude/skills/*` — the "for X reach for Y" doctrine | yes (model loads context) | **yes** (curated) |
| **3 — Directives** | accumulated lessons (`[TOOL_COST_AUDIT]`, `[option-D]`, …) | yes | **seed only** (operator curates) |

The git pre-commit cert gate is a fifth enforcement point but installs
*itself*: neo-mcp writes it to `.git/hooks/pre-commit` on every boot
(`cmd/neo-mcp/workspace_utils.go::installPreCommitHook`). The script does not
touch it.

### Layer 1 — the seven hooks

| Hook event | Script | Effect |
|---|---|---|
| `SessionStart` | `briefing.sh` | auto-runs `neo_radar(BRIEFING)` — primes "SRE mode", no "forgot to brief" |
| `UserPromptSubmit` | `userprompt-ds-premortem.sh` | RED-TEAM-LAYERING reminder on multi-file prompts |
| `PreToolUse` (Edit/Write) | `pre-edit-blast.sh` | auto-BLAST_RADIUS + AST_AUDIT gate before every edit |
| `PreToolUse` (neo_memory) | `pre-memory-supersedes.sh` | directive-dedup guard |
| `PostToolUse` (Edit/Write) | `post-edit-cert-reminder.sh` | tracks pending-certify files |
| `PostToolUse` (neo_radar) | `post-ast-audit-cache.sh` | caches AST_AUDIT passes (TTL window) |
| `Stop` | `stop-cert-gate.sh` | blocks ending a turn with uncertified `.go/.ts/...` |

### Layer 2 — the skills

All of `.claude/skills/*` **except `sre-db`** — its auto-load path globs
(`pkg/dba/`, `pkg/rag/`, `migrations/`) are neoanvil's layout and won't match
another project. Everything else (`sre-doctrine`, `sre-workflow`, `sre-tools`,
`sre-troubleshooting`, `sre-quality`, the `*-workflow` and `daemon-*` skills,
…) is neo-generic and ports verbatim.

### Layer 3 — the directive seed

`neo-synced-directives.md` is copied to `.claude/neo-directives-seed.md` — a
**non-active path on purpose**. Many directives are neoanvil-implementation-
specific (`[GO-ARM64-ASM]`, `[HNSW-QUANT-WIRING]`, file/commit references), and
dropping a full set into `.claude/rules/` could trip neo-mcp's
`LoadDirectivesFromDisk` destructive sweep against the target's own BoltDB
directives. The operator reviews, prunes, and activates the curated keepers
(rename into `.claude/rules/neo-synced-directives.md`, or re-add via
`neo_learn_directive`).

## Onboarding a workspace

```bash
scripts/neo-onboard.sh <target-workspace-path> [--dry-run] [--force]
```

What it does:
1. **Layer-0 preflight** — checks the target's `.mcp.json` for a neo MCP server
   (Nexus SSE url or a `neo-mcp` command); loud warning if absent, since hooks
   and skills are inert without it. Not a hard fail — the server may live in
   the operator's global `~/.claude.json`.
2. Resolves the target's workspace ID from `~/.neo/workspaces.json`.
3. Copies `.claude/hooks/*.sh` into the target.
4. **Merges** the neo `hooks` block into `.claude/settings.json` — target-owned
   hooks and other keys (`permissions`, `env`, …) are preserved; neo's
   matcher-groups are *appended*, not substituted; a timestamped backup first.
   The two `mcp__neoanvil__*` hook matchers are **retargeted** to the target's
   MCP server name (read from its `.mcp.json`) — otherwise those hooks would
   never fire on the target.
5. Injects `env.NEO_WORKSPACE_ID` so the hooks target the right workspace.
6. Copies the curated skill set into `.claude/skills/`.
7. Seeds `.claude/neo-directives-seed.md` for the operator to curate.

`--dry-run` prints the merged `settings.json` + the skill list, writes nothing.
`--force` re-applies even when the target already carries neo hooks (idempotency
is keyed on `briefing.sh` being present in the target settings).

What it deliberately does **not** do: install the git pre-commit hook (neo-mcp
self-installs it); register the workspace in `~/.neo/workspaces.json` (boot the
target's neo-mcp once); activate the directive seed (operator curates first).

## Post-onboarding verification

1. **Layer 0** — confirm the neo MCP is wired into the target's Claude Code
   config. Without it everything else is inert.
2. Boot the target's neo-mcp once (auto-registers the workspace + installs the
   git pre-commit gate). Re-run `neo-onboard.sh <target> --force` if the first
   run could not resolve `NEO_WORKSPACE_ID`.
3. Curate `.claude/neo-directives-seed.md` and activate the keepers.
4. Restart the target's Claude Code session — `SessionStart` should fire
   `briefing.sh` and the session opens with the auto-BRIEFING block.
5. Make a trivial edit — `pre-edit-blast.sh` should print a BLAST_RADIUS impact
   assessment before the edit lands.
6. Ask a perf question — the primed agent should reach for `neo_tool_stats`
   unprompted (fluency, layer 2).

## Known caveats

- **`briefing.sh` CWD fallback** — the script's `case "$PWD"` only knows
  `*neoanvil*`; other workspaces rely on `NEO_WORKSPACE_ID` (which `neo-onboard.sh`
  sets in `settings.json` env). If that env is missing the hook briefs the
  wrong workspace.
- **Hook-script display strings** — `post-edit-cert-reminder.sh` hardcodes
  `mcp__neoanvil__neo_sre_certify_mutation` in its operator-facing *message*
  text. The settings.json *matchers* are retargeted by the script, and the
  *gate* itself is path/state based, so behaviour is correct — only the hint
  text names neoanvil's MCP server. Cosmetic; follow-up to template it.
- The hook scripts invoke neo via `curl` to the Nexus dispatcher
  (`NEO_NEXUS_URL`, default `http://127.0.0.1:9000`) — the target must be a
  Nexus-managed workspace for the hooks to reach a live MCP.
- **Skills are reference, not enforcement.** Task-mode skills only enter context
  when invoked; auto-load skills (`sre-doctrine`, `sre-workflow`, …) are the
  always-on priming. Copying them gives the agent the *option* of fluency — the
  auto-BRIEFING hook is what makes it *take* that option.
