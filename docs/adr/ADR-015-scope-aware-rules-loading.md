# ADR-015 — Scope-aware rules + skills loading

**Status:** Phase 1 implemented (informational field). Phase 2 (hook-based filter) deferred.
**Date:** 2026-05-13.
**Author:** ctx-bloat refactor (step F).

## Context

After the 2026-05-13 context-bloat refactor (steps D, B, A, E, C), upfront session context dropped from ~64k tokens to ~14k tokens. The remaining ~14k is split:

| Source | Tokens | Scope-specific? |
|---|---|---|
| `CLAUDE.md` (51 lines) | ~940 | universal |
| `.claude/rules/neo-synced-directives.md` (51 entries) | ~4,300 | universal (BoltDB-managed) |
| `.claude/rules/neo-sre-doctrine.md` (22KB) | ~5,500 | mostly universal |
| `.claude/rules/neo-workflow.md` (14KB) | ~3,500 | mostly universal |
| `.claude/rules/neo-db.md` (3KB) | ~750 | **DB work only** |
| 2 auto-load skills (sre-doctrine, sre-troubleshooting) | ~1,500 | universal |

The single clearly scope-specific file (`neo-db.md`) is 3KB. The bigger lever for future scope-aware optimization is when more rules accumulate.

**Problem:** Claude Code auto-loads ALL `.claude/rules/*.md` as project instructions unconditionally — no built-in filter. SessionStart hooks can ADD context but cannot UNLOAD already-injected rules.

## Decision

**Phase 1 (this commit):** Add `workspace.scope` config field as an informational marker. Backfill default `"fullstack"`. Values: `fullstack | backend | frontend | infra | db`. Today the field is read by no code beyond config tests — it's a hook point for Phase 2.

**Phase 2 (deferred — separate PR):** SessionStart hook reads `cfg.Workspace.Scope`, conditionally moves scope-irrelevant rules to a `.claude/rules-disabled/` shadow directory at session start (and restores on session end). Or alternatively: Claude Code adds a `loaderScope` setting natively (waiting on upstream).

**Phase 2 implementation paths** (not chosen yet, document for future):

1. **Symlink-based** — at SessionStart, move `.claude/rules/neo-db.md` to `.claude/rules-disabled/` if `scope != "db" && scope != "fullstack"`. Restore on `Stop` hook. Pro: zero code change. Con: race condition if multiple Claude Code instances on same workspace.

2. **Hook-injected only** — move ALL rule files out of `.claude/rules/` to `.claude/rules-loader/<scope>/`; SessionStart hook reads the scope and `cat`s the relevant files to stdout (becomes injected context). Pro: clean separation. Con: requires deleting the `.claude/rules/` dir entirely — invasive.

3. **Wait for upstream** — file Anthropic feature request for `claude.config.rulesScope` setting. Pro: official solution. Con: timeline unknown.

## Why not implement Phase 2 now

- Today's savings ceiling is ~3KB (only `neo-db.md` is clearly scope-specific). Most other rules are universal.
- Symlink approach has cross-session race conditions worth a careful design.
- More leverage will exist after more rules accumulate (PILAR LXX+).

## Implementation (Phase 1)

```go
// pkg/config/config.go
type WorkspaceConfig struct {
    ...
    Scope string `yaml:"scope,omitempty"`  // 358.A
}

// defaultNeoConfig()
Workspace: WorkspaceConfig{
    ...
    Scope: "fullstack",
}

// applyWorkspaceDefaults()
if cfg.Workspace.Scope == "" {
    cfg.Workspace.Scope = "fullstack"
    *ns = true  // CONFIG-FIELD-BACKFILL-RULE
}
```

```yaml
# neo.yaml.example
workspace:
  scope: "fullstack"  # fullstack | backend | frontend | infra | db
```

```go
// pkg/config/config_test.go
TestLoadConfig_WorkspaceScope_Backfill {
    - empty scope → backfilled to "fullstack"
    - round-trip preserves "fullstack" (no perpetual "" loop)
    - explicit "backend" is preserved (no clobbering)
}
```

## Consequences

**Positive**
- Hook point exists for Phase 2.
- Operators of frontend/infra workspaces (strategosia_frontend, future infra workspaces) can declare scope today; will benefit automatically when Phase 2 lands.
- No breaking changes — additive field, default = current behavior.

**Negative**
- Field is dead weight in Phase 1 — read only by tests.
- Risk that Phase 2 never lands and the field becomes documentation-only.

## Mitigation for the "dead field" risk

Following PR (separate Epic): wire `cfg.Workspace.Scope` into BRIEFING compact output:

```
Mode: pair | Scope: backend | Phase: ... | ...
```

This makes the field visible to operators and to future me (the agent) — even before Phase 2 enforcement, the scope acts as a hint to the agent about which skills/rules are most relevant.

## References

- [CONFIG-FIELD-BACKFILL-RULE] in `.claude/rules/neo-synced-directives.md`
- ctx-bloat refactor commits: `821bb4c` (D), `d642bc1` (B), `8828c61` (A), `b7d5a79` (E), `396c560` (C)
- Upstream issue (TBD): file at `anthropics/claude-code` requesting `rules.scopeFilter` config.
