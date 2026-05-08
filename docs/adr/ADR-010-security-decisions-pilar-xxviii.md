# ADR-010 — Security Decisions: PILAR XXVIII (147.B / 147.C / 147.D / 147.E)

**Date:** 2026-05-03  
**Status:** Accepted  
**Deciders:** SRE Kernel

---

## Context

PILAR XXVIII is a security sprint driven by the 2026-05-01 DeepSeek red-team audit.
Several findings were classified as "decision items" rather than straightforward code
fixes: they expose fundamental design tensions between developer ergonomics and
production security posture. This ADR records the resolution for each.

---

## Decision 147.B — Stale SSE sessions after child restart

### Problem

When a child neo-mcp restarts (e.g. via `make rebuild-restart`), the process pool
may reuse the same port (determined deterministically by `hash(workspace_path) % range`).
Existing SSE sessions hold `childPort int`. The `isChildPortAlive` check only verifies
`port == p.Port && p.Status == StatusRunning` — it passes for the *new* child, so a
stale session from the *previous* child's lifetime silently routes to the new process.

Effect: tool calls arrive in the new child before its HNSW graph is fully loaded;
the client never knows the session is stale.

### Decision

**Implement a process-generation counter** in `nexus.ProcessEntry` (incremented on
each spawn). `sseSession` captures the generation at connect time. `isChildPortAlive`
gains an additional generation comparison.

This is deferred to a follow-up epic (target: PILAR XXX) because it requires:
1. Extending `ProcessPool.Start()` to stamp and expose `Generation uint64` per port.
2. Thread-safe read of the current generation from `handleSSEConnect` (new field
   in `sseSession`).
3. A `GenerationMismatch` error event pushed on the SSE stream to force client
   reconnect.

**Interim mitigation (active):** `respondSessionLost` already returns HTTP 404 +
`suggest_fallback_curl` when `store.Get(sessionID)` misses — the case where the
*session* (not just the child) is gone. The remaining gap (session present, child
restarted, same port) is low-risk in practice: `neo-mcp` rejects in-flight requests
with JSON-RPC errors while loading, and the `Vacuum_Memory` SIGTERM flush ensures
the WAL is consistent before the new child replaces the old one.

---

## Decision 147.C — Default-deny plugin workspace allowlist

### Problem

`pkg/plugin/manifest.go::PluginSpec.AllowedWorkspaces []string` was documented as
"empty = all workspaces permitted". This meant any new plugin entry in `~/.neo/plugins.yaml`
that omitted `allowed_workspaces` would be callable from every workspace — a
confused-deputy risk if a multi-workspace operator has workspaces with different trust
levels.

### Decision

**Flip to default-deny.** Effective 2026-05-03:

- `AllowedWorkspaces: []` (empty) → **DENY ALL** callers.
- `AllowedWorkspaces: ["*"]` → allow all workspaces (explicit opt-in).
- `AllowedWorkspaces: ["ws-abc", "ws-def"]` → allowlist as before.

**Implementation** (`cmd/neo-nexus/plugin_routing.go::callPluginTool`):
```
wsPermitted := false
for _, ws := range conn.AllowedWorkspaces {
    if ws == "*" || ws == workspaceID { wsPermitted = true; break }
}
if !wsPermitted { // DENY }
```

**Backward compatibility note:** existing entries that relied on the old "empty = allow
all" behavior must add `allowed_workspaces: ["*"]` or an explicit ID list. The Jira
plugin shipped with `enabled: false`, so it is unaffected. The DeepSeek plugin already
had an explicit `allowed_workspaces: [neoanvil-95248]`. Operators enabling new plugins
must now supply the field or the plugin is unreachable (fail-closed, intentional).

---

## Decision 147.D — Weak KDF in keyring file fallback

### Problem

The plugin credential vault (`pkg/auth/vault.go`) delegates secret storage to an OS
keyring library. When the OS keyring is unavailable (headless Linux, CI containers),
the library falls back to a local encrypted file using its own default KDF, which
uses a single SHA-256 round — insufficient for a password-derived key.

### Decision

**Document and recommendation:** operators are strongly advised to use the OS keyring
(available on all supported platforms via `secret-service` D-Bus on Linux, `Keychain`
on macOS). No custom KDF will be implemented unless evidence of real file-fallback
usage in production is presented (see below).

Rationale:
1. The threat model for the file-fallback path is "attacker has read access to
   `~/.neo/db/` but not write access to the process memory or OS keyring". In
   that scenario a custom KDF still falls to offline brute-force if the passphrase
   is weak.
2. Adding a custom KDF (Argon2id / scrypt) to the fallback file would require
   vendoring a new dependency and changes to the plugin binary's startup handshake.
   This engineering cost is not justified until the fallback path is known to be
   used in production.
3. The correct long-term fix is to migrate plugin credential injection from
   environment variable (`env_from_vault`) to fd-passing (see 147.E).

**Operator guidance:** set `export NEO_USE_OS_KEYRING=1` (or equivalent platform
flag from the vault library) in the Nexus service unit to force the OS keyring path.

---

## Decision 147.E — Plugin tokens in /proc/<pid>/environ

### Problem

Plugin subprocesses receive API keys via `os.Environ` (populated from
`PluginSpec.EnvFromVault`). On Linux, `/proc/<pid>/environ` exposes the full
environment of a running process to any user with `ptrace` capability or that
shares the UID — tokens are visible for the lifetime of the plugin process.

### Decision

**Document and accept with user-isolation mitigation.** The risk is mitigated by:
1. Nexus and all plugin subprocesses run as the same Unix user (the operator's
   personal account). Cross-user exposure requires `CAP_SYS_PTRACE`, which is not
   granted by default on modern Linux kernels (`kernel.yama.ptrace_scope ≥ 1`).
2. The plugin credential is scoped to the provider (e.g., DeepSeek API key) and
   has no elevated host permissions beyond that API's rate limits.

**Long-term migration:** replace `env_from_vault` with fd-passing:
1. Parent writes the secret to a `memfd_create` file descriptor.
2. Child reads the fd from `ExtraFiles` and immediately mprotects or unlinks it.
3. The kernel does not expose `memfd` contents in `/proc/<pid>/environ` or
   `/proc/<pid>/fd` after `O_CLOEXEC` closes the fd.

This migration is planned as a follow-up epic (target: PILAR XXXV) because it
requires coordination with the plugin subprocess SDK and backwards-compatibility
with existing plugin binaries.
