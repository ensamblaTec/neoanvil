# ADR-008 — Brain Portable snapshot format (manifest schema v1)

**Status:** Accepted, shipped 2026-05-01 (PILAR XXVI / 135.A.4)

## Context

PILAR XXVI moves neoanvil workspace state between machines via encrypted
archives. The receiver needs metadata about the archive *before*
decrypting it (to validate version, check whether the archive is even
relevant, decide where each canonical workspace lands). That metadata
is the **manifest** — a small JSON blob persisted alongside each
archive in the storage backend.

This ADR documents the v1 schema, version-bump rules, and the
migration story.

## Decision

The manifest is a versioned JSON object. v1 ships with the following
shape (Go struct in `pkg/brain/manifest.go::Manifest`):

```json
{
  "snapshot_version": 1,
  "hlc": { "wall_ms": 1714521743123, "logical_counter": 0 },
  "node_id": "node:a7f3b2c1...",
  "created_at": "2026-05-01T12:34:56Z",
  "workspaces": [
    {
      "id": "fake-1",
      "local_id_at_origin": "fake-1",
      "path": "/Users/alice/dev/fake-repo",
      "name": "fake-repo",
      "dominant_lang": "go",
      "type": "workspace",
      "canonical_id": "github.com/alice/fake-repo",
      "files": ["README.md", "main.go", "src/util.go"]
    }
  ],
  "projects": [
    {
      "path": "/Users/alice/dev",
      "canonical_id": "project:multi-repo:_root",
      "members": ["fake-1", "other-2"]
    }
  ],
  "orgs": [],
  "globals": [],
  "merged_from": []
}
```

### Required fields

| Field | Why required |
|-------|-------------|
| `snapshot_version` | gate forward-incompatibility |
| `hlc` | total order across snapshots from this node |
| `node_id` | salt for KDF + dedup detector ("my own snapshot reflected back") |
| `created_at` | human-readable counterpart to HLC.WallMS |
| `workspaces[].id` | local registry key at origin |
| `workspaces[].canonical_id` | cross-machine identity |

### Optional / semi-optional

| Field | Default when absent | Note |
|-------|---------------------|------|
| `local_id_at_origin` | mirrors `id` for v1 | reserved for v2 fork-aware semantics |
| `dominant_lang` | empty string | informational, not gating |
| `type` | "workspace" | "project" for federation roots |
| `files` | nil | populated by BuildArchive after walk |
| `projects` / `orgs` | empty array | only present when walk found federation |
| `globals` | empty array | reserved for ~/.neo files (workspaces.json, etc.) |
| `merged_from` | empty array | populated only by `neo brain merge` (136.*) |

### HLC scheme

Hybrid Logical Clock — `(wall_ms, logical_counter)`:

- `wall_ms` is `time.Now().UnixMilli()` at the moment of `NextHLC()`.
- `logical_counter` ticks per call within the same millisecond on the
  same node, reset to 0 when wall advances.
- `CompareHLC(a, b)` is lex order over the pair.
- Total order across nodes is wall-clock-perception with tie-break by
  counter; nodes whose clocks are ahead win ties — desirable, snapshots
  follow the operator's perception of time.

### NodeID derivation

`node:<sha256-prefix(seed)>` where seed is the first non-empty of:

1. `/etc/machine-id` (Linux systemd, persistent across boots)
2. `/var/lib/dbus/machine-id` (older Linux fallback)
3. `os.Hostname()`

Falls back to `node:unknown` only when all three are empty (stripped
container scenario).

### Canonical_id format

Documented in 135.A.1 (`pkg/brain/identity.go`). Four shapes:

- `<host>/<owner>/<repo>` — git remote (SSH or HTTPS normalized)
- `project:<project_name>:<basename>` — .neo-project member
- `project:<project_name>:_root` — .neo-project itself
- `local:<sha256-prefix(absolute path)>` — fallback when nothing else
  identifies the workspace

## Versioning rules

`CurrentSnapshotVersion = 1` lives in `pkg/brain/manifest.go`. Bump
rules:

1. **No bump** for backward-compatible additions (new optional fields
   with sensible default-when-absent).
2. **Major bump** for any field rename, removal, or semantic change in
   an existing field's interpretation.
3. Producers always emit the highest version they know.
4. Consumers MUST reject manifests with `snapshot_version >
   CurrentSnapshotVersion` (forward-incompatible).
5. Migration from v(N-1) → v(N) is provided as `neo brain migrate
   v(N-1)` — translates the manifest in place, leaves the encrypted
   archive bytes untouched (those are forward-compatible by design).

When v2 ships:

- Bump `CurrentSnapshotVersion` in the producing build first.
- Older clients receiving a v2 manifest hit `Validate` with
  `"snapshot v2 requires a newer neoanvil (this build supports up to
  v1) — run `neo brain migrate v1` after upgrading"`.
- The migration script reads the v2 manifest, drops/translates the
  fields the v1 schema doesn't carry, writes a v1 manifest beside it
  for the older client to consume.

## Consequences

### Positive

- Receivers can verify version + HLC + NodeID *before* decrypting,
  avoiding wasted Argon2id work on irrelevant snapshots.
- Manifest is human-readable JSON, debuggable without a special tool.
- Schema-versioned from day one — future migration story is documented
  rather than retrofitted.

### Negative

- Manifest size grows linearly with file count (the `files` array can
  be large for a fat workspace). Trade-off: the receiver knows what's
  in the archive without scanning the bytes. v2 may compress the file
  list separately.
- HLC's wall-clock component leaks the snapshot creation time even
  when the rest of the manifest is encrypted at the storage backend's
  layer. Acceptable today (snapshot timing isn't sensitive); revisit
  if the threat model changes.

### Edge cases

- **Empty `files` array.** Valid for the case where every file in a
  workspace was excluded by the blacklist. The manifest entry is still
  carried so the receiver knows the workspace existed.
- **`git config remote.origin.url` empty.** Resolver falls through to
  `local:<hash>` — receiver still has a stable identifier, just one
  scoped to the absolute path. Operator can override with `neo
  workspace canonical-id --set <value>` to make it stable across
  machines.
- **Snapshot from same node restored on same node.** Receiver detects
  matching NodeID and short-circuits as a no-op (avoids overwriting
  current state with a stale copy of itself). Implementation pending
  for `neo brain pull`.

## References

- `pkg/brain/manifest.go` — implementation
- `pkg/brain/manifest_test.go` — schema tests + JSON round-trip
- `docs/pilar-xxvi-brain-portable.md` — operator runbook
- 135.A.4 in `.neo/master_plan.md` (closed)
