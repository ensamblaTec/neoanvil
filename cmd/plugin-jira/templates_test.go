package main

import (
	"strings"
	"testing"
)

func TestResolveTemplate_Found(t *testing.T) {
	cfg := &PluginConfig{
		TemplateLibrary: map[string]*Template{
			"story_v1": {Version: "1", Body: "## Criteria\n- [ ] {criteria}"},
		},
	}
	proj := &ProjectCfg{Templates: map[string]string{"story": "story_v1"}}
	body := resolveTemplate(cfg, proj, "story")
	if body == "" {
		t.Error("expected template body")
	}
	if !strings.Contains(body, "{criteria}") {
		t.Errorf("body = %q", body)
	}
}

func TestResolveTemplate_NotConfigured(t *testing.T) {
	cfg := &PluginConfig{TemplateLibrary: map[string]*Template{}}
	proj := &ProjectCfg{Templates: map[string]string{}}
	body := resolveTemplate(cfg, proj, "story")
	if body != "" {
		t.Errorf("expected empty, got %q", body)
	}
}

func TestResolveTemplate_RefMissing(t *testing.T) {
	cfg := &PluginConfig{TemplateLibrary: map[string]*Template{}}
	proj := &ProjectCfg{Templates: map[string]string{"story": "nonexistent_ref"}}
	body := resolveTemplate(cfg, proj, "story")
	if body != "" {
		t.Errorf("expected empty for missing ref, got %q", body)
	}
}

func TestRenderTemplate_Substitution(t *testing.T) {
	body := "## {title}\n- [ ] {criteria}\n{description}"
	values := map[string]string{"title": "My Story", "criteria": "Tests pass", "description": "Details here"}
	got := renderTemplate(body, values)
	if !strings.Contains(got, "My Story") || !strings.Contains(got, "Tests pass") {
		t.Errorf("substitution failed: %q", got)
	}
}

func TestRenderTemplate_UnmatchedPlaceholders(t *testing.T) {
	body := "## {title}\n{unknown_placeholder}"
	got := renderTemplate(body, map[string]string{"title": "Test"})
	if !strings.Contains(got, "{unknown_placeholder}") {
		t.Error("unmatched placeholders should be left as-is")
	}
}

func TestApplyTemplate_EndToEnd(t *testing.T) {
	cfg := &PluginConfig{
		TemplateLibrary: map[string]*Template{
			"bug_v1": {Version: "1", Body: "## Bug: {description}\nSteps: {steps}"},
		},
	}
	proj := &ProjectCfg{Templates: map[string]string{"bug": "bug_v1"}}
	got := applyTemplate(cfg, proj, "bug", map[string]string{"description": "Crash on login", "steps": "1. Open app"})
	if !strings.Contains(got, "Crash on login") || !strings.Contains(got, "1. Open app") {
		t.Errorf("end-to-end rendering failed: %q", got)
	}
}

func TestApplyTemplate_NoTemplate(t *testing.T) {
	cfg := &PluginConfig{TemplateLibrary: map[string]*Template{}}
	proj := &ProjectCfg{Templates: map[string]string{}}
	got := applyTemplate(cfg, proj, "story", map[string]string{})
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestShouldTriggerDocPack_InProgress(t *testing.T) {
	proj := &ProjectCfg{DocPack: DocPackCfg{Timing: "on_in_progress", AutoAttach: true}}
	if !shouldTriggerDocPack(proj, "In Progress") {
		t.Error("should trigger on in_progress")
	}
	if shouldTriggerDocPack(proj, "Done") {
		t.Error("should not trigger on Done")
	}
}

func TestShouldTriggerDocPack_Disabled(t *testing.T) {
	proj := &ProjectCfg{DocPack: DocPackCfg{Timing: "on_in_progress", AutoAttach: false}}
	if shouldTriggerDocPack(proj, "In Progress") {
		t.Error("should not trigger when auto_attach=false")
	}
}

func TestTicketRegexForProject_Configured(t *testing.T) {
	proj := &ProjectCfg{Hooks: HooksCfg{OnCommit: OnCommitHook{TicketRegex: `STRATIA-\d+`}}}
	got := ticketRegexForProject(proj)
	if got != `STRATIA-\d+` {
		t.Errorf("got %q", got)
	}
}

func TestTicketRegexForProject_Fallback(t *testing.T) {
	proj := &ProjectCfg{ProjectKey: "TEST"}
	got := ticketRegexForProject(proj)
	if got != `TEST-\d+` {
		t.Errorf("got %q", got)
	}
}

func TestCheckTransitionDoneHooks_RequireDocPack(t *testing.T) {
	proj := &ProjectCfg{
		ProjectKey: "TEST",
		Hooks:      HooksCfg{OnTransitionDone: OnTransitionDoneHook{RequireDocPack: true}},
	}
	warnings := checkTransitionDoneHooks(proj, "Done", false, true)
	if len(warnings) == 0 {
		t.Error("expected doc-pack warning")
	}
}

func TestCheckTransitionDoneHooks_VerifyChildren(t *testing.T) {
	proj := &ProjectCfg{
		ProjectKey: "TEST",
		Hooks:      HooksCfg{OnTransitionDone: OnTransitionDoneHook{VerifyChildren: true}},
	}
	warnings := checkTransitionDoneHooks(proj, "Done", true, false)
	if len(warnings) == 0 {
		t.Error("expected children warning")
	}
}

func TestCheckTransitionDoneHooks_AllGood(t *testing.T) {
	proj := &ProjectCfg{
		ProjectKey: "TEST",
		Hooks:      HooksCfg{OnTransitionDone: OnTransitionDoneHook{RequireDocPack: true, VerifyChildren: true}},
	}
	warnings := checkTransitionDoneHooks(proj, "Done", true, true)
	if len(warnings) != 0 {
		t.Errorf("expected no warnings, got %v", warnings)
	}
}

func TestCheckTransitionDoneHooks_NotDone(t *testing.T) {
	proj := &ProjectCfg{
		ProjectKey: "TEST",
		Hooks:      HooksCfg{OnTransitionDone: OnTransitionDoneHook{RequireDocPack: true, VerifyChildren: true}},
	}
	warnings := checkTransitionDoneHooks(proj, "In Progress", false, false)
	if len(warnings) != 0 {
		t.Errorf("hooks should only fire on Done, got %v", warnings)
	}
}
