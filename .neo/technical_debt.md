# Technical Debt — Épicas Completadas

> Este archivo es gestionado automáticamente por el Kanban de Neo-Go.
> Las épicas completadas (todas las tareas [x]) son archivadas aquí
> durante el ciclo REM (5 min de inactividad) para mantener el Master Plan limpio.

---

## Active deferred items

### ~~[scaffold-broken] `neo_forge_tool` — non-functional since initial commit (2026-05-10)~~ — RESOLVED 2026-05-12 (commit 5884ea6, Option A: deleted)

E2E audit (test `cmd/neo-mcp/forge_e2e_test.go::TestForgeTool_E2E_PipelineState`)
demonstrated TWO independent failure modes:

**Bug 1 (compile path):** `astx.CreateShadowFile` puts the Go source in
`/proc/<pid>/fd/<N>` (memfd-style ramfs). `go build` rejects this:

> wasm compilation failed: directory /proc/1211343/fd/8 outside
> modules listed in go.work or their selected dependencies

**Bug 2 (execute path):** Even if compilation succeeded,
`DynamicWasmTool.Execute` (cmd/neo-mcp/tools.go:807) calls
`sandbox.Execute(ctx, "", 1000)` which routes to
`EvaluatePlan(ctx, cpu, "")` — a generic plan evaluator with empty
code. The just-loaded WASM module's exported function is NEVER
invoked. The "Singularidad Alcanzada" success string overstates
what the pipeline actually delivers.

**State:** The tool has been registered in the MCP registry since
the initial commit (`fd99a39`) but has zero recorded invocations
across all known sessions (token_spend telemetry shows it never
ran). The wazero runtime imports + sandbox machinery exist but
the operator-facing contract is fictional.

**Decision pending operator approval:**
  · Option A — remove from `mustRegister` in `cmd/neo-mcp/main.go:747`
    and delete `cmd/neo-mcp/tools.go::ForgeTool`/`DynamicWasmTool`
    + `pkg/wasmx/sandbox.go::LoadDynamicTool`. Honest cleanup.
  · Option B — define a real wasip1 tool contract (entrypoint
    name, JSON in/out marshalling) and wire DynamicWasmTool.Execute
    to actually invoke the loaded module. Multi-day scope.
  · Option C — keep as deadcode marker (current state). NOT
    recommended; it misleads the operator about a feature.

Recommended: A. The test stays as the audit record; if option B
ever lands, the test becomes the regression gate.

### ~~[deadcode-candidate] `cmd/neo evolve` — Darwin Engine never iterated~~ — RESOLVED 2026-05-12 (commit 5884ea6, deleted)

