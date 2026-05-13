# Directives Durability — Recovery Story (2026-05-13)

Born from the 2026-05-13 7-directive drift incident, this document
covers the corruption guards, pre-destructive snapshots, and restore
flow that protect `.claude/rules/neo-synced-directives.md` against
silent data loss.

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                       Dual-layer sync                            │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  BoltDB bucket `hnsw_directives` (.neo/db/hnsw.db)              │
│  Active + ~~OBSOLETO~~ entries with 1-based ordinal IDs         │
│                          ↕                                       │
│              [LoadDirectivesFromDisk]  ← boot path               │
│              [SyncDirectivesToDisk]    ← post-write              │
│                          ↕                                       │
│  Disk (.claude/rules/neo-synced-directives.md)                  │
│  Numbered list; ~~text~~ for soft-deleted entries               │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

**Invariants** (post-2026-05-13 hardening):

1. Disk is authoritative for *active* entries.
2. BoltDB preserves soft-delete history (`~~OBSOLETO~~` markers).
3. Hard-purge (`CompactDirectives`) is the ONLY operation that
   permanently removes BoltDB entries.
4. Every hard-purge writes a JSON snapshot BEFORE the destructive
   transaction.
5. Boot-time destructive sweep is gated by two corruption guards.

---

## Boot path — corruption guards

`pkg/rag/wal.go::LoadDirectivesFromDisk` runs at every neo-mcp boot.
If disk differs from BoltDB, the destructive sweep would deprecate
BoltDB entries missing from disk. Two guards prevent runaway loss:

### Guard 1 — absolute (since 2026-05-13 morning)

```
disk active < 5   AND   BoltDB active > 50
  → skip destructive sweep, log warning, additive UPSERT only
```

Catches catastrophic truncation. Numbers: `syncDestructiveMinDisk = 5`,
`syncDestructiveBoltDBThreshold = 50`.

### Guard 2 — relative (since commit `eca89dc`)

```
BoltDB active ≥ 10   AND   relativeLossPct(disk, BoltDB) > 20%
  → skip destructive sweep, log warning, additive UPSERT only
```

Catches subtle drift that slips the absolute guard. The 2026-05-13
incident (`disk=50 vs BoltDB=57`, 12% loss) would have tripped this
guard but did not exist at the time. Numbers:
`syncRelativeLossSampleMin = 10`, `syncDestructiveMaxRelLossPct = 20`.

If **either** guard fires, the additive UPSERT still runs — operator
hand-added entries on disk get picked up at next boot regardless.

---

## Hard-purge path — pre-destructive snapshot

`pkg/rag/wal.go::CompactDirectives` removes `~~OBSOLETO~~` entries
permanently and dedupes by tag. **Destructive.** Before the bbolt
batch transaction, `handleCompactDirectives` in
`cmd/neo-mcp/tools.go` invokes:

```go
wal.SnapshotDirectives(filepath.Join(workspace, ".neo", "db", "directives_snapshot.json"))
```

Snapshot format (`.neo/db/directives_snapshot.json`):

```json
{
  "snapshot_at_unix": 1715630000,
  "active_count": 57,
  "deprecated_count": 0,
  "directives": [
    "[SRE-BRIEFING] BRIEFING OBLIGATORIO...",
    "~~OBSOLETO~~ [OLD-TAG] retired entry...",
    "..."
  ]
}
```

- Includes both active and deprecated entries verbatim.
- Single file, overwritten on each compact (no rotation — git history
  of the `.md` file provides historical versioning).
- Located in `.neo/db/` which is gitignored. Local-disk recovery only.
- Non-fatal on write failure: log + proceed (operator velocity > 100%
  determinism).

---

## Restore path — closing the loop

`pkg/rag/wal.go::RestoreDirectivesFromSnapshot` reads the snapshot
JSON and re-adds entries to BoltDB. Conservative semantics:

- Only fills gaps (entry in snapshot but not in BoltDB by normalized
  text).
- Does NOT delete or modify existing BoltDB entries.
- Does NOT re-activate `~~OBSOLETO~~` entries from the snapshot.

Operator invokes via MCP:

```
neo_memory(
  action: "learn",
  action_type: "restore",
  snapshot_path: "<optional override>"
)
```

Default `snapshot_path`: `<workspace>/.neo/db/directives_snapshot.json`.

Returns `{"message": "Restored: N directive(s) added from <path>."}`.

---

## Recovery scenarios

### Scenario A — operator runs `compact` and regrets

