package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// [SRE-31.1.1] Memex Atomic Recovery — validates BoltDB durability across simulated restarts.
func TestMemexAtomicRecovery(t *testing.T) {
	dir := t.TempDir()

	if err := InitPlanner(dir); err != nil {
		t.Fatalf("InitPlanner: %v", err)
	}

	fixtures := []MemexEntry{
		{Topic: "timeout pattern", Scope: "backend", Content: "Always use 5s context timeout in createUser"},
		{Topic: "race condition", Scope: "pkg/swarm", Content: "Use LWW-CRDT for concurrent file access"},
		{Topic: "nil map panic", Scope: "pkg/config", Content: "Initialize modules map in defaultNeoConfig"},
	}
	for i, e := range fixtures {
		if err := MemexCommit(e); err != nil {
			t.Fatalf("MemexCommit[%d]: %v", i, err)
		}
	}

	// Simulate abrupt process crash: force-close BoltDB without graceful shutdown.
	plannerDB.Close()
	plannerDB = nil

	// Resume after "restart" — reopen the same DB path.
	if err := InitPlanner(dir); err != nil {
		t.Fatalf("InitPlanner reopen: %v", err)
	}

	// Validate: all 3 entries survived the simulated crash.
	entries, err := MemexDrain()
	if err != nil {
		t.Fatalf("MemexDrain after recovery: %v", err)
	}
	if len(entries) != len(fixtures) {
		t.Errorf("expected %d entries after recovery, got %d", len(fixtures), len(entries))
	}

	// Validate: drain is idempotent — second call returns empty buffer.
	entries2, err := MemexDrain()
	if err != nil {
		t.Fatalf("second MemexDrain: %v", err)
	}
	if len(entries2) != 0 {
		t.Errorf("expected empty buffer after drain, got %d entries", len(entries2))
	}
}