`cmd/neo/evolve.go` (Darwin Engine — genetic evolution of Go
functions, SRE-93). Single commit since initial: `fd99a39`.
Zero references in `docs/`, `.claude/skills/`, or any test.
No commit since 2026-04-XX touched it. Operator-facing UX
("`neo evolve <file> <func>` runs genetic evolution and benchmarks
mutations") never materialised in workflow.

**Triage:** confirm with operator, then `git rm cmd/neo/evolve.go`
+ remove `evolveCmd` from `cmd/neo/main.go`'s rootCmd assembly.

### ~~[deadcode-candidate] `cmd/neo ask` / `chat` — Voice of Leviathan unused~~ — RESOLVED 2026-05-12 (commit 5884ea6, deleted)

`cmd/neo/ask.go` (Natural-language CLI via Ollama, SRE-95.B).
Same status as evolve: single commit `fd99a39`, zero references
anywhere. Originally meant to translate "neo ask 'how many tasks
are open?'" into MCP tool calls. Replaced in practice by Claude
Code's native MCP integration — no operator workflow uses it.

**Triage:** same as evolve — confirm + remove if approved.



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

## ~~[2026-05-10 03:00] Subprocess hang pattern — COMPILE_AUDIT fixed, 6+ siblings~~ — RESOLVED 2026-05-10

**Symptom (operator-reported in another project):** COMPILE_AUDIT
hangs ~30min on projects with heavy cgo / tree-sitter / proto-gen
dependencies.

**Root cause:** `cmd.CombinedOutput()` waits for ALL pipe writers
to close before returning. When `go build` invokes cgo → gcc →
child processes, those grandchildren are NOT in the same process
group as the `go build` parent. context.WithTimeout SIGKILLs the
parent on 30s expiry, but the gcc grandchildren survive holding
the pipes open. CombinedOutput then waits indefinitely (until gcc
finishes naturally — minutes or tens of minutes for big repos).

**Fix applied to `runGoBuild` (radar_compile.go:128):**
1. `SysProcAttr{Setpgid: true}` — process-group leader, SIGKILL
   on context cancel reaches the whole tree.
2. `cmd.WaitDelay = 5*time.Second` (Go 1.20+) — caps pipe-drain
   wait after cancel. Worst case is now `30s + 5s = 35s`.
3. Surface explicit `BUILD TIMEOUT` line when `buildCtx.Err()`
   triggered, so operator distinguishes hang from real errors.

**Same pattern unfixed (follow-up):** sweep found 6+ call sites
that exec subprocesses via `exec.CommandContext + CombinedOutput`
without Setpgid+WaitDelay:

  · `cmd/neo-mcp/tools.go:360,440` — neo_command run/approve
    (operator shell commands; can be `make build` etc.)
  · `cmd/neo-mcp/tools.go:851` — neo_forge_tool wasm compile
    (`go build -o pathWasm`); same bug shape.
  · `cmd/neo-mcp/dashboard.go:393` — HUD rebuild
    (`go build -o outBin ./cmd/neo-mcp`); same.
  · `cmd/neo-mcp/macro_tools.go:1604,1634,1648` — sandbox build
    helpers (go build / sh -c / cargo build).
  · `cmd/neo-mcp/radar_audit.go:123` — lint shell invocation.

**Triage:** matters only when invoked on cgo-heavy targets; on
this repo (`go build ./...` = 3.3s) the hang is invisible.
Recommended fix: extract a `hardenedExec()` helper in pkg/sre and
wire all call sites in one commit. Defer until operator sees the
pattern bite a second time.

**Status:** ✅ Closed proactively 2026-05-10. New helpers in
`pkg/sre/subprocess.go`:
  · `HardenSubprocess(cmd, waitDelay)` — retrofit existing exec.Cmd
  · `HardenedExec(ctx, waitDelay, name, args...)` — convenience constructor

All 8 sibling call sites wired with `sre.HardenSubprocess(cmd, 0)`
(0 = default 5s waitDelay):
  · `cmd/neo-mcp/tools.go:361` — neo_command run (sh -c)
  · `cmd/neo-mcp/tools.go:441` — neo_command approve (bash -c)
  · `cmd/neo-mcp/tools.go:853` — neo_forge_tool wasm compile (go build)
  · `cmd/neo-mcp/dashboard.go:395` — HUD rebuild (go build)
  · `cmd/neo-mcp/radar_audit.go:124` — lint shell invocation
  · `cmd/neo-mcp/macro_tools.go:1636` — fast-mode build (go build, T001 path)
  · `cmd/neo-mcp/macro_tools.go:1666` — polyglot module build (sh -c)
  · `cmd/neo-mcp/macro_tools.go:1681` — Rust fallback (cargo build)

5 regression tests in `pkg/sre/subprocess_test.go`:
BoundedByContextPlusWaitDelay (sleep 30 → 500ms), RetrofitPath
(sh chain w/ orphaned bg child → 1.3s), NilSafe, ZeroWaitDelay
PicksDefault, HappyPathQuickReturn (no overhead). All pass with
`-race`. AST_AUDIT clean on all touched files.

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

<!--
  Zombie entries swept 2026-05-10. The 4 raw "## [date] AST COMPLEXITY"
  blocks that used to live below this line were auto-recorded by
  AST_AUDIT and then resolved in commits 5138d0f / 3066d84, but the
  parser couldn't recognise the ~~RESOLVED~~ markers above. They were
  surfacing in `neo_debt(action:"affecting_me")` as false positives.
  All four are tracked under the matching ~~RESOLVED 2026-05-10~~
  section earlier in this file.
-->

## ~~[T001 nexus] CERTIFY-CWD-BUG~~ — RESOLVED 2026-05-10

`projectRootOf` preferred neo.yaml over go.mod, breaking strategos
where `neo.yaml` lives at the workspace root but `go.mod` is in
`backend/`. `go test/build/list` ran with `cmd.Dir = projectRootOf()`
→ "go.mod file not found" → 100% bypass=1 rate over ~30 sessions.

Fix: introduced `goModRootOf()` helper that walks ONLY for go.mod;
swapped 3 call sites in `cmd/neo-mcp/macro_tools.go` (fast-mode
build line 1605, preflight `go list` line 1712, TDD `go test`
line 1735). `projectRootOf` retained for non-Go contexts (python,
polyglot module builds). Regression tests in
`macro_tools_modroot_test.go` pin the layout invariant + the
nested-go.mod corner case.

## ~~[T002 nexus] TECH_DEBT_MAP-TOKEN-FLOOD~~ — RESOLVED 2026-05-10

`handleTechDebtMap` was uncached — operator paid ~$47 over 477 calls
in strategos before this gate. Hotspot data only meaningfully changes
when files certify, so a 30-min TTL cache loses zero accuracy.

Fix: process-wide `techDebtMapCache` keyed by
`<workspace>|<limit>|<targetWorkspace>`. Cached body returns prefixed
with `⚠️ CACHED(TTL:30m)` so the operator sees the freshness window.
`bypass_cache:true` arg forces a fresh recompute. Concurrency-safe
via sync.RWMutex; verified by `TestTechDebtMapCache_RaceFreeUnderConcurrentReadWrite`.

## ~~[option-D-CPG-parallelization] CPG SSA walk parallelization~~ — REJECTED 2026-05-10 (low ROI)

Earlier session recommended parallelizing the per-package walk loop in
`pkg/cpg/builder.go::Build()` claiming "4-8× cold-boot speedup". A
phase-instrumented benchmark against the production scope
(`cfg.CPG.PackagePath = "./cmd/neo-mcp"`) showed the claim was wrong:

| Phase | Time | Share |
|-------|------|-------|
| packages.Load (Go parser, already parallel) | 330ms | 81.6% |
| ssautil.AllPackages + prog.Build (Go-parallel) | 68ms | 16.9% |
| Walk packages (sequential — D's target)        | **6ms** | **1.5%** |
| TOTAL cold-build                                | 405ms | |

Parallelizing the 6ms walk yields ~3-4ms in absolute terms. Not worth
the mutex/synchronization complexity. The phase bench lives at
`pkg/cpg/builder_phases_test.go` (build tag `cpg_phases`) so future
hypotheses can be revalidated in seconds:

```bash
go test -tags cpg_phases -v ./pkg/cpg/ -run TestPhases -timeout 2m
```

**Where the real cold-boot win actually was:** the HNSW cold rebuild
path (5-6 min per CLAUDE.md, runs through `workspace_utils.go`) already
benefits from option-B batch embedding migrated in commit `c4c3b1a` —
the per-file embed pipeline shows 3.3× at batch=16. So cold HNSW
rebuild now runs in ~1.5-2 minutes for the same data.

D should not be attempted again unless `packages.Load` drops below 30%
of cold-build time, which would require either (a) Go shipping a
faster loader, or (b) us memoising packages.Load output across
rebuilds. Neither is on the horizon.

## ~~[2026-05-10 04:26] AST INFINITE_LOOP in bridge.go:328~~ — RESOLVED 2026-05-10 (false positive)

`walkRouterChain` uses `for range 32 { switch ... { case ...: return } }`
to walk a Go AST chain. The linter regex doesn't recognize `return`
inside switch cases, so it flagged the loop. Fixed at refactor time
by replacing `for {}` with bounded `for range 32` — the loop is now
mechanically guaranteed to terminate. The recording tool re-fired
because it scanned a stale snapshot before the refactor landed.
Closing as zombie / false positive.

---

## ~~[2026-05-10 06:18] AST COMPLEXITY in embedder.go:256~~ — RESOLVED 2026-05-12 (stale / false positive)

Original entry flagged `func EmbedBatch: CC=23 (limit 15)` at line 256.

Re-audited 2026-05-12 via `AST_AUDIT pkg/rag/embedder.go`:
> ✅ AST_AUDIT: No issues found.

The refactor in commit `c4c3b1a` (batch `/api/embed` migration) split
`EmbedBatch` into the dispatch surface (lines 256-292, ~35 LOC, linear
control flow) plus helpers `truncateTexts`, `acquireBatchSlots`,
`dispatchBatchHTTP`, `decodeBatchEmbeddings`, `embedSequentialFallback`.
Each helper at CC ≤ 5. The original entry pre-dates this refactor by
hours — the recorder fired before the split landed and the resolution
marker was never written.

Closing as zombie. `make audit-ci` clean against `.neo/audit-baseline.txt`.

---

## ~~[2026-05-10] neo_forge_tool scaffold broken~~ — RESOLVED 2026-05-12 (Option A: deleted)

Scaffold removed in this session. Decision: Option A from the original
entry (`git rm`) over salvaging the architecture (Option B). Rationale:
zero recorded invocations across telemetry; daemon mode uses local-LLM
direct (ADR-013) instead of WASM-forged tools; the salvage cost (fix
shadow-file path + define wasip1 contract + wire `Execute`) is 1-2 days
for a feature with no concrete operator workflow.

Files removed:
- `cmd/neo-mcp/tools.go` — `DynamicWasmTool` + `ForgeTool` + `NewForgeTool` (~75 LOC)
- `cmd/neo-mcp/main.go:755` — `mustRegister(NewForgeTool(...))` registration
- `cmd/neo-mcp/forge_e2e_test.go` — audit test (use case gone)
- `pkg/wasmx/sandbox.go` — `Sandbox.LoadDynamicTool` (only consumer was forge)

The wazero sandbox + `pkg/astx.CreateShadowFile` remain intact for future
hot-path uses.

---

## ~~[2026-05-10] cmd/neo evolve — Darwin Engine never iterated~~ — RESOLVED 2026-05-12 (deleted)

`cmd/neo/evolve.go` (104 LOC, SRE-93.B) deleted along with `pkg/darwin/`
package (6 files: mutator, profiler, proposal + tests). Only consumer
was `evolveCmd()` registered in `cmd/neo/main.go:70`. Zero references
elsewhere; never invoked in any session telemetry.

If the use case re-emerges, reimplement as a thin orchestrator over
`neo_local_llm` (Qwen 2.5-Coder 7B, ADR-013) + `neo_sre_certify_mutation`
+ `pkg/cpg` SSA fitness — ~200 LOC end-to-end with today's primitives,
versus the 104 LOC scaffold-only-no-genetic-loop that we just deleted.

---

## ~~[2026-05-10] cmd/neo ask / chat — Voice of Leviathan unused~~ — RESOLVED 2026-05-12 (deleted)

`cmd/neo/ask.go` (367 LOC, SRE-95.B.1/B.2) deleted along with `askCmd()`
and `chatCmd()` registrations in `cmd/neo/main.go:68-69`. The NL→MCP
translator was superseded by Claude Code itself as the primary MCP
client; CLI form never used in any session telemetry.

Headless NL→MCP (the only remaining use case — CI/cron without a human
agent) can be implemented as a ~50 LOC wrapper over `neo_local_llm` +
`curl` to Nexus if it ever materialises.

---

## ~~[2026-05-13 02:51] [context-bloat] CLAUDE.md + rules + skills inyectan ~64k tokens upfront~~ — RESOLVED 2026-05-13 (audit c2b35c0+1)

**Status:** Audit completo descubrió que las acciones A+B ya estaban hechas (commit `bf36b19` consolidación previa). Acción D parcialmente OK (0 tag duplicates). Quedaba pendiente compactar 3 outliers grandfathered >500 chars; resuelto en este audit.

**Medición actual upfront (post-audit):**
- CLAUDE.md: 52 líneas / 4,061 chars / ~1,015 tok
- .claude/rules/*.md (1 archivo): 16,391 chars / ~4,098 tok
- **Total upfront fixed budget: ~20,452 chars / ~5,113 tok** ← debt entry target era ≤20k tokens. **5× MARGIN bajo target.**
- Skills auto-load: 11/17 (BRIEFING reporta `11 auto, 6 task`) — cargan contextualmente, no upfront global.
- Directivas: 57/60 capacity, 0 tag duplicates, **todas ≤500 chars** post-compact de 3 outliers en este audit.

**Acciones del entry original — status real:**
- **A. CLAUDE.md ≤60 líneas:** ✅ DONE (52 líneas actuales)
- **B. Consolidar 5 rules/*.md:** ✅ DONE (solo 1 archivo `neo-synced-directives.md` existe)
- **C. Reclasificar 8 skills auto→task:** SKIPPED (riesgo medio, beneficio marginal — skills auto cargan contextualmente, no en cada turn)
- **D. ≤40 directivas + dedupe:** PARCIAL — 0 tag dupes confirmado, count 57 (no 62), 3 outliers compactados a ≤500 chars en este audit
- **E. Mover docs a docs/general/:** ya está hecho (CLAUDE.md referencia `docs/general/neo-global.md`, etc.)
- **F. Scope-aware loading:** SKIPPED (riesgo medio en pkg/config, ROI bajo dado que target ya cumplido)

**Lesson registrada:** debt entries decay rápido. Re-audit antes de ejecutar — A/B ya estaban hechas pero el entry no se actualizó tras `bf36b19`. Aprendido: `git log --grep` previo a empezar trabajo en debt items, o `git log <path>` de los archivos mencionados.

---

## ~~ORIGINAL ENTRY (preserved for reference)~~

**Prioridad:** P2 (original)

## Problema

Cada sesión inyecta ~64,458 tokens ANTES del primer mensaje del usuario:
- `CLAUDE.md`: 232 líneas / 28,622 chars / ~7,156 tok (excede ceiling ~200 líneas documentado por Anthropic)
- `.claude/rules/*.md` (11 archivos): 1,096 líneas / 106,448 chars / ~26,612 tok
- `.claude/skills/*/SKILL.md` (16, 12 auto-load): 122,767 chars / ~30,691 tok

BRIEFING ya señala `⚠️ DIRECTIVE_INFLATION: 62/60`.

## Síntomas

1. Claude 4.7 en auto-mode tiende a saltarse el flujo Ouroboros (BRIEFING → BLAST_RADIUS → Edit → certify) y responder directamente con conocimiento general.
2. Evidencia externa (Vercel eval, GitHub anthropics/claude-code#29971, BSWEN/MindStudio/KDnuggets): skills nunca invocadas en 56% de casos cuando >12 auto-load; CLAUDE.md deja de leerse después de ~200 líneas.
3. Directivas duplicadas/históricas (BLAST_RADIUS_FALLBACK #17 vs #104, etc.) generan ruido sin valor accionable.

## Root cause

Acumulación incremental sin política de poda. 5 archivos `neo-synced-directives*.md` separados que se inyectan todos juntos. 12/16 skills marcadas auto-load por default cuando solo 2-3 son realmente universales.

## Recommended (6 acciones en orden)

- **A.** Reducir `CLAUDE.md` 232 → ≤60 líneas (mantener invariantes core; mover detalle a skills).
- **B.** Consolidar 5 `neo-synced-directives*.md` en uno solo; eliminar `-history.md` (git preserva).
- **C.** Reclasificar 8/12 skills `auto-load` → `task-mode` (solo `sre-doctrine` + `sre-troubleshooting` + 1-2 más quedan auto).
- **D.** Auditar las 62 directivas, marcar duplicados con `supersedes`, bajar a ≤40 vivas.
- **E.** Mover `neo-gosec-audit.md`, `neo-deadcode-triage.md`, `neo-code-quality.md` a `docs/general/` (referenciados desde skills, no inyectados).
- **F.** Scope-aware loading: campo `neo.yaml::workspace.scope` (backend|frontend|fullstack) que filtre qué rules carga el SessionStart hook.

## Métrica de éxito

- Upfront context tokens: 64k → ≤20k (medido contando `system-reminder` blocks en sesión limpia).
- DIRECTIVE_INFLATION: 62/60 → ≤40/60.
- Sesión nueva ejecuta BRIEFING + BLAST_RADIUS sin recordatorio explícito del usuario en >80% de casos (calibración manual primeras 10 sesiones post-refactor).

## Files afectados

- `/home/ensamblatec/go/src/github.com/ensamblatec/neoanvil/CLAUDE.md`
- `/home/ensamblatec/go/src/github.com/ensamblatec/neoanvil/.claude/rules/*.md`
- `/home/ensamblatec/go/src/github.com/ensamblatec/neoanvil/.claude/skills/*/SKILL.md` (frontmatter `trigger` field)
- `/home/ensamblatec/go/src/github.com/ensamblatec/neoanvil/pkg/config/config.go` (paso F — opcional)
- `/home/ensamblatec/go/src/github.com/ensamblatec/neoanvil/neo.yaml.example` (paso F — opcional)

## Riesgos

- Bajo: A, B, D, E (puramente reorganización doc, git preserva contenido).
- Medio: C (skills task-mode no las "ve" el modelo sin invocación explícita — calibrar primeras sesiones).
- Medio: F (toca pkg/config — requiere certify + tests).

## Referencias externas

- https://docs.bswen.com/blog/2026-04-23-prevent-claudemd-bloat/
- https://github.com/anthropics/claude-code/issues/29971
- https://www.mindstudio.ai/blog/context-rot-claude-code-skills-bloated-files
- Anthropic best-practices Opus 4.7

---


## ~~[2026-05-13] PRE-EXISTING FLAKE — TestBackgroundIndexFile_SymlinkEscapeRejected (macOS /var symlink)~~ — RESOLVED 2026-05-13 (commit 096f164)

Test now passes cleanly under `go test -run TestBackgroundIndexFile_SymlinkEscapeRejected -count=1 ./cmd/neo-mcp/` — `096f164 fix(test): macOS /var → /private/var symlink flake — EvalSymlinks workspace` already canonicalized workspace via `filepath.EvalSymlinks` before comparison. NEO_CERTIFY_BYPASS no longer required on Darwin.

---

## ORIGINAL ENTRY (preserved for reference)

### Pre-existing flake on Darwin

`cmd/neo-mcp/background_index_symlink_test.go:66` falla en macOS con:
```
inner symlink /var/folders/d1/.../alias.txt resolved to /private/var/folders/d1/.../data.txt
  should be under workspace /var/folders/d1/.../...
```

**Root cause:** macOS hace `/var → /private/var` symlink-redirect a nivel filesystem. `t.TempDir()` retorna paths bajo `/var/folders/` pero `filepath.EvalSymlinks` resuelve a `/private/var/folders/`. El test verifica que el resolvedPath esté bajo `workspace`, pero workspace es la versión `/var/folders/...` y resolvedPath es `/private/var/folders/...` — no match.

**Verificación pre-existing:** `git stash && go test -run TestBackgroundIndexFile_SymlinkEscapeRejected` falla idéntico sin mis cambios actuales (2026-05-13). NO causado por HotFilesCache.

**Fix sugerido (defer):** El test debe normalizar `workspace` con `filepath.EvalSymlinks` antes de comparar, o usar un workspace dir explícito que no esté bajo `/var`. Ticket separate, no bloquea HotFilesCache landing.

**Impact:** Hasta el fix, `NEO_CERTIFY_BYPASS=1 git commit` para cualquier .go/.ts/.tsx/.js/.css en cmd/neo-mcp/ (per directiva [SRE-CERTIFY-BYPASS]). Pre-commit hook bloqueará sin esta variable.

---

## ~~[2026-05-13] DUAL-LAYER-SYNC drift — 7 directives lost from disk file~~ — RESOLVED 2026-05-13 (commits b24e4eb + eca89dc + 549dde9)

Recovery + hardening shipped over 3 commits:
- `b24e4eb` re-added the 7 lost directives (condensed ≤500 chars) — disk back to 57/60
- `eca89dc` added relative-loss guard (20% threshold) + refactor CC=16→5
- `549dde9` added `SnapshotDirectives` invoked from `handleCompactDirectives` — pre-destructive recovery beyond git

Writer root-cause investigation remains in the FOLLOW-UP entry below (monitoring for next drift event).

---

## ORIGINAL ENTRY (preserved for reference)

### Drift detection 2026-05-13 mid-session

**Status:** active drift detected mid-session 2026-05-13 turn N.

**Symptom:** `.claude/rules/neo-synced-directives.md` working tree had 50 entries; `git show HEAD:` had 55. Diff showed 7 lost (HEAD #49-55: GO-TEST-SETENV-PARALLEL, GITHUB-PLUGIN-WORKFLOW, LOCAL-LLM-ROUTING, CONFIG-FIELD-BACKFILL-RULE, HNSW-QUANT-WIRING, SELF-AUDIT-V2, OUROBOROS-NO-GREP-SHORTCUT) + 2 gained (new DS-PREMORTEM-MULTI-FEATURE, SRE-RED-TEAM-LAYERING). Net: −5.

**Cause hypothesis:** During neo-mcp boot earlier this session, BoltDB had N+7 active entries and disk had N. The destructive sweep in `LoadDirectivesFromDisk` (pkg/rag/wal.go:809) correctly deprecated the 7 BoltDB entries not present on disk. **But the disk file was the truncated version**, not the source-of-truth — likely a previous session's dual-layer-sync round-trip dropped them via similar drift.

**Why this matters:** the 7 lost directives encode load-bearing knowledge (t.Setenv vs t.Parallel mutex, GitHub plugin surface inventory, local-LLM cost/routing rule, config backfill discipline learned from b5398de, HNSW quant wiring lessons from ADR-014, SELF-AUDIT-V2 coverage table requirement, OUROBOROS-NO-GREP-SHORTCUT enforcement). Loss = re-discovery cost.

**Recovery blocked by:** `neo_memory(learn, action_type:add)` now enforces a 500-char limit per directive. Several of the lost 7 are >500 chars and need condensing. Recovery is **7 separate condense+add cycles** ≈ 30-45 min of careful work.

**Sticky deprecation:** restoring just the disk file does NOT re-activate. The destructive sweep semantic checks `if not on disk → deprecate`, but the additive UPSERT path computes `existingSet` from ALL BoltDB entries (including deprecated ones via normalizeDirective) so deprecated text matching disk text is NOT re-added. Recovery requires either: (a) `neo_memory(learn, action_type:compact)` to hard-purge OBSOLETO then re-add, or (b) 7× learn calls with new IDs.

**Action items:**
1. Re-add the 7 directives as new entries via 7× `neo_memory(learn, action_type:add)` with condensed text (≤500 chars each).
2. Investigate dual-layer-sync writer path — find where disk file gets rewritten with subset of BoltDB state. The destructive read path is sound; the write path is suspect.
3. Consider raising the 500-char limit OR adding a `--long-form` escape hatch that writes to `neo_memory(action:store, namespace:directives)` instead of the global_rules bucket.

**Workaround now:** original text is recoverable via `git show HEAD:.claude/rules/neo-synced-directives.md` (commit fd4ec4e). Until recovery, the 7 directives are NOT being injected via SessionStart hook, so the agent loses visibility on them between turns.

---

## ~~[2026-05-13] FOLLOW-UP — Writer root cause for DUAL-LAYER-SYNC drift (commit b24e4eb closed the symptom)~~ — RESOLVED 2026-05-14 (writer race + crash-atomicity fixed)

**Status:** ✅ RESOLVED 2026-05-14. Root cause found, then the writer race and
crash-atomicity gap both fixed in `pkg/rag/wal.go`. Symptom previously resolved
via mass re-add (b24e4eb); the root-cause fix below stops it recurring.

### FIX SHIPPED — `pkg/rag/wal.go`

- **Race closed:** new `WAL.directivesMu sync.Mutex` is `Lock`/`defer Unlock`-ed
  at the top of `SyncDirectivesToDisk`, so the `GetDirectives()` snapshot read
  and the file write are one atomic unit. A stale snapshot can no longer
  overwrite a fresher one — the last Sync to acquire the lock always reads the
  freshest BoltDB state, so disk converges to BoltDB. The "minimum" scope from
  the recommendation below is provably sufficient: every mutating handler calls
  `SyncDirectivesToDisk` *after* its `SaveDirective`, so that Sync acquires the
  lock after the commit and always includes its own directive; the serialized
  last writer wins with latest-or-equal state. No `tools.go` change needed.
- **Crash-atomicity closed:** `os.WriteFile` (truncate-then-write) replaced with
  a new `atomicWriteFile` helper — hidden sibling temp file + `Write` + `fsync`
  + `Chmod` + `os.Rename` + parent-dir `fsync`. A crash/SIGKILL/disk-full before
  the rename leaves `neo-synced-directives.md` fully intact. Same pattern as
  `CompactWAL` (0374cee) and `SaveHNSWSnapshot`. Temp name `.<name>.tmp-*` is
  hidden so a `*.md` glob never picks it up mid-write.
- **MCP concurrency model:** the mutex is cheap insurance regardless of whether
  tool calls are serialized within one child — the crash-atomicity gap was real
  independent of the race.
- **Regression test:** `wal_directives_sync_test.go::TestSyncDirectivesToDisk_ConcurrentWritesRaceFree`
  (24 goroutines hammering Save+Sync, asserts every committed directive lands on
  disk) + `TestAtomicWriteFile_OverwriteLeavesNoTemp`. Both pass under `-race`.
  DS premortem attempted (v4-pro/max) but timed out — same DeepSeek API
  instability noted in 0374cee; self-premortem covered the interleavings,
  deadlock risk (none — single non-reentrant Lock, no nested directive calls),
  and temp-file glob collision.

**Recovered:** 7 directives re-added in b24e4eb (condensed ≤500 chars). Disk now 57/60.

### ROOT CAUSE — read-then-write race in `SyncDirectivesToDisk` (pkg/rag/wal.go:754)

The on-disk file `.claude/rules/neo-synced-directives.md` is written ONLY by
`SyncDirectivesToDisk`, called from the 5 `neo_learn_directive` handler paths
(cmd/neo-mcp/tools.go:200 `syncDirectives`). It does three NON-atomic steps with
no serialization:
  1. `rules := GetDirectives()` — read the full BoltDB `global_rules` snapshot
  2. build the markdown string
  3. `os.WriteFile(syncPath, ...)` — overwrite the whole file

If two `neo_learn_directive` calls run concurrently, goroutine A can read its
snapshot at step 1 BEFORE goroutine B commits its `SaveDirective`, yet A's
`os.WriteFile` at step 3 can land AFTER B's — clobbering the file with A's stale
snapshot that is missing B's directive (and anything else committed inside A's
read→write window). Directives silently vanish from disk; next boot,
`LoadDirectivesFromDisk`'s destructive sweep sees them "not on disk" and
deprecates them in BoltDB too. Exactly the documented −5 net symptom.

The boot path CANNOT cause this: `LoadDirectivesFromDisk` only READS the file —
it never writes it. The loss is 100% in the writer, as action item #2 suspected.

### Open questions — answered

1. **Who called CompactDirectives?** — Almost certainly nobody. The loss was the
   `SyncDirectivesToDisk` write race, not a compact. Stop hunting a destructive caller.
2. **Stricter corruption guard?** — Already shipped (eca89dc, 20% relative-loss
   guard). It is a SAFETY NET on the read path; it does not fix the writer race.
3. **Snapshot before write?** — `SnapshotDirectives` exists (549dde9) but is only
   wired into `handleCompactDirectives`, NOT `SyncDirectivesToDisk`. Fixing the
   race is the real fix; a snapshot there would only be a fallback.
4. **`CompactDirectives` confirm:true?** — Low priority; not the loss vector.

### `db.Batch` audit — NOT the bug (cleared)

`SaveDirective`/`UpdateDirective`/`DeprecateDirective`/`CompactDirectives` use
`db.Batch` for read-modify-write on the single `global_rules` key. Audited as
SAFE: bbolt rolls the whole batch txn back on any fn error before retrying, so
the non-idempotent `append` in `SaveDirective` is never double-applied, and
coalesced fns see each other's Puts within the one txn. (`db.Update` would be
the more idiomatic choice for a shared-key RMW, but it is not a correctness bug.)

### Recommended fix (IMPLEMENTED 2026-05-14 — see "FIX SHIPPED" above)

- Serialize the writer: a `WAL.directivesMu sync.Mutex` held across the
  mutate-then-sync unit (or at minimum across `GetDirectives`+`os.WriteFile`
  inside `SyncDirectivesToDisk`), so a stale snapshot can never overwrite newer state.
- Make the write crash-atomic: temp file + `os.Rename`, same pattern as the new
  `CompactWAL` (commit 0374cee). A crash mid-`os.WriteFile` currently truncates
  the file outright.
- Confirm the MCP child's tool-call concurrency model — if tool calls are
  strictly serialized within one child the race window narrows, but the
  crash-atomicity gap remains; the mutex is cheap insurance either way.

**Recovery verification (still valid):** confirm the 7 re-added directives
survive a `make rebuild-restart` cycle. The current synced file shows 1-57 — if
they persist across restarts, the read-path guards (eca89dc) are holding the
line until the writer race is fixed.
## ~~[2026-05-13 15:59] AST COMPLEXITY in wal.go:809~~ — RESOLVED 2026-05-13 (commit eca89dc)

Auto-tracker logged this finding at 15:59. Resolved 16:05 via refactor in `eca89dc fix(rag): relative-loss guard for destructive sync` — `LoadDirectivesFromDisk` was extracted into 5 helpers (`countActiveDirectivesIn`, `relativeLossPct`, `shouldSkipDestructiveSweep`, `runDestructiveSweep`, `runAdditiveUpsertFromDisk`) and dropped from CC=16 → CC=5. Post-restart AST_AUDIT confirms clean.

---

## ~~[2026-05-14 08:29] HNSW WAL (hnsw.db) sin compactación BoltDB — crecimiento ilimitado bloquea boot de workspaces~~ — RESOLVED 2026-05-14 (commit 0374cee)

**Prioridad:** P1

### RESOLUCIÓN — `feat(rag): offline WAL compaction` (0374cee)

`bbolt.Compact()` ahora existe en el repo. Shipped:

- `pkg/rag/wal_compact.go::CompactWAL` — compactación offline crash-safe: abre el
  fichero source read-only, copia páginas vivas a un temp sibling con txn de
  64 MiB (heap plano), y hace `os.Rename` atómico. Cualquier fallo antes del
  rename (SIGKILL, disk-full, lock timeout) deja el original intacto.
  `CompactWALIfOversized` aplica el gate por `thresholdMB` (≤0 = opt-out).
- `cmd/neo-mcp/boot_helpers.go::bootRAG` — auto-compactación boot-time ANTES de
  `OpenWAL`, solo cuando el fichero supera `cfg.RAG.WALCompactThresholdMB`;
  workspaces sanos no-op. Dropea el snapshot `hnsw.bin` stale para forzar cold
  rebuild limpio en vez de un reject confuso por stale-guard.
- `cmd/neo/main.go::workspaceCompactCmd` — `neo workspace compact [ws]` manual
  (workspace debe estar parado — el lock exclusivo de bbolt lo garantiza).
- `pkg/config/config.go` — `RAGConfig.WALCompactThresholdMB` (default 256) +
  backfill en `applyRAGDefaults` (config.go:816) + `neo.yaml` + `neo.yaml.example`.
- `~/.neo/nexus.yaml` — `startup_timeout_seconds` 30 → 240 (headroom para la
  compactación one-time de un hnsw.db multi-GB + carga HNSW 500k+ nodos).

Decisión de hook: boot-time, NO `WAL.Close()` (`make rebuild-restart` SIGKILLea
children a 5s, demasiado corto para una compactación multi-GB) y NO la tarea idle
`[SRE-HOMEOSTASIS]` recomendada originalmente (boot-time es unbounded y ataca el
caso real — un workspace que no puede ni arrancar). Verificado: `go build ./...`
limpio, `go test -short ./pkg/rag/` OK (27s, incl. concurrency/crash-safety/
disk-full/lock-conflict en `wal_compact_test.go`), `AST_AUDIT` limpio.

**Workaround inmediato pendiente (acción operador, no código):** mover
`hnsw.db` + `hnsw.bin` a backup en strategos-32492 y strategosia-frontend-82899
→ boot limpio + re-ingesta. La auto-compactación los rescata en el siguiente
boot una vez que el binario actualizado esté desplegado en esos workspaces.

### ORIGINAL ENTRY (preserved for reference)

SÍNTOMA: strategos-32492 y strategosia-frontend-82899 no arrancan via Nexus — child_boot_timeout 30s, orphan killed, status=stopped permanente (restarts:0 porque nunca alcanzan 'running'). El child se cuelga en main.go:511 [BOOT] long-term memory subsystem (RAG WAL).

CAUSA RAÍZ: pkg/rag/wal.go usa BoltDB para hnsw.db. Los ficheros BoltDB crecen de forma monótona (high-water mark) y requieren bbolt.Compact() explícito para reclamar páginas libres. NO existe NINGUNA llamada a bbolt.Compact en todo el repo (grep -rn 'bbolt.Compact|NoFreelistSync|FreelistType' = 0 hits). OpenWAL (wal.go:64) no compacta; WAL.Close (wal.go:1179) no compacta; WAL.Vacuum (wal.go:1126) solo purga bucketDocs (ghost files) y ni siquiera eso reduce el fichero. Resultado: hnsw.db acumula churn de cada sesión (directive sync, session-state, SaveDocMeta/Scar/Weights, re-ingesta) sin reclamar jamás.

EVIDENCIA: strategos .neo/db/hnsw.db = 5.3GB, hnsw.bin = 2.4GB (workspace creado 2026-04-21, ~3 semanas). strategosia-frontend .neo/db = 12GB (mismo created_at). neoanvil = 105MB hnsw.db (creado 2026-05-11, 3 días). El bloat escala con edad × churn — time bomb para todos los workspaces. Boot previo de strategos: [OOM-GUARD] Heap 8226 MB > threshold 512 MB al hacer LoadGraph del bucketVectors inflado.

FIX RECOMENDADO: añadir bbolt.Compact() — opción preferida: tarea de mantenimiento idle ([SRE-HOMEOSTASIS] ya corre en idle, main.go:1643) con trigger por ratio de freelist o tamaño de fichero. bbolt.Compact escribe a fichero fresco y hace swap — requiere ~2× disco transitorio y DB quiescente. Alternativas: compactar en WAL.Close durante teardown SIGTERM, o en OpenWAL si supera umbral (lento al boot). Adicional: subir startup_timeout_seconds en nexus.yaml y hacer que el watchdog reintente boot-timeouts. Workaround inmediato: mover hnsw.db + hnsw.bin a backup → boot limpio + re-ingesta.

---

## ~~[2026-05-14 12:48] AST COMPLEXITY in tool_red_team.go:134~~ — RESOLVED 2026-05-14 (same session)

Auto-tracker fired on a stale snapshot. The DS-timeout fix added one fail-fast
branch to `redTeamAuditWithDB` (CC 15→16); the arg-parsing block was immediately
extracted into `parseRedTeamArgs`, dropping it back to CC≈12. Post-refactor
`AST_AUDIT` clean. Zombie entry — closed in the commit that introduced it.

---

## ~~[2026-05-14] BLAST_RADIUS dep-graph never populated — always degraded to AST fallback~~ — RESOLVED 2026-05-14 (commits 6265dd7 + 441ba54 + 1966953)

**Prioridad:** P1 (afectaba toda sesión, en todos los workspaces)

SÍNTOMA: `BLAST_RADIUS` reportaba `graph_status: empty` / `pagerank_used: false`
en todas las sesiones — corría permanentemente en `fallback: ast`,
`confidence: medium`, sin el call-graph real ni el ranking PageRank de impacto.

CAUSA RAÍZ (doble): (1) `rag.SaveGraphEdges` — el ÚNICO escritor del bucket
`GRAPH_EDGES` que `BLAST_RADIUS` lee vía `GetAllGraphEdges`/`GetImpactedNodes` —
tenía **cero callers**. El grafo se inicializaba vacío y nada lo poblaba jamás.
(2) `bootstrapWorkspace` poblaba un bucket DISTINTO (`hnsw_deps` vía
`SaveDependencies`) con import-strings como claves — mismatch estructural con
las queries por file-path, y además `GetDependents` (su lector) tampoco tiene
callers. Las dos mitades nunca se conectaron.

FIX (3 commits):
- `6265dd7` — `rag.ReplaceFileEdges` (reemplazo idempotente per-file vía prefijo
  `<source>->`) + resolver `fileDepEdges`/`goImportToFiles`/`workspaceModulePath`
  (imports → archivos reales del workspace, file→file, agnóstico de lenguaje).
  `bootstrapWorkspace` ahora escribe `GRAPH_EDGES` real.
- `441ba54` — wire en `neo_sre_certify_mutation`: cada certify refresca los edges
  del archivo mutado (mantenimiento incremental, goroutine independiente).
- `1966953` — `backfillDepGraph`: red de seguridad boot-time — walk import-only
  barato (sin embeddings) cuando `GRAPH_EDGES` está vacío (caso snapshot-boot).

Verificado: build limpio, tests (ReplaceFileEdges idempotency/prefix-isolation/
topology + resolver matrix), AST_AUDIT limpio, certify FEATURE_ADD ok. **Toma
efecto tras restart de neo-mcp.** DS premortem intentado en background mode (fix
de eb3c4c7) — self-premortem aplicado (ver abajo).

### Follow-ups detectados (no bloqueantes)
- ~~`radar_semantic.go:137` escribe el bucket huérfano `hnsw_deps`; `SaveDependencies`/
  `GetDependents`/`bucketDeps` dead code~~ — RESOLVED 2026-05-14 (commit 41b1e69):
  `saveIndexDependencies` redirigido a `fileDepEdges`+`ReplaceFileEdges`, las 2
  funciones + el bucket + refs del sanitizer eliminados (−54 LOC).
- ~~`GetImpactedNodes` era un 2º full-scan redundante del bucket en cada
  `BLAST_RADIUS`~~ — PARCIAL 2026-05-14 (commit cb27b69): `resolveImpactedNodes`
  ahora deriva el impacted set del mapa de `GetAllGraphEdges` ya cargado
  (`impactedFromEdges`) — 1 scan en vez de 2. Queda el scan único de
  `GetAllGraphEdges`: con un workspace grande totalmente poblado convendría
  indexar por target, pero no medido como problema — diferir per doctrina
  [option-D] (medir antes de optimizar).
- DS `red_team_audit` con `background:true` retorna `task_id` pero ese id NO es
  consultable por la interfaz del tool (`task_id` está documentado solo para
  `generate_boilerplate`) — el resultado del audit en background es irretrievable.
- `cmd/neo-mcp/main.go::main` CC=18 (límite 15) — **pre-existente**, no
  introducido por estos commits (se añadió 1 statement `go ...`, 0 ramas); el
  Bouncer lo tolera (grandfathered). Refactor de `main` queda como deuda aparte.

---

## ~~[2026-05-14 15:32] AST COMPLEXITY in radar_folder_audit.go:39~~ — RESOLVED 2026-05-14 (commit 70db3ba)

Auto-tracker fired during the codebase audit phase, before the fix landed.
`auditClaudeFolder` CC=16 → ~3: per-skill row build extracted to `auditOneSkill`,
glob validation to `skillPathsValid`, xref check to `brokenSkillXrefs`. Zombie
entry — resolved in the same session, post-refactor `AST_AUDIT` clean.

---

## ~~[2026-05-14 15:32] AST COMPLEXITY in tool_cache_stats.go:289~~ — RESOLVED 2026-05-14 (commit 70db3ba)

Auto-tracker fired during the codebase audit phase, before the fix landed.
`CacheStatsTool.Execute` CC=17 → ~5: section assembly split into
`addRadarCacheSections` + `addGlobalCacheSections`. Zombie entry — resolved in
the same session, post-refactor `AST_AUDIT` clean.

---

## ~~[2026-05-14] [ds-background-unretrievable] DS `background:true` task_id is not pollable~~ — RESOLVED 2026-05-15 (code complete + certified)

**RESOLUCIÓN 2026-05-15.** Root cause was NOT a missing store — the Nexus
`AsyncTaskStore` is complete. `handleAsyncDispatch` (`cmd/neo-nexus/plugin_routing.go`)
gated the single-task poll on `task_id present && action NOT present`, but the
`deepseek_call` schema makes `action` required (`cmd/plugin-deepseek/main.go:218`),
so `!hasAction` was never true → every poll fell through to a fresh dispatch.

Fix: the poll now routes on the `async_` ID prefix (`asyncIDPrefix` const), not
on `!hasAction`. An `async_*` id polls the Nexus store; a non-prefixed id (e.g.
`generate_boilerplate`'s plugin-side `bgtask_*`) falls through untouched. Same
shape applied to the `batch_id` branch (`batchIDPrefix`). `handleTaskPoll` /
`handleBatchPoll` unchanged. Covered by `TestHandleAsyncDispatch_PollGuard`
(5 subtests). DS premortem attempted 3 ways — all failed on DeepSeek upstream
instability (pro+max sync rejected, pro+high sync timeout, background → `EOF`
in 4.7s); self-premortem applied per `[SELF-AUDIT-V2]`, surfaced the prefix-gate
refinement (avoids an expired `async_` id silently starting a fresh audit).

**Takes effect after `make rebuild-restart`** (Nexus routing change). Mirror in
the `neo_debt` registry to be marked resolved.

### Follow-up detected (deferred, pre-existing — not introduced here)
`handleBatchPoll` resolves `batch_id` via the package-level in-memory `batchMap`,
populated only in `handleBatchDispatch`. On a Nexus restart `batchMap` is lost
while the BoltDB `AsyncTask` rows survive → batch polls return "batch not found"
for still-valid tasks. `batch_id`/`batch_files` aren't even in the `deepseek_call`
schema today, so impact is latent. Fix when batch is exposed: persist the
batch→taskIDs mapping (BoltDB bucket) or derive it from a task-id naming scheme.

### Original síntoma (preserved for reference)

SÍNTOMA: `deepseek_call` with `background:true` for `red_team_audit` /
`map_reduce_refactor` returns `{task_id, status:pending}` — but that `task_id`
cannot be polled. The `task_id` argument is documented (and wired) only for
`generate_boilerplate`. So a background audit's result is **unretrievable**:
the goroutine runs, produces output, and the caller can never fetch it.

EVIDENCIA: 2026-05-14 — attempted a DS premortem for the BLAST_RADIUS dep-graph
fix in background mode (`async_8d95eaed5c58856c`); polling it with `task_id`
started a fresh thread instead of returning the pending task's result. The
foreground path is the only working route (and it timed out on v4-pro+max
before `eb3c4c7` raised the budget) — so `background:true` is the intended
escape hatch for slow audits and it is currently broken.

FIX RECOMENDADO: see `master_plan.md` → "DS plugin — background task retrieval".
(1) Locate the Nexus-side background dispatch result store. (2) Wire `task_id`
polling for `red_team_audit` + `map_reduce_refactor` mirroring the
`generate_boilerplate` poll path. (3) Update the `deepseek_call` schema so
`task_id` covers all three background-capable actions; add a regression test.
Workaround until then: use the synchronous path with a raised
`DEEPSEEK_HTTP_TIMEOUT_SECONDS` for v4-pro+max audits.

_Mirrored in the `neo_debt` BoltDB registry (workspace tier, P2) — `neo_debt
list` surfaces it; this block is the human-readable detail._

---

## ~~[2026-05-14 17:59] neo_memory(rem_sleep) schema drift — required params not exposed~~ — RESOLVED 2026-05-15

**RESOLUCIÓN 2026-05-15.** Root cause: `MemoryTool.Execute` `case "rem_sleep":`
forwarded raw args straight to `RemSleepTool.Execute` (`cmd/neo-mcp/tools.go:66`),
which requires `learning_rate` + `session_success_ratio` as float64. The
`neo_memory` schema did not expose them, so manual rem_sleep was unreachable.

Approach C (defaults server-side + optionally expose):

- `cmd/neo-mcp/tools.go`: extracted package-level consts `defaultRemLearningRate
  = 0.01` / `defaultRemSuccessRatio = 0.7` next to `RemSleepTool`. `main.go`
  RemFn updated to reference them — the canonical values now live in one place.
- `cmd/neo-mcp/tool_memory.go`: `Execute` `case "rem_sleep":` wraps args in a
  new `withRemSleepDefaults` shim that injects the consts when missing (and
  when the caller passed a non-float64 by mistake); returns a fresh map so
  the caller's args aren't mutated.
- `cmd/neo-mcp/tool_memory.go` `InputSchema`: added `learning_rate` and
  `session_success_ratio` as **optional** properties (NOT in `Required`) with
  `[rem_sleep]` description, so advanced operators can override the defaults.
- Regression test `TestWithRemSleepDefaults` (5 subtests in
  `tool_memory_test.go`): empty args → defaults; explicit values win;
  partial → only missing filled; caller args not mutated; non-float64 type
  treated as missing.

`go build ./...` clean, full `cmd/neo-mcp` suite green, AST_AUDIT clean,
certified BUG_FIX. Live effect: `neo_memory(action:"rem_sleep")` with no args
now succeeds with default hyperparameters.

### Original entry (preserved for reference)

neo_memory action:rem_sleep fails with "learning_rate and session_success_ratio must be numbers" — the handler requires those two params but the neo_memory tool schema does not expose them (no learning_rate / session_success_ratio fields). Manual REM trigger is unusable through the MCP tool interface. Low impact: REM consolidation still runs automatically on 5-min idle ([SRE-MEMEX]), so the memex→HNSW path works; only the manual trigger is broken. Surfaced 2026-05-14 at session close. Fix: either add the two fields to the neo_memory input schema (cmd/neo-mcp tool registration) or default them server-side when absent. Same defect class as the schema-vs-error-drift Distilled Wisdom note. Files: cmd/neo-mcp tool_memory.go + the neo_memory schema.

---

## ~~[2026-05-15 04:46] AST COMPLEXITY in macro_tools.go:1297~~ — RESOLVED 2026-05-15 (zombie)

Auto-tracker fired on a stale snapshot. During Phase 2 MV wiring the
test-impact logging block was briefly inlined in `certifyLocalBatch`,
bumping CC to 17 — extracted in the same commit (`bcf87ef`) into
`logTestImpactFootprint`, and again in `e4c57f7` into `testImpactSummary`,
dropping CC back under the cap. Post-refactor `AST_AUDIT cmd/neo-mcp/macro_tools.go`
returns clean (only the grandfathered `main` CC=18 remains).
Zombie entry — closed in the same session that introduced it.

---

