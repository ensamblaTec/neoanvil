package nexus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestRegistry(t *testing.T) *DebtRegistry {
	t.Helper()
	dir := t.TempDir()
	reg, err := OpenDebtRegistry(DebtConfig{
		Enabled:            true,
		File:               filepath.Join(dir, "nexus_debt.md"),
		DedupWindowMinutes: 15,
		MaxResolvedDays:    30,
	})
	if err != nil {
		t.Fatalf("OpenDebtRegistry: %v", err)
	}
	return reg
}

func TestDebt_AppendAndList(t *testing.T) {
	reg := newTestRegistry(t)
	ev, err := reg.AppendDebt(NexusDebtEvent{
		Priority:           "P0",
		Title:              "strategos BoltDB lock held by zombie PID=77184",
		AffectedWorkspaces: []string{"strategos-32492"},
		Source:             "verify_boot",
		Recommended:        "lsof +D .neo/db/ | kill zombie | restart",
	})
	if err != nil {
		t.Fatalf("AppendDebt: %v", err)
	}
	if ev.ID == "" {
		t.Error("expected generated ID")
	}
	if ev.OccurrenceCount != 1 {
		t.Errorf("OccurrenceCount = %d, want 1", ev.OccurrenceCount)
	}
	if ev.DedupKey == "" {
		t.Error("expected DedupKey to be set")
	}

	list := reg.ListOpen(DebtFilter{})
	if len(list) != 1 {
		t.Fatalf("ListOpen returned %d events, want 1", len(list))
	}
	if list[0].ID != ev.ID {
		t.Errorf("list[0].ID = %s, want %s", list[0].ID, ev.ID)
	}
}

func TestDebt_DedupWithinWindow(t *testing.T) {
	reg := newTestRegistry(t)
	base := NexusDebtEvent{
		Priority:           "P1",
		Title:              "Ollama embed unreachable",
		AffectedWorkspaces: []string{"neoanvil-45913"},
		Source:             "service_manager",
	}
	first, err := reg.AppendDebt(base)
	if err != nil {
		t.Fatal(err)
	}
	second, err := reg.AppendDebt(base)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Errorf("dedup failed: first=%s second=%s", first.ID, second.ID)
	}
	list := reg.ListOpen(DebtFilter{})
	if len(list) != 1 {
		t.Fatalf("expected 1 deduped entry, got %d", len(list))
	}
	if list[0].OccurrenceCount != 2 {
		t.Errorf("OccurrenceCount = %d, want 2 after dedup", list[0].OccurrenceCount)
	}
}

func TestDebt_DedupKeyDifferentWorkspacesDistinct(t *testing.T) {
	reg := newTestRegistry(t)
	a, _ := reg.AppendDebt(NexusDebtEvent{
		Priority: "P1", Title: "port conflict",
		AffectedWorkspaces: []string{"ws-a"}, Source: "port_allocator",
	})
	b, _ := reg.AppendDebt(NexusDebtEvent{
		Priority: "P1", Title: "port conflict",
		AffectedWorkspaces: []string{"ws-b"}, Source: "port_allocator",
	})
	if a.ID == b.ID {
		t.Error("different affected workspaces should produce distinct IDs")
	}
	if len(reg.ListOpen(DebtFilter{})) != 2 {
		t.Error("expected 2 distinct events")
	}
}

func TestDebt_Resolve(t *testing.T) {
	reg := newTestRegistry(t)
	ev, _ := reg.AppendDebt(NexusDebtEvent{
		Priority: "P0", Title: "disk full",
		AffectedWorkspaces: []string{"ws-a"}, Source: "watchdog",
	})
	if err := reg.ResolveDebt(ev.ID, "deleted old logs"); err != nil {
		t.Fatalf("ResolveDebt: %v", err)
	}
	if len(reg.ListOpen(DebtFilter{})) != 0 {
		t.Error("resolved entry should not appear in default ListOpen")
	}
	all := reg.ListOpen(DebtFilter{IncludeResolved: true})
	if len(all) != 1 {
		t.Fatalf("IncludeResolved returned %d, want 1", len(all))
	}
	if all[0].Resolution != "deleted old logs" {
		t.Errorf("resolution = %q", all[0].Resolution)
	}

	// Second resolve is an error.
	if err := reg.ResolveDebt(ev.ID, "again"); err != ErrDebtAlreadyResolved {
		t.Errorf("expected ErrDebtAlreadyResolved, got %v", err)
	}
	if err := reg.ResolveDebt("does-not-exist", "x"); err != ErrDebtNotFound {
		t.Errorf("expected ErrDebtNotFound, got %v", err)
	}
}

