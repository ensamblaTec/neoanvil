---
name: jira-doc-from-commit
description: Generate doc pack for a Jira ticket from a git commit, then attach. The plugin auto-derives files + summary from `git show <hash>` so the operator only needs the ticket key + commit hash. Default excludes drop auto-managed metadata (.neo/master_plan.md, master_done.md, technical_debt.md, go.sum, .gitignore). Use when closing a story right after the commit lands.
disable-model-invocation: true
argument-hint: <TICKET_KEY> <COMMIT_HASH>
allowed-tools: mcp__neoanvil__jira_jira
---

# Doc pack from commit (one-line invocation)

The user typed: `$ARGUMENTS`

## Parse arguments

The first argument is the Jira ticket key (e.g. `MCPI-28`). The
second is the git commit hash (short or long). Example invocations:

```
/jira-doc-from-commit MCPI-28 b19e7bc
/jira-doc-from-commit MCPI-22 d32ff23
```

If the operator passed only one argument or none, ask which combo
they meant. Don't guess.

## Validation

- Ticket key matches `^[A-Z]+-\d+$`
- Commit hash is 4+ hex chars (`git rev-parse --short` style)

If either fails the regex, abort and tell the operator the format.

## Workflow

Single tool call:

```
mcp__neoanvil__jira_jira(
  action: "prepare_doc_pack",
  ticket_id: "<TICKET_KEY>",
  repo_root: "/path/to/neoanvil",
  commit_hash: "<COMMIT_HASH>",
  auto_attach: true
)
```

Plugin behavior with `commit_hash`:
1. Runs `git show --name-status --pretty=format: <hash>` to enumerate
   files touched (skips D-status deletions and submodules)
2. Filters out paths matching `exclude_paths`. Default excludes apply
   when the field is omitted: `.neo/master_plan.md`,
   `.neo/master_done.md`, `.neo/technical_debt.md`, `.neo/.env`,
   `.neo/db/`, `go.sum`, `.gitignore`
3. Runs `git show -s --format=%B <hash>` for commit message body =
   the README summary
4. Copies remaining files to `~/.neo/jira-docs/<KEY>/code/<descriptor>`
   with snake_case descriptors derived from path
5. Writes a CONCISE README.md (summary + Cambios + 1-3 commits)
6. Zips with smart filter (skips auto-rendered code-snaps + empty
   subdirs) — root folder = `<KEY>/`
7. Uploads as `<KEY>.zip` to Jira via `/issue/<KEY>/attachments`
8. Audit log entry with kind=jira_prepare_doc_pack

## Output to operator

Tell the operator the result in 3-5 lines:

```
📦 MCPI-28 documented from commit b19e7bc
   Files: docs/adr/ADR-007-bidirectional-webhooks.md
   Zip: ~/.neo/jira-docs/MCPI-28.zip (5 KB)
   Atlassian: https://your-org.atlassian.net/browse/MCPI-28
```

## When the commit touches files outside the ticket

If the operator notices the pack includes files unrelated to the
ticket (e.g. a Makefile change that belongs to a sibling story), the
solution is to re-invoke with `exclude_paths: [...]` listing the
unwanted patterns. Don't auto-detect — the operator knows the
boundary best.

```
mcp__neoanvil__jira_jira(
  action: "prepare_doc_pack",
  ticket_id: "MCPI-28",
  repo_root: "/path/to/neoanvil",
  commit_hash: "b19e7bc",
  exclude_paths: ["Makefile", ".neo/master_plan.md", ".neo/master_done.md", ".neo/technical_debt.md"],
  auto_attach: true
)
```

## Anti-patterns

- ❌ Pasar `files: [...]` Y `commit_hash: ...` simultáneamente. Si
  files está populado, commit_hash sólo sirve para Summary y
  CommitRange — los archivos no se re-derivan
- ❌ Llamar varias veces para el mismo ticket sin limpiar
  `~/.neo/jira-docs/<KEY>/` primero — overlay de runs queda en el zip
- ❌ Usar este skill para tickets sin commit todavía (Backlog/Selected
  state). Wait until commit lands

## See also

- `.claude/skills/jira-workflow/SKILL.md` — overall doctrine
- `.claude/skills/neo-doc-pack/SKILL.md` — manual pack builder
- `docs/jira-integration-guide.md` — operator runbook
