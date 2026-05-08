package main

import (
	"context"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/pubsub"
)

// TestCheckNexusDebtAtBoot_NilGuards verifies the function returns cleanly
// on nil cfg/bus and when workspaceID cannot be resolved (graceful degrade
// when Nexus or registry are offline). [353.A]
func TestCheckNexusDebtAtBoot_NilGuards(t *testing.T) {
	// nil cfg → no-op.
	checkNexusDebtAtBoot(context.Background(), "/tmp", nil, pubsub.NewBus())
	// nil bus → no-op.
	checkNexusDebtAtBoot(context.Background(), "/tmp", &config.NeoConfig{}, nil)
	// Unregistered workspace path → early return via empty workspaceID.
	bus := pubsub.NewBus()
	sub, unsub := bus.Subscribe()
	defer unsub()
	cfg := &config.NeoConfig{}
	cfg.Server.NexusDispatcherPort = 9000
	checkNexusDebtAtBoot(context.Background(), "/does/not/exist", cfg, bus)
	// Verify no event fired (non-blocking read).
	select {
	case ev := <-sub:
		t.Errorf("unexpected event on unregistered workspace: %+v", ev)
	default:
		// expected: no event
	}
}

// TestNexusDebtWarningEventTypeExists guards the constant so a rename breaks
// the test instead of the runtime. [353.A]
func TestNexusDebtWarningEventTypeExists(t *testing.T) {
	if pubsub.EventNexusDebtWarning == "" {
		t.Error("EventNexusDebtWarning must have a non-empty string value")
	}
	if pubsub.EventNexusDebtWarning != "nexus_debt_warning" {
		t.Errorf("EventNexusDebtWarning = %q, want nexus_debt_warning", pubsub.EventNexusDebtWarning)
	}
}
