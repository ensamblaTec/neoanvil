# NeoAnvil Reference Documentation

Extended reference sections from README.md. See README.md for the quick-start guide.

---

## `neo.yaml` — referencia completa de campos

El config vive por-workspace. Resolución: búsqueda recursiva desde el CWD hacia arriba hasta encontrar `neo.yaml`. **Zero-Hardcoding**: ningún puerto/IP/endpoint está hardcoded en código. Secretos en `.neo/.env` (gitignoreado) con referencias `${VAR_NAME}` en el yaml.

```yaml
# ─────────────────────────────────────────────────────────────────
# Server — puertos + modo
# ─────────────────────────────────────────────────────────────────
server:
  host: "127.0.0.1"              # SIEMPRE localhost. Remoto va por Nexus mTLS.
  sse_port: 9142                 # Puerto del worker MCP
  sse_message_path: "/mcp/message"
  dashboard_port: 8087           # HUD SPA (servida por Nexus)
  diagnostics_port: 9371         # pprof / profiling
  mode: "pair"                   # pair | fast | daemon
  tactical_port: 8081            # sandbox mTLS (solo si usas cmd/sandbox)
  tailscale: false               # habilita gossip P2P
  gossip_port: 4242
  gossip_peers: []               # IPs Tailscale de fleet nodes

# ─────────────────────────────────────────────────────────────────
# AI / Embedder
# ─────────────────────────────────────────────────────────────────
ai:
  provider: "ollama"             # ollama | wasm
  base_url: "${NEO_OLLAMA_URL}"  # ${VAR} expansion desde .neo/.env
  embed_base_url: "http://localhost:11435"   # dedicado embeddings
  embedding_model: "nomic-embed-text"
  context_window: 8192
  embed_timeout_seconds: 30

# ─────────────────────────────────────────────────────────────────
# RAG / HNSW
# ─────────────────────────────────────────────────────────────────
rag:
  db_path: ".neo/db/hnsw.db"
  chunk_size: 512
  overlap: 50
  batch_size: 32
  ingestion_workers: 4
  ollama_concurrency: 3          # cap simultaneous Embed() — evitar HTTP 500
  embed_concurrency: 2           # separate para search (BLAST_RADIUS, SEMANTIC_CODE)
  max_nodes_per_workspace: 50000
  workspace_capacity_warn_pct: 0.80
  drift_threshold: 0.45          # rolling-avg distance → EventCognitiveDrift
  gc_pressure_threshold: 5.0     # NumGC/file during ingestion
  arena_miss_rate_threshold: 0.20
  vector_quant: "float32"        # float32 | int8 | binary
  query_cache_capacity: 256      # LRU para SEMANTIC_CODE node IDs
  embedding_cache_capacity: 128  # LRU para []float32 vectors (skip 30 ms Ollama)

# ─────────────────────────────────────────────────────────────────
# CPG — Code Property Graph
# ─────────────────────────────────────────────────────────────────
cpg:
  page_rank_iters: 50
  page_rank_damping: 0.85
  activation_alpha: 0.5          # decay por hop en spreading activation
  max_heap_mb: 512               # OOM guard. Hot-reloadable (Épica 229.4)
  persist_path: ".neo/db/cpg.bin"          # fast-boot snapshot (Gob, PILAR XXXII)
  persist_interval_minutes: 15             # 0 = desactivado (solo flush en SIGTERM)

# ─────────────────────────────────────────────────────────────────
# SRE policies + thresholds
# ─────────────────────────────────────────────────────────────────
sre:
  strict_mode: true
  cusum_target: 0.5
  cusum_threshold: 4.0
  ring_buffer_size: 1024
  auto_vacuum_interval: "6h"
  gameday_baseline_rps: 500
  trusted_local_ports: [11434, 11435]   # bypass SSRF guard
  safe_commands: ["ls", "pwd", "go version", "git status"]
  unsupervised_max_cycles: 20
  kinetic_monitoring: true              # RAPL + GC pressure
  kinetic_threshold: 0.15               # 15% power deviation
  digital_twin_testing: false
  consensus_enabled: false              # multi-agent voting (3 personalities)
  consensus_quorum: 0.66
  context_compress_threshold_kb: 600
  oracle_alert_threshold: 0.75          # FailProb24h ≥ → alert
  oracle_heap_limit_mb: 512
  oracle_power_limit_w: 80
  certify_ttl_minutes: 0                # 0 = mode default (pair:15, fast/daemon:5)
  session_state_ttl_hours: 24
  cpu_affinity_enabled: false           # pinar goroutines HNSW a cores (Linux only)
  cpu_affinity_cores: []                # vacío → usa [0,1,2,3] cuando enabled
  hnsw_batch_enabled: false             # coalescer micro-batches 2ms
  hnsw_batch_window_ms: 2
  hnsw_batch_max_size: 32

# ─────────────────────────────────────────────────────────────────
# Inference router — 4 levels
# ─────────────────────────────────────────────────────────────────
inference:
  cloud_token_budget_daily: 50000
  cloud_model: "claude-sonnet-4-6"
  ollama_model: "qwen2.5-coder:7b"
  ollama_base_url: ""            # vacío = usa ai.base_url
  confidence_threshold: 0.7
  max_local_attempts: 3
  offline_mode: false            # true = nunca escala a CLOUD
  mode: "hybrid"                 # local | hybrid | cloud
  surrender_after: 3             # fallbacks antes de graceful surrender → debt file
  max_auto_fix_attempts: 0       # 0 = disabled, N = retry con inferencia fix

# ─────────────────────────────────────────────────────────────────
# Cognitive / Ouroboros
# ─────────────────────────────────────────────────────────────────
cognitive:
  strictness: 0.85
  arena_size: 1048576            # bytes — Arena PMEM hot-path pool
  xai_enabled: false             # Explanation engine — diagnostics
  auto_approve: false            # turbo mode — peligroso

# ─────────────────────────────────────────────────────────────────
# Workspace scanner
# ─────────────────────────────────────────────────────────────────
workspace:
  ignore_dirs: [".git", "node_modules", ".neo/db", "bin", "vendor"]
  allowed_extensions: [".go", ".md", ".yaml", ".yml"]
  max_file_size_mb: 10
  modules:                       # subdir → build command
    web: "npm run build"

# ─────────────────────────────────────────────────────────────────
# DBs externas (solo DB_SCHEMA)
# ─────────────────────────────────────────────────────────────────
databases:
  - name: "prod-pg"
    driver: "postgres"           # postgres (lib/pq) | pgx (pgx/v5/stdlib) | sqlite3
    dsn: "${PROD_PG_DSN}"        # desde .neo/.env
    max_open_conns: 3

# ─────────────────────────────────────────────────────────────────
# Hardware GPU adaptive
# ─────────────────────────────────────────────────────────────────
hardware:
  gpu_available: "auto"          # auto | true | false
  gpu_ollama_model: "qwen2.5-coder:32b"
  gpu_embed_concurrency: 8
  gpu_batch_size: 400

# ─────────────────────────────────────────────────────────────────
# Varios
# ─────────────────────────────────────────────────────────────────
governance:
  ghost_mode: false
  policy_path: ".neo/policies.yaml"

sentinel:
  heap_threshold_mb: 500
  goroutine_explosion_limit: 10000
  cold_start_grace_sec: 30
  audit_log_max_size: 1000
  dream_cycle_count: 3
  immunity_confidence_init: 0.6
  immunity_activation_min: 0.5

kinetic:
  spectral_bins: 8
  anomaly_threshold_sigma: 2.0
  gc_pause_threshold_us: 10000
  heap_critical_sigma: 5.0
  heap_warning_sigma: 3.0

coldstore:
  max_open_conns: 3
  max_idle_conns: 2
```

