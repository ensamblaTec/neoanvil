package nexus

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestNewFleetRegistry_Empty [Épica 231.E]
func TestNewFleetRegistry_Empty(t *testing.T) {
	f := NewFleetRegistry()
	if f == nil {
		t.Fatal("NewFleetRegistry returned nil")
	}
	if got := f.All(); len(got) != 0 {
		t.Errorf("expected 0 nodes on empty registry, got %d", len(got))
	}
}

// TestRegister_AndLookup [Épica 231.E]
func TestRegister_AndLookup(t *testing.T) {
	f := NewFleetRegistry()
	f.Register(RemoteNode{Host: "10.0.0.1", Port: 9142, WorkspaceID: "alpha"})
	node := f.Lookup("alpha")
	if node == nil {
		t.Fatal("Lookup should find registered workspace")
	}
	if node.Host != "10.0.0.1" || node.Port != 9142 {
		t.Errorf("node details mismatch: %+v", node.RemoteNode)
	}
	if node.Status != FleetUnknown {
		t.Errorf("initial status should be FleetUnknown, got %s", node.Status)
	}
}

// TestLookup_MissingWorkspace [Épica 231.E]
func TestLookup_MissingWorkspace(t *testing.T) {
	f := NewFleetRegistry()
	if got := f.Lookup("ghost"); got != nil {
		t.Errorf("expected nil for unknown workspace, got %+v", got)
	}
}

// TestLookup_UnreachableHidden [Épica 231.E]
func TestLookup_UnreachableHidden(t *testing.T) {
	f := NewFleetRegistry()
	f.Register(RemoteNode{Host: "x", Port: 1, WorkspaceID: "dead"})
	// Force into unreachable state.
	f.nodes["dead"].Status = FleetUnreachable
	if got := f.Lookup("dead"); got != nil {
		t.Error("Lookup must hide unreachable nodes")
	}
}

// TestAll_SnapshotCount [Épica 231.E]
func TestAll_SnapshotCount(t *testing.T) {
	f := NewFleetRegistry()
	f.Register(RemoteNode{Host: "a", Port: 1, WorkspaceID: "ws1"})
	f.Register(RemoteNode{Host: "b", Port: 2, WorkspaceID: "ws2"})
	f.Register(RemoteNode{Host: "c", Port: 3, WorkspaceID: "ws3"})
	snap := f.All()
	if len(snap) != 3 {
		t.Errorf("expected 3 snapshots, got %d", len(snap))
	}
}

// TestHeartbeat_MarksHealthy [Épica 231.E]
func TestHeartbeat_MarksHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Parse srv.URL host:port.
	var host string
	var port int
	_, _ = srv.URL, srv.URL
	// httptest.NewServer returns URL of form http://127.0.0.1:PORT
	// use net/url to parse.
	hp := parseHostPort(t, srv.URL)
	host = hp.host
	port = hp.port

	f := NewFleetRegistry()
	f.Register(RemoteNode{Host: host, Port: port, WorkspaceID: "alive"})
	f.Heartbeat(http.DefaultClient)
	// Heartbeat spawns a goroutine per node; give it a moment.
	time.Sleep(80 * time.Millisecond)

	node := f.Lookup("alive")
	if node == nil {
		t.Fatal("node should still be reachable")
	}
	if node.Status != FleetHealthy {
		t.Errorf("expected FleetHealthy after successful /health, got %s", node.Status)
	}
	if node.FailureStreak != 0 {
		t.Errorf("FailureStreak should reset to 0, got %d", node.FailureStreak)
	}
}

