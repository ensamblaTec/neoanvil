package sre

import (
	"context"
	"testing"
)

func TestDreamEngineBasicCycle(t *testing.T) {
	de := NewDreamEngine(0.5, 0.6)
	results := de.DreamCycle(context.Background(), 5)

	if len(results) != 5 {
		t.Errorf("expected 5 dream results, got %d", len(results))
	}

	for _, r := range results {
		if r.Scenario.Category == "" {
			t.Error("dream scenario should have a category")
		}
		if !r.Recovered {
			t.Errorf("all default scenarios should have recovery, failed: %s", r.Scenario.Category)
		}
	}
}

func TestImmuneMemoryLookup(t *testing.T) {
	de := NewDreamEngine(0.5, 0.6)

	// Run enough dreams to ensure all 5 scenario types are covered (probabilistic).
	// With 30 cycles and 5 scenarios, P(miss any one) < 0.001%.
	de.DreamCycle(context.Background(), 30)

	// Check immunity for known patterns
	entry, found := de.CheckImmunity("runtime: out of memory")
	if !found {
		t.Fatal("should have immunity for OOM pattern after dreaming")
	}
	if entry.RecoveryAction == "" {
		t.Error("immune entry should have recovery action")
	}
}

func TestImmuneMemoryExportImport(t *testing.T) {
	de := NewDreamEngine(0.5, 0.6)
	de.DreamCycle(context.Background(), 5)

	data, err := de.ExportImmuneMemory()
	if err != nil {
		t.Fatal(err)
	}

	de2 := NewDreamEngine(0.5, 0.6)
	if err := de2.ImportImmuneMemory(data); err != nil {
		t.Fatal(err)
	}

	snap1 := de.ImmuneMemorySnapshot()
	snap2 := de2.ImmuneMemorySnapshot()

	if len(snap2) < len(snap1) {
		t.Errorf("imported memory should have at least %d entries, got %d", len(snap1), len(snap2))
	}
}

func TestDreamLogBounded(t *testing.T) {
	de := NewDreamEngine(0.5, 0.6)
	de.DreamCycle(context.Background(), 600)

	log := de.DreamLog()
	if len(log) > 500 {
		t.Errorf("dream log should be bounded to 500, got %d", len(log))
	}
}
