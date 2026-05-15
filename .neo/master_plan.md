# NeoAnvil — Speed-First Initiative (audited revision)

## Phase: Speed-First — agent tool latency reduction

### Context

Bottleneck-driven plan to reduce **tool latency × call frequency** across every
workspace running neo-mcp. Measured baselines on this workspace (2026-05-15):

- `neo_radar` — p99 **460ms** over 461 calls → ~3 min cumulative
- `neo_sre_certify_mutation` — p99 **24.6s** over 86 calls → ~6 min cumulative
- Caches present + sized + snapshotted, but hit_ratio **0%** post-boot
  (Qcache 0/0, Tcache 0H/4M, Ecache 0/0; only `pagerank_cache` at 86%)
- Search paths: `bin=0 hybrid=0 int8=0` — no HNSW quant active

**Cross-workspace leverage:** neoanvil ships the binary that runs in strategos,
strategosia, strategosia-frontend. A 10× speedup here propagates at zero deploy
cost. For strategos-class workspaces (10× the file count) absolute saving scales
linearly with N_files.

### Audit findings (vs original plan, 2026-05-15)

The original plan had three premise errors discovered during code audit:

1. **`symbolMapCache` ALREADY EXISTS** at `cmd/neo-mcp/radar_compile.go:22-28`.
   It's keyed by `absDir@aggregated_mtime@includeUnexported` — already does
   what original-Pilar-I proposed to build. The ONLY gap is in-memory-only
   storage (lost on restart). **Pilar I shrinks from "build symbol index" to
   "persist existing memo" → moved into Phase 0 as quick win 0.D.**

2. **QueryCache and TextCache already implement per-entry generation
   stamps** — `Get(key, currentGen uint64)` returns miss on gen mismatch
   (`pkg/rag/query_cache.go:110`, `text_cache.go:18`). The real problem is the
   generation is the GLOBAL `graph.Gen` counter, bumped on every `InsertBatch`
   (`pkg/rag/hsnw.go:521`) — editing one file invalidates ALL cache entries
   because they share the gen. **Pilar III redesigned: decouple cache validity
   from graph gen by adding parallel per-dependency-file mtime tagging.**

3. **Dep-graph functions live in `pkg/rag/graph.go`** (not a separate
   `dep_graph.go` as the plan said). Certify's test runner is
   `exec.CommandContext(ctx, "go", "test", "-short", pkgPath)` at
   `cmd/neo-mcp/macro_tools.go:1800` — exactly the line Pilar II rewrites.
   **Pilar II paths corrected, design unchanged.**

### North-Star metrics (validate before / after each phase)

1. `neo_tool_stats sort_by:p99` — `neo_radar` and `neo_sre_certify_mutation` p99
2. `neo_cache stats` — query / text / embedding cache hit_ratio
3. `make audit-ci` — 0 NEW findings vs baseline (no regressions)
4. Full short test suite green (`go test -short ./cmd/... ./pkg/...`)
5. Each phase: a `BenchmarkXxx_BeforeAfter` checked into testdata

---

### Phase 0 — Quick wins (each 1-2h, low risk, mostly independent)

- [x] **0.A — Cache auto-warmup at boot** — done 2026-05-15. Two pieces
      shipped together (the value-delivering combination):
      (a) `pkg/rag/cache_persist.go` — `persistedSnapshot` and
      `persistedTextSnapshot` extended with `RecentMisses []string`
      (`omitempty` so empty-ring snapshots stay tidy); SaveSnapshot harvests
      `c.RecentMissTargets(missRingPersistCap=64)`; LoadSnapshot rehydrates
      via `c.misses_.record(...)` newest-on-top.
      (b) `cmd/neo-mcp/main.go` — `warmupTool` extracted to a named var, async
      goroutine after `mustRegister` invokes
      `warmupTool.Execute(ctx, {"from_recent":true})`. Detached so warmup
      latency never blocks boot.
      Tests: `TestSnapshot_RecentMissesRoundTrip` 3 subtests — query cache,
      text cache, empty-ring omits the JSON field. Round-trip preserved
      newest-first ordering.

