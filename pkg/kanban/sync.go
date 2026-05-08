// Package kanban manages the lifecycle of epic blocks in master_plan.md.
// Completed epics (all tasks [x]) are archived to master_done.md during REM sleep.
// Technical debt detected by the system goes to technical_debt.md (separate concern).
// [SRE-30.1.2 / SRE-30.2.1]
package kanban

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SyncCompletedEpics scans master_plan.md for epic blocks where every task is [x],
// archives them to .neo/master_done.md, and removes them from master_plan.md.
// [SRE-30.1.2] Safe to call only during idle (REM sleep) — see SRE-30.2.1.
func SyncCompletedEpics(workspace string) (int, error) {
	planPath := filepath.Clean(filepath.Join(workspace, ".neo", "master_plan.md"))
	if !strings.HasPrefix(planPath, filepath.Clean(workspace)) {
		return 0, fmt.Errorf("[kanban] path traversal rejected")
	}

	//nolint:gosec // G304-WORKSPACE-CANON: planPath has explicit traversal rejection upstream (line 22)
	data, err := os.ReadFile(planPath)
	if err != nil {
		return 0, nil
	}

	blocks := splitEpicBlocks(string(data))
	var keepBlocks []string
	var doneBlocks []string

	for _, block := range blocks {
		if isEpicComplete(block) {
			doneBlocks = append(doneBlocks, block)
		} else {
			keepBlocks = append(keepBlocks, block)
		}
	}

	// [323.A] Second pass: if all épicas were archived and no open tasks remain,
	// archive orphan PILAR-intro blocks (header + description text, no tasks).
	// This ensures master_plan.md can reach a truly empty state.
	if len(keepBlocks) > 0 && planHasNoOpenTasks(keepBlocks) {
		doneBlocks = append(doneBlocks, keepBlocks...)
		keepBlocks = nil
	}

	if len(doneBlocks) == 0 {
		return 0, nil
	}

	// Archive completed epics to master_done.md (not technical_debt.md).
	if err := archiveDoneEpics(workspace, doneBlocks); err != nil {
		return 0, fmt.Errorf("[kanban] archive failed: %w", err)
	}

	// Rewrite master_plan.md without the completed epics.
	newContent := strings.Join(keepBlocks, "")
	//nolint:gosec // G304-WORKSPACE-CANON: planPath has explicit traversal rejection upstream (line 22)
	if err := os.WriteFile(planPath, []byte(newContent), 0600); err != nil {
		return 0, fmt.Errorf("[kanban] failed to rewrite master_plan.md: %w", err)
	}

	log.Printf("[SRE-KANBAN] %d épica(s) completadas archivadas en master_done.md.", len(doneBlocks))
	return len(doneBlocks), nil
}

// planHasNoOpenTasks returns true if none of the blocks contain a pending task. [323.A]
func planHasNoOpenTasks(blocks []string) bool {
	for _, block := range blocks {
		for line := range strings.SplitSeq(block, "\n") {
			if strings.HasPrefix(strings.TrimLeft(line, " \t"), "- [ ]") {
				return false
			}
		}
	}
	return true
}

// splitEpicBlocks splits the plan file into epic-level blocks.
// Detects both ## and ### epic/pillar headers, case-insensitive ("Épica" == "ÉPICA").
// Preserves leading preamble (frontmatter + title) as first element.
func splitEpicBlocks(content string) []string {
	lines := strings.Split(content, "\n")
	var blocks []string
	var current strings.Builder

	for _, line := range lines {
		lineU := strings.ToUpper(line)
		isEpicHeader := false
		// Match "### Épica" / "### ÉPICA" (epic sub-headers under pillar)
		if strings.HasPrefix(line, "### ") && strings.Contains(lineU, "ÉPICA") {
			isEpicHeader = true
		}
		// Match "## Épica" / "## ÉPICA" (top-level epic headers)
		if strings.HasPrefix(line, "## ") && strings.Contains(lineU, "ÉPICA") {
			isEpicHeader = true
		}
		// Match "## PILAR" (pillar headers — block boundary)
		if strings.HasPrefix(line, "## ") && strings.Contains(lineU, "PILAR") {
			isEpicHeader = true
		}

		if isEpicHeader && current.Len() > 0 {
			blocks = append(blocks, current.String())
			current.Reset()
		}
		current.WriteString(line)
		current.WriteByte('\n')
	}
	if current.Len() > 0 {
		blocks = append(blocks, current.String())
	}
	return blocks
}

// isEpicComplete returns true if a block has at least one task marker
// and every task marker is [x] (none are [ ]).
func isEpicComplete(block string) bool {
	hasTasks := false
	lines := strings.SplitSeq(block, "\n")
	for line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- [ ]") {
			return false // Still has pending task
		}
		if strings.HasPrefix(trimmed, "- [x]") || strings.HasPrefix(trimmed, "- [X]") {
			hasTasks = true
		}
	}
	// Must have "épica" (any case) in the block and at least one completed task.
	return hasTasks && strings.Contains(strings.ToUpper(block), "ÉPICA")
}

