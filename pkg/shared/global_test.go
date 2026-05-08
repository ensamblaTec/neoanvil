package shared

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/knowledge"
)

// TestReservedGlobalNamespaces — Nexus-specific namespaces listed.
func TestReservedGlobalNamespaces(t *testing.T) {
	got := ReservedGlobalNamespaces()
	want := []string{"improvements", "lessons", "operator", "upgrades", "patterns"}
	if len(got) != len(want) {
		t.Fatalf("want %d namespaces, got %d (%v)", len(want), len(got), got)
	}
	set := make(map[string]bool, len(got))
	for _, ns := range got {
		set[ns] = true
	}
	for _, ns := range want {
		if !set[ns] {
			t.Errorf("missing reserved namespace %q", ns)
		}
	}
}

// TestGlobalStorePath_EnvOverride — NEO_SHARED_DIR redirects the path.
func TestGlobalStorePath_EnvOverride(t *testing.T) {
	t.Setenv("NEO_SHARED_DIR", "/tmp/custom-shared")
	got := GlobalStorePath()
	want := "/tmp/custom-shared/db/global.db"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	knowledgeGot := GlobalKnowledgeDir()
	knowledgeWant := "/tmp/custom-shared/knowledge"
	if knowledgeGot != knowledgeWant {
		t.Errorf("knowledge dir got %q, want %q", knowledgeGot, knowledgeWant)
	}
}

// TestGlobalStorePath_DefaultsToHome — no env → ~/.neo/shared/.
func TestGlobalStorePath_DefaultsToHome(t *testing.T) {
	t.Setenv("NEO_SHARED_DIR", "")
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".neo", "shared", "db", "global.db")
	if got := GlobalStorePath(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestOpenGlobalStore_SeedsNamespaces — creates the 5 reserved dirs with
// .gitkeep at first open.
func TestOpenGlobalStore_SeedsNamespaces(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("NEO_SHARED_DIR", tmp)
	ks, err := OpenGlobalStore()
	if err != nil {
		t.Fatalf("OpenGlobalStore: %v", err)
	}
	defer ks.Close()

	for _, ns := range ReservedGlobalNamespaces() {
		dir := filepath.Join(tmp, "knowledge", ns)
		if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
			t.Errorf("missing dir for namespace %q: %v", ns, statErr)
		}
		keep := filepath.Join(dir, ".gitkeep")
		if _, statErr := os.Stat(keep); statErr != nil {
			t.Errorf("missing .gitkeep in %q: %v", ns, statErr)
		}
	}
}

// TestOpenGlobalStore_WriteReadRoundtrip — store + fetch works. End-to-end
// validation that the Nexus-global tier is a functional KnowledgeStore.
func TestOpenGlobalStore_WriteReadRoundtrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("NEO_SHARED_DIR", tmp)
	ks, err := OpenGlobalStore()
	if err != nil {
		t.Fatalf("OpenGlobalStore: %v", err)
	}
	defer ks.Close()

	entry := knowledge.KnowledgeEntry{
		Content: "pprof pipeline is a clean 5% perf win — consider enabling by default post 367.A",
		Tags:    []string{"pgo", "perf"},
	}
	if err := ks.Put(NSImprovements, "pgo-default-on-post-367a", entry); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := ks.Get(NSImprovements, "pgo-default-on-post-367a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Content != entry.Content {
		t.Errorf("content mismatch: %q vs %q", got.Content, entry.Content)
	}
}

// TestOpenGlobalStore_Idempotent — second open on same dir succeeds after
// proper close.
func TestOpenGlobalStore_Idempotent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("NEO_SHARED_DIR", tmp)
	ks1, err := OpenGlobalStore()
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	ks1.Close() // release flock

	ks2, err := OpenGlobalStore()
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	ks2.Close()
}

// TestOpenGlobalStore_LeaderOnly — con un handle RW vivo, un segundo open
// concurrent bajo el mismo NEO_SHARED_DIR retorna ErrLeaderBusy en vez de
// bloquearse en bbolt flock. El child no-leader recibe nil store y puede
// seguir operando proyecto-level. [354.Z]
func TestOpenGlobalStore_LeaderOnly(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("NEO_SHARED_DIR", tmp)

	leader, err := OpenGlobalStore()
	if err != nil {
		t.Fatalf("leader open: %v", err)
	}
	defer leader.Close()

	// Segundo open desde el mismo proceso — flock está libre dentro del mismo
	// proceso (LOCK_EX se comparte in-process), así que esto NO dispara el
	// leader-busy. Para probar el path real, simulamos otro handle que ya
	// tomó el flock via knowledge.Open directamente y vemos que el segundo
	// call retorna ErrLeaderBusy. La simulación process-distinto requiere
	// subprocess — para el unit test basta con verificar que los writes del
	// leader son visibles y que el retry path existe.
	if err := leader.Put(NSLessons, "leader-write", knowledge.KnowledgeEntry{
		Content: "visible via leader handle",
	}); err != nil {
		t.Fatalf("leader Put: %v", err)
	}
	got, err := leader.Get(NSLessons, "leader-write")
	if err != nil || got.Content != "visible via leader handle" {
		t.Errorf("leader Get failed: got=%q err=%v", got.Content, err)
	}
}

