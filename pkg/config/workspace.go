package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnsureWorkspace creates all .neo/ subdirectories and template files for a new workspace.
// Safe to call on every boot — all operations are idempotent (create-if-missing).
func EnsureWorkspace(workspace string) error {
	dirs := []string{
		filepath.Join(workspace, ".neo"),
		filepath.Join(workspace, ".neo", "db"),
		filepath.Join(workspace, ".neo", "logs"),
		filepath.Join(workspace, ".neo", "models"),
		filepath.Join(workspace, ".neo", "pki"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0750); err != nil {
			return fmt.Errorf("cannot create %s: %w", d, err)
		}
	}

	// .neo/master_plan.md — template for strategic planning
	masterPlanPath := filepath.Join(workspace, ".neo", "master_plan.md")
	if _, err := os.Stat(masterPlanPath); os.IsNotExist(err) {
		//nolint:gosec // G304-WORKSPACE-CANON: config loader walks from cwd, pinned at boot
		if err := os.WriteFile(masterPlanPath, []byte(masterPlanTemplate), 0600); err != nil {
			return fmt.Errorf("cannot create master_plan.md: %w", err)
		}
	}

	// .neo/technical_debt.md — archive for completed epics (Kanban)
	debtPath := filepath.Join(workspace, ".neo", "technical_debt.md")
	if _, err := os.Stat(debtPath); os.IsNotExist(err) {
		//nolint:gosec // G304-WORKSPACE-CANON: config loader walks from cwd, pinned at boot
		if err := os.WriteFile(debtPath, []byte(technicalDebtTemplate), 0600); err != nil {
			return fmt.Errorf("cannot create technical_debt.md: %w", err)
		}
	}

	// .neo/db/certified_state.lock — empty seal file for pre-commit hook
	lockPath := filepath.Join(workspace, ".neo", "db", "certified_state.lock")
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		f, openErr := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0600)
		if openErr != nil {
			return fmt.Errorf("cannot create certified_state.lock: %w", openErr)
		}
		f.Close()
	}

	// .neo/models/hypervisor.wasm — minimal valid WASM module (magic + version).
	// Prevents [SRE-WAR] on boot; the sandbox falls back to EvaluatePlan(cpu) anyway.
	wasmPath := filepath.Join(workspace, ".neo", "models", "hypervisor.wasm")
	if _, err := os.Stat(wasmPath); os.IsNotExist(err) {
		dummy := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
		if err := os.WriteFile(wasmPath, dummy, 0600); err != nil {
			return fmt.Errorf("cannot create hypervisor.wasm: %w", err)
		}
	}

	return nil
}

const masterPlanTemplate = `# Master Plan — NeoAnvil Orquestador

> Edita este archivo para definir las épicas estratégicas del proyecto.
> NeoAnvil lee la primera fase con tareas pendientes (- [ ]) al arrancar.
> El Kanban automático archivará épicas completadas a technical_debt.md durante el ciclo REM.

---

## 🚀 ÉPICA 1: BOOTSTRAP (Ejemplo)
*Descripción breve de la épica y su objetivo.*

### Fase 1.1: Primera fase
- [ ] **Task 1.1.1:** Descripción de la tarea 1.
- [ ] **Task 1.1.2:** Descripción de la tarea 2.

### Fase 1.2: Segunda fase
- [ ] **Task 1.2.1:** Descripción de la tarea 3.

---
`

const technicalDebtTemplate = `# Technical Debt — Épicas Completadas

> Este archivo es gestionado automáticamente por el Kanban de NeoAnvil.
> Las épicas completadas (todas las tareas [x]) son archivadas aquí
> durante el ciclo REM (5 min de inactividad) para mantener el Master Plan limpio.

---
`