- [x] **0.B — Per-intent latency breakdown in `neo_tool_stats`** — done
      2026-05-15. Found the dispatcher already records both `neo_radar` and
      `neo_radar/<intent>` to the in-memory ring (`main.go:1005-1007`); the
      persisted store (`bucketToolAggregate`) was keying only by `rec.Name`,
      dropping the action. Fixed in `pkg/observability/store.go::persistCall`
      via a new `updateToolAggregate` helper + dual-write (bare + composite
      when `action != ""`). Backward-compat: existing dashboards keep working;
      neo_tool_stats now surfaces `neo_radar/BLAST_RADIUS` p99 separately
      from `neo_radar/AST_AUDIT`. `TestStore_RecordCall_PerActionAggregate`
      regression test asserts the dual-write + that p99s diverge.
      `TestStore_ConcurrentRecordCall` updated to count bare-tool rows only
      (the dual-write is intentional double-write at aggregate level).

- [x] **0.C — HNSW quant `hybrid` audit + decision** — done 2026-05-15.
      Audit found: (a) `populateQuantCompanion` wiring at `main.go:237-262`
      handles all 4 modes correctly; (b) this workspace's `neo.yaml` already
      had `vector_quant: hybrid`; (c) the HUD `search_paths bin=0 hybrid=0`
      reflects no-searches-yet, NOT mis-wiring; (d) `pkg/rag/hnsw_hybrid_test.go`
      exists and ADR-014 already documents recall=1.000 across 3 production
      workspaces. Gap closed: `neo.yaml.example` template flipped from
      `"float32"` to `"hybrid"` with a comment pointing at ADR-014 so new
      workspaces inherit the validated default. `applyRAGDefaults` binary-
      level default left at `"float32"` to avoid silently flipping workspaces
      with no explicit yaml.

- [x] **0.D — Persist `symbolMapCache` across restart** — done 2026-05-15
      commit `332c01c`. Added `saveSymbolMapSnapshot` / `loadSymbolMapSnapshot`
      (versioned JSON envelope) in `radar_compile.go`; wired load in
      `setupCaches`, save in `persistCachesOnShutdown` (cache_setup.go). 4
      regression tests: round-trip, missing-file no-error, version-mismatch
      no-crash, corrupt-JSON returns-error. Takes effect after
      `make rebuild-restart`.

---

### Phase 1 — Pilar III: File-scoped cache invalidation (redesigned per audit)

#### Problem (revised)
QueryCache and TextCache already use per-entry generation stamps, but the
generation is `graph.Gen` — a single global atomic counter bumped on every
`InsertBatch` (`pkg/rag/hsnw.go:521`). One certify of one file → graph insert
→ gen bump → all 20+ cache entries become "stale". Post-restart hit_ratio is
0% not because validity check is missing but because validity scope is too
broad.

#### Solution
Add a **parallel per-target-mtime tag** to cache entries. Hit requires either
(a) graph gen still matches OR (b) the target file's mtime AND its known
dependency files' mtimes are all unchanged. (b) becomes the primary check;
graph gen falls back to nuclear-only for cross-cutting invalidations.

#### Tasks
- [ ] **1.1 — Schema migration in `pkg/rag/text_cache.go` +
      `query_cache.go`.** Each entry already stores `gen uint64`. Add
      `targetMtime int64` and `depMtimes map[string]int64` (only when known
      cheaply — BLAST_RADIUS has them, SEMANTIC_CODE may not). Backward-compat:
      zero values mean "fall back to old gen-only path".
- [ ] **1.2 — Caller-supplied dep mtimes in BLAST_RADIUS path.** At
      `cmd/neo-mcp/radar_blast.go::blastRadiusCacheLookup`, pass the target's
      mtime + each impacted node's mtime when populating the cache entry. On
      Get, single `os.Stat` per dep, compare; match → return, any miss → evict
      lazily and recompute.
