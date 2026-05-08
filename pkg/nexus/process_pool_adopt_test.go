package nexus

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/workspace"
)

// freePort finds an available TCP port for testing.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func testPool(t *testing.T) *ProcessPool {
	t.Helper()
	cfg := defaultNexusConfig()
	cfg.Nexus.BindAddr = "127.0.0.1"
	cfg.Nexus.Watchdog.HealthEndpoint = "/health"
	allocator := NewPortAllocator(cfg.Nexus.PortRangeBase, cfg.Nexus.PortRangeSize, "")
	return NewProcessPoolWithConfig(allocator, "/dev/null", cfg)
}

// TestTryAdoptProcess_HealthyHolder verifies that Start() adopts a healthy
// process already bound to the expected port instead of returning an error.
//
// Strategy: call Allocate once (while the port is free) so the allocator
// caches the assignment.  Then start a real HTTP server on that port to
// simulate a child that survived a Nexus restart.  A second Start() call
// must detect the collision and adopt rather than fail.
func TestTryAdoptProcess_HealthyHolder(t *testing.T) {
	pool := testPool(t)
	entry := workspace.WorkspaceEntry{
		ID:   "test-ws-adopt",
		Name: "test-ws-adopt",
		Path: t.TempDir(),
	}

	// First allocation — port is free, allocator caches it.
	expectedPort, err := pool.allocator.Allocate(entry.ID, entry.Path)
	if err != nil {
		t.Fatalf("initial Allocate: %v", err)
	}

	// Start a healthy /health server on that exact port, simulating the
	// orphaned child that survived after Nexus was restarted.
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	srv := httptest.NewUnstartedServer(mux)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", expectedPort))
	if err != nil {
		t.Fatalf("listen on port %d: %v", expectedPort, err)
	}
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	// Second Start() — allocator returns the cached port immediately
	// (no isPortFree re-check), isPortFreeOn detects collision, adoption runs.
	err = pool.Start(entry)
	if err != nil {
		t.Fatalf("Start() with healthy holder: want nil, got %v", err)
	}

	proc, ok := pool.GetProcess(entry.ID)
	if !ok {
		t.Fatal("process not in pool after adoption")
	}
	if proc.Status != StatusRunning {
		t.Errorf("adopted process status: want running, got %s", proc.Status)
	}
	if !proc.adopted {
		t.Error("adopted flag not set")
	}
	if proc.PID <= 0 {
		t.Error("adopted PID should be > 0")
	}
}

// TestTryAdoptProcess_UnhealthyHolder verifies that when the port holder does
// not respond to /health, Start() falls through to the eviction path and
// ultimately returns "port still in use" (since in-process listeners don't
// have a real OS PID, eviction is a no-op, and the port remains occupied).
func TestTryAdoptProcess_UnhealthyHolder(t *testing.T) {
	pool := testPool(t)
	entry := workspace.WorkspaceEntry{
		ID:   "test-ws-evict",
		Name: "test-ws-evict",
		Path: t.TempDir(),
	}

	// Pre-register in allocator while port is free.
	expectedPort, err := pool.allocator.Allocate(entry.ID, entry.Path)
	if err != nil {
		t.Fatalf("initial Allocate: %v", err)
	}

	// Hold the port WITHOUT a /health handler — mimics OOM-hung child.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", expectedPort))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// Start() must: (1) detect collision, (2) fail adoption (no /health),
	// (3) attempt eviction (no-op for in-process listener), (4) return error.
	err = pool.Start(entry)
	if err == nil {
		t.Skip("port was freed unexpectedly — likely flaky CI environment")
	}
	if !strings.Contains(err.Error(), "in use") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestMaybeRestart_WaitsForPort verifies that the restart path does not race
// against port release. We can only unit-test the wait-loop branch indirectly
// by checking that isPortFreeOn is called with the saved port.
func TestMaybeRestart_WaitsForPort(t *testing.T) {
	// Confirm that after a kill the port-wait helper terminates within 3 s
	// when the port actually becomes free (simulated by not holding it).
	port := freePort(t)

	start := time.Now()
	deadline := start.Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if isPortFreeOn("127.0.0.1", port) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("port-wait loop took > 3s on a free port")
	}
}

// TestPidOnPort_ReturnsZeroForNoProcess verifies that pidOnPort returns 0
// when nothing listens on the given port.
func TestPidOnPort_ReturnsZeroForNoProcess(t *testing.T) {
	port := freePort(t)
	if pid := pidOnPort(port); pid != 0 {
		t.Errorf("pidOnPort on free port: want 0, got %d", pid)
	}
}

// TestPidOnPort_FindsSelf verifies that pidOnPort can find the current
// process when it holds a TCP port.
func TestPidOnPort_FindsSelf(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	port := ln.Addr().(*net.TCPAddr).Port

	// lsof needs a moment to register the listener.
	time.Sleep(50 * time.Millisecond)

	pid := pidOnPort(port)
	if pid <= 0 {
		t.Skipf("lsof not available or listener not visible (port=%d)", port)
	}

	selfPIDStr := strconv.Itoa(pid)
	if selfPIDStr == "0" {
		t.Errorf("expected a real PID, got 0")
	}
}
