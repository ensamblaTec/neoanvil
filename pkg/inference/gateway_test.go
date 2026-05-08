package inference

import (
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/config"
)

func TestNewGateway_Fields(t *testing.T) {
	cfg := config.InferenceConfig{
		OllamaBaseURL:       "http://127.0.0.1:11434",
		ConfidenceThreshold: 0.75,
	}
	gw := NewGateway(cfg, "http://fallback")
	if gw.ollamaURL != "http://127.0.0.1:11434" {
		t.Errorf("ollamaURL = %q, want cfg.OllamaBaseURL", gw.ollamaURL)
	}
	if gw.cfg.ConfidenceThreshold != 0.75 {
		t.Errorf("confidence threshold = %.2f, want 0.75", gw.cfg.ConfidenceThreshold)
	}
}

func TestNewGateway_FallbackURL(t *testing.T) {
	cfg := config.InferenceConfig{} // OllamaBaseURL empty
	gw := NewGateway(cfg, "http://ai-fallback")
	if gw.ollamaURL != "http://ai-fallback" {
		t.Errorf("expected fallback URL, got %q", gw.ollamaURL)
	}
}

func TestAutoFixSuccessRate_ZeroAttempts(t *testing.T) {
	gw := NewGateway(config.InferenceConfig{}, "")
	if rate := gw.AutoFixSuccessRate(); rate != 0 {
		t.Errorf("AutoFixSuccessRate with 0 attempts = %.2f, want 0", rate)
	}
}

func TestRecordAutoFixAttempt_SuccessRate(t *testing.T) {
	gw := NewGateway(config.InferenceConfig{}, "")
	gw.RecordAutoFixAttempt(true)
	gw.RecordAutoFixAttempt(true)
	gw.RecordAutoFixAttempt(false)
	rate := gw.AutoFixSuccessRate()
	// 2 successes / 3 attempts ≈ 0.666
	if rate < 0.66 || rate > 0.67 {
		t.Errorf("AutoFixSuccessRate = %.4f, want ~0.666", rate)
	}
}

func TestRecordAutoFixAttempt_AllFailed(t *testing.T) {
	gw := NewGateway(config.InferenceConfig{}, "")
	gw.RecordAutoFixAttempt(false)
	gw.RecordAutoFixAttempt(false)
	if rate := gw.AutoFixSuccessRate(); rate != 0 {
		t.Errorf("all-failed rate = %.2f, want 0", rate)
	}
}

func TestBudgetExhausted_WhenOverLimit(t *testing.T) {
	cfg := config.InferenceConfig{
		CloudTokenBudgetDaily: 100,
	}
	gw := NewGateway(cfg, "")
	gw.dailyTokens.Store(101)
	if !gw.BudgetExhausted() {
		t.Error("expected BudgetExhausted() = true when dailyTokens > CloudTokenBudgetDaily")
	}
}

func TestBudgetExhausted_WithinLimit(t *testing.T) {
	cfg := config.InferenceConfig{
		CloudTokenBudgetDaily: 1000,
	}
	gw := NewGateway(cfg, "")
	gw.dailyTokens.Store(50)
	if gw.BudgetExhausted() {
		t.Error("expected BudgetExhausted() = false when under budget")
	}
}

func TestTokensUsedToday_Initial(t *testing.T) {
	gw := NewGateway(config.InferenceConfig{}, "")
	if n := gw.TokensUsedToday(); n != 0 {
		t.Errorf("initial tokens = %d, want 0", n)
	}
}
