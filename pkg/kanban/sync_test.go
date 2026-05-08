package kanban

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitEpicBlocks(t *testing.T) {
	content := `---
title: test plan
---

# MASTER PLAN

## 🔬 PILAR I

### 🌟 ÉPICA 1: "FOO"
- [x] Task 1.1

### 🌟 ÉPICA 2: "BAR"
- [ ] Task 2.1
`
	blocks := splitEpicBlocks(content)
	// Should split: preamble, PILAR I, ÉPICA 1, ÉPICA 2
	if len(blocks) < 3 {
		t.Errorf("expected at least 3 blocks, got %d", len(blocks))
	}
}

func TestSplitEpicBlocksNoHeaders(t *testing.T) {
	content := "just some text\nno headers\n"
	blocks := splitEpicBlocks(content)
	if len(blocks) != 1 {
		t.Errorf("expected 1 block for no-header content, got %d", len(blocks))
	}
}

// TestSplitEpicBlocks_TitleCase verifies that titlecase "Épica" headers are recognized
// as block boundaries (production bug: worker used ÉPICA all-caps, plan uses Épica titlecase).
func TestSplitEpicBlocks_TitleCase(t *testing.T) {
	content := `---
title: test plan
---

# MASTER PLAN

## PILAR XLIV — Deuda Técnica

### Épica 302 — CC real
- [x] **302.A — done**

### Épica 303 — Embedding
- [x] **303.A — done**
`
	blocks := splitEpicBlocks(content)
	// Expected: preamble, PILAR block, Épica 302 block, Épica 303 block = 4
	if len(blocks) < 3 {
		t.Errorf("expected at least 3 blocks with titlecase Épica headers, got %d", len(blocks))
	}
}

// TestIsEpicComplete_TitleCase verifies that "Épica" (titlecase) is detected by isEpicComplete.
func TestIsEpicComplete_TitleCase(t *testing.T) {
	block := "### Épica 302 — CC real\n- [x] Task A\n- [x] Task B\n"
	if !isEpicComplete(block) {
		t.Error("block with titlecase 'Épica' and all-[x] tasks should be complete")
	}
}

// TestSyncCompletedEpics_TitleCase verifies end-to-end sync with titlecase Épica headers.
func TestSyncCompletedEpics_TitleCase(t *testing.T) {
	dir := t.TempDir()
	workspace := dir
	os.MkdirAll(filepath.Join(workspace, ".neo"), 0750)

	planContent := `---
title: test
---

# PLAN

## PILAR XLIV — Deuda Técnica

### Épica 302 — DONE
- [x] Task A
- [x] Task B

### Épica 303 — PENDING
- [x] Task C
- [ ] Task D
`
	planPath := filepath.Join(workspace, ".neo", "master_plan.md")
	os.WriteFile(planPath, []byte(planContent), 0600)

	archived, err := SyncCompletedEpics(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if archived != 1 {
		t.Errorf("expected 1 archived epic with titlecase headers, got %d", archived)
	}

	remaining, _ := os.ReadFile(planPath)
	if strings.Contains(string(remaining), "Épica 302") {
		t.Error("Épica 302 should have been removed from master_plan")
	}
	if !strings.Contains(string(remaining), "Épica 303") {
		t.Error("Épica 303 should remain in master_plan")
	}
}

func TestIsEpicComplete(t *testing.T) {
	complete := "### 🌟 ÉPICA 1\n- [x] Task A\n- [x] Task B\n"
	if !isEpicComplete(complete) {
		t.Error("should be complete")
	}

	incomplete := "### 🌟 ÉPICA 2\n- [x] Task A\n- [ ] Task B\n"
	if isEpicComplete(incomplete) {
		t.Error("should be incomplete")
	}

	noTasks := "### 🌟 ÉPICA 3\nJust description.\n"
	if isEpicComplete(noTasks) {
		t.Error("no tasks should not be considered complete")
	}

	notEpic := "## Some Header\n- [x] Done\n"
	if isEpicComplete(notEpic) {
		t.Error("block without ÉPICA should not be complete")
	}
}

func TestAppendTechDebt(t *testing.T) {
	dir := t.TempDir()
	workspace := dir
	os.MkdirAll(filepath.Join(workspace, ".neo"), 0750)

	err := AppendTechDebt(workspace, "Test Debt", "Something is wrong", "alta")
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(workspace, ".neo", "technical_debt.md"))
	if err != nil {
		t.Fatal(err)
	}

	content := string(data)
	if !strings.Contains(content, "Test Debt") {
		t.Error("expected debt title in file")
	}
	if !strings.Contains(content, "alta") {
		t.Error("expected priority in file")
	}
	if !strings.Contains(content, "Deficiencias Detectadas") {
		t.Error("expected header in file")
	}
}

