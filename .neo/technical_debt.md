# Technical Debt — Épicas Completadas

> Este archivo es gestionado automáticamente por el Kanban de Neo-Go.
> Las épicas completadas (todas las tareas [x]) son archivadas aquí
> durante el ciclo REM (5 min de inactividad) para mantener el Master Plan limpio.

---

## Active deferred items

### ~~Pre-existing plugin-jira input validation gaps~~ — RESOLVED 2026-05-09

**Status:** closed across two commits this session.

- **SEV 10 (path traversal in `attach_artifact` + `prepare_doc_pack`):**
  fixed in `4296483 fix(plugin-jira): close DS-audited input validation
  gaps (Phase E)`. `validateSafeFolderPath` anchors `folder_path` under
  `~/.neo/jira-docs/` via `filepath.Rel` + `..`-segment rejection;
  `validateSafeRepoRoot` requires the path to exist and be a directory.
  Both are wired in `callAttachArtifact` (line 776) and
  `callPrepareDocPack` (line 818). Verified by
  `TestValidateSafeFolderPath_*` + `TestValidateSafeRepoRoot_*`.

- **SEV 8 (ticket-ID injection in URL paths):** the SEV 8 attack surface
  was effectively neutralised by `pkg/jira/client.go` which uses
  `url.PathEscape(key)` on every path-interpolated key (see lines 169,
  270, 337, 680, 794) — `MCPI-1/../rest/api/3/serverInfo` becomes
  `MCPI-1%2F..%2Frest%2Fapi%2F3%2FserverInfo`, sent as a literal issue
  key to Jira (404 not-found). Defense-in-depth `validateTicketID`
  applied at the action boundary in `callGetContext` (489),
  `callTransition` (516), `callUpdateIssue` (604),
  `callAttachArtifact` (769), `callPrepareDocPack` (815).

- **Residual gap closed:** `callLinkIssue` (line 735) was not running
  `validateTicketID` on `from_key`/`to_key`. Not exploitable today
  (those keys go in the JSON body of `pkg/jira/Client.LinkIssue`, not
  URL path) but breaks symmetry. Closed via this commit; covered by
  `TestCallLinkIssue_RejectsMalformedKeys`.



### [ds-audit-pending] Pattern D Docker stack — DeepSeek pro audit

**Status:** 2026-05-09 — Nexus down during the planned DS pro audit
(operator stopped native to test docker-up). Manual pen-and-paper
audit performed instead, covering 8 threat surfaces (UID mismatch,
bind-mount escape via symlinks, concurrent BoltDB, volume lifecycle,
docker-seed race, GPU sharing, project name collision, backward
compat). Findings 1, 2, 5, 7 applied; 3, 4, 6, 8 documented or
already covered.

**Re-run when Nexus is available:**
```bash
mcp__neoanvil__deepseek_call \
  action: red_team_audit \
  model: deepseek-v4-pro \
  reasoning_effort: high \
  files: ["Dockerfile", "docker-compose.yaml", "scripts/docker-entrypoint.sh", "docs/onboarding/docker-architecture.md"]
```

The pro+max audit may surface CVEs in the cgo toolchain (apk add gcc
musl-dev pulls a compiler chain into stage 3) or scheduler-level
issues with GPU sharing under sustained load that the pen-and-paper
trace can't reach.

---

### ~~[ds-audit-pending] DS audit unreachable for two new security primitives~~ — RESOLVED 2026-05-09

**Re-attempt outcome (~7h after first try):**
- `SafeOperatorHTTPClient`: DS v4-flash high completed in 63s after
  4096 reasoning tokens, returned **no actionable findings** (empty
  content body, only the cache-cold telemetry). Interpretation:
  model thought through the threat surface and produced no SEV
  output — consistent with the pen-and-paper trace conclusion.
  task_id `async_ada191b0ea736110`.
- `isHUDAllowed`: DS v4-flash high EOFed again at 85s. task_id
  `async_e6b53891980834b8`.

**Status:** the pen-and-paper compensating control documented below
remains valid. Closing this debt entry — if a future audit cycle
surfaces a real issue we'll re-open with the specific finding.
The infra-level DS API instability (intermittent EOFs on long
audits) is itself documented in directive #54 and tracked by the
plugin team; not a security gap in our code.

### Original pen-and-paper trace (kept for audit trail)

**Files added in commit b56fb11 that need DS-audit re-run when API recovers:**

- `pkg/sre/ssrf.go::SafeOperatorHTTPClient` — new HTTP client that
  intentionally relaxes the SSRF guard to permit RFC 1918 private and
  loopback IPs (Docker bridge use case). Multicast/unspecified/link-local
  still blocked.
- `cmd/neo-nexus/dashboard.go::isHUDAllowed` — new access-control
  function that allows loopback + RFC 1918 to reach the HUD (Docker NAT
  case where operator hits HUD via the published port → bridge IP).

**Why pending:** DS pro+high audits queued (task_ids
async_0f1a530a53e33930 and async_07dc2f8b6076d891) returned EOF after
113s — the same DeepSeek API issue called out in directive #54.

**Pen-and-paper coverage applied (compensating control):**
- DNS-rebinding TOCTOU: pinned via `net.JoinHostPort(ips[0].String(), port)`.
- IPv4-mapped IPv6 (::ffff:X): handled by `canonicalIP()` for SSRF and
  by Go 1.17+ `ip.IsPrivate()` semantics for HUD.
- Cloud metadata 169.254.169.254: link-local-unicast → rejected by both
  primitives (SafeOperator blocks link-local; HUD: IsPrivate/IsLoopback
  both false).
- Header bypass on isHUDAllowed: impossible because Go's
  `r.RemoteAddr` is the TCP socket peer, not headers.
- Domain-shape RemoteAddr: cannot reach `HasPrefix("127.")` because
  RemoteAddr is always IP:port from the socket (no DNS).

**Triage rule:** rerun DS pro+high on these two files when the
DeepSeek API returns 200s consistently. If DS finds nothing new,
remove this entry. If DS surfaces a SEV ≥ 9, walk-through the chain
mechanically before applying any fix (DS hallucinates SEV 10s ~25%
of the time per `feedback_deepseek_hallucination_patterns.md`).
