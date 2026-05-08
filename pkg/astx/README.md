# pkg/astx — Multi-language AST Auditor

Static analysis engine que exponen `neo_radar(intent: "AST_AUDIT")` a todo el codebase. Detecta CC>15, bucles infinitos y shadow variables en múltiples lenguajes.

---

## Analyzers soportados

| Extensión | Analyzer | Backing | Precisión | Cobertura |
|-----------|----------|---------|-----------|-----------|
| `.go` | `GoAnalyzer` | `go/ast` + `go/ssa` (cuando CPG disponible) | **SSA-exact** (McCabe E-N+2) | 85%+ |
| `.py` | `PythonAnalyzer` | Regex keyword counting | Approximate | 95%+ |
| `.ts`, `.tsx`, `.js`, `.jsx` | `TSAnalyzer` | Regex + brace-depth tracking | Approximate | 90%+ |
| `.rs` | `RustAnalyzer` | Regex keyword counting | Approximate | 90%+ |

Los analyzers están registrados automáticamente en `DefaultRouter`. Nuevos lenguajes se añaden con `DefaultRouter.Register(".ext", FooAnalyzer{})`.

---

## Contrato

```go
type LanguageAnalyzer interface {
    Analyze(ctx context.Context, path string, src []byte) ([]AuditFinding, error)
}

type AuditFinding struct {
    File    string  // absolute path
    Line    int     // 1-based
    Kind    string  // "COMPLEXITY" | "SHADOW" | "SHADOW_INFO" | "INFINITE_LOOP"
    Message string  // human-readable with function name + metric
}
```

Cada analyzer DEBE:

- Ser **thread-safe** (se pueden invocar múltiples en paralelo).
- Manejar archivos grandes via scanner buffering (no ReadAll).
- No panic ante input malformado — error explícito o findings vacíos.
- Respetar `ctx.Deadline()` en loops largos.

---

## Cómo añadir un nuevo lenguaje

1. Crear `pkg/astx/foo_analyzer.go`:

```go
package astx

import "context"

type FooAnalyzer struct{}

func (FooAnalyzer) Analyze(_ context.Context, path string, src []byte) ([]AuditFinding, error) {
    var findings []AuditFinding
    // parse src, detect issues, append to findings
    return findings, nil
}
```

2. Registrar en `analyzer.go.newDefaultRouter()`:

```go
r.m[".foo"] = FooAnalyzer{}
```

3. Crear `pkg/astx/foo_analyzer_test.go` con al menos:
   - `TestFooAnalyzer_CleanLowCC` — fn simple, 0 findings esperados
   - `TestFooAnalyzer_ComplexCC16` — fn con CC>15 → 1 COMPLEXITY
   - `TestFooAnalyzer_Shadow...` — si el lenguaje tiene shadow semantics
   - Test para cualquier edge case específico (comments, docstrings, etc.)

4. Verificar coverage: `go test -short -cover ./pkg/astx/` — el analyzer nuevo debe estar >80%.

---

## Known limitations

### TSAnalyzer — phantom frames en control-flow

El regex `tsFuncRe` matchea la forma `<identifier>(<args>) {` para detectar funciones, métodos y arrow expressions. Esto también matchea construcciones de control-flow como `if (...) { }`, `for (...) { }`, `while (...) { }`, `switch (...) { }`, creando frames fantasma.

**Consecuencia:** las variables declaradas dentro de un `if`/`for`/`while` quedan aisladas del frame de la función enclosing. Shadow detection no dispara para el patrón común:

```typescript
function compute(data: any) {
    const cfg = loadConfig();
    if (data.override) {
        const cfg = data.override;  // no se detecta como shadow
    }
}
```

**Workarounds:**
- Shadow detection SÍ funciona en bloques desnudos `{ ... }`.
- CC detection funciona con operadores (`&&`, `||`, `?:`) a nivel de función.
- Arreglar requiere parser TypeScript real (tree-sitter o swc).

Documentado en tests:
- `TestTSAnalyzer_ShadowLetInBareBlock` (path que funciona)
- `TestTSAnalyzer_ControlFlowShadowNotDetected` (locks current behaviour)

### PythonAnalyzer — overestimate en strings

El regex `pyCCRe` cuenta keywords `if/elif/for/while/except/with/and/or/lambda` sin distinguir strings literals. Una función con `msg = "if this and that"` añade 2 al CC aunque sea solo un string.

**Mitigación:** `pyCommentRe` strip-ea comentarios Python (`#...`). Docstrings `"""..."""` no se strip-ean aún — edge case.

### RustAnalyzer — match arms contadas via `=>`

Cada arm de un `match` genera un `=>` que cuenta como +1 CC. Esto es fiel al espíritu de CC (cada arm es un path), pero closures `|x| => ...` también disparan +1, ligeramente sobre-estimando.

---

## Testing

```bash
# Todo el paquete
go test -short ./pkg/astx/

# Cobertura detallada
go test -short -cover -coverprofile=/tmp/astx.out ./pkg/astx/
go tool cover -func=/tmp/astx.out

# Solo un analyzer
go test -short -run TestPythonAnalyzer ./pkg/astx/
go test -short -run TestRustAnalyzer  ./pkg/astx/
go test -short -run TestTSAnalyzer    ./pkg/astx/
```

Tests por analyzer cubren: CC detection, shadow detection (donde aplique), comments stripping, edge cases (empty functions, async, self/cls, underscore, etc.).

---

## Integración con el resto del sistema

- `neo_radar(intent: "AST_AUDIT", target: "file.py")` dispatcha via `DefaultRouter`.
- `AST_AUDIT` acepta archivo individual, directorio (`pkg/rag/`) o glob (`pkg/**/*.go`).
- Cuando el CPG Manager está disponible (`astx.SetCPGManager(mgr)`), `GoAnalyzer` prefiere SSA-exact CC sobre regex.
- `make audit` NO corre `AST_AUDIT` directamente (requiere MCP live) — usa `staticcheck` como proxy.
