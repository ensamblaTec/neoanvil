## Summary

Release-ready merge of `develop` into `main`. Brings the **B1 Adaptive Runtime** experiment and **launch-prep** sanitization into the publishable branch.

### B1 — Tool Discipline Mirror (adaptive runtime experiment)

- SessionStart Tool Discipline Mirror in BRIEFING (auto-injected behaviour-diff)
- A/B measurement scaffolding — auto-snapshot via SessionStart/Stop hooks; CSV history at `.neo/b1-measurements.csv` (gitignored)
- Cross-workspace port — strategos mirror + shared CSV
- Zero-touch auto-arm rotation from CSV history (no operator memory required between sessions)
- Audit fixes: `.gitignore` covers CSV backups; report ceiling now dynamic
- Drop deprecated `neo_forge_tool` + dead `$PWD` case in hook helpers

### Launch-prep

- Sanitize `/Users/manufactura/...` absolute paths in public docs (replaced with `<neoanvil-repo>`, `<workspace-parent>`, `<strategos>`, `<strategosia_frontend>`, `<umbrella>` placeholders)
- Auto-distilled REM-cycle wisdom rotation in `.neo/master_plan.md`
- Full launch documentation set under `docs/launch/` (server.json, awesome-mcp-servers entry, community post drafts, publish checklist)

## Verification

- [ ] CI green (audit-ci, build-mcp, build-tui)
- [ ] `go test -short ./pkg/...` passes
- [ ] No personal paths in any tracked file (`git grep '/Users/'` returns empty)
- [ ] No tracked secrets (gitleaks-equivalent grep returns only `.example` templates)

## Ship sequencing

1. Merge this PR
2. Add `server.json` to root (per `docs/launch/PUBLISH-CHECKLIST.md` §2.2)
3. Publish to MCP Registry (`mcp-publisher publish`)
4. Open awesome-mcp-servers PR (per `docs/launch/awesome-mcp-entry.md`)
5. Wait 24h, then community posts (per `docs/launch/PUBLISH-CHECKLIST.md` §4)

## Risk

- B1 trial is still actively measuring (B1.9-11 open). Merging to main does NOT freeze the trial — measurement CSV continues to populate. The verdict commit will land separately when ≥5 baseline + ≥5 treatment sessions accumulate.
- No public-API changes; all MCP tool signatures stable.
