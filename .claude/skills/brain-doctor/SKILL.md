---
name: brain-doctor
description: Diagnose Brain Portable health for the active workspace — canonical_id resolution, push status, conflict potential, archive size estimate. Use before first push from a new machine, after large `.neo/db` cleanup, or when `neo brain pull` reports a path mismatch. Task-mode skill (invoke with `/brain-doctor`).
disable-model-invocation: true
---

# /brain-doctor

Surface the four signals an operator wants before a `neo brain push`
or to diagnose a `neo brain pull` failure. Read-only — no network, no
mutations.

## Checks

1. **canonical_id resolution.** Run `neo workspace canonical-id` for
   the active workspace (or `--workspace=<id>`). Report which rule
   produced the result (`config_override` / `git_remote` /
   `project_name` / `path_hash`). If `path_hash` (the fallback), warn
   that cross-machine restores need a `path_map` entry unless the
   operator sets a `canonical_id` override via `neo workspace
   canonical-id --set <value>`.

2. **Last push HLC.** Inspect `~/.neo/brain-doctor.cache.json` (or
   parse recent `neo brain log --remote=$NEO_BRAIN_REMOTE` output) for
   the highest HLC ever pushed from this node. Report time-since-last-
   push so the operator notices stale snapshots.

3. **Conflict potential.** Compare the local registry workspace count
   against the latest remote manifest's count. Mismatch = something
   was added/removed since the last push. Informational only.

4. **Estimated archive size.** Walk the workspaces and sum file sizes
   minus the exclusion list (`.git/`, `bin/`, `*.log`, `.neo/pki/`,
   `.neo/db/`, `.neo/logs/`). Pre-flight check so a 4 GiB push doesn't
   surprise the operator on slow uplink.

## Invocation

```
/brain-doctor                            # current workspace
/brain-doctor --workspace=neoanvil-95248   # explicit
/brain-doctor --remote=$NEO_BRAIN_REMOTE # also probe the remote for log + status
```

## Output

Plain text, one section per check. No emoji unless every check
passes (then `🟢 ready to push`). Numbers in absolute units (MiB,
seconds since last push) so the output greps cleanly into bug reports.

## Exit code

Non-zero when a hard warning fires:

- workspace path no longer exists
- registry inconsistent with `~/.neo/workspaces.json`
- canonical_id resolves to `path_hash` AND no override set AND
  `--remote` is given (about to push something receivers can't easily
  pin to a known location)

## When to invoke

- **Before first push from a new machine** — confirms canonical_id
  resolves cleanly and paths are sensible.
- **After mass-cleanup of `.neo/db/` or `.git/`** — confirms nothing
  unintended will leak into the archive.
- **When `neo brain pull` reports a path mismatch** — the doctor will
  show which canonical_id the receiver expects vs what the local
  registry knows.
- **Before passphrase rotation** — confirms the new passphrase will
  derive a key for the same NodeID (it always will, but it's the kind
  of detail an operator wants in writing).

## Related

- `docs/pilar-xxvi-brain-portable.md` — full operator runbook
- `docs/adr/ADR-008-brain-snapshot-format.md` — manifest schema v1
- `pkg/brain/identity.go` — canonical_id resolver
- `pkg/brain/pathmap.go` — cross-host path remapping
- `cmd/neo/brain.go` — `neo brain` CLI