### Hot-reload (campos safe)

Los siguientes campos se recargan al editar `neo.yaml` sin restart (fsnotify dir-watch, robusto a `sed -i`):

- `inference.*`, `governance.*`, `sentinel.*`
- `cognitive.strictness`
- `sre.safe_commands`, `sre.unsupervised_max_cycles`, `sre.kinetic_monitoring`, `sre.kinetic_threshold`, `sre.digital_twin_testing`, `sre.consensus_enabled`, `sre.consensus_quorum`
- `rag.query_cache_capacity`, `rag.embedding_cache_capacity` — re-resize inmediato de los LRUs
- `cpg.max_heap_mb` — re-evaluado en cada call a `Graph()` (Épica 229.4)

Los unsafe (puertos, DB paths, certs, provider) requieren `make rebuild-restart`.

### `.neo/.env`

```bash
# .neo/.env (gitignoreado)
NEO_OLLAMA_URL=http://localhost:11434
PROD_PG_DSN=postgres://user:pass@localhost:5432/db?sslmode=disable
```

---

## `~/.neo/nexus.yaml` — dispatcher multi-workspace

Un único archivo global controla el dispatcher. Vive **fuera** del repo del proyecto.

```yaml
# ~/.neo/nexus.yaml

nexus:
  bind_addr: "127.0.0.1"
  dispatcher_port: 9000
  dashboard_port: 8087           # HUD SPA
  port_range_base: 9100          # hijos neo-mcp se asignan aquí
  port_range_size: 200           # → 9100..9299
  managed_workspaces: []         # vacío = todos los workspaces SSE

logs:
  mode: "file"                   # file = ~/.neo/logs/nexus-<id>.log
  rotate_mb: 100
  keep_files: 5

watchdog:
  check_interval_seconds: 3
  failure_threshold: 5           # consecutivos antes de restart
  max_restarts_per_hour: 10      # circuit breaker → Quarantined

boot:
  stdin_mode: "devnull"          # NUNCA "inherit" en producción
  startup_timeout_seconds: 30

api:
  auth_token: ""                 # si no vacío, requiere X-Nexus-Token header

services:                         # gestión automática de Ollama
  ollama_llm:
    enabled: true
    port: 11434
    models: ["qwen2.5-coder:7b"]
    # Variables inyectadas a cada child: OLLAMA_MAX_LOADED_MODELS, OLLAMA_NUM_PARALLEL, OLLAMA_FLASH_ATTENTION
  ollama_embed:
    enabled: true
    port: 11435
    models: ["nomic-embed-text"]
    # OLLAMA_EMBED_HOST se inyecta automáticamente en child.extra_env

oauth:                            # proxy .well-known/* → hijo activo (RFC 9728)
  enabled: true
```

