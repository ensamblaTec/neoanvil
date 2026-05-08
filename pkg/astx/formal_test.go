package astx

import (
	"strings"
	"testing"
)

func TestDetectDeadlockCycles_Safe(t *testing.T) {
	code := `
		func Worker() {
			mu1.Lock()
			mu1.Unlock()

			mu2.Lock()
			mu2.Unlock()
		}
	`
	if err := DetectDeadlockCycles(code); err != nil {
		t.Fatalf("Falso positivo en código seguro: %v", err)
	}
}

func TestDetectDeadlockCycles_Deadlock(t *testing.T) {
	code := `
		func PhilosopherA() {
			mu1.Lock()
			mu2.Lock() // A espera mu2
			mu2.Unlock()
			mu1.Unlock()
		}

		func PhilosopherB() {
			mu2.Lock()
			mu1.Lock() // B espera mu1
			mu1.Unlock()
			mu2.Unlock()
		}
	`
	err := DetectDeadlockCycles(code)
	if err == nil {
		t.Fatal("Model Checker falló al detectar el Deadlock de Filósofos (Ciclo Bi-Direccional)")
	}
	if !strings.Contains(err.Error(), "S9 DEADLOCK VETO") {
		t.Fatalf("Texto de error inesperado: %v", err)
	}
}

func TestDetectDeadlockCycles_WaitGroup(t *testing.T) {
	code := `
		func BadWorker() {
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				// Olvidé el wg.Done()
				log.Printf("[SRE] Test Routine Hi")
			}()
			wg.Wait()
		}
	`
	err := DetectDeadlockCycles(code)
	if err == nil {
		t.Fatal("Model Checker falló al detectar Asimetría de WaitGroup")
	}
	if !strings.Contains(err.Error(), "S9 WAITGROUP VETO") {
		t.Fatalf("Texto de error inesperado: %v", err)
	}
}

func TestDetectDeadlockCycles_WaitGroupSafe(t *testing.T) {
	code := `
		func SafeWorker() {
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				log.Printf("[SRE] Test Routine Hi")
			}()
			wg.Wait()
		}
	`
	if err := DetectDeadlockCycles(code); err != nil {
		t.Fatalf("Falso positivo en código WaitGroup seguro: %v", err)
	}
}

func TestPRM_S9_DeadlockVeto(t *testing.T) {
	code := `
		func Bug() {
			a.Lock()
			b.Lock()
			// ...
		}
		func Bug2() {
			b.Lock()
			a.Lock()
		}
	`
	verdict := EvaluatePolyglotPRM(code, ".go", nil)
	if verdict.Score != 0.05 {
		t.Fatalf("Score debería colapsar a 0.05 por el VETO Matemático, recibió %v", verdict.Score)
	}
	if !strings.Contains(strings.Join(verdict.Explanations, " "), "S9 Formal Verifier") {
		t.Fatalf("Falta la explicación del S9 en el Veredicto")
	}
}
