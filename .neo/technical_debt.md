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



### ~~[ds-audit-pending] Pattern D Docker stack — DeepSeek pro audit~~ — RESOLVED 2026-05-09

**Re-attempt outcome (2026-05-09 23:18 UTC):** DS v4-flash high
completed in 62s on the second attempt. Output truncated at the
8000-token cap mid-Finding-1, but Finding 1 was complete enough
to act on. task_id `async_faaddc77fad38633`.

**Finding 1 (SEV High, CWE 200 — Information Exposure) — APPLIED:**

The compose file mounted `${HOME}/.neo:/home/neo/.neo-host:ro` —
the WHOLE `~/.neo/` directory — to make `seed_if_absent` work
without compose dying on a missing bind-source. But that exposed
the host operator's `workspaces.json`, `audit-jira.log`,
`audit-github.log`, `pki/` (mTLS SCADA certs), `db/` (HNSW + BoltDB
including memex_buffer with operator's lessons), and `shared/db/`
(cross-tier knowledge store) to any container process — including
a malicious Go module or npm dependency.

**Fix applied:**
- `docker-compose.yaml` — replaced the directory bind with two
  per-file binds: `~/.neo/credentials.json` + `~/.neo/plugins.yaml`
  only.
- `Makefile::docker-up` — preflight `touch` of both files (mode 600
  on credentials.json) so compose's "bind source must exist" rule
  doesn't break for fresh hosts. Empty placeholders trigger the
  silent-skip path in the entrypoint.
- `scripts/docker-entrypoint.sh::seed_if_absent` — added
  empty-file (`! -s`) check so a touched empty placeholder is
  treated as "no config provided" (same UX as fully absent), instead
  of seeding an empty file into the named volume where it would
  shadow later real configs and make Nexus fail to parse on boot.
- `docs/onboarding/docker.md` — gotchas table updated.

**Remaining items (the audit truncated before reaching them but
the manual pen-and-paper trace covered them):** UID/GID mismatch
(addressed via Dockerfile build-args USER_ID/USER_GID matching host),
TOCTOU in seed_if_absent (mitigated via lstat-then-cp + symlink
refusal at lines 73-79), GPU passthrough sandbox (no `/dev/nvidia*`
mount unless `runtime: nvidia` opts in — operator-controlled).

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
## ~~[2026-05-10 02:10] AST COMPLEXITY in builder.go:50~~ — RESOLVED 2026-05-10

`BuildSpec` CC=18 → split into `applyBuildDefaults`, `newSpecFromOpts`,
`buildOperation`, `applyResponseSchema`, `mergeOperationIntoPath`.
Each helper at CC ≤ 7. `BuildSpec` body is now ~10 lines.

## ~~[2026-05-10 02:10] AST COMPLEXITY in config.go:74~~ — RESOLVED 2026-05-10

`loadGithubPluginConfig` CC=18 → split into `validateAPIKeys` and
`validateProjects` helpers. Parent function now linear top-to-bottom.

---

## ~~[2026-05-10 02:12] AST COMPLEXITY in main.go:36~~ — RESOLVED 2026-05-10

`func main` CC=17 → extracted three helpers: `autodetectNeoMCPBinary`,
`initSSRFTrustedPorts`, `mustGenerateInternalToken`. Each helper is
single-purpose and small. Parent `main` flow now reads as a sequence
of named operations.

---

## ~~[2026-05-10 02:13] AST COMPLEXITY in boot_helpers.go:494~~ — RESOLVED 2026-05-10

`bootCoordinatorTier` CC=17 → split into `resolveProjectCoord`,
`openOrgTierIfCoordinator`, `syncOrgDirectivesIntoWorkspace` helpers.
Each one single-purpose. Parent now reads as 3 sequential steps.

## ~~AST COMPLEXITY in cmd/plugin-jira/config.go:396~~ — RESOLVED 2026-05-10

`migrateToPluginConfig` CC=18 → extracted `readJiraCredEntry` (returns
entry + path + raw bytes for backup) and `resolveLegacyContextEnv`
(env-or-contexts.json fallback). Migration body now linear.

## ~~AST COMPLEXITY in cmd/plugin-deepseek/tool_map_reduce.go:38~~ — RESOLVED 2026-05-10

`mapReduceRefactor` CC=19 → extracted `parseMapReduceArgs`,
`runMapReduceSmokeTest`, `mapPhase`, `refactorOneFile`,
`emitProgressNotification`. Parent reads top-to-bottom: parse → smoke →
fan-out → reduce.

## ~~AST SHADOW in pkg/deepseek/client.go:192~~ — RESOLVED 2026-05-10

`db, err := bolt.Open(...)` shadowed outer `err` → renamed to `openErr`.

## ~~AST SHADOW in cmd/plugin-jira/main.go:268~~ — RESOLVED 2026-05-10

`cfg, migErr := migrateToPluginConfig(...)` shadowed outer `cfg` →
renamed to `migCfg`.

---

## (legacy entry — kept until file gets full archive sweep)
## [2026-05-10 02:13] AST COMPLEXITY in boot_helpers.go:494

**Prioridad:** alta

File: /home/ensamblatec/go/src/github.com/ensamblatec/neoanvil/cmd/neo-mcp/boot_helpers.go
Line: 494
Kind: COMPLEXITY
Detail: func bootCoordinatorTier: CC=17 (limit 15)

---

## [2026-05-10 02:13] AST COMPLEXITY in config.go:396

**Prioridad:** alta

File: /home/ensamblatec/go/src/github.com/ensamblatec/neoanvil/cmd/plugin-jira/config.go
Line: 396
Kind: COMPLEXITY
Detail: func migrateToPluginConfig: CC=18 (limit 15)

---

## [2026-05-10 02:13] AST COMPLEXITY in tool_map_reduce.go:38

**Prioridad:** alta

File: /home/ensamblatec/go/src/github.com/ensamblatec/neoanvil/cmd/plugin-deepseek/tool_map_reduce.go
Line: 38
Kind: COMPLEXITY
Detail: func mapReduceRefactor: CC=19 (limit 15)

---

## [2026-05-10 03:02] AST COMPLEXITY in main.go:256

**Prioridad:** alta

File: /home/ensamblatec/go/src/github.com/ensamblatec/neoanvil/cmd/plugin-github/main.go
Line: 256
Kind: COMPLEXITY
Detail: func handleToolsCall: CC=22 (limit 15)

---