---

## Hardware de referencia validado

### Entorno A — Apple Silicon (macOS) · 96 GB Unified Memory

> Referencia: workspace `strategos` — proyecto 80 k+ LOC, PostgreSQL, multi-lenguaje (Go/TS/Python).

| Subsistema | Campo `neo.yaml` | Valor recomendado | Notas |
|------------|-----------------|-------------------|-------|
| **HNSW** | `rag.max_nodes_per_workspace` | `500000 – 1000000` | 80k LOC genera ~200-300k nodos |
| | `rag.ingestion_workers` | `8` | Aprovecha todos los P-cores |
| | `rag.ollama_concurrency` | `8` | Embed pipeline saturado |
| | `rag.embed_concurrency` | `4` | |
| | `rag.batch_size` | `200` | Batches más grandes = menos round-trips |
| | `rag.query_cache_capacity` | `512` | Workspace grande → más patrones únicos |
| | `rag.embedding_cache_capacity` | `256` | |
| **CPG** | `cpg.max_heap_mb` | `8192` | SSA graph 80k LOC ocupa 4-8 GB |
| **OOM Guard** | `sentinel.heap_threshold_mb` | `16384` | HNSW 500k nodos ≈ 3-4 GB; margen amplio |
| | `sre.oracle_heap_limit_mb` | `16384` | Alineado con sentinel |
| **MCTS** | `cognitive.arena_size` | `500000` | Árbol de decisión más profundo |
| **Inference** | `inference.ollama_model` | `qwen2.5-coder:7b` | Coder-optimizado; cabe en VRAM |
| | `llm.max_tokens` | `8192` | Contexto completo para análisis |
| **WAL** | `storage.flush_interval_ms` | `25` | Flush más frecuente en WAL grande |
| **Embed** | `ai.embed_timeout_seconds` | `15` | Robustez con batches grandes |

**Nexus** para 96 GB:
- `OLLAMA_MAX_LOADED_MODELS: "8"` / `OLLAMA_NUM_PARALLEL: "8"` / `OLLAMA_FLASH_ATTENTION: "1"`
- `ollama_embed.OLLAMA_NUM_PARALLEL: "32"`
- `child.startup_timeout_seconds: 30`

### Entorno B — Windows · RTX 3090 (24 GB VRAM) · 32 GB RAM

| Subsistema | Campo | Valor | Notas |
|------------|-------|-------|-------|
| **Headroom** | `sentinel.heap_threshold_mb` | `16384` | Deja ~16 GB para OS + otros procesos |
| **CPG** | `cpg.max_heap_mb` | `8192` | SSA cabe dentro de los 32 GB |
| **Inference** | `inference.ollama_model` | `qwen2.5-coder:7b` | Q4_K_M ≈ 5.5 GB VRAM; cabe en 3090 |
| **Embed** | `ai.embed_base_url` | `http://127.0.0.1:11435` | Instancia dedicada evita contención con LLM |