func TestDebt_AffectingFilter(t *testing.T) {
	reg := newTestRegistry(t)
	reg.AppendDebt(NexusDebtEvent{Priority: "P0", Title: "a", AffectedWorkspaces: []string{"ws-a"}})
	reg.AppendDebt(NexusDebtEvent{Priority: "P1", Title: "b", AffectedWorkspaces: []string{"ws-b"}})
	reg.AppendDebt(NexusDebtEvent{Priority: "P2", Title: "c", AffectedWorkspaces: []string{"ws-a", "ws-b"}})

	affA := reg.Affecting("ws-a")
	if len(affA) != 2 {
		t.Errorf("ws-a affecting: got %d, want 2", len(affA))
	}
	// Sort: P0 first, then P2.
	if affA[0].Priority != "P0" || affA[1].Priority != "P2" {
		t.Errorf("unexpected priority order: %v", affA)
	}

	affB := reg.Affecting("ws-b")
	if len(affB) != 2 {
		t.Errorf("ws-b affecting: got %d, want 2", len(affB))
	}
}

func TestDebt_PersistAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus_debt.md")
	cfg := DebtConfig{Enabled: true, File: path, DedupWindowMinutes: 15}
	reg1, err := OpenDebtRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ev, _ := reg1.AppendDebt(NexusDebtEvent{
		Priority: "P0", Title: "x", AffectedWorkspaces: []string{"ws-a"}, Source: "verify_boot",
	})

	// Re-open to simulate Nexus restart.
	reg2, err := OpenDebtRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	list := reg2.ListOpen(DebtFilter{})
	if len(list) != 1 {
		t.Fatalf("reload lost events: got %d", len(list))
	}
	if list[0].ID != ev.ID {
		t.Errorf("reloaded ID mismatch: %s vs %s", list[0].ID, ev.ID)
	}

	// File should contain both the JSON block and the rendered table.
	raw, _ := os.ReadFile(path)
	s := string(raw)
	if !strings.Contains(s, "neo-nexus-debt-db-v1") {
		t.Error("persisted file missing JSON marker")
	}
	if !strings.Contains(s, "Open P0") {
		t.Error("persisted file missing P0 section header")
	}
	if !strings.Contains(s, ev.ID) {
		t.Error("persisted file missing event ID in rendered table")
	}
}

func TestDebt_ArchiveOldResolved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus_debt.md")
	cfg := DebtConfig{
		Enabled:         true,
		File:            path,
		MaxResolvedDays: 30,
	}
	reg, _ := OpenDebtRegistry(cfg)
	ev, _ := reg.AppendDebt(NexusDebtEvent{Priority: "P2", Title: "old", AffectedWorkspaces: []string{"ws"}})
	_ = reg.ResolveDebt(ev.ID, "fixed")

	// Age the resolved timestamp on-disk so reload() observes it.
	reg.mu.Lock()
	for i := range reg.events {
		if reg.events[i].ID == ev.ID {
			reg.events[i].ResolvedAt = time.Now().UTC().AddDate(0, 0, -40)
		}
	}
	// Persist directly without the AppendDebt path to avoid reload() overwriting.
	if err := reg.lockFile(); err != nil {
		t.Fatalf("lockFile: %v", err)
	}
	// archiveOldResolved runs inside persist — bypass to keep the aged event
	// on disk for this test.
	savedCutoff := reg.cfg.MaxResolvedDays
	reg.cfg.MaxResolvedDays = 0
	if err := reg.persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}
	reg.cfg.MaxResolvedDays = savedCutoff
	reg.unlockFile()
	reg.mu.Unlock()

	// Trigger another AppendDebt which runs archiveOldResolved inside persist.
	reg.AppendDebt(NexusDebtEvent{Priority: "P2", Title: "fresh", AffectedWorkspaces: []string{"ws2"}})

	all := reg.ListOpen(DebtFilter{IncludeResolved: true})
	for _, e := range all {
		if e.ID == ev.ID {
			t.Errorf("resolved entry older than MaxResolvedDays should have been archived: %+v", e)
		}
	}
}
