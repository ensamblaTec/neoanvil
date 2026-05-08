package inference

import (
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/config"
)

// TestConsumeTokens_Success [Épica 231.C]
func TestConsumeTokens_Success(t *testing.T) {
	g := NewGateway(config.InferenceConfig{CloudTokenBudgetDaily: 1000}, "http://noop")
	if err := g.consumeTokens(100); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if rem := g.budgetRemaining(); rem != 900 {
		t.Errorf("budget remaining: want 900, got %d", rem)
	}
}

// TestConsumeTokens_ExhaustsBudget [Épica 231.C]
func TestConsumeTokens_ExhaustsBudget(t *testing.T) {
	g := NewGateway(config.InferenceConfig{CloudTokenBudgetDaily: 100}, "http://noop")
	if err := g.consumeTokens(60); err != nil {
		t.Fatal(err)
	}
	if err := g.consumeTokens(60); err == nil {
		t.Error("expected budget-exhausted error when exceeding daily cap")
	}
}

// TestBudgetRemaining_Initial [Épica 231.C]
func TestBudgetRemaining_Initial(t *testing.T) {
	g := NewGateway(config.InferenceConfig{CloudTokenBudgetDaily: 500}, "http://noop")
	if rem := g.budgetRemaining(); rem != 500 {
		t.Errorf("initial remaining: want 500, got %d", rem)
	}
}

// TestMaybeResetBudget_RollsOverAtMidnight [Épica 231.C]
func TestMaybeResetBudget_RollsOverAtMidnight(t *testing.T) {
	g := NewGateway(config.InferenceConfig{CloudTokenBudgetDaily: 1000}, "http://noop")
	_ = g.consumeTokens(700)
	// Simulate "yesterday" by setting budgetResetD to a past date.
	g.budgetResetD = "2020-01-01"
	g.maybeResetBudget()
	if g.dailyTokens.Load() != 0 {
		t.Errorf("after reset dailyTokens should be 0, got %d", g.dailyTokens.Load())
	}
	if g.budgetResetD != time.Now().UTC().Format("2006-01-02") {
		t.Errorf("budgetResetD not updated to today: %s", g.budgetResetD)
	}
}

// TestEstimateTokens_RoughHeuristic [Épica 231.C]
func TestEstimateTokens_RoughHeuristic(t *testing.T) {
	// target 100 chars + err 200 chars + 2×50-char flashbacks
	// → 25 + 50 + 12 + 12 = 99 tokens (±)
	got := estimateTokens(
		"a very short target — 30 bytes here, padded to just over 100 chars total XXXXXXXXXXXXXXXXXXXX",
		"error context — longer, fills more of the budget so 200 bytes fits in this string token shape",
		[]string{"flashback one entry", "flashback two entry"},
	)
	if got < 40 || got > 120 {
		t.Errorf("estimateTokens out of expected range 40-120: got %d", got)
	}
}

// TestAutoFixSuccessRate_NoAttempts [Épica 231.C]
func TestAutoFixSuccessRate_NoAttempts(t *testing.T) {
	g := NewGateway(config.InferenceConfig{CloudTokenBudgetDaily: 1}, "http://noop")
	if r := g.AutoFixSuccessRate(); r != 0 {
		t.Errorf("initial rate should be 0, got %v", r)
	}
}

// TestRecordAutoFixAttempt_CountsSuccess [Épica 231.C]
func TestRecordAutoFixAttempt_CountsSuccess(t *testing.T) {
	g := NewGateway(config.InferenceConfig{CloudTokenBudgetDaily: 1}, "http://noop")
	g.RecordAutoFixAttempt(true)
	g.RecordAutoFixAttempt(true)
	g.RecordAutoFixAttempt(false)
	if got := g.AutoFixSuccessRate(); got < 0.65 || got > 0.68 {
		t.Errorf("2/3 success rate expected ~0.666, got %v", got)
	}
}

// TestToday_Format [Épica 231.C]
func TestToday_Format(t *testing.T) {
	s := today()
	// YYYY-MM-DD
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		t.Errorf("today() unexpected format: %q", s)
	}
}
