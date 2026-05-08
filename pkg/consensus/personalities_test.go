package consensus

import (
	"strings"
	"testing"
)

// TestAuditorProfile_Defaults [Épica 231.F]
func TestAuditorProfile_Defaults(t *testing.T) {
	p := AuditorProfile()
	if p.Name != "Auditor" {
		t.Errorf("name mismatch: %s", p.Name)
	}
	if !strings.Contains(p.SystemPrompt, "security") {
		t.Errorf("system prompt should mention security, got: %s", p.SystemPrompt[:80])
	}
	if p.EvalWeight <= 0 || p.EvalWeight > 1 {
		t.Errorf("eval weight out of range: %v", p.EvalWeight)
	}
}

// TestOptimizerProfile_Defaults [Épica 231.F]
func TestOptimizerProfile_Defaults(t *testing.T) {
	p := OptimizerProfile()
	if p.Name != "Optimizer" {
		t.Errorf("name mismatch: %s", p.Name)
	}
	if !strings.Contains(p.SystemPrompt, "performance") {
		t.Error("optimizer should reference performance")
	}
}

// TestArchitectProfile_Defaults [Épica 231.F]
func TestArchitectProfile_Defaults(t *testing.T) {
	p := ArchitectProfile()
	if p.Name != "Architect" {
		t.Errorf("name mismatch: %s", p.Name)
	}
	if !strings.Contains(p.SystemPrompt, "complexity") {
		t.Error("architect should reference complexity")
	}
}

// TestProfiles_WeightsSumToOne [Épica 231.F]
func TestProfiles_WeightsSumToOne(t *testing.T) {
	sum := AuditorProfile().EvalWeight + OptimizerProfile().EvalWeight + ArchitectProfile().EvalWeight
	// Allow tiny floating-point drift.
	if sum < 0.999 || sum > 1.001 {
		t.Errorf("weights should sum to 1.0, got %v", sum)
	}
}

// TestBuildConsensusPrompt_Format [Épica 231.F]
func TestBuildConsensusPrompt_Format(t *testing.T) {
	prompt := buildConsensusPrompt("rename foo → bar")
	if !strings.Contains(prompt, "rename foo → bar") {
		t.Error("prompt should embed the mutation verbatim")
	}
	if !strings.Contains(prompt, "APPROVE") || !strings.Contains(prompt, "REJECT") {
		t.Error("prompt should reference APPROVE/REJECT contract")
	}
}

// TestParseVerdict_Approve [Épica 231.F]
func TestParseVerdict_Approve(t *testing.T) {
	approved, conf, reason := parseVerdict("APPROVE 0.9 looks solid")
	if !approved {
		t.Error("should parse approval")
	}
	if conf != 0.9 {
		t.Errorf("confidence 0.9 expected, got %v", conf)
	}
	if reason != "looks solid" {
		t.Errorf("reason mismatch: %q", reason)
	}
}

// TestParseVerdict_Reject [Épica 231.F]
func TestParseVerdict_Reject(t *testing.T) {
	approved, conf, _ := parseVerdict("REJECT 0.75 regression")
	if approved {
		t.Error("should parse rejection")
	}
	if conf != 0.75 {
		t.Errorf("confidence 0.75 expected, got %v", conf)
	}
}

// TestParseVerdict_MissingConfidence [Épica 231.F]
func TestParseVerdict_MissingConfidence(t *testing.T) {
	approved, conf, _ := parseVerdict("APPROVE")
	if !approved {
		t.Error("should still parse APPROVE without confidence")
	}
	if conf != 0.5 {
		t.Errorf("default confidence 0.5 expected, got %v", conf)
	}
}

// TestParseVerdict_GarbageInput [Épica 231.F]
func TestParseVerdict_GarbageInput(t *testing.T) {
	approved, _, _ := parseVerdict("I don't know")
	if approved {
		t.Error("garbage input must NOT parse as approval")
	}
}

// TestParseVerdict_LowercaseAndWhitespace [Épica 231.F]
func TestParseVerdict_LowercaseAndWhitespace(t *testing.T) {
	approved, _, _ := parseVerdict("  approve 0.6 fine  ")
	if !approved {
		t.Error("lowercase + leading/trailing whitespace should still match APPROVE")
	}
}

func TestTruncate_Short(t *testing.T) {
	s := "hello"
	got := truncate(s, 10)
	if got != s {
		t.Errorf("short string should not be truncated: %q", got)
	}
}

func TestTruncate_Exact(t *testing.T) {
	s := "hello"
	got := truncate(s, 5)
	if got != s {
		t.Errorf("string equal to limit should not be truncated: %q", got)
	}
}

func TestTruncate_Long(t *testing.T) {
	s := "hello world"
	got := truncate(s, 5)
	if len(got) <= 5 {
		// Contains the ellipsis char
		if !strings.HasPrefix(got, "hello") {
			t.Errorf("truncated string should start with first 5 chars, got %q", got)
		}
	}
	if strings.HasSuffix(got, "world") {
		t.Errorf("truncated string should not end with 'world': %q", got)
	}
}

func TestDebateSignature_Empty(t *testing.T) {
	d := DebateResult{}
	sig := DebateSignature(d)
	if sig != "" {
		t.Errorf("empty CertifiedBy should return \"\", got %q", sig)
	}
}

func TestDebateSignature_WithCertifiers(t *testing.T) {
	d := DebateResult{CertifiedBy: []string{"Auditor", "Optimizer"}}
	sig := DebateSignature(d)
	if !strings.Contains(sig, "Auditor") {
		t.Errorf("signature should mention Auditor: %q", sig)
	}
	if !strings.Contains(sig, "Optimizer") {
		t.Errorf("signature should mention Optimizer: %q", sig)
	}
	if !strings.HasPrefix(sig, "Certified by") {
		t.Errorf("signature should start with 'Certified by': %q", sig)
	}
}