// TestMemexDrain_EmptyBuffer ensures MemexDrain is safe on an empty buffer (no panic, no error).
func TestMemexDrain_EmptyBuffer(t *testing.T) {
	dir := t.TempDir()
	if err := InitPlanner(dir); err != nil {
		t.Fatalf("InitPlanner: %v", err)
	}
	entries, err := MemexDrain()
	if err != nil {
		t.Errorf("MemexDrain on empty buffer returned error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

// TestMemexCommit_FieldsPreserved validates all fields survive the BoltDB round-trip.
func TestMemexCommit_FieldsPreserved(t *testing.T) {
	dir := t.TempDir()
	if err := InitPlanner(dir); err != nil {
		t.Fatalf("InitPlanner: %v", err)
	}

	want := MemexEntry{
		Topic:   "test topic",
		Scope:   "cmd/neo-mcp",
		Content: "test lesson with unicode: 🧠",
	}
	if err := MemexCommit(want); err != nil {
		t.Fatalf("MemexCommit: %v", err)
	}

	entries, err := MemexDrain()
	if err != nil {
		t.Fatalf("MemexDrain: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	got := entries[0]
	if got.Topic != want.Topic {
		t.Errorf("Topic: got %q, want %q", got.Topic, want.Topic)
	}
	if got.Scope != want.Scope {
		t.Errorf("Scope: got %q, want %q", got.Scope, want.Scope)
	}
	if got.Content != want.Content {
		t.Errorf("Content: got %q, want %q", got.Content, want.Content)
	}
	if got.ID == "" {
		t.Error("ID must be set by MemexCommit")
	}
	if got.Timestamp == 0 {
		t.Error("Timestamp must be set by MemexCommit")
	}
}

// TestReadOpenTasks verifies filter_open returns only - [ ] lines + parent headings. [318.A]
func TestReadOpenTasks(t *testing.T) {
	const plan = `## PILAR A — Descripción
### Épica 1 — Algo
- [x] **1.A — done**
- [ ] **1.B — open**
### Épica 2 — Otra
- [x] **2.A — done**
## PILAR B — Otro
### Épica 3 — Cerrada
- [x] **3.A — done**
`
	dir := t.TempDir()
	neoDir := filepath.Join(dir, ".neo")
	if err := os.MkdirAll(neoDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(neoDir, "master_plan.md"), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ReadOpenTasks(dir)
	if err != nil {
		t.Fatalf("ReadOpenTasks: %v", err)
	}

	// Must include the open task
	if !strings.Contains(got, "1.B — open") {
		t.Errorf("open task missing from output: %q", got)
	}
	// Must include parent headings
	if !strings.Contains(got, "PILAR A") {
		t.Errorf("parent ## heading missing: %q", got)
	}
	if !strings.Contains(got, "Épica 1") {
		t.Errorf("parent ### heading missing: %q", got)
	}
	// Must NOT include closed tasks
	if strings.Contains(got, "1.A — done") {
		t.Errorf("closed task should not appear: %q", got)
	}
	// Must NOT include PILAR B (no open tasks there)
	if strings.Contains(got, "PILAR B") {
		t.Errorf("section with no open tasks should not appear: %q", got)
	}
}

func TestReadOpenTasks_AllDone(t *testing.T) {
	const plan = `## PILAR A — Completo
- [x] **1.A — done**
- [x] **1.B — done**
`
	dir := t.TempDir()
	neoDir := filepath.Join(dir, ".neo")
	if err := os.MkdirAll(neoDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(neoDir, "master_plan.md"), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ReadOpenTasks(dir)
	if err != nil {
		t.Fatalf("ReadOpenTasks: %v", err)
	}
	if !strings.Contains(got, "Open: 0") {
		t.Errorf("expected all-done message, got: %q", got)
	}
}

// TestReadOpenTasks_CodeFenceHeadings verifies that `## ...` headings inside
// fenced code blocks are NOT parsed as real phase boundaries. [330.M]
func TestReadOpenTasks_CodeFenceHeadings(t *testing.T) {
	const plan = "## PILAR REAL — Real phase\n" +
		"### Épica 1 — Real epic\n" +
		"\n" +
		"Example schema below:\n" +
		"\n" +
		"```markdown\n" +
		"## Fake Heading\n" +
		"### Fake Sub\n" +
		"- [ ] **fake.A — should not appear**\n" +
		"```\n" +
		"\n" +
		"- [ ] **1.A — real open task**\n"

	dir := t.TempDir()
	neoDir := filepath.Join(dir, ".neo")
	if err := os.MkdirAll(neoDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(neoDir, "master_plan.md"), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ReadOpenTasks(dir)
	if err != nil {
		t.Fatalf("ReadOpenTasks: %v", err)
	}

	// Real task must appear under the REAL heading.
	if !strings.Contains(got, "1.A — real open task") {
		t.Errorf("real open task missing: %q", got)
	}
	if !strings.Contains(got, "PILAR REAL") {
		t.Errorf("real ## heading missing: %q", got)
	}
	// Fake task inside fence must NOT appear as an open task.
	if strings.Contains(got, "fake.A") {
		t.Errorf("task inside code fence leaked into output: %q", got)
	}
	// Fake heading must NOT be emitted as a parent heading.
	if strings.Contains(got, "Fake Heading") {
		t.Errorf("heading inside code fence leaked into output: %q", got)
	}
}

// TestReadActivePhase_CodeFenceHeadings verifies that fenced `## ...` lines
// don't prematurely terminate the active phase. [330.M]
func TestReadActivePhase_CodeFenceHeadings(t *testing.T) {
	const plan = "## PILAR REAL — Real phase\n" +
		"\n" +
		"Schema example:\n" +
		"\n" +
		"```markdown\n" +
		"## Fake Next Phase\n" +
		"- [ ] **fake.A — should not terminate parsing**\n" +
		"```\n" +
		"\n" +
		"- [ ] **1.A — real open task**\n" +
		"\n" +
		"## PILAR NEXT — Not active\n" +
		"- [x] **2.A — done**\n"

	dir := t.TempDir()
	neoDir := filepath.Join(dir, ".neo")
	if err := os.MkdirAll(neoDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(neoDir, "master_plan.md"), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ReadActivePhase(dir)
	if err != nil {
		t.Fatalf("ReadActivePhase: %v", err)
	}

	// Real open task must appear.
	if !strings.Contains(got, "1.A — real open task") {
		t.Errorf("real open task missing: %q", got)
	}
	// Active phase must be PILAR REAL, not truncated by the fenced fake.
	if !strings.Contains(got, "PILAR REAL") {
		t.Errorf("real ## heading missing: %q", got)
	}
	// PILAR NEXT must NOT be included (it's after the active phase).
	if strings.Contains(got, "PILAR NEXT") {
		t.Errorf("next phase should not be emitted: %q", got)
	}
}