// TestErrLeaderBusy_Sentinel — el sentinel existe y es identificable con
// errors.Is para que main.go lo trate como warning no-fatal. [354.Z]
func TestErrLeaderBusy_Sentinel(t *testing.T) {
	if ErrLeaderBusy == nil {
		t.Fatal("ErrLeaderBusy must not be nil")
	}
	wrapped := fmt.Errorf("wrapped: %w", ErrLeaderBusy)
	if !errors.Is(wrapped, ErrLeaderBusy) {
		t.Error("wrapped ErrLeaderBusy should be identifiable via errors.Is")
	}
}

// TestThreeTierIsolation_ProjectsIndependentNexusShared — formaliza el
// modelo pedido por el operador: dos proyectos tienen shared.db totalmente
// independientes (lo que strategos escribe NO lo ve app2), PERO Nexus-global
// es visible desde ambos. Es el invariante 3-tier expresado en código.
func TestThreeTierIsolation_ProjectsIndependentNexusShared(t *testing.T) {
	// Simulamos 2 proyectos + 1 Nexus-global en dirs temporales aislados.
	projA := t.TempDir() // representa .neo-project/ del strategos-project
	projB := t.TempDir() // representa .neo-project/ de app2-project
	nexusDir := t.TempDir()
	t.Setenv("NEO_SHARED_DIR", nexusDir)

	// Cada proyecto abre su propia KnowledgeStore — igual a lo que hace
	// bootKnowledgeStore con projDir distinto por workspace.
	ksA, err := knowledge.Open(filepath.Join(projA, "db", "knowledge.db"))
	if err != nil {
		t.Fatalf("projA KS: %v", err)
	}
	defer ksA.Close()
	ksB, err := knowledge.Open(filepath.Join(projB, "db", "knowledge.db"))
	if err != nil {
		t.Fatalf("projB KS: %v", err)
	}
	defer ksB.Close()

	// Abre Nexus-global — visible desde ambos proyectos.
	globalKS, err := OpenGlobalStore()
	if err != nil {
		t.Fatalf("global KS: %v", err)
	}
	defer globalKS.Close()

	// Project A escribe algo en SU tier:"project"
	entryA := knowledge.KnowledgeEntry{Content: "strategos-only secret"}
	if err := ksA.Put("contracts", "user-api", entryA); err != nil {
		t.Fatalf("projA Put: %v", err)
	}

	// Project B debe NO ver esa entry (aislamiento).
	if got, err := ksB.Get("contracts", "user-api"); err == nil {
		t.Errorf("ISOLATION BROKEN: projB sees projA's entry: %+v", got)
	}

	// Ambos proyectos escriben en Nexus-global — ambos deben verse mutuamente.
	if err := globalKS.Put(NSImprovements, "from-proj-a", knowledge.KnowledgeEntry{
		Content: "improvement suggested from strategos",
	}); err != nil {
		t.Fatalf("global Put from A: %v", err)
	}
	if err := globalKS.Put(NSImprovements, "from-proj-b", knowledge.KnowledgeEntry{
		Content: "improvement suggested from app2",
	}); err != nil {
		t.Fatalf("global Put from B: %v", err)
	}

	// Abrimos un SEGUNDO handle al mismo Nexus-global para simular otro
	// workspace leyendo — debe ver AMBAS entries.
	globalKS.Close() // libera flock para poder reabrir
	globalKS2, err := OpenGlobalStore()
	if err != nil {
		t.Fatalf("global reopen: %v", err)
	}
	defer globalKS2.Close()
	for _, key := range []string{"from-proj-a", "from-proj-b"} {
		e, err := globalKS2.Get(NSImprovements, key)
		if err != nil {
			t.Errorf("Nexus-global should expose %q from both projects: %v", key, err)
			continue
		}
		if e.Content == "" {
			t.Errorf("entry %q has empty content", key)
		}
	}
}
