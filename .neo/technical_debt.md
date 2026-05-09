# Technical Debt — Épicas Completadas

> Este archivo es gestionado automáticamente por el Kanban de Neo-Go.
> Las épicas completadas (todas las tareas [x]) son archivadas aquí
> durante el ciclo REM (5 min de inactividad) para mantener el Master Plan limpio.

---

## Active deferred items

### Pre-existing plugin-jira input validation gaps (surfaced by 3.4 DS audit)

**SEV 10 — Path traversal in `attach_artifact` + `prepare_doc_pack`:**
`folder_path` and `repo_root` action arguments flow directly to
`jira.AttachZipFolder` / `jira.PrepareDocPack` with no allowlist.
A client authenticated for any tenant can request
`folder_path=/etc/ssh` or `repo_root=/` to make the plugin zip and
upload arbitrary host-readable files to a Jira ticket as evidence of
exfiltration. Fix: anchor `folder_path` under `~/.neo/jira-docs/`
(or operator-configured base) + validate `repo_root` against
registered workspaces only; reject `..` segments after `filepath.Clean`.

**SEV 8 — Ticket ID injection in URL paths:**
`ticket_id` argument is interpolated into `<base>/rest/api/3/issue/<id>`
without validation. An input like `MCPI-1/../rest/api/3/serverInfo`
could (depending on URL normalization in `pkg/jira/client.go`) bypass
issue-scoped routing and reach arbitrary Jira REST endpoints.
Fix: validate against `^[A-Z][A-Z0-9]+-[0-9]+$` regex at the action
boundary; rely on `net/url.PathEscape` not raw `fmt.Sprintf` in the
client.

**Both findings are pre-existing in the plugin codebase** (not
introduced by 3.4 wire-up). Tracked here so the next plugin-jira
hardening pass can scoop them up. Out of scope for the 3.4 epic
(which was about wiring forward-pass scaffolding, not adding input
validation).



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

### [ds-audit-pending] DS audit unreachable for two new security primitives (2026-05-09)

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
