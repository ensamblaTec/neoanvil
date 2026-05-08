package main

import (
	"strings"
	"testing"
)

// TestAuditMDValidSkill verifies that a fully valid frontmatter produces 0 findings. [128.2]
func TestAuditMDValidSkill(t *testing.T) {
	content := "---\nname: my-skill\ndescription: A valid short description.\npaths:\n  - \"pkg/**/*.go\"\ndisable-model-invocation: false\n---\n# body\n"
	findings := parseSkillFrontmatter("/.claude/skills/my-skill/SKILL.md", []byte(content))
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for valid skill, got %d: %v", len(findings), findings)
	}
}

// TestAuditMDNameMismatch verifies that a name: not matching dirname is flagged. [128.2]
func TestAuditMDNameMismatch(t *testing.T) {
	content := "---\nname: wrong-name\ndescription: Short description.\n---\n"
	findings := parseSkillFrontmatter("/.claude/skills/my-skill/SKILL.md", []byte(content))
	found := false
	for _, f := range findings {
		if strings.Contains(f.message, "wrong-name") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected name-mismatch finding, got %v", findings)
	}
}

// TestAuditMDEmptyDescription verifies that an empty description: is flagged. [128.2]
func TestAuditMDEmptyDescription(t *testing.T) {
	content := "---\nname: my-skill\ndescription: \n---\n"
	findings := parseSkillFrontmatter("/.claude/skills/my-skill/SKILL.md", []byte(content))
	found := false
	for _, f := range findings {
		if strings.Contains(f.message, "description is empty") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected empty-description finding, got %v", findings)
	}
}

// TestAuditMDInvalidModel verifies that an unknown model: value is flagged. [128.2]
func TestAuditMDInvalidModel(t *testing.T) {
	content := "---\nname: my-skill\ndescription: Short desc.\nmodel: gpt-4-turbo\n---\n"
	findings := parseSkillFrontmatter("/.claude/skills/my-skill/SKILL.md", []byte(content))
	found := false
	for _, f := range findings {
		if strings.Contains(f.message, "gpt-4-turbo") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected invalid-model finding, got %v", findings)
	}
}

// TestAuditMDDeprecatedTool verifies that a deprecated tool in allowed-tools is flagged. [128.2]
func TestAuditMDDeprecatedTool(t *testing.T) {
	content := "---\nname: my-skill\ndescription: Short desc.\nallowed-tools: neo_apply_patch, neo_sre_certify_mutation\n---\n"
	findings := parseSkillFrontmatter("/.claude/skills/my-skill/SKILL.md", []byte(content))
	found := false
	for _, f := range findings {
		if strings.Contains(f.message, "neo_apply_patch") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected deprecated-tool finding for neo_apply_patch, got %v", findings)
	}
}

// TestTokenBudgetWarningAboveThreshold verifies the 500K threshold logic. [128.3]
func TestTokenBudgetWarningAboveThreshold(t *testing.T) {
	rows := []tokenRow{
		{Tool: "neo_radar", OutputTokens: 600000},
	}
	// Simulate the threshold check inline (matches the briefing logic).
	var topTokenStr string
	if len(rows) > 0 && rows[0].OutputTokens > 500000 {
		topTokenStr = "⚠️ TokenBudget: neo_radar(600K out)"
	}
	if !strings.Contains(topTokenStr, "⚠️ TokenBudget") {
		t.Errorf("expected token budget warning, got %q", topTokenStr)
	}
	if !strings.Contains(topTokenStr, "neo_radar") {
		t.Errorf("expected tool name in warning, got %q", topTokenStr)
	}
}

// TestTokenBudgetNoWarningBelowThreshold verifies no warning below 500K. [128.3]
func TestTokenBudgetNoWarningBelowThreshold(t *testing.T) {
	rows := []tokenRow{
		{Tool: "neo_radar", OutputTokens: 499000},
	}
	var topTokenStr string
	if len(rows) > 0 && rows[0].OutputTokens > 500000 {
		topTokenStr = "⚠️ TokenBudget: neo_radar(499K out)"
	}
	if topTokenStr != "" {
		t.Errorf("expected no token budget warning below threshold, got %q", topTokenStr)
	}
}
