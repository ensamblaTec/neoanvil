# LEYES DE CALIDAD SRE (NeoAnvil V10.6)

Directivas de código obligatorias para todo agente operando en este repositorio.
Estado actual: `make audit` reporta **0 staticcheck + 0 ineffassign + 0 modernize + 0 CC>15** en código productivo. Cada regresión bloquea merge vía `make audit-ci`.

---

## LEY 1: EXCELENCIA ALGORÍTMICA Y ZERO-ALLOCATION

- Priorizar algoritmos O(1) o O(log N) sobre iteraciones O(N) de fuerza bruta.
- Erradicar GC Thrashing: prohibido crear objetos dentro de bucles críticos o de telemetría.
- Reciclar memoria: slices con `[:0]`, `sync.Pool`, structs por valor (no puntero a heap).
- Prohibido `any`/`interface{}` innecesarios que debiliten el compilador.
- Manejar errores a nivel raíz, no silenciarlos con `_ =`.
- **Ciclomatic complexity cap: 15.** `AST_AUDIT` lo enforce con SSA-exact (McCabe E-N+2) cuando CPG está activo. Falsos positivos de regex (AST CC>15 pero SSA CC≤15) se descartan automáticamente.

## LEY 2: ZERO-HARDCODING

- PROHIBIDO quemar IPs, localhost, o puertos fijos en el código.
- Todo enlace a BD, puertos, o endpoints debe venir de `neo.yaml` (per-workspace) o variables de entorno.
- Nexus usa `~/.neo/nexus.yaml` — NO mezclar con `neo.yaml` por-workspace.
- Resolución de config: búsqueda recursiva por árbol de directorios.
- Secretos en `.neo/.env` (gitignoreado) + referencias `${VAR_NAME}` en `neo.yaml`.
- Un binario agnóstico de su CWD sobrevive en cualquier contenedor.

## LEY 3: AISLAMIENTO I/O Y CLAUSURA MCP

- PROHIBIDO `fmt.Println`/`fmt.Printf` en código MCP — mata el canal JSON-RPC (aunque con arquitectura headless post-Épica 85 el riesgo es menor, sigue siendo buena práctica).
- Usar `log.Printf` o buffers de log explícitos.
- PROHIBIDO herramientas destructivas directas (`cat >>`, `sed` en producción) sobre archivos del orquestador.
- Mutaciones van por `neo_sre_certify_mutation`.

## LEY 4: MODOS DE OPERACIÓN

| Modo | Edición | Certificación | `neo_daemon` | Frontend build | TTL seal |
|------|---------|---------------|--------------|----------------|----------|
| **pair** | Nativa | AST + Bouncer + Tests | PROHIBIDO | Bypass | 15 min |
| **fast** | Nativa | Solo AST + Index | PROHIBIDO | Bypass | 5 min |
| **daemon** | Via `neo_daemon` | Completa | Habilitado | Síncrono | 5 min |

Override TTL via `sre.certify_ttl_minutes` en `neo.yaml`.

## LEY 5: SEGURIDAD

