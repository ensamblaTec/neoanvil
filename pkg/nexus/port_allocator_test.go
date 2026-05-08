package nexus

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAllocateDeterministic(t *testing.T) {
	dir := t.TempDir()
	pa := NewPortAllocator(9100, 200, filepath.Join(dir, "ports.json"))

	port1, err := pa.Allocate("ws1", "/home/user/project-a")
	if err != nil {
		t.Fatal(err)
	}
	if port1 < 9100 || port1 >= 9300 {
		t.Errorf("port %d outside range [9100, 9300)", port1)
	}

	// Same workspace → same port (idempotent)
	port2, err := pa.Allocate("ws1", "/home/user/project-a")
	if err != nil {
		t.Fatal(err)
	}
	if port1 != port2 {
		t.Errorf("expected same port %d, got %d", port1, port2)
	}
}

func TestAllocateCollisionResolution(t *testing.T) {
	dir := t.TempDir()
	pa := NewPortAllocator(9100, 10, filepath.Join(dir, "ports.json"))

	ports := make(map[int]bool)
	for i := range 8 {
		port, err := pa.Allocate(
			"ws"+string(rune('0'+i)),
			"/project/"+string(rune('a'+i)),
		)
		if err != nil {
			t.Fatalf("allocation %d failed: %v", i, err)
		}
		if ports[port] {
			t.Errorf("duplicate port %d on allocation %d", port, i)
		}
		ports[port] = true
	}
}

func TestAllocateExhausted(t *testing.T) {
	dir := t.TempDir()
	pa := NewPortAllocator(19100, 3, filepath.Join(dir, "ports.json"))

	for i := range 3 {
		_, err := pa.Allocate("ws"+string(rune('0'+i)), "/p/"+string(rune('a'+i)))
		if err != nil {
			t.Fatalf("allocation %d should succeed: %v", i, err)
		}
	}

	// 4th should fail (range exhausted)
	_, err := pa.Allocate("ws4", "/p/d")
	if err == nil {
		t.Error("expected error when port range exhausted")
	}
}

func TestRelease(t *testing.T) {
	dir := t.TempDir()
	pa := NewPortAllocator(9100, 200, filepath.Join(dir, "ports.json"))

	pa.Allocate("ws1", "/project/a")
	pa.Release("/project/a")

	if port := pa.GetPort("/project/a"); port != 0 {
		t.Errorf("expected 0 after release, got %d", port)
	}
}

func TestAll_ReturnsAllocations(t *testing.T) {
	dir := t.TempDir()
	pa := NewPortAllocator(9100, 200, filepath.Join(dir, "ports.json"))
	pa.Allocate("ws-a", "/project/a")
	pa.Allocate("ws-b", "/project/b")

	all := pa.All()
	if len(all) != 2 {
		t.Errorf("expected 2 allocations, got %d", len(all))
	}
}

func TestAll_EmptyAllocator(t *testing.T) {
	dir := t.TempDir()
	pa := NewPortAllocator(9100, 200, filepath.Join(dir, "ports.json"))
	if got := pa.All(); len(got) != 0 {
		t.Errorf("expected 0 allocations, got %d", len(got))
	}
}

func TestAllocateMockPort_ReturnsValidPort(t *testing.T) {
	port := AllocateMockPort(0, 0) // triggers defaults: base=34800, size=100
	if port == 0 {
		t.Skip("no free port found in default range — environment constrained")
	}
	if port < 34800 || port >= 34900 {
		t.Errorf("port %d outside default range [34800, 34900)", port)
	}
}

func TestAllocateMockPort_SmallRange(t *testing.T) {
	// Use high unprivileged range likely to have a free port
	port := AllocateMockPort(55000, 50)
	if port != 0 && (port < 55000 || port >= 55050) {
		t.Errorf("port %d outside given range [55000, 55050)", port)
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	portsFile := filepath.Join(dir, "ports.json")

	pa1 := NewPortAllocator(9100, 200, portsFile)
	port1, _ := pa1.Allocate("ws1", "/project/x")

	// Load from disk
	pa2 := NewPortAllocator(9100, 200, portsFile)
	port2 := pa2.GetPort("/project/x")

	if port1 != port2 {
		t.Errorf("persistence failed: %d vs %d", port1, port2)
	}

	// Verify file exists
	if _, err := os.Stat(portsFile); os.IsNotExist(err) {
		t.Error("ports file should exist on disk")
	}
}
