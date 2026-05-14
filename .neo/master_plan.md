# NeoAnvil — Master Plan

## Phase: DS plugin — background task retrieval

### Problem

`deepseek_call` with `background:true` for `red_team_audit` / `map_reduce_refactor`
returns `{task_id, status:pending}` immediately — but that `task_id` **cannot be
polled**. The `task_id` argument is documented and wired ONLY for
`generate_boilerplate`. So a background audit's result is unretrievable: the
goroutine runs, produces output, and the caller can never fetch it.

Surfaced 2026-05-14 trying to run a DS premortem in background mode
(`async_8d95eaed5c58856c`). The foreground path timed out on v4-pro+max — that
was fixed in `eb3c4c7` (config-driven HTTP timeout) — but `background:true`
remains the intended escape hatch for slow audits and it is broken.
Debt: `technical_debt.md` → `[ds-background-unretrievable]`, mirrored in
`neo_debt` (workspace tier, P2).

### Reference — the working pattern

`cmd/plugin-deepseek/tool_boilerplate.go` ALREADY does plugin-side background
correctly. Use it as the template:
- `bgTaskStatus` struct + a BoltDB bucket tracking `{task_id, status, result}`.
- `runBoilerplateTask` — the work runs in a goroutine, writes status/result to BoltDB.
- `queryBGTaskStatus` — given a `task_id`, returns the persisted status/result.
- `generateBoilerplateWithDB` short-circuits at the TOP: `if taskID != "" { return queryBGTaskStatus(...) }` — BEFORE any new-task logic.

GOTCHA observed: polling `red_team_audit` with `task_id` today starts a *fresh
audit thread* — because the handler treats unrecognised args as a new prompt.
The poll branch MUST short-circuit before the thread-creation logic, exactly
like `tool_boilerplate.go:59`.

### Tasks

- [ ] **Investigate the current `background` dispatch path.** Determine whether
      `background:true` for `red_team_audit`/`map_reduce_refactor` is handled
      Nexus-side (and the result store is missing/incomplete) or simply
      unhandled. Grep `cmd/neo-mcp` + `cmd/neo-nexus` for the `background` arg
      handling and any async task registry. Decision to record: extend the
      proven plugin-side pattern (recommended — consistent with
      `generate_boilerplate`, no Nexus changes) vs build a Nexus-side store.
- [ ] **Wire plugin-side background for `red_team_audit`** in
      `cmd/plugin-deepseek/tool_red_team.go`: add a `task_id` poll branch at the
      top of `redTeamAuditWithDB` (mirror `tool_boilerplate.go:59`); when
      `background:true`, launch the audit in a goroutine that persists
      `{task_id, status, result}` to a BoltDB bucket via `getPluginDB`, and
      return the `task_id` immediately.
- [ ] **Wire the same for `map_reduce_refactor`** in `tool_map_reduce.go` —
      same poll-branch + background-launch shape. Factor the shared
      task-store helpers (`launchBGTask` / `queryBGTask`) so red_team and
      map_reduce don't each re-implement it.
- [ ] **Schema + regression test.** Update `cmd/plugin-deepseek/main.go` so the
      `deepseek_call` `task_id` description covers all three background-capable
      actions (not just `generate_boilerplate`). Add a test:
      `background:true` → `task_id` → poll returns `pending` → poll again
      returns `complete` + result text.
- [ ] **DS premortem** before the first edit — this touches background task
      lifecycle + concurrency on the plugin (≥3 files, new subsystem), so per
      `[SRE-RED-TEAM-LAYERING]` run `deepseek_call(red_team_audit)` on the plan.
      Use the *synchronous* path with `DEEPSEEK_HTTP_TIMEOUT_SECONDS` raised
      (background is the thing being fixed — can't rely on it yet).

### Done when

`deepseek_call(red_team_audit, background:true, …)` returns a `task_id`, and a
follow-up `deepseek_call(red_team_audit, task_id:"<id>")` returns
`status:pending` while running and `status:complete` + the full audit text once
done — same for `map_reduce_refactor`. Regression test green; schema accurate;
certified.
