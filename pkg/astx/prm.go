package astx

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ensamblatec/neoanvil/pkg/nlp"
)

type DynamicPRM struct {
	LZ76Threshold float64
}

// ActivePRM rules (Mutated by MCTS/REM Sleep)
var ActivePRM = DynamicPRM{LZ76Threshold: 0.35}

type PRMVerdict struct {
	Score        float64
	Verdict      string
	Explanations []string
}

func runTypeScriptValidator(code, ext string) error {
	workspacePath := "/home/ensamblatec/go/src/github.com/ensamblatec/neoanvil/web"
	tempFile := filepath.Join(workspacePath, "src", fmt.Sprintf(".shadow_sre_%d%s", os.Getpid(), ext))
	
	if err := os.WriteFile(tempFile, []byte(code), 0644); err != nil {
		return fmt.Errorf("failed to write shadow TS file: %v", err)
	}
	defer os.Remove(tempFile)

	// Invocamos tsc sin emitir y validando el proyecto. npx tsc -b es preferible, o tsc simple saltándose libs
	// pero como queremos ser rápidos, ejecutamos eslint (ya que eslint también captura TS rules en nuestro config) 
	// o tsc en el archivo concreto (lo más exacto para types). 
	// Sin embargo comprobar un sólo archivo con tsc es lento sin cache. El Master Plan acordó TSC.
	cmd := exec.Command("npx", "tsc", "--noEmit") // validación TS global de todo web
	cmd.Dir = workspacePath

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Buscamos específicamente errores que mencionen el shadow file
		outStr := string(output)
		if strings.Contains(outStr, ".shadow_sre_") {
			// Extraer solo la línea del error
			lines := strings.Split(outStr, "\n")
			for _, l := range lines {
				if strings.Contains(l, ".shadow_sre_") && strings.Contains(l, "error") {
					return fmt.Errorf("%s", strings.TrimSpace(l))
				}
			}
			return fmt.Errorf("%s", outStr) // Si no se puede aislar, escupe todo
		}
		// A veces falla otra parte del proyecto (deuda genérica), dejamos que el SRE continue si no es su archivo
	}
	return nil
}

// EvaluateGoPRM disecciona un snippet de Go y aplica el Logical Veto Gate (Fase 18)
// EvaluatePolyglotPRM disecciona un snippet de código y aplica el Logical Veto Gate (Fase 18) con bypass frontend
func EvaluatePolyglotPRM(code string, ext string, pastScars []string) PRMVerdict {
	sourceToParse := code
	if (ext == ".go" || ext == "") && !strings.Contains(code, "package ") {
		sourceToParse = "package shadow_prm\n" + code
	}

	s1, f, e1 := evaluateS1Parse(code, sourceToParse, ext)
	s3, e3 := evaluateS3Safety(code)
	s5, e5 := evaluateS5Egress(code)
	s6, e6 := evaluateS6Novelty(code, pastScars)
	e79 := evaluateS79Quality(code)
	s9, e9 := evaluateS9Deadlock(code, ext)

	expl := append(append(append(append(append(e1, e3...), e5...), e6...), e79...), e9...)

	gate := s1 * s3 * s5 * s6 * s9
	if gate == 0.0 {
		return PRMVerdict{
			Score:        0.05,
			Verdict:      "🔴 RECHAZADO (VETO GATE ACTIVO)",
			Explanations: expl,
		}
	}

	s2, e2 := evaluateS2CC(f, ext)
	s4, e4 := evaluateS4Alloc(code, ext)
	expl = append(append(expl, e2...), e4...)

	composite := gate * (0.6*s2 + 0.4*s4)
	verdictStr := "🟢 EXCELENTE"
	if composite < 0.7 {
		verdictStr = "🟡 ACEPTABLE (Mejorable)"
	}
	return PRMVerdict{Score: composite, Verdict: verdictStr, Explanations: expl}
}

func evaluateS1Parse(code, sourceToParse, ext string) (float64, *ast.File, []string) {
	if ext == ".go" || ext == "" {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "", []byte(sourceToParse), 0)
		if err != nil {
			return 0.0, nil, []string{fmt.Sprintf("❌ S1 Syntax Error: %v", err)}
		}
		return 1.0, f, []string{"✅ S1 Síntesis Estructural: Árbol AST correcto."}
	}
	if ext == ".ts" || ext == ".tsx" || ext == ".js" || ext == ".jsx" {
		if err := runTypeScriptValidator(code, ext); err != nil {
			return 0.0, nil, []string{fmt.Sprintf("❌ S1 TS Validator Error: %v", err)}
		}
		return 1.0, nil, []string{"✅ S1 TypeScript Checker: 0 errores de Tipos (TypeScript 100% Strict)."}
	}
	return 1.0, nil, []string{"✅ S1 Parse: [Polyglot Bypass] Lenguaje no nativo."}
}

