# NeoAnvil Rules — Go

Plantilla de reglas específicas para proyectos Go orquestados por NeoAnvil V10.6.
Copiar a `.claude/rules/` del proyecto destino junto con las reglas universales.

---

## Certificación

- **OBLIGATORIO** tras editar `.go` → `neo_sre_certify_mutation`
- **Flujo Pair:** AST SSA-exact (McCabe E-N+2) → Bouncer → `go test -short` → Index
- **Flujo Fast:** AST → Index
- `O(1)_OPTIMIZATION` falla con nested loops o channels — usar `FEATURE_ADD` cuando hay control flow
- Pre-commit hook bloquea `.go` sin sello

## AST_AUDIT en Go

- CC calculado con **SSA-exact** cuando CPG activo (McCabe E-N+2 sobre bloques SSA)
- Falsos positivos AST-regex descartados automáticamente si SSA CC ≤ 15
- Findings incluyen `[cc_method:ssa_exact]` o `[cc_method:ast_regex]`
- Acepta globs: `AST_AUDIT pkg/**/*.go`
- **Obligatorio** antes de editar `pkg/state/`, `pkg/dba/`, cualquier código con transacciones BoltDB/SQLite

## COMPILE_AUDIT en Go

- Retorna `symbol_map` JSON con línea exacta de cada símbolo exportado (go/ast, excluye _test.go)
- Formato: `{"Type.Method": lineNo, "FuncName": lineNo}`
- Usar para `READ_SLICE start_line=symbol_map[target]` — lectura quirúrgica O(1)
- `include_unexported: true` para helpers privados

## WIRING_AUDIT

- Ejecutar tras cualquier `import` nuevo en `main.go` o nuevo `cmd/`
- Detecta paquetes importados pero no instanciados
- No esperar a certify para descubrir un import muerto

## Zero-Allocation (hot-paths)

- `sync.Pool` o `ObservablePool` — NUNCA `make()` dentro de bucles críticos
- Slices reciclados con `[:0]` + `defer Put`
- `bytes.Buffer`: adquirir de `bufPool`, `Reset()`, devolver tras uso
- `[]float32`: devolver al pool con `[:0]` post-InsertBatch
- Prohibido `any`/`interface{}` innecesarios — debilitan el compilador

## HTTP

- **Externo:** `sre.SafeHTTPClient()` — aplica SSRF guard completo con `trusted_local_ports`
- **Interno servidor→servidor:** `sre.SafeInternalHTTPClient(timeoutSec)` — solo loopback, bloquea IPs no-loopback
- PROHIBIDO `http.Client{}` crudo

## Seguridad Go

- Sockets Unix: `os.Chmod(0600)` post-Listen
- `//nolint:gosec` PROHIBIDO sin categoría + control compensatorio (ver `docs/general/gosec-audit-policy.md`)
- Sanitizar inputs antes de pasar a `exec.Command`: strip `"`, `&`, `;`, `$`, backticks

## SIMD Portable (sin CGO)

- NUNCA archivos `.s` (Go assembly) salvo justificación extraordinaria
- `GOAMD64=v3` (default): AVX2/FMA/BMI2 — Intel Haswell+ / AMD Zen1+
- `GOAMD64=v4` (opt-in): AVX-512 — `make build-fast`
- `GOARM64=v8.2` (default arm64): NEON+fma — Apple Silicon M1+
- Loops compiler-friendly: slices lineales `float32`/`int32`, sin branches en el inner loop
- `math.FMA(a, b, c)` explícito → `VFMADD231PS` / `FMLA`

## Comandos seguros

```
go test ./...          go test -short ./pkg/...
go build ./...         go vet ./...
go fmt ./...           make audit
make audit-ci          make rebuild-restart
git status / log / diff
```

## Estructura de módulos Go

- `cmd/<binary>/main.go` — entrypoint, minimal, solo wiring
- `pkg/<domain>/` — lógica de negocio
- Config: `pkg/config/config.go` — Zero-Hardcoding, backfill en `LoadConfig()`
- Secretos: `${VAR_NAME}` en `neo.yaml` → valor en `.neo/.env`
- `go.mod` / `go.sum` commiteados — nunca editar manualmente

## BoltDB / Transacciones

- `AST_AUDIT` obligatorio antes de editar código con cursores BoltDB
- Cursor iteration: `b.Cursor()` + `c.Next()` — anti-patrón común: `b.Cursor().Next()` en loop
- Sin transaction leaks: `defer tx.Rollback()` o `defer tx.Commit()`
- `SanitizeWAL()` se ejecuta automáticamente al arrancar — idempotente

## Commits

`feat(sre):`, `fix(auth):`, `refactor(db):`, `test(pkg):`, `chore:`, `docs:`
