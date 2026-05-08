package main

import (
	"strings"
	"testing"
)

// ── 3.B Issue type validation ───────────────────────────────────────────────

func TestValidateIssueType_Allowed(t *testing.T) {
	proj := minimalConfig().Projects["test"]
	if err := validateIssueType(proj, "epic"); err != nil {
		t.Errorf("epic should be allowed: %v", err)
	}
	if err := validateIssueType(proj, "Epic"); err != nil {
		t.Errorf("Epic (uppercase) should be allowed: %v", err)
	}
}

func TestValidateIssueType_Rejected(t *testing.T) {
	proj := minimalConfig().Projects["test"]
	err := validateIssueType(proj, "subtask")
	if err == nil {
		t.Error("subtask should be rejected")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateIssueType_NilProject(t *testing.T) {
	if err := validateIssueType(nil, "epic"); err != nil {
		t.Errorf("nil project should pass: %v", err)
	}
}

// ── 3.C Custom fields mapping ───────────────────────────────────────────────

func TestMapCustomField_Mapped(t *testing.T) {
	proj := &ProjectCfg{CustomFields: map[string]string{"story_points": "customfield_10016"}}
	got := mapCustomField(proj, "story_points")
	if got != "customfield_10016" {
		t.Errorf("got %q", got)
	}
}

func TestMapCustomField_Unmapped(t *testing.T) {
	proj := &ProjectCfg{CustomFields: map[string]string{}}
	got := mapCustomField(proj, "unknown_field")
	if got != "unknown_field" {
		t.Errorf("got %q, want passthrough", got)
	}
}

// ── 3.D Naming enforcement ──────────────────────────────────────────────────

func TestValidateNaming_Valid(t *testing.T) {
	proj := &ProjectCfg{
		Naming: NamingCfg{
			Pattern:    "[{category}][{scope}] {text}",
			Categories: []string{"FEATURE", "BUG"},
			Scopes:     []string{"ENGINE", "UI"},
		},
	}
	if err := validateNaming(proj, "[FEATURE][ENGINE] Add multi-tenant support"); err != nil {
		t.Errorf("valid naming rejected: %v", err)
	}
}

func TestValidateNaming_Invalid(t *testing.T) {
	proj := &ProjectCfg{
		Naming: NamingCfg{
			Pattern:    "[{category}][{scope}] {text}",
			Categories: []string{"FEATURE", "BUG"},
			Scopes:     []string{"ENGINE", "UI"},
		},
	}
	err := validateNaming(proj, "bad title no brackets")
	if err == nil {
		t.Error("invalid naming should be rejected")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateNaming_NoPattern(t *testing.T) {
	proj := &ProjectCfg{}
	if err := validateNaming(proj, "anything goes"); err != nil {
		t.Errorf("no pattern should pass: %v", err)
	}
}

func TestValidateNaming_WrongCategory(t *testing.T) {
	proj := &ProjectCfg{
		Naming: NamingCfg{
			Pattern:    "[{category}][{scope}] {text}",
			Categories: []string{"FEATURE", "BUG"},
			Scopes:     []string{"ENGINE"},
		},
	}
	err := validateNaming(proj, "[CHORE][ENGINE] cleanup")
	if err == nil {
		t.Error("CHORE should be rejected when not in categories")
	}
}

// ── 3.E Transition rules: no_skip ───────────────────────────────────────────

func TestValidateTransition_Adjacent(t *testing.T) {
	proj := storyProject()
	err := validateTransition(proj, "story", "Backlog", "Selected for Development")
	if err != nil {
		t.Errorf("adjacent transition should pass: %v", err)
	}
}

func TestValidateTransition_Skip(t *testing.T) {
	proj := storyProject()
	err := validateTransition(proj, "story", "Backlog", "In Progress")
	if err == nil {
		t.Error("skipping 'Selected for Development' should fail")
	}
	if !strings.Contains(err.Error(), "cannot skip") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateTransition_Backward(t *testing.T) {
	proj := storyProject()
	err := validateTransition(proj, "story", "In Progress", "Backlog")
	if err != nil {
		t.Errorf("backward transition should be allowed: %v", err)
	}
}

func TestValidateTransition_NoSkipDisabled(t *testing.T) {
	proj := storyProject()
	proj.Transitions.Rules.NoSkipStates = false
	err := validateTransition(proj, "story", "Backlog", "Done")
	if err != nil {
		t.Errorf("no_skip disabled should allow skip: %v", err)
	}
}

func TestValidateTransition_UnknownType(t *testing.T) {
	proj := storyProject()
	err := validateTransition(proj, "subtask", "Backlog", "Done")
	if err != nil {
		t.Errorf("unknown type should pass (no config): %v", err)
	}
}

func TestValidateTransition_TargetNotInWorkflow(t *testing.T) {
	proj := storyProject()
	err := validateTransition(proj, "story", "Backlog", "NONEXISTENT")
	if err == nil {
		t.Error("target not in workflow should fail")
	}
}

// ── 3.F Gating ──────────────────────────────────────────────────────────────

func TestCheckEpicGating_ParentInProgress(t *testing.T) {
	proj := storyProject()
	proj.Transitions.Rules.StoryRequiresEpicInProgress = true
	getParent := func(_ string) (string, error) { return "In Progress", nil }
	err := checkEpicGating(proj, "story", "EPIC-1", getParent)
	if err != nil {
		t.Errorf("parent in progress should pass: %v", err)
	}
}

func TestCheckEpicGating_ParentNotInProgress(t *testing.T) {
	proj := storyProject()
	proj.Transitions.Rules.StoryRequiresEpicInProgress = true
	getParent := func(_ string) (string, error) { return "Backlog", nil }
	err := checkEpicGating(proj, "story", "EPIC-1", getParent)
	if err == nil {
		t.Error("parent in Backlog should fail gating")
	}
}

func TestCheckEpicGating_NotStory(t *testing.T) {
	proj := storyProject()
	proj.Transitions.Rules.StoryRequiresEpicInProgress = true
	getParent := func(_ string) (string, error) { return "Backlog", nil }
	err := checkEpicGating(proj, "task", "EPIC-1", getParent)
	if err != nil {
		t.Errorf("gating should only apply to stories: %v", err)
	}
}

func TestCheckEpicGating_Disabled(t *testing.T) {
	proj := storyProject()
	proj.Transitions.Rules.StoryRequiresEpicInProgress = false
	getParent := func(_ string) (string, error) { return "Backlog", nil }
	err := checkEpicGating(proj, "story", "EPIC-1", getParent)
	if err != nil {
		t.Errorf("disabled gating should pass: %v", err)
	}
}

// ── 3.G Required fields ─────────────────────────────────────────────────────

func TestCheckRequiredFields_Present(t *testing.T) {
	proj := storyProject()
	proj.Transitions.RequiredFieldsOnTransit = map[string][]string{"Done": {"resolution"}}
	err := checkRequiredFields(proj, "Done", map[string]string{"resolution": "Fixed"})
	if err != nil {
		t.Errorf("fields present should pass: %v", err)
	}
}

func TestCheckRequiredFields_Missing(t *testing.T) {
	proj := storyProject()
	proj.Transitions.RequiredFieldsOnTransit = map[string][]string{"Done": {"resolution"}}
	err := checkRequiredFields(proj, "Done", map[string]string{})
	if err == nil {
		t.Error("missing resolution should fail")
	}
	if !strings.Contains(err.Error(), "resolution") {
		t.Errorf("error should mention missing field: %v", err)
	}
}

func TestCheckRequiredFields_NoConfig(t *testing.T) {
	proj := storyProject()
	err := checkRequiredFields(proj, "Done", map[string]string{})
	if err != nil {
		t.Errorf("no config should pass: %v", err)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func storyProject() *ProjectCfg {
	return &ProjectCfg{
		ProjectKey: "TEST",
		IssueTypes: map[string]*IssueTypeCfg{
			"story": {
				Workflow: []string{"Backlog", "Selected for Development", "In Progress", "REVIEW", "READY TO DEPLOY", "Done"},
			},
			"epic": {
				Workflow: []string{"Backlog", "In Progress", "Done"},
			},
		},
		Transitions: TransitionCfg{
			Rules: TransitionRules{NoSkipStates: true},
		},
	}
}
