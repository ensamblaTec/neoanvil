package telemetry

import (
	"testing"
)

// resetHeatmap clears the package globals so each test starts clean.
// Called via t.Cleanup so parallel tests don't leak state. [Épica 230.E]
func resetHeatmap(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		heatMu.Lock()
		if heatDB != nil {
			_ = heatDB.Close()
			heatDB = nil
		}
		heatPath = ""
		heatMu.Unlock()
	})
	heatMu.Lock()
	if heatDB != nil {
		_ = heatDB.Close()
		heatDB = nil
	}
	heatPath = ""
	heatMu.Unlock()
}

// seedWorkspace creates the .neo/db subdir under a tempdir and returns
// the workspace path. InitHeatmap expects that layout.
func seedWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := testMkdirAll(dir + "/.neo/db"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInitHeatmap_CreatesBuckets(t *testing.T) {
	resetHeatmap(t)
	ws := seedWorkspace(t)
	if err := InitHeatmap(ws); err != nil {
		t.Fatalf("InitHeatmap: %v", err)
	}
	if heatDB == nil {
		t.Fatal("heatDB still nil after init")
	}
}

func TestRecordMutation_IncrementsCounter(t *testing.T) {
	resetHeatmap(t)
	ws := seedWorkspace(t)
	if err := InitHeatmap(ws); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := RecordMutation("pkg/rag/hnsw.go"); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	top, err := GetTopHotspots(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 {
		t.Fatalf("expected 1 hotspot, got %d", len(top))
	}
	if top[0].File != "pkg/rag/hnsw.go" {
		t.Errorf("file mismatch: %s", top[0].File)
	}
	if top[0].Mutations != 3 {
		t.Errorf("expected 3 mutations, got %d", top[0].Mutations)
	}
}

func TestRecordBypassMutation_Separated(t *testing.T) {
	resetHeatmap(t)
	ws := seedWorkspace(t)
	if err := InitHeatmap(ws); err != nil {
		t.Fatal(err)
	}
	_ = RecordMutation("main.go")
	_ = RecordMutation("main.go")
	_ = RecordBypassMutation("main.go")

	top, err := GetTopHotspots(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 {
		t.Fatalf("expected 1 hotspot, got %d", len(top))
	}
	if top[0].Mutations != 2 {
		t.Errorf("certified mutations: want 2, got %d", top[0].Mutations)
	}
	if top[0].Bypassed != 1 {
		t.Errorf("bypassed mutations: want 1, got %d", top[0].Bypassed)
	}
}

func TestGetTopHotspots_OrderedByCertifiedCount(t *testing.T) {
	resetHeatmap(t)
	ws := seedWorkspace(t)
	if err := InitHeatmap(ws); err != nil {
		t.Fatal(err)
	}
	// Seed three files with different counts.
	for i := 0; i < 5; i++ {
		_ = RecordMutation("hot.go")
	}
	for i := 0; i < 3; i++ {
		_ = RecordMutation("warm.go")
	}
	_ = RecordMutation("cold.go")

	top, err := GetTopHotspots(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 3 {
		t.Fatalf("expected 3 hotspots, got %d", len(top))
	}
	if top[0].File != "hot.go" || top[0].Mutations != 5 {
		t.Errorf("expected hot.go @5 first, got %s @%d", top[0].File, top[0].Mutations)
	}
	if top[1].File != "warm.go" || top[1].Mutations != 3 {
		t.Errorf("expected warm.go @3 second, got %s @%d", top[1].File, top[1].Mutations)
	}
	if top[2].File != "cold.go" || top[2].Mutations != 1 {
		t.Errorf("expected cold.go @1 third, got %s @%d", top[2].File, top[2].Mutations)
	}
}

func TestGetTopHotspots_LimitRespected(t *testing.T) {
	resetHeatmap(t)
	ws := seedWorkspace(t)
	if err := InitHeatmap(ws); err != nil {
		t.Fatal(err)
	}
	_ = RecordMutation("a.go")
	_ = RecordMutation("b.go")
	_ = RecordMutation("c.go")

	top, _ := GetTopHotspots(2)
	if len(top) != 2 {
		t.Fatalf("expected limit=2, got %d", len(top))
	}
}

func TestGetTopHotspots_EmptyBucket(t *testing.T) {
	resetHeatmap(t)
	ws := seedWorkspace(t)
	if err := InitHeatmap(ws); err != nil {
		t.Fatal(err)
	}
	top, err := GetTopHotspots(5)
	if err != nil {
		t.Errorf("unexpected error on empty bucket: %v", err)
	}
	if len(top) != 0 {
		t.Errorf("expected 0 hotspots on empty bucket, got %d", len(top))
	}
}
