package main

// Basic smoke tests — ensure the rewritten TUI constructs cleanly, tab
// navigation wraps correctly, and the refresh cycle doesn't panic when
// the backend is absent. [PILAR-XXVII/246.Q]

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestNewModel_ZeroValue(t *testing.T) {
	c := NewClient("")
	m := newModel(c, "test-ws", "Test Workspace")
	if m.wsID != "test-ws" {
		t.Errorf("wsID = %q, want test-ws", m.wsID)
	}
	if m.tab != 0 {
		t.Errorf("initial tab = %d, want 0", m.tab)
	}
	if m.status != "connecting" {
		t.Errorf("initial status = %q, want connecting", m.status)
	}
	if m.client == nil {
		t.Error("client should not be nil")
	}
}

func TestModel_TabNavigation(t *testing.T) {
	c := NewClient("")
	m := newModel(c, "x", "X")
	// Right cycles forward.
	for i := 0; i < len(tabNames); i++ {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("tab")})
		m = next.(model)
	}
	if m.tab != 0 {
		t.Errorf("after full cycle, tab = %d, want 0 (wrap)", m.tab)
	}
	// Number jumps directly.
	jumpTo, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'5'}})
	m = jumpTo.(model)
	if m.tab != 4 {
		t.Errorf("after '5' jump, tab = %d, want 4", m.tab)
	}
	// Left wraps backward.
	back, _ := m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = back.(model)
	if m.tab != 3 {
		t.Errorf("after left, tab = %d, want 3", m.tab)
	}
}

func TestModel_MetricsMsg_OK(t *testing.T) {
	c := NewClient("")
	m := newModel(c, "x", "X")
	snap := &Snapshot{WorkspaceID: "x", WorkspaceName: "X", GeneratedAt: time.Now()}
	nm, _ := m.Update(metricsMsg{snap: snap})
	m = nm.(model)
	if m.status != "ok" {
		t.Errorf("status = %q, want ok", m.status)
	}
	if m.snap == nil {
		t.Error("snap not stored")
	}
}

func TestModel_View_NoSnapYet(t *testing.T) {
	c := NewClient("")
	m := newModel(c, "x", "X")
	view := m.View()
	if len(view) == 0 {
		t.Error("View() returned empty string")
	}
}

func TestNewClient_Defaults(t *testing.T) {
	c := NewClient("")
	if c.NexusBase != "http://127.0.0.1:9000" {
		t.Errorf("default NexusBase = %q, want http://127.0.0.1:9000", c.NexusBase)
	}
}
