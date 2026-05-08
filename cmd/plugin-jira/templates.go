package main

import (
	"fmt"
	"strings"
)

// ── 4.A+4.B Template library loader + renderer ─────────────────────────────

// resolveTemplate finds the template body for an issue type in a project.
// Looks up project.templates[issueType] → template_library[ref].body.
// Returns empty string if no template configured (not an error).
func resolveTemplate(cfg *PluginConfig, proj *ProjectCfg, issueType string) string {
	if cfg == nil || proj == nil {
		return ""
	}
	ref, ok := proj.Templates[strings.ToLower(issueType)]
	if !ok || ref == "" {
		return ""
	}
	tmpl, ok := cfg.TemplateLibrary[ref]
	if !ok || tmpl == nil {
		return ""
	}
	return tmpl.Body
}

// renderTemplate substitutes {placeholder} markers in a template body
// with values from the provided map. Unmatched placeholders are left as-is.
func renderTemplate(body string, values map[string]string) string {
	result := body
	for k, v := range values {
		result = strings.ReplaceAll(result, "{"+k+"}", v)
	}
	return result
}

// ── 4.C create_issue template integration ───────────────────────────────────

// applyTemplate resolves and renders the template for a create_issue call.
// Returns the rendered body or empty string if no template.
func applyTemplate(cfg *PluginConfig, proj *ProjectCfg, issueType string, values map[string]string) string {
	body := resolveTemplate(cfg, proj, issueType)
	if body == "" {
		return ""
	}
	return renderTemplate(body, values)
}

// ── 4.D Doc-pack timing ────────────────────────────────────────────────────

// shouldTriggerDocPack checks if a doc-pack should be generated based on
// the project config and the transition target status.
func shouldTriggerDocPack(proj *ProjectCfg, targetStatus string) bool {
	if proj == nil || !proj.DocPack.AutoAttach {
		return false
	}
	target := strings.ToLower(strings.TrimSpace(targetStatus))
	switch proj.DocPack.Timing {
	case "on_in_progress":
		return target == "in progress"
	case "on_review":
		return target == "review"
	case "on_done":
		return target == "done"
	default:
		return false
	}
}

// ── 4.E on_commit hook: ticket regex ────────────────────────────────────────

// ticketRegexForProject returns the regex pattern to match tickets in commit
// messages for the given project. Falls back to PROJECT_KEY-\d+.
func ticketRegexForProject(proj *ProjectCfg) string {
	if proj != nil && proj.Hooks.OnCommit.TicketRegex != "" {
		return proj.Hooks.OnCommit.TicketRegex
	}
	if proj != nil && proj.ProjectKey != "" {
		return proj.ProjectKey + `-\d+`
	}
	return ""
}

// ── 4.F on_transition_done enforcement ──────────────────────────────────────

// checkTransitionDoneHooks validates post-transition requirements.
// Returns a list of warnings (non-blocking) for the caller to include in response.
func checkTransitionDoneHooks(proj *ProjectCfg, targetStatus string, hasDocPack bool, childrenAllDone bool) []string {
	if proj == nil {
		return nil
	}
	var warnings []string
	target := strings.ToLower(strings.TrimSpace(targetStatus))

	if proj.Hooks.OnTransitionDone.RequireDocPack && target == "done" && !hasDocPack {
		warnings = append(warnings, fmt.Sprintf("⚠️ project %s requires doc-pack before Done", proj.ProjectKey))
	}
	if proj.Hooks.OnTransitionDone.VerifyChildren && target == "done" && !childrenAllDone {
		warnings = append(warnings, fmt.Sprintf("⚠️ project %s requires all children Done before epic Done", proj.ProjectKey))
	}
	return warnings
}
