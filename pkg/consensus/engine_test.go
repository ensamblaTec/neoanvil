package consensus

import (
	"context"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/config"
)

func testConfig(enabled bool, quorum float64) *config.NeoConfig {
	return &config.NeoConfig{
		SRE: config.SREConfig{
			ConsensusEnabled: enabled,
			ConsensusQuorum:  quorum,
		},
		AI: config.AIConfig{
			BaseURL: "http://localhost:11434",
		},
		Inference: config.InferenceConfig{
			OllamaModel: "qwen2:0.5b",
		},
	}
}

func TestEvaluateDisabled(t *testing.T) {
	ce := NewConsensusEngine(testConfig(false, 0.66))
	result, err := ce.Evaluate(context.Background(), "test mutation")
	if err != nil {
		t.Fatal(err)
	}
	if result.VetoActive {
		t.Error("disabled consensus should not veto")
	}
	if result.Agreement != 1.0 {
		t.Errorf("disabled consensus should have agreement=1.0, got %.2f", result.Agreement)
	}
}

func TestCircuitBreakerDegradedMode(t *testing.T) {
	ce := NewConsensusEngine(testConfig(true, 0.66))

	// Simulate 3 consecutive failures
	ce.failCount.Store(3)

	result, err := ce.Evaluate(context.Background(), "test mutation")
	if err != nil {
		t.Fatal(err)
	}
	if result.VetoActive {
		t.Error("circuit breaker should auto-approve in degraded mode")
	}
	if result.VetoReason != "consensus_unavailable — degraded mode" {
		t.Errorf("expected degraded mode reason, got: %s", result.VetoReason)
	}
}

func TestCircuitBreakerReset(t *testing.T) {
	ce := NewConsensusEngine(testConfig(true, 0.66))

	ce.failCount.Store(2)
	// Not yet at threshold
	if ce.failCount.Load() >= 3 {
		t.Error("should not be in circuit breaker with 2 failures")
	}

	// Reset
	ce.failCount.Store(0)
	if ce.failCount.Load() != 0 {
		t.Error("fail count should be reset to 0")
	}
}

func TestModelPool(t *testing.T) {
	ce := NewConsensusEngine(testConfig(true, 0.66))
	models := ce.modelPool()
	if len(models) == 0 {
		t.Error("model pool should not be empty when consensus enabled")
	}
}

func TestBuildVetoReason_WithRejections(t *testing.T) {
	ce := NewConsensusEngine(testConfig(true, 0.66))
	result := ConsensusResult{
		Agreement: 0.33,
		Quorum:    0.66,
		Verdicts: []Verdict{
			{Model: "model-a", Approved: false, Reason: "race condition"},
			{Model: "model-b", Approved: true, Reason: "looks ok"},
		},
	}
	reason := ce.buildVetoReason(result)
	if !containsStr(reason, "VETO") {
		t.Errorf("veto reason should contain VETO: %q", reason)
	}
	if !containsStr(reason, "model-a") {
		t.Errorf("veto reason should mention rejecting model: %q", reason)
	}
	if !containsStr(reason, "race condition") {
		t.Errorf("veto reason should include rejection cause: %q", reason)
	}
}

func TestBuildVetoReason_AllApproved(t *testing.T) {
	ce := NewConsensusEngine(testConfig(true, 0.66))
	result := ConsensusResult{
		Agreement: 1.0,
		Quorum:    0.66,
		Verdicts: []Verdict{
			{Model: "model-a", Approved: true, Reason: "solid"},
		},
	}
	reason := ce.buildVetoReason(result)
	// No rejections → rejections list is empty but format still wraps
	if !containsStr(reason, "100%") {
		t.Errorf("veto reason should show 100%% agreement: %q", reason)
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
