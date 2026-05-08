# Storage domain checklist

Append this to the audit prompt when files involve persistent storage
(BoltDB, SQLite, file I/O, snapshot persistence, archive formats).

## Invariants to verify

- **Atomic writes**: every persistent write must follow the pattern
  `tmp_file + fsync(tmp) + rename(tmp, final) + fsync(parent_dir)`.
  SIGKILL between fsync and rename leaves final unchanged. Without
  parent_dir fsync, the rename can be lost on kernel crash even after
  fsync of the tmp file.
- **TOCTOU windows**: any pattern of `stat → check → open` is a TOCTOU
  hazard. Use `O_NOFOLLOW + Lstat post-open` or `openat2 RESOLVE_NO_SYMLINKS`
  on Linux. For path validation, `filepath.EvalSymlinks` before prefix
  check.
- **Symlink traversal**: paths from user input or filesystem walks
  must be resolved before any prefix check. `filepath.HasPrefix(absPath, root)`
  on a raw symlink fails to prevent escape because `os.ReadFile` follows
  the link at read time.
- **BoltDB cursor reuse**: cursors are bound to a single Tx. Storing a
  cursor and reusing it across Tx boundaries causes use-after-free.
  Verify all `b.Cursor()` calls happen inside the same View/Update.
- **BoltDB tx leaks**: Update returns must not leave a dangling tx.
  Defer rollback before any return path inside the closure.
- **Bounds**: any size/count from a file header must be capped before
  allocation. `make([]T, header.N)` with attacker-controlled N is OOM bait.
- **Schema version**: format changes need explicit version handling.
  Rejecting schema > known prevents corrupting unknown formats. Schema
  < known triggers cold rebuild (overwrite OK if data is derived).
- **Checksum scope**: integrity checksums must cover BOTH header and
  body. A checksum over body only allows header tampering.

## Severity floor

For findings in this domain, severity ≥ 6 unless the failure mode is
a transient retry-recoverable error (e.g. ENOSPC during write where
the tmp file just gets cleaned up).

## Common compose-2-true-into-false-conclusion patterns

- "TOCTOU race" + "single-threaded process" → claim of "no impact"
  (false: cross-process races between Nexus + plugins).
- "Atomic rename" + "no fsync" → claim of "safe enough" (false: kernel
  may reorder writes; needs fsync of both file and parent dir).