// TestAppendTechDebt_Dedup verifies [321.A]: same function name is not written twice.
func TestAppendTechDebt_Dedup(t *testing.T) {
	dir := t.TempDir()
	workspace := dir
	os.MkdirAll(filepath.Join(workspace, ".neo"), 0750)

	desc := "func handleSemanticCode: CC=25 (limit 15)"
	AppendTechDebt(workspace, "AST COMPLEXITY in radar_handlers.go:1227", desc, "alta")
	AppendTechDebt(workspace, "AST COMPLEXITY in radar_handlers.go:1230", desc, "alta") // same func, shifted line
	AppendTechDebt(workspace, "AST COMPLEXITY in radar_handlers.go:1227", desc, "alta") // exact duplicate

	data, _ := os.ReadFile(filepath.Join(workspace, ".neo", "technical_debt.md"))
	count := strings.Count(string(data), "func handleSemanticCode")
	if count != 1 {
		t.Errorf("expected 1 occurrence of func name, got %d (dedup failed)", count)
	}
}

// TestAppendTechDebt_WorkspaceIsolation verifies [321.B]: abs paths outside workspace are rejected.
func TestAppendTechDebt_WorkspaceIsolation(t *testing.T) {
	dir := t.TempDir()
	workspace := dir
	os.MkdirAll(filepath.Join(workspace, ".neo"), 0750)

	// External abs path (macOS strategos workspace — different machine/user)
	externalTitle := "AST COMPLEXITY in /path/to/strategos/backend/file.go:387"
	AppendTechDebt(workspace, externalTitle, "func AddQuantityGood: CC=25 (limit 15)", "alta")

	data, _ := os.ReadFile(filepath.Join(workspace, ".neo", "technical_debt.md"))
	if strings.Contains(string(data), "AddQuantityGood") {
		t.Error("external workspace entry should have been rejected by workspace isolation")
	}

	// Local relative path — should be accepted
	localTitle := "AST COMPLEXITY in cmd/neo-mcp/radar_handlers.go:1054"
	AppendTechDebt(workspace, localTitle, "func backgroundIndexFile: CC=16 (limit 15)", "alta")

	data, _ = os.ReadFile(filepath.Join(workspace, ".neo", "technical_debt.md"))
	if !strings.Contains(string(data), "backgroundIndexFile") {
		t.Error("local relative path entry should have been written")
	}
}

// TestDebtExtractFuncName verifies parsing of function name from AST description.
func TestDebtExtractFuncName(t *testing.T) {
	cases := []struct{ desc, want string }{
		{"func handleSemanticCode: CC=25 (limit 15)", "handleSemanticCode"},
		{"func gatherBriefingData: CC=35 (limit 15)", "gatherBriefingData"},
		{"plain description without the magic word", ""},
		{"func myFunc (space only)", "myFunc"}, // cuts on space when no colon
	}
	for _, c := range cases {
		if got := debtExtractFuncName(c.desc); got != c.want {
			t.Errorf("debtExtractFuncName(%q) = %q, want %q", c.desc, got, c.want)
		}
	}
}

// TestSyncCompletedEpics_PilarIntroArchived verifies [323.A]: PILAR intro blocks are
// archived when the plan has no open tasks after épica archiving.
func TestSyncCompletedEpics_PilarIntroArchived(t *testing.T) {
	dir := t.TempDir()
	workspace := dir
	os.MkdirAll(filepath.Join(workspace, ".neo"), 0750)

	// PILAR with intro block + one complete épica — all done.
	planContent := `---
title: test
---

## PILAR XLIV — Deuda Técnica

This is the PILAR intro paragraph with no tasks.

### Épica 302 — DONE
- [x] Task A
- [x] Task B
`
	planPath := filepath.Join(workspace, ".neo", "master_plan.md")
	os.WriteFile(planPath, []byte(planContent), 0600)

	archived, err := SyncCompletedEpics(workspace)
	if err != nil {
		t.Fatal(err)
	}
	// Épica 302 block archived + PILAR intro orphan archived = 2 total
	if archived < 1 {
		t.Errorf("expected at least 1 archived block, got %d", archived)
	}

	remaining, _ := os.ReadFile(planPath)
	// No open tasks — master_plan should be clean (no PILAR intro orphan left)
	if strings.Contains(string(remaining), "PILAR intro paragraph") {
		t.Error("PILAR intro orphan should have been archived when Open=0")
	}
}

func TestSyncCompletedEpics(t *testing.T) {
	dir := t.TempDir()
	workspace := dir
	os.MkdirAll(filepath.Join(workspace, ".neo"), 0750)

	planContent := `---
title: test
---

# PLAN

### 🌟 ÉPICA 1: "DONE"
- [x] Task A
- [x] Task B

### 🌟 ÉPICA 2: "PENDING"
- [x] Task C
- [ ] Task D
`
	planPath := filepath.Join(workspace, ".neo", "master_plan.md")
	os.WriteFile(planPath, []byte(planContent), 0600)

	archived, err := SyncCompletedEpics(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if archived != 1 {
		t.Errorf("expected 1 archived epic, got %d", archived)
	}

	// master_plan should no longer contain ÉPICA 1
	remaining, _ := os.ReadFile(planPath)
	if strings.Contains(string(remaining), "ÉPICA 1") {
		t.Error("ÉPICA 1 should have been removed from master_plan")
	}
	if !strings.Contains(string(remaining), "ÉPICA 2") {
		t.Error("ÉPICA 2 should remain in master_plan")
	}

	// master_done.md should contain ÉPICA 1
	doneData, err := os.ReadFile(filepath.Join(workspace, ".neo", "master_done.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doneData), "ÉPICA 1") {
		t.Error("ÉPICA 1 should be in master_done.md")
	}
}
