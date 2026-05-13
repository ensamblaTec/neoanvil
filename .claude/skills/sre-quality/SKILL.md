---
name: sre-quality
description: Leyes de calidad de código para neoanvil — Zero-Allocation, Aislamiento MCP, Safe HTTP, certify TTL, AST audit policy, gosec annotations, deadcode triage, MCP Tool Schema requirements. Path-scoped auto-load cuando editas pkg/**/*.go o cmd/**/*.go; también invocable explícitamente con `/sre-quality`.
paths:
  - "pkg/**/*.go"
  - "cmd/**/*.go"
---

# Leyes de Calidad SRE

> Constraints duros aplicados a TODO el código de neoanvil.
> Migrado de `docs/general/code-quality-laws.md` (15 leyes — archivado 2026-05-13) +
> directives extracted del `neo-synced-directives.md`.

---

## LEY 1 — Excelencia algorítmica + Zero-Allocation

- O(1) o O(log N) > O(N) brute force
- PROHIBIDO crear objetos dentro de bucles críticos / telemetría
- Reciclar memoria: `[:0]`, `sync.Pool`, struct por valor (no puntero a heap)
- PROHIBIDO `any`/`interface{}` innecesarios (debilitan compilador)
- Errores en root, NO silenciar con `_ =`
- **CC cap = 15** — `AST_AUDIT` enforce con SSA-exact (McCabe E-N+2)

## LEY 2 — Zero-Hardcoding

- PROHIBIDO IPs, localhost, puertos fijos en código
- Todo viene de `neo.yaml` (per-workspace) o `nexus.yaml` (dispatcher)
- Resolución recursiva por árbol de directorios
- Secretos en `.neo/.env` (gitignored) + referencias `${VAR_NAME}` en yaml
- Un binario agnóstico de su CWD sobrevive en cualquier contenedor

## LEY 3 — Aislamiento I/O y Clausura MCP

- PROHIBIDO `fmt.Println`/`fmt.Printf` en código MCP — destruye JSON-RPC
- Usar `log.Printf` o buffers de log explícitos
- PROHIBIDO `cat >>`, `sed` en producción sobre archivos del orquestador
- Mutaciones van por `neo_sre_certify_mutation`

## LEY 4 — Modos de operación

| Modo | Edición | Cert | neo_daemon | Frontend | TTL seal |
|------|---------|------|------------|----------|----------|
| pair | Nativa | AST + Bouncer + Tests | PROHIBIDO | Bypass | 15 min |
| fast | Nativa | Solo AST + Index | PROHIBIDO | Bypass | 5 min |
| daemon | Vía neo_daemon | Completa | Habilitado | Sync | 5 min |

Override: `sre.certify_ttl_minutes` en neo.yaml.

## LEY 5 — Seguridad