```
Pre:   BoltDB has 57 active + 12 ~~OBSOLETO~~
       → compact wants to purge the 12 + dedupe
Compact:
  1. SnapshotDirectives writes 57+12=69 entries to JSON
  2. CompactDirectives removes the 12 → BoltDB has 57 active
  3. SyncDirectivesToDisk writes 57 to .md
Realize the dedupe also dropped legitimate variant:
  neo_memory(action:"learn", action_type:"restore")
  → reads snapshot, finds the dropped variant NOT in BoltDB
  → SaveDirective on the missing entry
  → BoltDB back to 57+1 = 58 active
```

### Scenario B — disk file gets corrupted between sessions

```
Pre:   BoltDB has 57 active, disk has 57 active.
External edit: disk truncated to 40 active.
Boot:  LoadDirectivesFromDisk
  → activeOnDisk=40, activeInBoltDB=57, loss=30%
  → relative-loss guard fires (BoltDB≥10 AND >20%)
  → destructive sweep SKIPPED
  → log: [DIRECTIVES-SYNC] corruption guard: disk=40 active, BoltDB=57 active (loss=30%) — skipping destructive sweep
  → additive UPSERT runs (no-op, disk is subset)
  → BoltDB stays at 57
Operator inspects logs, restores disk from git, restarts.
Net loss: 0.
```

### Scenario C — operator intentionally removes 3 entries

```
Pre:   BoltDB has 57 active, disk has 57.
Operator deletes 3 lines from disk (4.4% loss).
Boot:  LoadDirectivesFromDisk
  → activeOnDisk=54, activeInBoltDB=57, loss=5%
  → neither guard fires
  → destructive sweep runs, deprecates 3 BoltDB entries
  → BoltDB has 54 active + 3 ~~OBSOLETO~~
Result: deliberate intent respected, with full audit trail
(deprecated entries recoverable via UpdateDirective).
```

---

## Code map

| Function | File | Purpose |
|---|---|---|
| `LoadDirectivesFromDisk` | pkg/rag/wal.go | Boot sync, calls guards + sweep + UPSERT |
| `shouldSkipDestructiveSweep` | pkg/rag/wal.go | Combined abs+rel guard |
| `relativeLossPct` | pkg/rag/wal.go | Math helper |
| `runDestructiveSweep` | pkg/rag/wal.go | Soft-delete loop |
| `runAdditiveUpsertFromDisk` | pkg/rag/wal.go | Disk → BoltDB UPSERT |
| `SnapshotDirectives` | pkg/rag/wal.go | Write JSON backup |
| `RestoreDirectivesFromSnapshot` | pkg/rag/wal.go | Read JSON + fill gaps |
| `CompactDirectives` | pkg/rag/wal.go | Hard-purge (destructive) |
| `handleCompactDirectives` | cmd/neo-mcp/tools.go | Wrap compact with pre-snapshot |
| `handleRestoreSnapshot` | cmd/neo-mcp/tools.go | MCP entry point for restore |

## Tests

`pkg/rag/wal_directives_sync_test.go`:

- `TestLoadDirectivesFromDisk_DestructiveSync_DeprecatesMissing` —
  small-set regime where sweep is legitimate (<sample-min, guards
  pass).
- `TestLoadDirectivesFromDisk_CorruptionGuard` — absolute-loss
  scenario (disk=3, BoltDB=60+).
- `TestLoadDirectivesFromDisk_RelativeLossGuard` — 33% loss → skip.
- `TestLoadDirectivesFromDisk_RelativeLossWithinThreshold` — 5%
  loss → sweep runs.
- `TestRelativeLossPct` — 7-case table for math helper.
- `TestSnapshotDirectives_WritesValidJSON` — schema + counts.
- `TestSnapshotDirectives_CreatesParentDir` — MkdirAll defensive.
- `TestRestoreDirectivesFromSnapshot_FillsGaps` — round-trip:
  snapshot → loss → restore.
- `TestRestoreDirectivesFromSnapshot_Idempotent` — second restore
  adds 0.
- `TestRestoreDirectivesFromSnapshot_SkipsObsoleted` — OBSOLETO from
  snapshot stays purged.

---

## See also

- `[SRE-DUAL-LAYER-SYNC]` directive — high-level invariant statement
- `.neo/technical_debt.md` 2026-05-13 entries — incident timeline
- Commits this story: `33980ed b24e4eb bae3a8e c2b35c0 eca89dc
  549dde9 a4750a1 ca34448 ff01bc7 8baca5b`