- [ ] **1.3 — Loosen `tool_cache_flush` semantics.** Today it bumps `graph.Gen`
      (nukes all). After 1.1+1.2, file-edit-driven gen bumps in `InsertBatch`
      can be replaced by per-file mtime invalidation. `tool_cache_flush`
      remains as nuclear-option only.
- [ ] **1.4 — Deprecate `bypass_cache:true` arg.** Used in 6+ call sites
      (`radar_blast.go`, `radar_digest.go`, `radar_semantic.go`,
      `radar_graph.go`, `cmd/neo-nexus/graph_walk_scatter.go`). Make it a
      no-op once 1.1+1.2 land — entries self-invalidate. Schedule removal one
      cycle later.
- [ ] **1.5 — Surface mtime-mismatch evictions in `neo_cache stats`.** New
      field `stale_invalidations_mtime` per layer (separate from existing
      `stale_invalidations`).
- [ ] **1.6 — Tests + benchmarks.** Concurrent Get under file-mutate race;
      `BenchmarkCacheHotPath_FileMtimeGate` must show overhead per Get < 10µs
      vs no-validation baseline.
- [ ] **1.7 — Config knob.** `cache.invalidation_mode` enum
      `gen_only | mtime_first | mtime_only` (default `mtime_first` once
      validated) per `[CONFIG-FIELD-BACKFILL-RULE]`.

#### Exit
Post-restart Tcache hit_ratio jumps from 0% to ≥ 60% on the first replay of
recent BLAST_RADIUS targets. A typical edit→certify cycle no longer evicts
cache entries for unrelated files. `bypass_cache:true` calls in the codebase
become no-ops; new code stops adding them.

---

### Phase 2 — Pilar II: Dep-graph-aware Test Impact selection in certify

(Largely unchanged from original plan; paths corrected per audit.)

#### Problem
Certify runs the entire package's test suite after any single-line change.
`pkg/rag` is ~17s of tests for a 1-line change in any of its 69 files.
strategos-class suites measured in minutes pay this on every certify. The
dep-graph that knows the answer is **already populated** (Phase
DS-background-dep-graph: `6265dd7+441ba54+1966953`).

#### Solution
Use `GRAPH_EDGES` to compute the reverse index `test_file →
imports_transitive`. On certify, intersect with `mutated_files` to find the
test files whose execution depends on the mutated set. Run only those via
`go test -run 'TestA|TestB'`.

#### Tasks
- [ ] **2.1 — Reverse-edge helper `testsImpactedBy(files []string) []string`
      in `pkg/rag/graph.go`** (not `dep_graph.go` — that file doesn't exist;
      dep-graph code lives in `graph.go` alongside the HNSW graph). Walks
      `GetAllGraphEdges` backward from mutated files to find all `_test.go`
      files transitively reaching them. Reuses the same edge map BLAST_RADIUS
      already loads.
- [ ] **2.2 — Certify pipeline integration in
      `cmd/neo-mcp/macro_tools.go::runGoBouncer:1800`.** Replace blanket
      `go test -short <pkgPath>` with selective
      `go test -short -run 'TestA|TestB' <pkgPath>` from the impacted set.
- [ ] **2.3 — Safe fallback.** If reverse index empty (workspace not yet
      indexed) or graph coverage < 50%: run the package as today + log
      `test_impact_fallback:true` in the certify response. Never skip tests
      due to graph staleness alone.
