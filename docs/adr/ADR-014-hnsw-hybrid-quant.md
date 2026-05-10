# ADR-014: HNSW hybrid quantization — wire dispatcher + production opt-in

- **Fecha:** 2026-05-10
- **Estado:** Aceptado (initial wire shipped)
- **Hereda de:** PILAR XXV/170 (int8/binary primitives), [ADR-013](./ADR-013-local-llm-tool.md)

## Contexto

PILAR XXV (170.A-C) shipped int8 + binary + hybrid HNSW search primitives in
`pkg/rag/{hnsw_int8.go, hnsw_binary.go, hnsw_hybrid.go, quantize.go}`. The
boot path already calls `populateQuantCompanion()` based on
`cfg.RAG.VectorQuant`, so the int8/binary companion arrays were getting
populated correctly.

**Critical gap discovered during 2026-05-10 audit:** the four `Graph.Search()`
call sites in `cmd/neo-mcp/{main.go, radar_semantic.go, radar_briefing.go}`
**always used the float32 path** regardless of `cfg.RAG.VectorQuant`. The
quantization primitives were dead code in terms of search dispatch — the
companion was populated but never queried.

Operator's question that triggered this work: *"el HNSW si es largo aquí…
podríamos usar el HNSW de aquí para validar… 100k+ code en strategosia"*.

## Decisiones específicas

### 1. Companion-only design — verified, NOT replacement

DeepSeek validation (2026-05-10): `PopulateInt8` is COMPANION storage. The
`Vectors []float32` source-of-truth slice is NEVER released. So:

- `int8`: float32 + int8 companion = **+25% RAM**, NOT a 4× saving
- `binary`: float32 + binary companion = **+3% RAM**
- `hybrid`: same as binary (companion) — uses binary for candidate filter,
  float32 for rerank

**Real RAM savings would require a refactor (drop float32 after Populate,
persist int8 to disk, schema bump).** That refactor is multi-day, breaks
hnsw.bin format, forces cold rebuild on all workspaces. Out of scope for
this ADR. Documented as future work.

### 2. Empirical recall measurement on production corpus

`pkg/rag/recall_measure_live_test.go` (build tag `hnsw_live`) loads the
operator's actual `.neo/db/hnsw.bin` snapshot and measures top-K overlap
of int8/binary/hybrid vs the float32 baseline. On 25,023-vector neoanvil
corpus, 50 random queries top-10:

| Backend | Recall median | Latency median | RAM extra |
|---------|--------------:|---------------:|----------:|
| float32 (baseline) | 1.000 | (9 µs bench) | 0 |
| int8     | **1.000** | 10 µs | +25% |
| binary   | **1.000** | **4 µs** (2.5× faster!) | +3% |
| **hybrid** | **1.000** | **5 µs** | +3% |

The synthetic test "binary 2/10 random vectors" was **worst-case**.
Production embeddings have semantic structure that binary popcount
preserves perfectly at this corpus size.

### 3. SearchAuto dispatcher — single entry, quant-aware

Added `Graph.SearchAuto(ctx, query, topK, cpu, quant string)` in
`pkg/rag/hsnw.go`. Dispatch logic:

- `quant == "hybrid"` AND BinaryPopulated AND full Vectors → SearchHybridBinary
- `quant == "binary"` AND BinaryPopulated → SearchBinary
- `quant == "int8"` AND Int8Populated → SearchInt8
- otherwise → Search (float32)

Falls back to float32 if the requested companion is not populated. All four
production call sites now route through `SearchAuto` with
`cfg.RAG.VectorQuant`:

- `cmd/neo-mcp/radar_semantic.go:483` — SEMANTIC_CODE main search
- `cmd/neo-mcp/radar_briefing.go:1568` — BRIEFING architectural memory
- `cmd/neo-mcp/main.go:266` — Gossip search closure
- `cmd/neo-mcp/main.go:819` — Audit recall closure

### 4. Default stays `float32`; hybrid is opt-in per workspace

`neo.yaml.example` documents all four modes with measured numbers and the
companion-storage caveat. This workstation's `neo.yaml` activates `hybrid`
for the neoanvil workspace as the first production trial. Other workspaces
(strategosia, strategosia_frontend) stay on float32 until measured.

Acceptance criteria for promoting `hybrid` to default in next release:

1. Recall ≥ 0.95 measured on 3+ workspaces of varying size and language
2. Search p95 latency ≤ 30% of float32 baseline (already achieved at 25k)
3. RAM overhead ≤ 5% of total HNSW memory (already 3%)
4. No regression in BLAST_RADIUS / SEMANTIC_CODE warm-cache UX over 1 week

