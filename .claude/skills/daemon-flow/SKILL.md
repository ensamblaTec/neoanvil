---
name: daemon-flow
description: Operator UI for the iterative MCP-driven daemon (PILAR XXVII). Surfaces pending tasks, active task, top-5 trust patterns, and the next daemon suggestion in a compact conversational format. Use when running the daemon manually or auditing what the iterative loop sees. Task-mode skill (invoke with `/daemon-flow`).
---

# Daemon Flow — Operator Conversational UI

> PILAR XXVII iterative daemon ergonomic surface. Replaces "ask the
> agent to query 4 different actions and parse JSON manually" with
> a single skill that paints the operator's view of the daemon.

## When to invoke

- Running the daemon manually with `execute_next` / `approve` / `reject`
- Verifying what the next task in the queue is and what the daemon
  would suggest before committing to it
- Auditing the trust state for the current workspace
- Quick debugging when an `execute_next` returns an unexpected backend
  or suggested_action

## What the skill outputs

A markdown report with three sections:

```
## Daemon Flow — <workspace_id>

### Queue
| ID        | Description                | Target            | Lifecycle    |
|-----------|----------------------------|-------------------|--------------|
| TASK-007  | refactor logger split      | pkg/state/...     | pending      |
| TASK-008  | distill INC corpus         | .neo/incidents/   | pending      |

### Active task
- TASK-006 (in_progress, started 14m ago) · backend: deepseek (distill_payload)

### Top-5 patterns by trust
| Pattern   | Scope                | α    | β    | Tier | LB   | Execs |
|-----------|----------------------|------|------|------|------|-------|
| refactor  | .go:pkg/state        |  41  |   3  | L1   | 0.78 |   42  |
| audit     | .go:pkg/sre          |  18  |   5  | L0   | 0.41 |   22  |
...

### Next suggestion
- task: TASK-007
- backend: deepseek (map_reduce_refactor)
- pattern:scope: refactor:.go:pkg/state
- tier: L1 → suggested_action: auto-approve (RNG bypassed spot-check)
```

## How the skill maps to MCP actions

The skill calls four MCP actions in sequence and renders the merged
output. Each call is read-only (or near-readonly) — invoking
`/daemon-flow` does NOT advance the queue.

| Section          | MCP action                     | Notes                       |
|------------------|--------------------------------|-----------------------------|
| Queue            | `neo_daemon(action:PullTasks)` | Returns next-in-line + summary |
| Active task      | derived from PullTasks response (`active_task` field) | |
| Top-5 patterns   | `neo_daemon(action:trust_status, top:5)` | Sorted by LowerBound DESC |
| Next suggestion  | dry-run of `execute_next` (skeleton — no actual dispatch) | Reads what the loop WOULD return |

Implementation note: PullTasks moves the task to `in_progress` as a
side-effect — that's an existing daemon contract, not introduced by
this skill. If the operator wants to inspect without advancing,
PullTasks should not be called; the skill should display "(queue
inspection requires advancing — set `peek_only:true` to defer)" and
exit.

## Pair-mode caveat

`neo_daemon` is prohibited in Pair-mode for write actions
(execute_next/approve/reject). `trust_status` is exempted (read-only).
This skill surfaces a clear notice at the top when running in pair:

```
⚠️ Pair-mode: trust_status is shown but execute_next/approve/reject
    are gated. Switch to NEO_SERVER_MODE=daemon to advance the loop.
```

## Output style

Compact tables, no prose. The skill is for fast operator scanning, not
narrative. Latencies, full task descriptions, and lifecycle history
live in `daemon_results` BoltDB bucket — query them via
`neo_daemon(action: trust_status)` with `top: 0` for the full state.

## Related

- `/daemon-trust` — dashboard of trust score evolution
- `docs/pilar-xxvii-daemon-mcp.md` — operator runbook (full)
- `.claude/rules/neo-synced-directives.md` — directive 16+ describes
  the daemon Ouroboros cycle
