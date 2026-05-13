# ADR-017 — Directives durability: corruption guards, pre-destructive snapshot, restore

**Status:** Implemented.
**Date:** 2026-05-13.
**Driver:** the 2026-05-13 7-directive drift incident — `.claude/rules/neo-synced-directives.md` silently lost 7 load-bearing directives between sessions with no recovery path beyond `git revert`.

## Context

The dual-layer sync between BoltDB (`hnsw_directives` bucket) and disk (`.claude/rules/neo-synced-directives.md`) had two failure modes when this incident hit:

1. **Boot-time destructive sweep** (`LoadDirectivesFromDisk`) deprecates BoltDB entries that disappeared from disk. Original guard: `disk<5 AND BoltDB>50`. A 7-entry loss (disk=50 vs BoltDB=57, 12%) slipped through silently — disk wasn't "almost empty" so the absolute guard never fired.
2. **Hard-purge** (`CompactDirectives`) removes all `~~OBSOLETO~~` entries permanently and dedupes by tag. No backup written; recovery required `git show HEAD:.claude/rules/neo-synced-directives.md` of the disk file alone. If the disk file had also drifted at compact time, the original text was unrecoverable.

The chronic root cause is operational ambiguity: disk is authoritative for *active* entries, BoltDB is authoritative for *history* (soft-deletes), but the writer path (`SyncDirectivesToDisk`) and reader path (`LoadDirectivesFromDisk`) don't share that mental model — both treat the other as truth selectively.

### Concrete failure trace (2026-05-13)

```
Pre:  disk = 55 active, BoltDB = 55 active
Some session — exact trigger TBD — runs CompactDirectives
  → removes the ~~OBSOLETO~~ entries (legitimate)
  → AND the tag-dedupe pass collapses 7 active entries that shared
    tag prefixes (over-aggressive)
Post: disk = 48 active, BoltDB = 48 active
Next session adds 2 new directives (DS-PREMORTEM, RED-TEAM-LAYERING)
Final: disk = 50 active, BoltDB = 50 active
```

7 entries lost: GO-TEST-SETENV-PARALLEL, GITHUB-PLUGIN-WORKFLOW, LOCAL-LLM-ROUTING, CONFIG-FIELD-BACKFILL-RULE, HNSW-QUANT-WIRING, SELF-AUDIT-V2, OUROBOROS-NO-GREP-SHORTCUT. All load-bearing.

## Decision

Four-part hardening, shipped over 11 commits:

### 1. Recovery (b24e4eb)

Re-add the 7 lost directives via `neo_memory(learn, action_type:add)` with condensed text (≤500 chars per the new char-count guard). Original verbose versions remain in git history at `fd4ec4e` for reference. Net result: 50 → 57 active.

### 2. Relative-loss guard (eca89dc)

Add a second corruption guard to `LoadDirectivesFromDisk` that triggers on subtle drift:

```
BoltDB active ≥ 10   AND   relativeLossPct(disk, BoltDB) > 20%
  → skip destructive sweep, log warning, additive UPSERT only
```

Constants: `syncRelativeLossSampleMin = 10`, `syncDestructiveMaxRelLossPct = 20`. The 7-directive scenario (12% loss) would have tripped this guard but did not exist at the time.

Refactor `LoadDirectivesFromDisk` from CC=16 → CC=5 by extracting five helpers (`countActiveDirectivesIn`, `relativeLossPct`, `shouldSkipDestructiveSweep`, `runDestructiveSweep`, `runAdditiveUpsertFromDisk`). Brings the function back under the AST audit complexity limit.

### 3. Pre-destructive snapshot (549dde9)

New API `wal.SnapshotDirectives(snapshotPath) error` writes the complete BoltDB state (active + deprecated + counts + timestamp) to `.neo/db/directives_snapshot.json`. Wired into `handleCompactDirectives` (cmd/neo-mcp/tools.go) to fire BEFORE the destructive transaction.

Schema:

```json
{
  "snapshot_at_unix": 1715630000,
  "active_count": 57,
  "deprecated_count": 0,
  "directives": ["[SRE-BRIEFING] ...", "~~OBSOLETO~~ [OLD-TAG] ...", "..."]
}
```

Non-fatal on write failure: log + proceed. The destructive op is not blocked by snapshot failure — preserves operator velocity at the cost of recovery determinism.

### 4. Restore (ff01bc7)

New API `wal.RestoreDirectivesFromSnapshot(snapshotPath) (added int, err error)` reads the JSON and re-adds entries missing from BoltDB. Conservative semantics:

- Only fills gaps (entry in snapshot but not in BoltDB by normalized text).
- Does NOT delete or modify existing BoltDB entries.
- Does NOT re-activate `~~OBSOLETO~~` entries from the snapshot.

Exposed as new MCP action `neo_memory(action:learn, action_type:restore[, snapshot_path:"..."])`. Default snapshot path: `<workspace>/.neo/db/directives_snapshot.json`.

## Consequences

### User-facing API additions

- New `action_type` values for `neo_memory(action:learn)`: `compact` and `restore` (was {add, update, delete}).
- New optional arg `snapshot_path` (only meaningful for `restore`).
- Pre-existing `compact` was always available, just now writes a backup.

### Code-facing additions

- `pkg/rag/wal.go`: `SnapshotDirectives`, `RestoreDirectivesFromSnapshot`, plus 5 refactor helpers.
- `pkg/rag/wal.go`: 2 new constants for relative-loss guard.
- `cmd/neo-mcp/tools.go`: schema enum expanded, new handler `handleRestoreSnapshot`, snapshot call inside `handleCompactDirectives`.

### Test coverage

10 tests in `pkg/rag/wal_directives_sync_test.go`:

- 4 cover the boot path guards (abs, rel, within-threshold, math edge cases).
- 2 cover snapshot write semantics.
- 3 cover restore (fill-gaps, idempotent, skip-OBSOLETO).
- 1 covers the small-set regime where destructive sweep is legitimate.

### Documentation

- `docs/general/directives-durability.md` — single source of truth for the recovery story (3 worked scenarios, code map, test inventory).
- `docs/general/sre-tools-reference.md` — `neo_memory` table row updated.
- `.claude/skills/sre-tools/SKILL.md` — `learn` action_type list + durability callout.
- `docs/general/neo-global.md` G18 — action_type list.

## Alternatives considered

- **(A) Make `CompactDirectives` require `confirm:true` arg.** Rejected: adds friction to legitimate use and doesn't help against silent boot-time drift. Snapshot is the better hedge.
- **(B) Append-only directives bucket (no in-place deprecation).** Rejected: would balloon BoltDB size and break the existing soft-delete semantics that downstream tooling (`syncStatusSuffix`, `compact_directives`) depends on.
- **(C) Snapshot rotation (keep last N).** Rejected for now: single-file overwrite + git history of the `.md` file provide sufficient recovery surface. Easy to add later if the operational pattern justifies it.
- **(D) Auto-restore at boot when guard fires.** Rejected: surprising semantics. Operator should see the warning and decide explicitly via `neo_memory(action_type:restore)`.

## Open question

The root cause of the 2026-05-13 `compact` invocation that lost the 7 directives is still unidentified. Theories:

1. An old neo-mcp binary's `CompactDirectives` had a buggy tag-dedup that grouped unrelated tags. The current code groups by exact bracket-tag match; an older version may have used prefix matching.
2. Operator ran `neo_memory(learn, action_type:compact)` deliberately and the dedup over-collapsed.
3. A separate code path wrote directly to the BoltDB bucket and bypassed the deprecation semantics.

Investigation remains in `.neo/technical_debt.md` "FOLLOW-UP — Writer root cause" entry. The defenses (guards + snapshot + restore) are sufficient that the next occurrence will produce a captured snapshot for forensics, not silent data loss.

## Implementation commits

Series 2026-05-13, branch `develop`:

```
33980ed feat(hooks): userprompt-ds-premortem — multi-layer red-team decision tree
bae3a8e docs(debt): track DUAL-LAYER-SYNC drift
b24e4eb chore(directives): recover 7 lost directives from BoltDB drift
c2b35c0 docs(debt): writer root-cause follow-up for directive drift (b24e4eb)
8baca5b chore(directives): compact 3 grandfathered outliers + close context-bloat
eca89dc fix(rag): relative-loss guard for destructive sync — closes drift gap
a4750a1 docs(debt): close auto-tracked wal.go CC=16 (resolved by eca89dc refactor)
549dde9 feat(rag): pre-destructive directives snapshot — recovery beyond git
ca34448 docs(debt): close macOS flake + DUAL-LAYER-SYNC drift entries
ff01bc7 feat(rag): RestoreDirectivesFromSnapshot — close the snapshot loop
23ccf27 docs(directives): durability story doc + schema sync for new actions
```

## See also

- `docs/general/directives-durability.md` — operator-facing recovery guide.
- `.neo/technical_debt.md` — incident timeline + open follow-up.
- `pkg/rag/wal.go` — implementation.
- ADR-016 — Ouroboros lifecycle hooks (related: hook framework also delivered this session).