- HTTP clients siempre vía `sre.SafeHTTPClient()` (anti-SSRF) — para URLs externas.
- Para tráfico Nexus→hijos: `sre.SafeInternalHTTPClient(timeoutSec)` — permite SOLO loopback, bloquea cualquier IP no-loopback.
- Sockets Unix con `os.Chmod(0600)` post-Listen.
- Phoenix Protocol requiere `SRE_PHOENIX_ARMED=true` para activarse.
- Sanitizar inputs antes de pasar a shell (strip `"`, `&`, `;`, `$`, `` ` ``).
- Dashboard HUD restringido a `127.0.0.1` (nunca exponer en red).
- **`//nolint:gosec` PROHIBIDO sin categoría.** Cada supresión debe tener `//nolint:gosec // G304-WORKSPACE-CANON: <control compensatorio>`. Categorías válidas en `.claude/rules/neoanvilsec-audit.md`. Estado actual: 0 bare supresiones repo-wide.

## LEY 6: CERTIFICACIÓN OBLIGATORIA

- Todo archivo `.go/.ts/.tsx/.js/.jsx/.css` editado DEBE pasar por `neo_sre_certify_mutation`.
- El pre-commit hook bloquea commits sin sello de certificación.
- **TTL de sellos:** 15 min en pair mode, 5 min en daemon/fast — certificar justo antes del commit.
- Las directivas se persisten en dual-layer: BoltDB (`neo_memory action:learn`) + `.claude/rules/neo-synced-directives.md`.
- **TRAMPA CERTIFY:** `O(1)_OPTIMIZATION` falla si el archivo tiene nested loops (aunque sean pipeline/channel). Usar `FEATURE_ADD` para cualquier feature con control flow.
- **Bypass de emergencia:** `NEO_CERTIFY_BYPASS=1 git commit` se registra en heatmap como `bypassed` ⚠️ — revisar luego.

## LEY 7: ARENA PMEM (HOT-PATHS ZERO-GC)

- Hot-paths RAG/embedding deben usar `sync.Pool` o `ObservablePool` — nunca `make()` dentro de bucles críticos.
- `bytes.Buffer` en Embed: adquirir de `bufPool`, `Reset()`, devolver tras `client.Do()`.
- `[]float32` en ingesta: devolver al `vecPool` con `[:0]` post-InsertBatch (la copia ya está en graph.Vectors).
- `ObservablePool.MissRate() > 0.20` → pool subdimensionado → emitir `EventArenaThresh`.
- GC Pressure: `runtime.ReadMemStats` antes/después de WalkDir; `gcPerFile > 5` → `EventGCPressure`.
- `SearchState` en HNSW: `clear(visited)`, `results[:0]`, `defer Put` — patrón correcto, no romper.

## LEY 8: NEXUS — CHILDREN Y DISPATCHER

- Los hijos neo-mcp DEBEN exponer `GET /health` en su mux (requerido para `verifyBoot` → `StatusRunning`).
- `managed_workspaces` en `~/.neo/nexus.yaml` — lista de IDs/nombres SSE; vacío = todos.
- Hijos corren con `stdin_mode: "devnull"` — NUNCA `"inherit"` en producción (causa hang bajo cliente MCP con stdin-pipe).
- Hijos bajo Nexus: no intentar guardar el workspace registry global (solo Nexus escribe `workspaces.json`).
- **SIGTERM gracioso** (Épica 229.1): `make rebuild-restart` envía SIGTERM primero, espera 5s, escala a SIGKILL solo si necesario — permite flush de cache snapshots.
- **OAuth proxy strip-prefix** (Épica 229.3): Nexus strip `/workspaces/<id>` antes de `proxyTo()` → el child recibe rutas OAuth en su root (`.well-known/oauth-authorization-server`, RFC 9728 `.well-known/oauth-protected-resource`).

## LEY 9: MCP TOOL SCHEMA — JSON SCHEMA ESTRICTO

- Todo `InputSchema()` DEBE setear `Required: []string{...}` o `Required: []string{}` — **NUNCA** dejarlo `nil`. Un slice nil serializa como `"required": null`, inválido por JSON Schema spec.
- Síntoma de violación: Claude Code muestra `MCP dialog dismissed` tras `/mcp` pese a que el backend funciona.
- Causa: el MCP SDK valida cada tool contra JSON Schema — si `required` no es array (o está ausente), descarta silenciosamente toda la conexión tras el handshake.
- Diagnóstico rápido: simular el flow con curl y validar que ninguna tool tenga `required=null`:
  ```bash
  curl -sN http://127.0.0.1:9000/workspaces/<id>/mcp/sse | head -5
  # Luego POST al endpoint devuelto, parsear tools/list, validar required de cada tool
  ```
- Prevención defensiva: `omitempty` en `MCPToolSchema.Required []string json:"required,omitempty"` (registry.go) para que nil slice omita el campo.
- Aplica a todos los tools. Si agregas uno nuevo, set `Required` explícito en `InputSchema()`.

## LEY 10: SIMD PORTABLE — GOAMD64 / GOARM64 SIN CGO

- **Nunca escribir archivos `.s` (Go assembly) salvo justificación extraordinaria** — cada archivo `.s` es por arquitectura (amd64, arm64, riscv64) × por nivel de feature, y la matriz de mantenimiento explota.
- Usar **compile-time flags** para activar SIMD auto-vectorizado:
  - `GOAMD64=v3` — Intel Haswell+ (2013) / AMD Zen1+ (2017): AVX2/FMA/BMI2. Default del `Makefile`.
  - `GOAMD64=v4` — Intel Skylake-X+ / AMD Zen4+: AVX-512. Opt-in vía `make build-fast`.
  - `GOARM64=v8.2` — Apple Silicon M1-M4 y Graviton3+: NEON+fma+crypto. Default arm64.
  - `GOARM64=v9.0` — SVE2. Opt-in.
- Fallback portable: `make build-generic` compila `GOAMD64=v1` / `GOARM64=v8.0` sin SIMD auto-vec — corre en cualquier CPU x86-64 / ARMv8.
- **Escribir loops compiler-friendly** (Ley 1 + esto):
  - Slices lineales de `float32`/`int32`/`byte` — el compilador los vectoriza.
  - Sin branches en el cuerpo del loop (mover fuera o usar bitmask).
  - Hints de bounds: `v[:len]` en vez de `v` reduce bounds-check en el inner loop.
  - `math.FMA(a, b, c)` explícito donde aplique — genera una sola instrucción `VFMADD231PS` / `FMLA`.
  - `math/bits.OnesCount64` → `POPCNT` / `CNT.8H`; `bits.LeadingZeros64` → `LZCNT` / `CLZ`.
  - Estructuras `struct-of-arrays` ganan a `array-of-structs` en hot loops.
- **Verificar portabilidad con el Makefile:**
  - `make archinfo` — imprime OS/ARCH detectados + flag seleccionado.
  - `make cpufeat` — imprime flags del CPU (avx2/avx512/neon), recomienda GOAMD64 óptimo.
  - `make bench-compare` — corre `pkg/rag` benchmarks a v1 vs v3, cuantifica el speedup real en tu hardware.
  - `make build-all` — cross-compila la matriz (linux+darwin × amd64+arm64).
- **Regla de oro:** un binario compilado con `v3` corre en **cualquier** CPU que soporte AVX2, sin importar vendor ni generación.

## LEY 11: HOT-RELOAD SAFE LIST

Los siguientes campos de `neo.yaml` se recargan automáticamente vía fsnotify (dir-watch, robusto a `sed -i`) sin restart:

- `inference.*`, `governance.*`, `sentinel.*`
- `cognitive.strictness`
- `sre.safe_commands`, `sre.unsupervised_max_cycles`, `sre.kinetic_monitoring`, `sre.kinetic_threshold`, `sre.digital_twin_testing`, `sre.consensus_enabled`, `sre.consensus_quorum`
- `rag.query_cache_capacity`, `rag.embedding_cache_capacity` → `Resize()` inmediato
- `cpg.max_heap_mb` → re-evaluado en cada `Graph()` call (Épica 229.4)

**Unsafe** (requieren `make rebuild-restart`):
- `server.*` (puertos), `ai.provider`, DB paths, certs, `rag.vector_quant` (estructura HNSW cambia).

## LEY 12: TEST COVERAGE + AUDIT CI

- `make audit` corre en <3s: staticcheck + ineffassign + modernize + coverage por paquete.
- `make audit-ci` falla si aparece cualquier NEW finding vs `.neo/audit-baseline.txt`. Usar como CI gate.
- Regenerar baseline solo tras cerrar una PR limpia: `make audit-baseline`.
- Estado actual: 25/25 paquetes pass, coverage promedio ~40%, 0 findings en los 3 linters.
- Paquetes con 0% coverage están prohibidos: cada paquete productivo debe tener al menos un `_test.go`.

## LEY 13: CPG + INCIDENT INTELLIGENCE

- `cpg.max_heap_mb` default 512. Hot-reloadable — raise inmediato re-habilita serving sin rebuild. El graph se preserva en memoria cuando el guard tripa.
- CPG build es lazy: primera llamada a `PROJECT_DIGEST`/`GRAPH_WALK` espera hasta 200ms; si no listo, degrada a heatmap-only.
- CPG limitations documentadas: receiver methods retornan "No reachable nodes" en GRAPH_WALK (SSA no emite call edges desde métodos con receiver). Workaround: BLAST_RADIUS.
- INC corpus en `.neo/incidents/INC-*.md`. Para PATTERN_AUDIT, el INC debe tener header `**Affected Services:**` (post-Épica 153).
- `INCIDENT_SEARCH` tri-tier: BM25 (Ollama-free) → HNSW → text_search. Opcional `force_tier` para exercise específico.
