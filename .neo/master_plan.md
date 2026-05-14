# NeoAnvil — Master Plan

## Phase: DS plugin — background task retrieval

The DeepSeek plugin's `background:true` mode for `red_team_audit` /
`map_reduce_refactor` returns `{task_id, status:pending}` but that `task_id` is
**not pollable** through the tool interface — the `task_id` arg is documented
(and wired) only for `generate_boilerplate`. Background audit results are
currently unretrievable. Surfaced 2026-05-14 trying to run a DS premortem in
background mode (the foreground path timed out on v4-pro+max — fixed in
`eb3c4c7`, but background remains the intended escape hatch and it's broken).
See `technical_debt.md` → `[ds-background-unretrievable]`.

- [ ] Investigate the Nexus-side background dispatch — where the goroutine
      result lands, and whether there is a task store that can be queried.
- [ ] Wire `task_id` polling for `red_team_audit` + `map_reduce_refactor`,
      mirroring the existing `generate_boilerplate` poll path.
- [ ] Update the `deepseek_call` tool schema so `task_id` documents all three
      background-capable actions; add a regression test.
