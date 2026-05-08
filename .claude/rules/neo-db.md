---
paths:
  - "pkg/dba/**"
  - "pkg/rag/**"
  - "**/migrations/**"
---

# DOCTRINA DATABASE: ZERO-ALLOC & PUREZA ARQUITECTONICA

Aplica cuando se trabaja con codigo de base de datos, RAG WAL, o migraciones.

---

## [PROHIBIDO] ANTI-PATRONES SQL

- `SELECT *` en tablas industriales (>1M filas) — asfixia de heap por hidratacion innecesaria
- Mutaciones sin `WHERE` deterministico — riesgo de destruccion de consistencia
- Joins cuadruples en tiempo real — usar desnormalizacion o grafos RAG
- `http.Client` crudo para llamadas externas — usar `sre.SafeHTTPClient()`

## [FORZADO] PATRONES DE EXCELENCIA

- **Zero-Alloc Rows:** Usar `dba.Analyzer` para escanear resultados a buffers pre-alocados
- **ACID Transaccional:** Toda mutacion envuelta en bloque atomico
- **EXPLAIN antes de inyectar:** Validar indices con `EXPLAIN QUERY PLAN` antes de queries nuevas
- **Imports de serialization:** Usar `pkg/sre/allocs.go` (ZeroAllocJSONMarshal), NO `pkg/utils/`

## Drivers soportados (DB_SCHEMA)

- PostgreSQL: `driver: "postgres"` (lib/pq) o `driver: "pgx"` (pgx/v5/stdlib) — blank import en `cmd/neo-mcp/main.go`
- SQLite: driver por defecto
- Configurar alias en `neo.yaml → databases:` con campos `driver`, `dsn` (usar `${VAR}` expansion desde `.neo/.env`), `max_open_conns`
- Si el binario fue compilado sin el blank import del driver → recompilar (`make rebuild-restart`)

## BoltDB — WAL + buckets

- `hnsw.db` — vectores HNSW + WAL de InsertBatch (exclusivo, requiere `make rebuild-restart` para migraciones)
- `brain.db` — memex_buffer + directivas BoltDB (dual-layer con `.claude/rules/neo-synced-directives.md`)
- `planner.db` — task queue + session_state (purgado por `Vacuum_Memory` > 24h)
- `telemetry_heatmap.db` — mutation counters (por path → `certified` | `bypassed`)
- `coldstore.db` — SQLite OLAP archival (separado de BoltDB)

Al editar código BoltDB, `AST_AUDIT` es obligatorio — verifica cursor iteration (`b.Cursor()` debe guardarse en variable, no en closure) y ausencia de transaction leaks.

## Migraciones

- Apply via `neo_apply_migration` (single tool, no dispatcher). Schema-evolution with ACID guardrails.
- Int8 HNSW migration (Épica 170) NO requiere schema change — las companion arrays (`Int8Vectors`, `BinaryVectors`) son derivadas en memoria del float32 source of truth. `cmd/neo-migrate-quant` es un reporter offline, no muta disco.

## Ejemplo: Antes vs Despues

```go
// PROHIBIDO (Legacy)
rows, _ := db.Query("SELECT * FROM logs")
for rows.Next() {
    var log Log
    rows.Scan(&log.ID, &log.Msg, ...) // Heap alloc por fila
    results = append(results, log)
}

// FORZADO (SRE Zero-Alloc)
err := dbaEngine.ScanFast(ctx, "SELECT id, msg FROM logs WHERE severity='error'", func(row []any) {
    telemetry.ReportError(row[1].(string)) // Sin escape al heap
})
```