En Windows, Nexus **no** gestiona Ollama (`services.ollama_llm/embed: enabled: false`). Levantar manualmente o via `bin/start-ollama.sh`.

### Regla de escalabilidad

```
HNSW nodes ≈ LOC × 0.003  (estimación con chunk_size=3000, overlap=500)

LOC      nodos estimados   max_nodes_per_workspace  sentinel.heap_threshold_mb
─────────────────────────────────────────────────────────────────────────────
 10 000        30 000           100 000                  2 048
 50 000       150 000           500 000                  8 192
 80 000+      240 000+        1 000 000                 16 384
```

---

## Posicionamiento en el ecosistema MCP

### Proyectos comparados

| Proyecto | Lenguaje | Categoría |
|---|---|---|
| **NeoAnvil** (este repo) | Go puro | Orquestador SRE monolítico |
| **cuba** ([LeandroPG19](https://github.com/LeandroPG19)) | Rust + Python | Suite de 4 micro-MCP especializados |
| **mcp-go** ([mark3labs](https://github.com/mark3labs/mcp-go)) | Go | SDK de referencia |
| **FastMCP** ([jlowin](https://github.com/jlowin/fastmcp)) | Python | Framework decorator-driven |
| **rmcp** ([modelcontextprotocol/rust-sdk](https://github.com/modelcontextprotocol/rust-sdk)) | Rust | SDK oficial |

### Cuadro comparativo

| Dimensión | NeoAnvil | cuba | mcp-go | FastMCP | rmcp |
|---|---|---|---|---|---|
| **Naturaleza** | Orquestador end-to-end | Suite de micro-MCP | SDK/librería | Framework | SDK oficial |
| **Multi-workspace** | Sí — Nexus :9000 + pool dinámico | No | N/A | Parcial | No |
| **Persistencia** | BoltDB × 3 + rollback atómico | PostgreSQL+pgvector (memorys) | Ninguna | Ninguna | Ninguna |
| **RAG / embeddings** | HNSW propio + cache stack + int8/binary quant | fastembed ONNX + pgvector | N/A | N/A | N/A |
| **Pipeline de mutaciones** | Obligatorio — AST + Bouncer + tests + seal TTL | No aplica | No aplica | No aplica | No aplica |
| **Observabilidad** | Web HUD multi-ws + TUI + token cost table | Métricas mínimas | Ninguna | Inspector dev | Tower middleware |
| **Seguridad** | SafeHTTPClient, Policy Engine, Phoenix Protocol | PyO3 sandbox + RLIMIT_AS | Caller | Caller | Caller |

### Matriz de decisión

| Si tu prioridad es… | Elige |
|---|---|
| Agente LLM disciplinado con memoria persistente y audit pipeline | **NeoAnvil** |
| Capacidades cognitivas especializadas componibles entre agentes | **cuba** |
| Construir un MCP Go desde cero con libertad total de arquitectura | **mcp-go** |
| Prototipo MCP en Python con DX excelente | **FastMCP** |
| MCP embebido en un tool host Rust con garantías memory-safe | **rmcp** |
| Pipeline ACID end-to-end sobre mutaciones de código | **NeoAnvil** (único) |

### Posicionamiento visual

```
         Pure SDK ────────────────────────── Monolito opinionado
            │         │              │                 │
          rmcp     mcp-go       FastMCP             NeoAnvil
                                   │
                          (+ mount/compose)
                                   │
                                 cuba
                      (suite de micro-MCP reutilizables)
```

- **Eje de opinión**: rmcp/mcp-go agnósticos. FastMCP opinión ligera. cuba por capacidad aislada. NeoAnvil opinión total sobre el ciclo.
- **Eje de reutilización**: cuba gana (cualquiera adopta `cuba-memorys` sin el resto). NeoAnvil no es reutilizable à la carte por diseño.
- **Eje de observabilidad**: NeoAnvil único con web HUD multi-workspace + TUI + cost table.

> **Caveat**: Las descripciones de mcp-go / FastMCP / rmcp vienen del corte de entrenamiento de enero 2026. Features pueden haber derivado. Las comparaciones con NeoAnvil y cuba sí están verificadas contra el estado actual en disco.

---

## Decisiones de diseño + racional

### Por qué Go (no Rust, no Python)

- **GC controlado**: zero-alloc en hot paths + sync.Pool + Arena PMEM.
- **Compilación rápida**: full rebuild < 30s en máquina modesta.
- **Go 1.26 auto-vectorizer con `GOAMD64=v3`**: emite AVX2/FMA en loops multi-acumulador → 44% speedup en CosineDistance.
- **NEON kernel hand-asm** (`pkg/rag/distance_arm64.s`): `cosineNEON` con VFMLA (3 acumuladores paralelos, 4 float32/ciclo) + reducción VEXT.
- **Stdlib fuerte**: `go/ast`, `go/parser`, `go/ssa` son de producción → CPG + AST_AUDIT sin deps externas.
- **Binary único** sin runtime dependency.

### Por qué MCP (no REST)

- Cliente MCP genérico — no hay integración por-cliente.
- Schema JSON con `enum` + `required` se valida cliente-side.
- SSE + JSON-RPC 2.0 soporta streaming para tool output grande.

### Por qué 14 tools (no 50, no 1)

- **Demasiadas tools** (23 previas): el LLM olvida qué elegir. Fatiga de decisión.
- **Una god-tool**: schema ambiguo, validación imposible.
- **Trade-off**: macros con enum `action`/`intent` agrupan operaciones relacionadas; specialists para dominios que no solapan.

### Por qué `bbolt` (no BadgerDB, no RocksDB)

- Fsync-por-transacción: ACID garantizado. Zero external deps. Lock exclusivo — un proceso por DB.
- Contra: no hay escrituras concurrentes. Para ese workload: SQLite OLAP en `pkg/coldstore`.

### Por qué CPG lazy

- Build SSA cuesta ~1.3s. Hacerlo on-boot bloquearía el health-check.
- Solución: builder arranca en goroutine al boot, cache BoltDB con invalidación por mtime.

### Por qué HNSW en RAM + persistencia lazy

- Search <200µs requiere vectores en memoria.
- 50k nodes × 768d × 4B = 150 MB. Aceptable para un workspace.
- WAL persiste cada `InsertBatch`. Graph reconstruye en 1-3s al boot.

### Por qué 3 caches distintos (no 1)

- `QueryCache` stores `[]uint32`. `TextCache` stores markdown bodies (expensive CPG PageRank recompute). `EmbeddingCache` stores `[]float32` (skip 30ms Ollama roundtrip). Cada uno tiene **costo de miss** distinto.

### Por qué NO hay workspace-switching en runtime

- Un proceso neo-mcp = un workspace fijo, pinned al boot. Simplifica todo: no hay races de state.
- Multi-workspace se logra con múltiples procesos + Nexus dispatcher.

---

## Estado actual + roadmap (histórico de PILARES)

### Historial de PILARES

| PILAR | Épicas | Tema |
|-------|--------|------|
| I–X | 1-36 | Fundación: 4 Macro-Tools, RAG, Shadow Compiler, SSRF, CLI, HUD, Arena PMEM |
| XI–XIV | 37-67 | Merkle P2P, SQLite OLAP, Sentinel Policy, Dreaming, Hypergraph, Kinetic SRE, Oracle |
| XV–XIX | 68-96 | Multi-Workspace Router, Reliability, Headless Architecture, Darwin Engine, Federated Dream |
| XX–XXIII | 97-165 | CPG (SSA), CodeRank, Incident Intelligence, BRIEFING Resilience, Tool Quality Debt |
| XXIV | 166 | PROJECT_DIGEST + GRAPH_WALK + sync summary |
| XXV | 167-228 | CPU Efficiency (GOAMD64=v3), portable SIMD, 3-layer cache stack, CC cleanup |
| XXVI | 229-241 | MCP OAuth, CPG hot-reload, tool consolidation 23→13, tech-debt harvest, int8 HNSW |
| XXVII–XXIX | 242-260 | Nexus OAuth proxy, CPG fast-boot persistence, workspace federation wiring |
| XXX–XXXIII | 261-282 | Cross-boundary CPG (Go↔TS), Project Federation MergeConfigs, Auth/Tenant, PKI scope |
| XXXIV–XXXVI | 283-288 | Fleet topology, project grouping HUD, SharedGraph wiring, Nexus topology |
| XXXVII | 287-288 | SharedGraph: project-level shared HNSW tier + REM sleep propagation |
| XXXVIII | 289-292 | Contract Symbiosis: CONTRACT_QUERY + Mock Server + Contract Drift Detection |
| XXXIX | 293-299 | Project Knowledge Base: cross-workspace store/fetch/list/drop/search |
| XL–XLIII | 299-301 | filter_symbol, FILE_EXTRACT AST body, Embedding 8k |
| XLIV–XLV | 302-306 | CC debt, boot race, HNSW Hyperscalar CPU (AVX2/AVX512/NEON runtime dispatch) |
| XLVII | 311-313 | Token Budget Observatory: per-intent tracking, session ceiling, top offenders |
| XLVIII–L | 314-316 | Shared.db coordinator · BRIEFING mode:delta · Contract Gap Detection |
| LI–LII | 317-318 | Audit/archive épicas (auto-kanban) · PILAR intro orphan cleanup |
| LIII | 319-320 | CC reduction radar_handlers.go — 12 hotspots → ≤12 via helper extraction |
| LIV | 321-323 | AppendTechDebt dedup · ctx.Done() en 7 goroutinas · Kanban orphan fix |
| LV | 324-325 | Coverage sprint: 8 paquetes de 10-28% → ≥30% |
| LVI | 326 | Coverage final: pkg/incidents 28.1% → 56.2% · audit-ci Makefile fix |
| LVII | 327-328 | int8 HNSW alloc-free search path + boot vector load + recall gate |
| LIX | 330.F | BLAST_RADIUS scatter restringido a member_workspaces (SSRF fix) |
| LX | 332-335 | Federation Coordination Base: KnowledgeStore coordinator, inbox, shared.db Nexus tier |
| LXI | 336-338 | Multi-Agent Presence & Identity: heartbeat table, SessionAgentID, inbox routing |
| LXII | 339-341 | Cross-Workspace Observability: token budget agg, INCIDENT_SEARCH RRF, HUD Federation Panel |
| LXIII | 342-344 | Conflict & Consistency: LWW conflict log + BRIEFING signal + manual resolve |
| LXIV–LXV | 345-350 | Cross-Workspace Workflow + Agent Workflow Automation |
| LXVI | 351-353 | Nexus-Level Debt System (neo_debt tool — 4-tier scope) |
| LXVII | 354-357 | Super-Federation / Org-Level Coordination — OrgConfig walk-up + OrgStore coordinator |
| LXVIII–LXIX | 358-370 | Resilience & Tooling Hardening · Low-Level Performance Tuning |
| **PILAR XXIII** | 123-127 | Subprocess MCP Plugin Architecture: Jira plugin 6 actions, Auth foundation, 7 skills |
| **PILAR XXIV** | 131.A-K | DeepSeek Fan-Out Engine: 5 tools, rate limit, structural cache, session router |
| **PILAR XXV** | 132.A-F | Daemon Mode V2: checkpoint, token-budget, TTL auto-renew, RAPL macOS compat |
| **PILAR XXVI Fase 1+2** | 135-136 | Brain Portable + Merge Sync: ChaCha20-Poly1305 streaming crypto, CRDT merge |
| **PILAR XXVI Fase 3 (parcial)** | 137.A.1-A.3+B.1 | Pixel mobile peer scaffolding (21 sub-épicas bloqueadas por Android tooling) |
| **Adversarial audit (2026-05-01)** | 5 SEV 9/8 closed | ChaCha20 nonce reuse, Lock TOCTOU, plugin thinking_type, keystore symlink, IPv4-mapped SSRF |

### Trabajo pendiente

`master_plan.md` tiene **21 sub-épicas abiertas**, todas dentro de PILAR XXVI Fase 3 (Pixel mobile peer):

- **137.A.4** smoke test en Android emulator
- **137.B.2-B.4** Compose UI + Android Keystore + power policy
- **137.C.1-C.4** Foreground Service + JNI bridge + BatteryManager + boot handler
- **137.D.1-D.3** JobScheduler con constraint chains
- **137.E.1-E.3** Tailscale (`tsnet`) en Android
- **137.F.1-F.4** Pixel runtime + Anthropic Custom Connectors
- **137.G.1-G.3** GitHub webhook handler dentro de `NeoMeshService`

Ninguno se puede ejecutar desde Linux pura — requieren Android Studio + emulador/Pixel + Tailscale. Detalle: [`docs/mobile-explained.md`](./mobile-explained.md).

**Audit follow-ups (PILAR XXVIII propuesto)**: 53 findings restantes del adversarial audit del 2026-05-01. Ver [`docs/codebase-audit-2026-05-01.md`](./codebase-audit-2026-05-01.md).

---

## Métricas y benchmarks completos

### Internal hot paths (p99, GOAMD64=v3)

| Operation | Latencia | Alloc |
|-----------|----------|-------|
| `memx.ObservablePool.Acquire/Release` | **0.98 ns/op** | 0 B/op |
| `rag.HNSW.Search` (top-16) | **~137 ns/op** | 64 B/op (pooled) |
| `rag.LexicalIndex.Search` (BM25) | **~137 ns/op** | 64 B/op (pooled) |
| `astx.Audit` (parse + walk) | **~1.1 ms/op** | ~0 (zero-garbage hot path) |
| `tensorx.CosineDistance` (768-d f32) | **557 ns/op** | 0 B/op |
| `tensorx.MatMulF32` | **0.02 ms/op** | ~4.6 KB/op |
| `rag.DotProductFloat32_768` | **234 ns** | 0 B |
| `rag.DotProductInt8_768` | **582 ns** | 0 B |
| `rag.HammingDistance_768` (binary) | **~3 ns** | 0 B |
| `rag.cosineNEON` (ARM64 NEON, 768-d f32) | **~185 ns** | 0 B |
| `sre.Policy.Evaluate` (6 rules) | **< 1 µs** | 0 B |
| `cache.QueryCache.Get` (hit) | **~54 ns** | 0 B |
| `cache.TextCache.Get` (hit) | **~33 ns** | 0 B |

### SIMD speedup — x86-64

| Benchmark | v1 (baseline) | v3 (AVX2/FMA) | Speedup |
|-----------|---------------|---------------|---------|
| `CosineDistance_768` | 989 ns/op | 557 ns/op | **1.78×** |
| `HNSW.Search` | ~180 ns/op | ~137 ns/op | **1.31×** |
| Full `pkg/rag` bench | — | — | **~28-44%** avg |

### SIMD speedup — ARM64

| Benchmark | scalar (auto-vec) | NEON hand-asm |
|-----------|-------------------|----------------|
| `CosineDispatch` (768-d) | — | **185 ns/op** |

### Int8 kernel dispatch table (368.A)

| Tier | Condición (`golang.org/x/sys/cpu`) | Kernel seleccionado |
|------|------------------------------------|---------------------|
| `v4` | x86-64 con AVX-512 + VNNI | `dotProductInt8AVX512VNNI` |
| `v3` | x86-64 con AVX2 + FMA | `dotProductInt8AVX2` |
| `v2` | x86-64 con SSE4.2 | `dotProductInt8SSE42` |
| `arm64-crypto` | ARM64 con SHA2 + AES + PMULL | `dotProductInt8ARM64Crypto` |
| `arm64-neon` | ARM64 base (NEON) | `dotProductInt8ARM64NEON` |
| `scalar` | fallback universal | `dotProductInt8Scalar` |

### Coverage actual (27 paquetes — todos ≥30%)

```
pkg/pubsub       100.0%   pkg/finops        94.4%   pkg/kanban        87.1%
pkg/wasm          84.2%   pkg/dba           82.5%   pkg/auth          68.8%
pkg/cpg           64.7%   pkg/workspace     58.8%   pkg/edgesync      56.3%
pkg/incidents     56.2%   pkg/nlp           56.2%   pkg/wms           53.9%
pkg/coldstore     51.4%   pkg/graph         50.9%   pkg/knowledge     50.3%
pkg/integrations  50.0%   pkg/phoenix       45.5%   pkg/darwin        41.3%
pkg/federation    41.0%   pkg/shadow        37.6%   pkg/state         35.7%
pkg/nexus         34.7%   pkg/sre           34.5%   pkg/consensus     34.3%
pkg/inference     31.6%   pkg/llm           31.0%   pkg/mes           30.7%
```

### Live system health

| Metric | Value |
|--------|-------|
| Heap idle (no CPG) | ~28 MB |
| Heap con CPG primed + cache warm | ~180-240 MB |
| GC pauses | < 1 ms (zero-alloc hot paths) |
| RAG capacity | 50 000 vectors/workspace (configurable) |
| CPG build (neo-mcp) | ~1.3 s → 2294 nodes / 4730 edges |
| INC corpus index (BM25) | 8 INCs en < 5 ms |
