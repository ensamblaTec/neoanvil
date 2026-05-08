package sre

import (
	"os"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/config"
)

// testSentinelConfig returns a SentinelConfig with default values for testing.
func testSentinelConfig() config.SentinelConfig {
	return config.SentinelConfig{
		HeapThresholdMB:         500,
		GoroutineExplosionLimit: 10000,
		ColdStartGraceSec:       30,
		AuditLogMaxSize:         1000,
		DreamCycleCount:         3,
		ImmunityConfidenceInit:  0.6,
		ImmunityActivationMin:   0.5,
	}
}

func TestPolicyEngineDenyDaemonInPairMode(t *testing.T) {
	os.Setenv("NEO_SERVER_MODE", "pair")
	defer os.Unsetenv("NEO_SERVER_MODE")

	pe := NewPolicyEngine(testSentinelConfig())
	d := pe.Evaluate("daemon_exec", nil)
	if d.Allowed {
		t.Error("daemon action should be denied in pair mode")
	}
	if d.Rule != "mode_isolation" {
		t.Errorf("expected mode_isolation rule, got %s", d.Rule)
	}
}

func TestPolicyEngineAllowDaemonInDaemonMode(t *testing.T) {
	os.Setenv("NEO_SERVER_MODE", "daemon")
	defer os.Unsetenv("NEO_SERVER_MODE")

	pe := NewPolicyEngine(testSentinelConfig())
	d := pe.Evaluate("daemon_exec", nil)
	if !d.Allowed {
		t.Error("daemon action should be allowed in daemon mode")
	}
}

func TestPolicyEnginePhoenixRequiresArmed(t *testing.T) {
	pe := NewPolicyEngine(testSentinelConfig())

	d := pe.Evaluate("phoenix_protocol", nil)
	if d.Allowed {
		t.Error("phoenix should be denied without armed flag")
	}

	d = pe.Evaluate("phoenix_protocol", map[string]string{"phoenix_armed": "true"})
	if !d.Allowed {
		t.Errorf("phoenix should be allowed with armed flag, got: %s", d.Reason)
	}
}

func TestPolicyEngineDestructiveGuard(t *testing.T) {
	pe := NewPolicyEngine(testSentinelConfig())

	d := pe.Evaluate("rm_rf", nil)
	if d.Allowed {
		t.Error("rm_rf should be denied without explicit approval")
	}

	d = pe.Evaluate("rm_rf", map[string]string{"explicit_approval": "true"})
	if !d.Allowed {
		t.Errorf("rm_rf should be allowed with explicit approval, got: %s", d.Reason)
	}
}

func TestVerifyInvariantsCleanLog(t *testing.T) {
	pe := NewPolicyEngine(testSentinelConfig())
	checks := pe.VerifyInvariants()
	for _, c := range checks {
		if !c.Holds {
			t.Errorf("invariant %s violated on clean log: %s", c.Name, c.Example)
		}
	}
}

func TestAuditLogBounded(t *testing.T) {
	pe := NewPolicyEngine(testSentinelConfig())
	for range 1500 {
		pe.Evaluate("test_action", nil)
	}
	log := pe.AuditLog()
	if len(log) > 1000 {
		t.Errorf("audit log should be bounded to 1000, got %d", len(log))
	}
}