- HTTP clients SIEMPRE vía `sre.SafeHTTPClient()` (anti-SSRF) para URLs externas
- Tráfico Nexus→hijos: `sre.SafeInternalHTTPClient(timeoutSec)` — solo loopback
- Sockets Unix con `os.Chmod(0600)` post-Listen
- Phoenix Protocol requiere `SRE_PHOENIX_ARMED=true`
- Sanitizar inputs antes de shell (strip `"`, `&`, `;`, `$`, `` ` ``)
- Dashboard HUD restringido a 127.0.0.1
- **`//nolint:gosec` PROHIBIDO sin categoría.** Cada supresión:
  `//nolint:gosec // G304-WORKSPACE-CANON: <control compensatorio>`
  Categorías válidas en [`docs/general/gosec-audit-policy.md`](../../../docs/general/gosec-audit-policy.md)

## LEY 6 — Certificación obligatoria

Todo `.go/.ts/.tsx/.js/.jsx/.css` editado DEBE pasar por
`neo_sre_certify_mutation`. Pre-commit hook bloquea sin sello.

- TTL: 15 min pair / 5 min fast/daemon
- TRAMPA `O(1)_OPTIMIZATION`: falla con nested loops aunque sean
  pipeline/channel. Usar `FEATURE_ADD` para feature con control flow
- Bypass de emergencia: `NEO_CERTIFY_BYPASS=1 git commit` — registrado
  en heatmap como ⚠️, revisar luego

### Para batches grandes
- Certificar TODOS los archivos en UNA llamada
- INMEDIATAMENTE antes del `git commit` (TTL agota rápido)
- Si pre-commit rechaza por TTL: re-certify y commit en la misma secuencia

### Excepción BUG_FIX (shadow-rename, CC-only-extraction)
OMITIR BLAST_RADIUS previo. Aplica solo cuando:
- No hay cambio de firma pública
- Solo renombrado de variable interna o extracción a helper privado
- NO si afecta flujo compartido (green path certify, boot, función con
  múltiples callers)

## LEY 7 — Arena PMEM (Hot-Paths)

- RAG/embedding `sync.Pool` o `ObservablePool` — NO `make()` en bucles
- `bytes.Buffer` en Embed: adquirir de `bufPool`, `Reset()`, devolver
- `[]float32` ingesta: devolver al `vecPool` con `[:0]` post-InsertBatch
- `ObservablePool.MissRate() > 0.20` → emite `EventArenaThresh`
- GC Pressure: `gcPerFile > 5` → `EventGCPressure`
- HNSW SearchState: `clear(visited)`, `results[:0]`, `defer Put`

## LEY 8 — Nexus children y dispatcher

- Hijos neo-mcp DEBEN exponer `GET /health` (sino `verifyBoot` falla)
- `managed_workspaces` en nexus.yaml — vacío = todos
- `stdin_mode: "devnull"` SIEMPRE (NUNCA `inherit` en producción —
  causa hang bajo cliente MCP con stdin-pipe)
- Hijos NO escriben workspaces.json (solo Nexus)
- SIGTERM gracioso 5s → SIGKILL (Épica 229.1)
- OAuth proxy strip-prefix `/workspaces/<id>` (Épica 229.3)

## LEY 9 — MCP Tool Schema strict

- Todo `InputSchema()` DEBE setear `Required: []string{...}` o `[]string{}`
- **NUNCA dejar `Required: nil`** (serializa como `"required": null`,
  inválido por JSON Schema spec)
- Síntoma de violación: Claude Code muestra `MCP dialog dismissed`
  pese a backend funcional
- Causa: el MCP SDK descarta silenciosamente toda la conexión
- `omitempty` en `MCPToolSchema.Required []string json:"required,omitempty"`
  para que nil omita el campo

## LEY 10 — SIMD portable (GOAMD64/GOARM64)

- NUNCA escribir `.s` (Go assembly) salvo justificación extraordinaria
- Activar SIMD via compile-time flags:
  - `GOAMD64=v3` Haswell+ / Zen1+ AVX2/FMA/BMI2 (default Makefile)
  - `GOAMD64=v4` Skylake-X+ / Zen4+ AVX-512 (`make build-fast`)
  - `GOARM64=v8.2` M1-M4 / Graviton3+ NEON+fma+crypto (default arm64)
- Loops compiler-friendly: slices lineales, sin branches en inner,
  `v[:len]` para bounds-check, `math.FMA(a,b,c)` explícito
- Verificar con `make archinfo`, `make cpufeat`, `make bench-compare`

## LEY 11 — Hot-reload safe list

Hot-reload via fsnotify sin restart:
- `inference.*`, `governance.*`, `sentinel.*`
- `cognitive.strictness`
- `sre.safe_commands`, `sre.unsupervised_max_cycles`, `sre.kinetic_*`,
  `sre.digital_twin_testing`, `sre.consensus_*`
- `rag.query_cache_capacity`, `rag.embedding_cache_capacity`
- `cpg.max_heap_mb`

**Unsafe** (requieren `make rebuild-restart`):
- `server.*` (puertos), `ai.provider`, DB paths, certs,
  `rag.vector_quant`

## LEY 12 — Test coverage + audit CI

- `make audit` <3s: staticcheck + ineffassign + modernize + coverage
- `make audit-ci` falla en NEW finding vs `.neo/audit-baseline.txt`
- Regenerar baseline solo tras PR limpio: `make audit-baseline`
- Estado: 25/25 paquetes pass, 0 findings en linters
- Paquetes con 0% coverage PROHIBIDOS — cada productivo debe tener `_test.go`

## LEY 13 — CPG + Incident Intelligence

- `cpg.max_heap_mb` default 512. Hot-reloadable
- CPG build lazy: primera llamada `PROJECT_DIGEST`/`GRAPH_WALK` espera
  hasta 200ms; si no listo → degrade a heatmap-only
- GRAPH_WALK limitations: receiver methods retornan "No reachable nodes"
  (SSA no emite call edges desde métodos con receiver). Workaround:
  `BLAST_RADIUS`
- INC corpus en `.neo/incidents/INC-*.md`. PATTERN_AUDIT requiere
  header `**Affected Services:**` (post-Épica 153.C)
- INCIDENT_SEARCH tri-tier: BM25 → HNSW → text_search

## LEY 14 — Deadcode policy (Épica 235)

- `deadcode ./...` NO confiable en este repo (multi-entrypoint)
- Usar exclusivamente `staticcheck -checks U1000 ./...`
- `make audit` ya lo enforce
- Estado actual: 0 findings repo-wide

## LEY 15 — DB Zero-Alloc

- PROHIBIDO `SELECT *` en tablas >1M filas
- PROHIBIDO mutaciones sin WHERE determinístico
- PROHIBIDO joins cuádruples en tiempo real
- FORZADO: `dba.Analyzer` con buffers pre-alocados, ACID transaccional,
  `EXPLAIN QUERY PLAN` antes de queries nuevas
- Serialización via `pkg/sre/allocs.go (ZeroAllocJSONMarshal)`, NO `pkg/utils/`
- DB_SCHEMA soporta PostgreSQL (`postgres` via lib/pq, `pgx` via pgx/v5/stdlib),
  SQLite, cualquier driver registrado como blank import en main.go

---

## See also

- [`sre-doctrine`](../sre-doctrine/SKILL.md) — flujo operativo
- [`sre-tools`](../sre-tools/SKILL.md) — referencia tools
- [`docs/general/gosec-audit-policy.md`](../../../docs/general/gosec-audit-policy.md) — categorías gosec válidas
- [`docs/general/deadcode-policy.md`](../../../docs/general/deadcode-policy.md) — política deadcode
- Skill `/sre-db` ([`../sre-db/SKILL.md`](../sre-db/SKILL.md)) — doctrina DB scoped (paths-scoped auto-load)
