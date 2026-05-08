package inference

import (
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/pubsub"
)

func TestNewWatchdog_Fields(t *testing.T) {
	bus := pubsub.NewBus()
	cmds := []string{"go test", "go build"}
	w := NewWatchdog(cmds, 10, bus, "ws-1")
	if w.workspaceID != "ws-1" {
		t.Errorf("workspaceID: got %q, want %q", w.workspaceID, "ws-1")
	}
	if w.maxCycles != 10 {
		t.Errorf("maxCycles: got %d, want 10", w.maxCycles)
	}
	if w.IsUnsupervised() {
		t.Error("should start in supervised mode")
	}
	if w.CyclesUsed() != 0 {
		t.Errorf("initial cycles: got %d, want 0", w.CyclesUsed())
	}
}

func TestWatchdog_EnableDisable(t *testing.T) {
	w := NewWatchdog(nil, 5, nil, "ws")
	w.EnableUnsupervised()
	if !w.IsUnsupervised() {
		t.Error("should be unsupervised after Enable")
	}
	w.DisableUnsupervised()
	if w.IsUnsupervised() {
		t.Error("should be supervised after Disable")
	}
}

func TestWatchdog_IsSafe_PrefixMatch(t *testing.T) {
	w := NewWatchdog([]string{"go test", "go build", "make audit"}, 10, nil, "ws")
	cases := []struct {
		cmd  string
		want bool
	}{
		{"go test ./...", true},
		{"go build ./cmd/neo-mcp/", true},
		{"make audit-ci", true},
		{"rm -rf /", false},
		{"Go Test ./...", true}, // case-insensitive
		{"", false},
	}
	for _, c := range cases {
		if got := w.IsSafe(c.cmd); got != c.want {
			t.Errorf("IsSafe(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestWatchdog_AutoApprove_SupervisedMode(t *testing.T) {
	w := NewWatchdog([]string{"go test"}, 10, nil, "ws")
	res := w.AutoApprove("go test ./...")
	if res.Approved {
		t.Error("should not approve in supervised mode")
	}
}

func TestWatchdog_AutoApprove_UnsafeCommand(t *testing.T) {
	w := NewWatchdog([]string{"go test"}, 10, nil, "ws")
	w.EnableUnsupervised()
	res := w.AutoApprove("rm -rf /")
	if res.Approved {
		t.Error("unsafe command should not be approved")
	}
	if w.CyclesUsed() != 0 {
		t.Error("cycles should not increment for rejected command")
	}
}

func TestWatchdog_AutoApprove_SafeCommand(t *testing.T) {
	bus := pubsub.NewBus()
	w := NewWatchdog([]string{"go test", "make"}, 5, bus, "ws-2")
	w.EnableUnsupervised()
	res := w.AutoApprove("go test -short ./pkg/...")
	if !res.Approved {
		t.Errorf("safe command should be approved, reason: %s", res.Reason)
	}
	if res.CycleNumber != 1 {
		t.Errorf("first approval should be cycle 1, got %d", res.CycleNumber)
	}
	if w.CyclesUsed() != 1 {
		t.Errorf("cycles should be 1 after one approval, got %d", w.CyclesUsed())
	}
}

func TestWatchdog_AutoApprove_ExhaustsMaxCycles(t *testing.T) {
	w := NewWatchdog([]string{"go"}, 2, nil, "ws")
	w.EnableUnsupervised()

	// Exhaust the cycle limit
	w.AutoApprove("go test ./...")
	w.AutoApprove("go test ./...")
	// Third call should exceed maxCycles
	res := w.AutoApprove("go test ./...")
	if res.Approved {
		t.Error("should not approve after max cycles exhausted")
	}
	if w.IsUnsupervised() {
		t.Error("should revert to supervised mode after exhaustion")
	}
}

func TestWatchdog_EnableUnsupervised_ResetsCycles(t *testing.T) {
	w := NewWatchdog([]string{"go"}, 5, nil, "ws")
	w.EnableUnsupervised()
	w.AutoApprove("go test")
	w.AutoApprove("go test")
	if w.CyclesUsed() != 2 {
		t.Fatalf("expected 2 cycles, got %d", w.CyclesUsed())
	}
	w.EnableUnsupervised() // reset
	if w.CyclesUsed() != 0 {
		t.Errorf("EnableUnsupervised should reset cycles, got %d", w.CyclesUsed())
	}
}

func TestWatchdog_NilBus_NoopPublish(t *testing.T) {
	// Ensure no panic when bus is nil
	w := NewWatchdog([]string{"go"}, 5, nil, "ws")
	w.EnableUnsupervised()
	res := w.AutoApprove("go test ./...")
	if !res.Approved {
		t.Errorf("should approve safe cmd with nil bus, reason: %s", res.Reason)
	}
}
