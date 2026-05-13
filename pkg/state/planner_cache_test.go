package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writePlan creates .neo/master_plan.md inside workspace with given content.
func writePlan(t *testing.T, workspace, content string) {
	t.Helper()
	dir := filepath.Join(workspace, ".neo")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "master_plan.md"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// resetPlanCache flushes the planner cache between tests so atomic counters
// reflect only this test's behaviour.
func resetPlanCache(t *testing.T) {
	t.Helper()
	planCacheMu.Lock()
	planCache = make(map[string]*planParseEntry)
	planCacheMu.Unlock()
	planCacheHits.Store(0)
	planCacheMisses.Store(0)
	planCacheStale.Store(0)
}

func TestReadActivePhase_CacheMissThenHit(t *testing.T) {
	resetPlanCache(t)
	ws := t.TempDir()
	writePlan(t, ws, "## ÉPICA 1\n- [ ] open task\n")

	got1, err1 := ReadActivePhase(ws)
	if err1 != nil {
		t.Fatal(err1)
	}
	got2, err2 := ReadActivePhase(ws)
	if err2 != nil {
		t.Fatal(err2)
	}
	if got1 != got2 {
		t.Errorf("identical calls returned different output:\nfirst=%q\nsecond=%q", got1, got2)
	}
	st := GetPlannerCacheStats()
	if st.Hits < 1 {
		t.Errorf("expected at least 1 hit after second call, got %d", st.Hits)
	}
	if st.Entries != 1 {
		t.Errorf("expected 1 cache entry, got %d", st.Entries)
	}
}

func TestReadActivePhase_InvalidatesOnMtimeChange(t *testing.T) {
	resetPlanCache(t)
	ws := t.TempDir()
	writePlan(t, ws, "## ÉPICA 1\n- [ ] task A\n")
	first, _ := ReadActivePhase(ws)
	if first == "" {
		t.Fatal("first read returned empty")
	}
	// Wait for filesystem mtime tick, then rewrite with different content.
	time.Sleep(15 * time.Millisecond)
	writePlan(t, ws, "## ÉPICA 2\n- [ ] task B different\n")
	second, _ := ReadActivePhase(ws)
	if second == first {
		t.Errorf("rewrite should invalidate cache; got same content twice: %q", first)
	}
	st := GetPlannerCacheStats()
	if st.Stale < 1 {
		t.Errorf("expected at least 1 stale invalidation, got %d", st.Stale)
	}
}

func TestReadActivePhase_NoFileReturnsError(t *testing.T) {
	resetPlanCache(t)
	ws := t.TempDir()
	// no master_plan.md
	got, err := ReadActivePhase(ws)
	if err == nil {
		t.Errorf("expected error for missing file, got data: %q", got)
	}
	// No entry should be cached.
	if GetPlannerCacheStats().Entries != 0 {
		t.Errorf("cache should be empty when file missing, got entries=%d", GetPlannerCacheStats().Entries)
	}
}

func TestReadOpenTasks_SharesCacheWithReadActivePhase(t *testing.T) {
	resetPlanCache(t)
	ws := t.TempDir()
	writePlan(t, ws, "## ÉPICA 1\n### Section\n- [ ] open\n- [x] closed\n")

	// First call populates the cache for both views.
	_, err := ReadActivePhase(ws)
	if err != nil {
		t.Fatal(err)
	}
	// ReadOpenTasks should hit the cache (no second parse).
	beforeMisses := GetPlannerCacheStats().Misses
	openTasks, err := ReadOpenTasks(ws)
	if err != nil {
		t.Fatal(err)
	}
	afterMisses := GetPlannerCacheStats().Misses
	if afterMisses != beforeMisses {
		t.Errorf("ReadOpenTasks should hit cache; misses went %d → %d", beforeMisses, afterMisses)
	}
	if openTasks == "" {
		t.Error("openTasks empty — expected at least the open line")
	}
}

func TestInvalidatePlannerCache_Works(t *testing.T) {
	resetPlanCache(t)
	ws := t.TempDir()
	writePlan(t, ws, "## ÉPICA 1\n- [ ] task\n")
	if _, err := ReadActivePhase(ws); err != nil {
		t.Fatal(err)
	}
	if GetPlannerCacheStats().Entries != 1 {
		t.Errorf("expected 1 entry, got %d", GetPlannerCacheStats().Entries)
	}
	InvalidatePlannerCache(ws)
	if GetPlannerCacheStats().Entries != 0 {
		t.Errorf("InvalidatePlannerCache should empty the entry, got %d", GetPlannerCacheStats().Entries)
	}
}

func TestReadActivePhase_NoOpenTasksMessage(t *testing.T) {
	resetPlanCache(t)
	ws := t.TempDir()
	writePlan(t, ws, "## ÉPICA 1\n- [x] all done\n")
	got, err := ReadActivePhase(ws)
	if err != nil {
		t.Fatal(err)
	}
	if got != "🎉 NO HAY FASES PENDIENTES. Todas las tareas de master_plan.md están marcadas con [x]. Épica completada." {
		t.Errorf("unexpected closed-epic message: %q", got)
	}
	// Cache the result and verify hit.
	got2, _ := ReadActivePhase(ws)
	if got != got2 {
		t.Errorf("cached output differs: %q vs %q", got, got2)
	}
	if GetPlannerCacheStats().Hits < 1 {
		t.Error("closed-epic result should also hit cache on second call")
	}
}
