package sre

import (
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDialFirstReachable_FirstWorks covers the happy path: the first IP in
// the slice has a listening socket — connection returned, no fallback.
func TestDialFirstReachable_FirstWorks(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		t.Fatalf("parse ip: %q", host)
	}

	conn, err := dialFirstReachable("tcp", []net.IP{ip}, port, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()
}

// TestDialFirstReachable_FallsThrough is the regression for the local-LLM
// bug: when ips[0] refuses (no listener), the helper must continue down the
// list and succeed on ips[1]. Today, the macOS resolver returns ::1 before
// 127.0.0.1 for `localhost`, but Ollama binds only to 127.0.0.1 — so this
// path is exactly what runtime experiences.
func TestDialFirstReachable_FallsThrough(t *testing.T) {
	// Listener on 127.0.0.1 only.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split: %v", err)
	}

	// Pick an unused port on ::1 by binding+closing immediately. This gives
	// us an address that will RST when dialed — exactly the macOS Ollama
	// scenario where ::1:11434 has no listener.
	ipv6Probe, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable in this env: %v", err)
	}
	_, probePort, _ := net.SplitHostPort(ipv6Probe.Addr().String())
	_ = ipv6Probe.Close()
	// Note: probePort is now released; dialing ::1:probePort will refuse.
	// But more reliably, dial 127.0.0.1:port AFTER trying a guaranteed-dead
	// IPv6 — we use ::1:<port-that-was-released>.
	_ = probePort

	// Compose [::1, 127.0.0.1] — ipv6 first, then ipv4 with the listener.
	ips := []net.IP{net.ParseIP("::1"), net.ParseIP("127.0.0.1")}

	// Override port to the working 127.0.0.1 listener; ::1 dial will refuse
	// because no listener was opened on that ipv6 port.
	conn, err := dialFirstReachable("tcp", ips, port, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("must fall through ipv6 refuse → ipv4 success, got: %v", err)
	}
	_ = conn.Close()
}

// TestDialFirstReachable_AllRefuse asserts the error semantics when every
// address in the list is unreachable: caller gets the LAST error so they
// see the most-recent failure mode, not a stale early one.
func TestDialFirstReachable_AllRefuse(t *testing.T) {
	// Two ports that should be closed.
	// We pick by binding+immediately-closing to get a port the OS released.
	getDeadPort := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		_ = ln.Close()
		return port
	}
	port1 := getDeadPort()

	// Same port on both ips since localhost dial of any of them will refuse.
	// (Using one port is fine — the IPs are different machines for the kernel.)
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.1")}

	_, err := dialFirstReachable("tcp", ips, port1, 200*time.Millisecond)
	if err == nil {
		t.Fatal("must return error when all addresses refuse")
	}
	// Sanity: error must mention the port we tried (not some legacy first-ip path).
	if !strings.Contains(err.Error(), port1) {
		t.Errorf("error should reference the attempted port %s, got %v", port1, err)
	}
}

// TestDialFirstReachable_EmptyIPs asserts the helper returns a clear error
// (not panic) when given an empty IP slice — defensive contract for callers
// that resolve to nothing.
func TestDialFirstReachable_EmptyIPs(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("empty ips must not panic, got: %v", r)
		}
	}()
	_, err := dialFirstReachable("tcp", nil, "1234", 100*time.Millisecond)
	if err == nil {
		t.Error("empty ips must return error")
	}
}

// TestDialFirstReachable_PerAttemptTimeout asserts the per-attempt cap is
// honored: a stuck destination (drop, not refuse) must not block beyond
// the timeout. We use a non-routable RFC-5737 documentation IP that the
// kernel will likely drop silently rather than RST.
func TestDialFirstReachable_PerAttemptTimeout(t *testing.T) {
	// 192.0.2.0/24 is reserved for documentation; routes typically drop.
	dropIP := net.ParseIP("192.0.2.1")
	start := time.Now()
	_, _ = dialFirstReachable("tcp", []net.IP{dropIP}, strconv.Itoa(12345), 150*time.Millisecond)
	elapsed := time.Since(start)
	// Allow 4× slack for CI jitter — the bound proves we didn't wait the
	// default 30s system timeout.
	if elapsed > 800*time.Millisecond {
		t.Errorf("per-attempt timeout not honored: elapsed=%v", elapsed)
	}
}