// TestHeartbeat_FailureStreakReachesUnreachable [Épica 231.E]
func TestHeartbeat_FailureStreakReachesUnreachable(t *testing.T) {
	f := NewFleetRegistry()
	// Point at an unused port — connection will always refuse.
	f.Register(RemoteNode{Host: "127.0.0.1", Port: 1, WorkspaceID: "broken"})

	// Run heartbeat 3 times to accumulate 3 failures → FleetUnreachable.
	for range 3 {
		f.Heartbeat(&http.Client{Timeout: 100 * time.Millisecond})
		time.Sleep(150 * time.Millisecond)
	}

	// Lookup hides unreachable — query raw map via All().
	found := false
	for _, s := range f.All() {
		if s.WorkspaceID == "broken" {
			found = true
			if s.Status != FleetUnreachable {
				t.Errorf("expected FleetUnreachable after 3 failures, got %s", s.Status)
			}
			if s.FailureStreak < 3 {
				t.Errorf("FailureStreak should be >= 3, got %d", s.FailureStreak)
			}
		}
	}
	if !found {
		t.Error("broken node missing from All()")
	}
}

// helper
type hostport struct {
	host string
	port int
}

func parseHostPort(t *testing.T, url string) hostport {
	t.Helper()
	// url: http://127.0.0.1:PORT
	var host string
	var port int
	if _, err := fmtScanURL(url, &host, &port); err != nil {
		t.Fatalf("parseHostPort %q: %v", url, err)
	}
	return hostport{host, port}
}

// fmtScanURL parses "http://H:P" into host + port without pulling net/url.
func fmtScanURL(url string, host *string, port *int) (int, error) {
	// strip scheme
	const prefix = "http://"
	u := url
	if len(u) > len(prefix) && u[:len(prefix)] == prefix {
		u = u[len(prefix):]
	}
	// split host:port
	for i := len(u) - 1; i >= 0; i-- {
		if u[i] == ':' {
			*host = u[:i]
			return fmtSscanInt(u[i+1:], port)
		}
	}
	return 0, errNoColon
}

func fmtSscanInt(s string, p *int) (int, error) {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			break
		}
		n = n*10 + int(s[i]-'0')
	}
	*p = n
	return 1, nil
}

var errNoColon = &simpleErr{"no colon in host:port"}

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }

func TestResolveHost_Found(t *testing.T) {
	f := NewFleetRegistry()
	f.Register(RemoteNode{Host: "192.168.1.1", Port: 9200, WorkspaceID: "ws-remote"})
	f.nodes["ws-remote"].Status = FleetHealthy

	hp := f.ResolveHost("ws-remote")
	if hp.Host != "192.168.1.1" {
		t.Errorf("Host mismatch: %q", hp.Host)
	}
	if hp.Port != 9200 {
		t.Errorf("Port mismatch: %d", hp.Port)
	}
	if !hp.IsRemote {
		t.Error("should be marked IsRemote")
	}
}

func TestResolveHost_NotFound(t *testing.T) {
	f := NewFleetRegistry()
	hp := f.ResolveHost("ghost-ws")
	if hp.Host != "" || hp.Port != 0 || hp.IsRemote {
		t.Errorf("unexpected non-zero HostPort for missing workspace: %+v", hp)
	}
}

func TestFleetStatusReport_Mixed(t *testing.T) {
	f := NewFleetRegistry()
	f.Register(RemoteNode{Host: "a", Port: 1, WorkspaceID: "ws1"})
	f.Register(RemoteNode{Host: "b", Port: 2, WorkspaceID: "ws2"})
	f.Register(RemoteNode{Host: "c", Port: 3, WorkspaceID: "ws3"})
	f.nodes["ws1"].Status = FleetHealthy
	f.nodes["ws2"].Status = FleetUnreachable

	report := f.FleetStatusReport()
	if report["total_nodes"].(int) != 3 {
		t.Errorf("total_nodes should be 3, got %v", report["total_nodes"])
	}
	if report["healthy"].(int) != 1 {
		t.Errorf("healthy should be 1, got %v", report["healthy"])
	}
	if report["unreachable"].(int) != 1 {
		t.Errorf("unreachable should be 1, got %v", report["unreachable"])
	}
}

func TestFleetStatusReport_Empty(t *testing.T) {
	f := NewFleetRegistry()
	report := f.FleetStatusReport()
	if report["total_nodes"].(int) != 0 {
		t.Errorf("empty registry: total_nodes should be 0, got %v", report["total_nodes"])
	}
}