### 5. Plans rejected during this audit

| Plan | Reason rejected |
|------|-----------------|
| int8 as RAM saver | Companion mode → ADDS 25%, doesn't save 75% |
| Binary alone (no rerank) | Recall risk on unmeasured corpora; hybrid gives same speed with rerank insurance |
| Refactor to replacement mode | 1-2 day effort, breaks hnsw.bin schema, all workspaces need cold rebuild. Defer until 200k+ vector workloads where float32 RAM actually hurts |
| Embed model upgrade nomic v1→v1.5 | nomic-embed-text:latest already IS v1.5; no upgrade available |
| CPG SSA parallelization | Earlier ADR retraction — walk = 1.5% of cold-build, not worth |

## Consecuencias

- ✅ Search dispatch wired end-to-end. `cfg.RAG.VectorQuant` now actually
  changes runtime behaviour at the 4 call sites.
- ✅ Hybrid mode delivers ~equal speed with ZERO recall hit on this corpus.
- ✅ Empirical bench harness (`recall_measure_live_test.go`) reusable for
  any workspace with `make` + tag `hnsw_live`.
- ✅ neo.yaml.example documents companion-mode reality; future readers
  won't repeat the "4× RAM saving" mistake.
- ⚠️ Real RAM savings still require a refactor (replacement mode). Not
  done; documented as future work in technical_debt.md.
- ⚠️ Binary-only mode (no rerank) gated on per-corpus recall measurement.
  Operator must run the bench before flipping a workspace to `binary`.

## Validación

- `go test -race -short ./...` — full pass (52 ok)
- `make audit-ci` ✓ 0 NEW findings vs baseline
- `recall_measure_live_test.go` on neoanvil hnsw.bin: int8/binary/hybrid all 1.000 recall
- Bench numbers reproducible: `go test -tags hnsw_live -v ./pkg/rag/ -run TestRecall_Live -timeout 5m`

## Silent fallback bug + lazy re-populate fix (2026-05-10, commit 338b945)

Initial implementation shipped a critical regression that the first
"validation" missed because neoanvil was idle during the bench window.
Cross-workspace validation against the live strategosia + strategosia_frontend
children surfaced it: boot logs confirmed `hybrid companion populated:
N nodes, +K KB`, yet `search_paths` counter stayed at `binary=0 hybrid=0`
after real SEMANTIC_CODE queries. Every search silently fell back to
float32. Quality and recall were unaffected (float32 still works), but the
operator was paying the RAM cost of the binary companion without getting
any of the speed benefit.

**Root cause.** `PopulateBinary` builds `BinaryVectors` with `len = len(Nodes)
* BinaryWords` at the moment it runs. `Graph.Insert` / `InsertBatch` grow
`Vectors[]` and `Nodes[]` but never touch `BinaryVectors[]`. After even ONE
Insert post-boot, `BinaryPopulated()` returns false because the count check
fails. `SearchAuto`'s switch falls through to `Search(float32)` silently —
no log line, no metric, no test signal. Strategosia / strategosia_frontend
ingest continuously (file watcher + workspace_utils worker pool), so the
binary companion was always stale by the time a search ran.

**Empirical evidence (pre-fix):**

| Workspace | Boot populated | Counter post-search | Verdict |
|-----------|---------------|--------------------:|---------|
| neoanvil | 25k nodes | hybrid=122 ✓ | works (workspace was idle) |
| strategosia | 65k nodes | **hybrid=0** ❌ | silent fallback |
| strategosia_frontend | 133k nodes | **hybrid=0** ❌ | silent fallback |

**Fix.** Two new helpers in `pkg/rag/hsnw.go`:

- `ensureBinaryPopulated()`: when `BinaryWords > 0` (operator opted in) AND
  companion is stale → re-populate under `snapshotMu`. No-op when up-to-date.
- `ensureInt8Populated()`: same shape for int8 companion.

`SearchAuto` now calls these before the population check. Re-populate cost
is amortised: paid once per ingest cycle, not per search. Concurrency-safe
via the existing `snapshotMu` (reused from snapshot Save coordination).

**Empirical evidence (post-fix):**

