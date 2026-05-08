package graph

import (
	"math"
	"testing"
)

func TestApplyHubDampening(t *testing.T) {
	baseScore := 10.0

	// Test 1: Inbound degree 0 -> No dampening
	damped0 := ApplyHubDampening(baseScore, 0)
	// log2(2 + 0) = 1.0 => baseScore / 1.0 = baseScore
	if math.Abs(damped0-baseScore) > 1e-5 {
		t.Errorf("Expected dampening to be neutral (%.2f), got %.2f", baseScore, damped0)
	}

	// Test 2: Inbound degree 1022 (God object) -> Heavily penalized
	dampedGod := ApplyHubDampening(baseScore, 1022)
	// log2(2 + 1022) = log2(1024) = 10.0 => baseScore / 10.0
	expected := baseScore / 10.0
	if math.Abs(dampedGod-expected) > 1e-5 {
		t.Errorf("Expected heavy penalization (%.2f), got %.2f", expected, dampedGod)
	}
}