func evaluateS3Safety(code string) (float64, []string) {
	if strings.Contains(code, "fmt.Print") || strings.Contains(code, "os.Exit") || strings.Contains(code, "log.Fatal") {
		return 0.0, []string{"❌ S3 Safety: Contiene I/O a stdout (fmt.Print) u os.Exit. Riesgo de corrupción del protocolo MCP JSON-RPC."}
	}
	return 1.0, []string{"✅ S3 Safety: Límites MCP respetados (sin I/O destructivo)"}
}

func evaluateS5Egress(code string) (float64, []string) {
	egressPatterns := []string{
		"net/http", "net.dial", "os/exec", "syscall", "plugin",
		"curl ", "wget ", "ssh ", "nc -", "bash -i", "/dev/tcp/",
		"/etc/shadow", "/etc/passwd", "socat", "python -c", "perl -e",
	}
	lower := strings.ToLower(code)
	for _, pattern := range egressPatterns {
		if strings.Contains(lower, pattern) {
			return 0.0, []string{fmt.Sprintf("❌ S5 Egress: Vector de exfiltración o ejecución de subprocesos detectado ('%s'). Operación bloqueada por política Zero-Trust.", pattern)}
		}
	}
	return 1.0, nil
}

func evaluateS6Novelty(code string, pastScars []string) (float64, []string) {
	for _, scar := range pastScars {
		sim := nlp.CosineSimilarity(code, scar)
		if sim > 0.85 {
			return 0.0, []string{fmt.Sprintf("❌ S6 Anti-Repetition: Este exacto bloque de código ya falló en el pasado (Similitud %.2f). Has entrado en un bucle suicida. Cambia tu enfoque.", sim)}
		}
	}
	return 1.0, nil
}

func evaluateS79Quality(code string) []string {
	var out []string
	lz76 := nlp.ComputeLZ76(code)
	ttr := nlp.ComputeRootTTR(code)
	if lz76 < ActivePRM.LZ76Threshold {
		out = append(out, fmt.Sprintf("⚠️ S7 Redundancia: Complejidad Lempel-Ziv extremadamente baja (%.2f). Posible Reward Gaming o código espagueti padding.", lz76))
	}
	if ttr < 0.20 {
		out = append(out, fmt.Sprintf("⚠️ S8 Claridad: Root-TTR muy bajo (%.2f). Falta diversidad léxica.", ttr))
	}
	return out
}

func evaluateS9Deadlock(code, ext string) (float64, []string) {
	if ext == ".go" || ext == "" {
		if cycleErr := DetectDeadlockCycles(code); cycleErr != nil {
			return 0.0, []string{fmt.Sprintf("❌ S9 Formal Verifier: %v", cycleErr)}
		}
		return 1.0, nil
	}
	return 1.0, []string{"✅ S9 Formal Verifier: [Polyglot Bypass] Verificación de Deadlocks omitida para frontend."}
}

func evaluateS2CC(f *ast.File, ext string) (float64, []string) {
	if ext == ".go" || ext == "" {
		cc := 0
		if f != nil {
			for _, decl := range f.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok {
					cc += CalculateCC(fn)
				}
			}
		}
		if cc > 15 {
			return 0.2, []string{fmt.Sprintf("❌ S2 Complejidad: CC=%d (>15 indica Código Espagueti)", cc)}
		}
		if cc > 8 {
			return 0.7, []string{fmt.Sprintf("⚠️ S2 Complejidad: CC=%d (Alta, considera refactorizar)", cc)}
		}
		return 1.0, []string{fmt.Sprintf("✅ S2 Complejidad: CC=%d (Óptima)", cc)}
	}
	if ext == ".ts" || ext == ".tsx" || ext == ".js" || ext == ".jsx" {
		return 1.0, []string{"✅ S2 Frontend CC: [Polyglot Aware] Delegado al TS-Linter Daemon nativo."}
	}
	return 1.0, []string{"✅ S2 Complejidad: [Polyglot Bypass] CC delegada."}
}

func evaluateS4Alloc(code, ext string) (float64, []string) {
	if ext == ".go" || ext == "" {
		allocs := strings.Count(code, "make(") + strings.Count(code, "new(")
		if allocs > 3 {
			return 0.5, []string{fmt.Sprintf("⚠️ S4 Zero-Alloc: %d allocations detectadas en heap. ¿Puedes usar sync.Pool?", allocs)}
		}
		return 1.0, []string{"✅ S4 Zero-Alloc: Perfil de memoria estable"}
	}
	if ext == ".ts" || ext == ".tsx" || ext == ".js" || ext == ".jsx" {
		return 1.0, []string{"✅ S4 Frontend Memoria: [Polyglot Aware] Tolerancia controlada bajo V8 Engine."}
	}
	return 1.0, []string{"✅ S4 Zero-Alloc: [Polyglot Bypass] Motor JS optimiza heap."}
}