- [ ] **2.4 — Always-run escape hatch.** Files with `//go:build integration`
      tag always included. Optional allowlist for any other "always-run" test
      files (config'd via `neo.yaml::sre.test_impact.always`).
- [ ] **2.5 — Surface in certify response.** New fields `tests_selected`
      (count + names) and `tests_skipped_via_dep_graph` (count) in the certify
      JSON. Operator sees the win per call.
- [ ] **2.6 — Tests + benchmarks.** Regression: a 1-line change in
      `tool_memory.go` must select `TestWithRemSleepDefaults` and tests whose
      deps include `tool_memory.go`, NOT the whole `cmd/neo-mcp` suite. Compare
      end-to-end certify wall-clock before/after on a fixed 5-file mutation set.
- [ ] **2.7 — Config field.** `sre.test_impact.enabled` (default false until
      validated) + `sre.test_impact.always_run []string` + backfill per
      `[CONFIG-FIELD-BACKFILL-RULE]`.

#### Exit
Certify p99 drops from 24.6s → estimated 3-8s on typical 1-file mutations.
For strategos with multi-minute suites the win is 10× larger in absolute terms.

---

### Phase 4 — Deferred debt (sweep, post-pillars)

- [ ] **4.A — `cmd/neo-mcp/main.go::main` CC=18.** 1496-line beast, currently
      grandfathered in `.neo/audit-baseline.txt`. With Phase 1 caches sane
      and Phase 2 selective tests, the long-deferred refactor (extract
      `newDaemonHooks`, `newMcpHandler`, `installHTTPMux`) lands safely in
      2-3 PRs.
- [ ] **4.B — `batchMap` restart-persistence gap** (deferred from
      DS-background phase). In-memory map lost on Nexus restart while BoltDB
      `AsyncTask` rows survive → batch polls return "batch not found" for
      valid tasks. Promote to BoltDB bucket OR derive from a task-id naming
      convention. Out of scope until batch is exposed in plugin schemas.
- [x] **4.C — `cmd/plugin-jira` 100% error_rate audit** — done 2026-05-15.
      Audit found the plugin already logs `connectivity OK/FAILED` per tenant
      at boot (`ops.go:146-148`) — the actual gap was operator-facing
      documentation. Added a "Troubleshooting — `jira/jira` error_rate 1.0"
      section in `docs/plugins/jira-integration-guide.md` covering: the
      symptom-vs-cause mapping (plugin running ≠ calls working), the
      diagnostic flow (plugin log → jira.json schema → `curl /myself` probe
      → audit-jira.log), and the design rationale for staying-alive-with-
      bad-creds (hot-reload of credentials without `make rebuild-restart`).
      No code change required.

---

### Anexos — explicitly NOT in scope (and why)

| Idea | Veredicto | Razón |
|---|---|---|
| **Sharded maps** | ❌ Skip | No measured contention. MCP dispatch is single-threaded within a child. |
| **Rewrite hot paths in Rust / manual SIMD** | ❌ Skip | Bottleneck is IO + parse, not CPU. The pillars deliver orders of magnitude first. |
| **CAN-BUS / network-edge logic** | ❌ Skip | Doesn't map to single-process MCP + Nexus federation. SSE is the existing analog. |
| **Parallel AST + Bouncer + RAG-index in certify** | ⏸ Defer | Worth doing AFTER Phase 2 — once dep-graph test impact lands, absolute latency may be low enough that parallelism is overkill. Revisit with measured data. |
| **MessagePack/CBOR transport instead of JSON-RPC** | ⏸ Defer | Plumbing-layer change. Pillars deliver more agent-visible speedup first. |

---

### Done when

- Phase 0 (0.A-0.D): shipped + measured. `make audit-ci` 0-new throughout.
- Phase 1: caches survive certify-driven graph mutations, `bypass_cache`
  deprecated, Tcache hit_ratio > 60% post-restart on replayed BLAST_RADIUS
  targets, `BenchmarkCacheHotPath_FileMtimeGate` overhead < 10µs.
- Phase 2: certify p99 < 8s on typical workspace changes, `tests_selected`
  surfaced in response.
- Phase 4 items resolved or formally archived.
- Live measurement across **3 workspaces** (neoanvil + strategos +
  strategosia) shows the speedup propagated end-to-end.