// archiveDoneEpics appends completed epic blocks to .neo/master_done.md.
func archiveDoneEpics(workspace string, blocks []string) error {
	donePath := filepath.Clean(filepath.Join(workspace, ".neo", "master_done.md"))
	if !strings.HasPrefix(donePath, filepath.Clean(workspace)) {
		return fmt.Errorf("path traversal rejected")
	}

	if err := os.MkdirAll(filepath.Dir(donePath), 0750); err != nil {
		return err
	}

	// Create header if file doesn't exist
	if _, err := os.Stat(donePath); os.IsNotExist(err) {
		header := "# Master Done — Épicas Completadas\n\n" +
			"> Archivo automático gestionado por el Kanban de NeoAnvil.\n" +
			"> Las épicas completadas (todas las tareas [x]) son archivadas aquí\n" +
			"> durante el ciclo REM (5 min de inactividad).\n\n---\n\n"
		if err := os.WriteFile(donePath, []byte(header), 0600); err != nil {
			return err
		}
	}

	f, err := os.OpenFile(donePath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	timestamp := time.Now().Format("2006-01-02")
	for _, block := range blocks {
		fmt.Fprintf(f, "<!-- archived: %s -->\n%s\n---\n\n", timestamp, strings.TrimRight(block, "\n"))
	}
	return nil
}

// AppendTechDebt writes a new technical debt entry to .neo/technical_debt.md.
// Called by the system when it detects deficiencies (not for completed epics).
// [321.A] Deduplicates by function name extracted from description.
// [321.B] Rejects entries whose file path is outside the current workspace.
func AppendTechDebt(workspace, title, description, priority string) error {
	debtPath := filepath.Clean(filepath.Join(workspace, ".neo", "technical_debt.md"))
	if !strings.HasPrefix(debtPath, filepath.Clean(workspace)) {
		return fmt.Errorf("path traversal rejected")
	}

	if err := os.MkdirAll(filepath.Dir(debtPath), 0750); err != nil {
		return err
	}

	// Create header if file doesn't exist.
	if _, err := os.Stat(debtPath); os.IsNotExist(err) {
		header := "# Technical Debt — Deficiencias Detectadas\n\n" +
			"> Archivo gestionado automáticamente por NeoAnvil.\n" +
			"> Contiene deuda técnica detectada por el sistema en background.\n" +
			"> No confundir con master_done.md (épicas completadas).\n\n---\n\n"
		if err := os.WriteFile(debtPath, []byte(header), 0600); err != nil {
			return err
		}
	}

	// [321.B] Workspace isolation: reject absolute paths outside this workspace.
	if externalPath := debtExtractAbsPath(title); externalPath != "" {
		if !strings.HasPrefix(filepath.Clean(externalPath), filepath.Clean(workspace)) {
			return nil // silently skip — entry belongs to another workspace
		}
	}

	// [321.A] Deduplication: skip if function name already recorded.
	if existing, err := os.ReadFile(debtPath); err == nil { //nolint:gosec // G304-WORKSPACE-CANON
		if name := debtExtractFuncName(description); name != "" {
			if bytes.Contains(existing, []byte("func "+name)) {
				return nil // already recorded, do not duplicate
			}
		}
	}

	f, err := os.OpenFile(debtPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	timestamp := time.Now().Format("2006-01-02 15:04")
	fmt.Fprintf(f, "## [%s] %s\n\n**Prioridad:** %s\n\n%s\n\n---\n\n", timestamp, title, priority, description)
	return nil
}

// debtExtractFuncName parses "func FuncName: CC=N (limit M)" from an AST description.
// Returns empty string if no function name is found. [321.A]
func debtExtractFuncName(description string) string {
	_, after, found := strings.Cut(description, "func ")
	if !found {
		return ""
	}
	if name, _, ok := strings.Cut(after, ":"); ok {
		return strings.TrimSpace(name)
	}
	if name, _, ok := strings.Cut(after, " "); ok {
		return name
	}
	return after
}

// debtExtractAbsPath extracts an absolute file path from a debt title like
// "AST COMPLEXITY in /home/user/project/file.go:42". Returns "" if no abs path. [321.B]
func debtExtractAbsPath(title string) string {
	_, rest, found := strings.Cut(title, " in ")
	if !found {
		return ""
	}
	rest = strings.TrimSpace(rest)
	if !filepath.IsAbs(rest) {
		return ""
	}
	// Strip trailing ":lineNo" if present.
	if j := strings.LastIndex(rest, ":"); j > 0 {
		rest = rest[:j]
	}
	return rest
}
