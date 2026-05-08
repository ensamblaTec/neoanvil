---
name: jira-id
description: Resolve a master_plan epic ID (e.g. "134.A.1", "130") to its corresponding Jira ticket key (MCPI-N). Use when the user asks "qué MCPI corresponde a la épica X", "give me the Jira ID for 132.D", or before composing a commit message that needs `[EPIC-FINAL MCPI-N]`. Wraps `pkg/jira.ResolveMasterPlanID` via the `neo jira-id` subcommand.
disable-model-invocation: true
argument-hint: <master_plan_epic_id>
---

# Resolve master_plan ID → Jira ticket key

The user passed: `$ARGUMENTS`

Goal: print the `MCPI-N` ticket key whose summary references the given
master_plan epic ID, so the operator can paste it into a commit's
`[EPIC-FINAL MCPI-N]` tag without guessing.

## Step 1 — Validate the argument

If `$ARGUMENTS` is empty, ask for the master_plan epic ID (e.g. "130",
"134.A.1", "131.B"). The format is the same one used in the
`.neo/master_plan.md` checkboxes.

If the user passed something that already starts with `MCPI-`, that's
already a Jira ticket — show them the get_context output instead via
`mcp__neoanvil__jira_jira(action: "get_context", ticket_id: "<their input>")`.

## Step 2 — Resolve via the CLI

Run:

```bash
neo jira-id "$ARGUMENTS"
```

Possible outputs:

- **Single match** (most common): stdout is `MCPI-N`. Report it to the
  user as: "Use `[EPIC-FINAL MCPI-N]` for the commit closing épica
  $ARGUMENTS."
- **Ambiguous match**: stdout is the first `MCPI-N`, stderr contains
  `master_plan ID matches multiple Jira tickets: ... yielded N tickets,
  picked MCPI-X`. Show both lines to the user — they may want to narrow
  the input (e.g. switch from "130" to "130.4") or pick a different
  ticket manually via the Jira board.
- **Not found**: command exits non-zero with an error containing
  `not found`. Tell the user: "No Jira ticket matches `$ARGUMENTS` in
  the active project. Either the master_plan ID is too new (no ticket
  yet — create one with `/jira-create-pilar`), or the summary doesn't
  contain the ID literally."
- **Auth error**: stderr `no jira credentials — run \`neo login --provider jira\``.
  Relay the suggestion verbatim.
- **No active project**: stderr contains `--project is required`.
  Suggest: `neo space use --provider jira --id MCPI --name "Project"`.

## Step 3 — Report the result

Format the response tightly:

```
<source_id> → MCPI-N

(Use [EPIC-FINAL MCPI-N] in the commit subject when closing the épica.)
```

If the resolver returned ambiguous, append the warning so the operator
can audit.

## Why this skill exists

The `[EPIC-FINAL MCPI-N]` tag in commit messages MUST reference a real
Jira ticket — the post-commit hook fires `prepare_doc_pack` against it
and a phantom ID surfaces as `MCPI-N: not found` in the hook output.
See [jira-workflow Regla #8](../jira-workflow/SKILL.md) for the full
convention. The original 30220ae bug came from confusing master_plan ID
130 with ticket MCPI-130 (the actual ticket was MCPI-52).

## Related

- `mcp__neoanvil__jira_jira(action: "get_context", ticket_id: ...)` — once
  you have the MCPI-N from this skill, fetch full context for review.
- `/jira-create-pilar <PILAR>` — create a new Epic + child Stories when
  the resolver returns "not found" because the tickets don't exist yet.
- `pkg/jira.ResolveMasterPlanID` — the underlying Go helper if you want
  to embed the resolution in another tool.
