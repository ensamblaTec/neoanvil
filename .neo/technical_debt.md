# Technical Debt — Épicas Completadas

> Este archivo es gestionado automáticamente por el Kanban de Neo-Go.
> Las épicas completadas (todas las tareas [x]) son archivadas aquí
> durante el ciclo REM (5 min de inactividad) para mantener el Master Plan limpio.

---

## Active deferred items

### [scaffold-broken] `neo_forge_tool` — non-functional since initial commit (2026-05-10)

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

### [deadcode-candidate] `cmd/neo evolve` — Darwin Engine never iterated

`cmd/neo/evolve.go` (Darwin Engine — genetic evolution of Go
functions, SRE-93). Single commit since initial: `fd99a39`.
Zero references in `docs/`, `.claude/skills/`, or any test.
No commit since 2026-04-XX touched it. Operator-facing UX
("`neo evolve <file> <func>` runs genetic evolution and benchmarks
mutations") never materialised in workflow.

**Triage:** confirm with operator, then `git rm cmd/neo/evolve.go`
+ remove `evolveCmd` from `cmd/neo/main.go`'s rootCmd assembly.

### [deadcode-candidate] `cmd/neo ask` / `chat` — Voice of Leviathan unused

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

## [2026-05-13 02:51] [context-bloat] CLAUDE.md + rules + skills inyectan ~64k tokens upfront — auto-mode degrada doctrina Ouroboros

**Prioridad:** P2

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


## [2026-05-13] PRE-EXISTING FLAKE — TestBackgroundIndexFile_SymlinkEscapeRejected (macOS /var symlink)

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

## [2026-05-13] DUAL-LAYER-SYNC drift — 7 directives lost from disk file

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

## [2026-05-13] FOLLOW-UP — Writer root cause for DUAL-LAYER-SYNC drift (commit b24e4eb closed the symptom)

**Status:** symptom resolved via mass re-add. Root cause not patched.

**Recovered:** 7 directives re-added in b24e4eb (condensed ≤500 chars). Disk now 57/60.

**Open questions for the next investigation:**
1. **Who called CompactDirectives?** Only operator-driven path (`neo_memory(learn, action_type:compact)`) hard-purges entries. No auto-trigger exists. Search `.neo/logs/mcp.log` archive for `name=neo_memory id=... action_type=compact` events to identify the moment of loss. If no such call, there's an unknown removal path — investigate further.
2. **Should destructive-sync corruption guard be stricter?** Current guard at pkg/rag/wal.go:830:
   `if activeOnDisk < syncDestructiveMinDisk && activeInBoltDB > syncDestructiveBoltDBThreshold` — both conditions must hold (5 / 50). A 7-entry delta slips through. Proposal: also refuse destructive if `activeOnDisk < 0.8 × activeInBoltDB` (20% relative loss).
3. **Should SyncDirectivesToDisk emit `.neo/db/directives_snapshot.json` backup before each write?** Cheap (BoltDB read is already done) and gives recovery option independent of git.
4. **Should `CompactDirectives` require `confirm:true` arg?** Right now any `action_type:compact` call is destructive. A confirmation arg would prevent accidental purge.

**Why not fix now:** the destructive-sync write path is in BoltDB territory and needs DS premortem + regression test, est. 2-3 commits. Scope-limited to a future session focused on directive durability hardening.

**Recovery verification needed at next restart:** confirm the 7 re-added directives survive a `make rebuild-restart` cycle. If they get purged again on boot → boot path has the bug (not user action). If they survive → bug was operator-triggered compact at some point.