| Workspace | Query 1 (cold, includes re-populate) | Queries 2-3 (warm) | Counter |
|-----------|-------------------------------------:|-------------------:|---------|
| neoanvil (25k) | n/a | warm | hybrid=1 ✓ |
| strategosia (65k) | 192 ms | 20-31 ms | hybrid=3 ✓ |
| strategosia_frontend (133k) | **525 ms** | 125-129 ms | hybrid=3 ✓ |

The 525 ms first-query cost on strategosia_frontend matches the predicted
cost model: `3.2 µs/vector × 132,866 vectors = 425 ms` plus search overhead.
After that single payment, search latency drops to the ~125 ms regime
which is dominated by Ollama embed (HNSW search itself is ~5 µs warm).

**Why this fix is right (vs. alternatives):**

- *Incremental update inside `Insert`* — cleanest but invasive; touches the
  hot Insert path and requires careful concurrency. Adds complexity to the
  ingestion code that's already complicated. Lazy re-populate is a drop-in.
- *Background goroutine re-populate after each Insert* — adds complexity;
  lazy on demand has the right cost model for our workload (sparse search,
  sparse ingest).
- *Re-populate after every InsertBatch synchronously* — adds 400ms to every
  ingest batch, hurts ingestion throughput. Lazy amortises better.

The fix only kicks in when the operator opted into a quant mode at boot
(BinaryWords > 0 / Int8Scales != nil). Workspaces on `vector_quant: float32`
never pay any cost — `ensureBinaryPopulated` returns early.

**Lesson learned.** Boot logs prove population happened. They DO NOT prove
search dispatch is using the populated companion. The dispatch counter
(`search_paths` field in HUD_STATE / neo_cache stats) is the only honest
runtime signal. Future quant work should always validate via the counter,
not via boot logs alone.

## Cross-workspace validation (2026-05-10 follow-up)

Bench harness extended via `HNSW_BIN_PATH` env var so any workspace's
hnsw.bin can be measured. Run for each of the 3 production workspaces:

| Workspace | Lang | Nodes | Float32 RAM | hybrid recall | hybrid lat | binary RAM extra |
|-----------|------|------:|------------:|:-------------:|----------:|-----------------:|
| neoanvil | Go (mixed) | 25,342 | 74.2 MB | **1.000** | 5 µs | 2.3 MB (3.1%) |
| strategosia | Go monolith | **44,469** | 130.3 MB | **1.000** | 5 µs | 4.1 MB (3.1%) |
| **strategosia_frontend** | TypeScript | **124,642** | 365.2 MB | **1.000** | 5 µs | 11.4 MB (3.1%) |

**Cross-corpus generalisation confirmed.** Latency stays at ~5µs across the
3-5× scale jump (25k → 44k → 124k), exactly the O(log N) profile HNSW promises.
RAM overhead holds at 3.1% across all sizes. Population time scales linearly
with N (89ms / 134ms / 402ms for binary at 25k / 44k / 124k = ~3.2 µs/vector).

`vector_quant: hybrid` activated in `neo.yaml` for both strategosia
workspaces alongside neoanvil. Operator-facing impact: identical search
quality at minimal RAM cost across the entire 195k-vector fleet.

## Remaining Follow-ups

1. ~~Run recall measurement on strategosia + strategosia_frontend~~ ✅ DONE
2. Real-world UX validation (1 week) — observe BLAST_RADIUS / SEMANTIC_CODE
   warm-cache UX in daemon mode across all 3 workspaces.
3. If 1-week observation clean → promote `hybrid` to default in
   `defaultNeoConfig()`. With recall 1.000 + minimal RAM cost confirmed
   on 3 workspaces with very different language profiles, this is now
   defensible.
4. (Long-term) Replacement-mode refactor for actual RAM savings — gated on
   real RAM pressure (current 569 MB total float32 vectors across 3
   workspaces = 1.8% of 32 GB box). Not justified yet.

## Referencias

- `pkg/rag/hsnw.go::SearchAuto` — dispatch entry point
- `pkg/rag/hnsw_hybrid.go::SearchHybridBinary` — binary candidate + float32 rerank
- `pkg/rag/hnsw_binary.go::SearchBinary` + `PopulateBinary` — popcount path
- `pkg/rag/hnsw_int8.go::SearchInt8` + `PopulateInt8` — quant path
- `pkg/rag/recall_measure_live_test.go` — empirical recall measurement
- `cmd/neo-mcp/main.go::populateQuantCompanion` — boot-time population
- Directive #55 `[HNSW-QUANT-WIRING]` — operational rule
- ADR-013 — local LLM tool (related GPU-leverage work this session)
