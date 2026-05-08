package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ── 3.A Workspace resolution (already in config.go resolveProject) ──────────

// ── 3.B Issue type validation ───────────────────────────────────────────────

func validateIssueType(proj *ProjectCfg, issueType string) error {
	if proj == nil {
		return nil
	}
	it := strings.ToLower(issueType)
	if _, ok := proj.IssueTypes[it]; !ok {
		allowed := make([]string, 0, len(proj.IssueTypes))
		for k := range proj.IssueTypes {
			allowed = append(allowed, k)
		}
		return fmt.Errorf("issue type %q is not allowed for project %s (allowed: %s)",
			issueType, proj.ProjectKey, strings.Join(allowed, ", "))
	}
	return nil
}

// ── 3.C Custom fields mapping ───────────────────────────────────────────────

// mapCustomField translates a generic field name to the Jira custom field ID.
// Returns the original name if no mapping exists.
func mapCustomField(proj *ProjectCfg, genericName string) string {
	if proj == nil || proj.CustomFields == nil {
		return genericName
	}
	if mapped, ok := proj.CustomFields[genericName]; ok {
		return mapped
	}
	return genericName
}

// ── 3.D Naming enforcement ──────────────────────────────────────────────────

// validateNaming checks a summary against the project's naming convention.
// Returns nil if valid, or an error with an example of the correct format.
func validateNaming(proj *ProjectCfg, summary string) error {
	if proj == nil || proj.Naming.Pattern == "" {
		return nil
	}
	pat := proj.Naming.Pattern
	// Build regex from pattern like "[{category}][{scope}] {text}"
	// → \[(FEATURE|BUG|...)\]\[(ENGINE|UI|...)\] .+
	re := buildNamingRegex(pat, proj.Naming.Categories, proj.Naming.Scopes)
	if re == nil {
		return nil
	}
	if !re.MatchString(summary) {
		example := buildNamingExample(proj)
		return fmt.Errorf("summary %q does not match naming pattern %q\nExample: %s", summary, pat, example)
	}
	return nil
}

func buildNamingRegex(pattern string, categories, scopes []string) *regexp.Regexp {
	s := regexp.QuoteMeta(pattern)
	if len(categories) > 0 {
		s = strings.Replace(s, regexp.QuoteMeta("{category}"), "("+strings.Join(categories, "|")+")", 1)
	}
	if len(scopes) > 0 {
		s = strings.Replace(s, regexp.QuoteMeta("{scope}"), "("+strings.Join(scopes, "|")+")", 1)
	}
	s = strings.Replace(s, regexp.QuoteMeta("{text}"), ".+", 1)
	s = "^" + s + "$"
	re, err := regexp.Compile(s)
	if err != nil {
		return nil
	}
	return re
}

func buildNamingExample(proj *ProjectCfg) string {
	cat := "FEATURE"
	if len(proj.Naming.Categories) > 0 {
		cat = proj.Naming.Categories[0]
	}
	scope := "ENGINE"
	if len(proj.Naming.Scopes) > 0 {
		scope = proj.Naming.Scopes[0]
	}
	return fmt.Sprintf("[%s][%s] Breve descripción de la tarea", cat, scope)
}

// ── 3.E Transition rules: no_skip_states ────────────────────────────────────

// validateTransition checks if moving from currentStatus to targetStatus is
// valid according to the project's workflow definition.
// Returns nil if valid, or an error describing the violation.
func validateTransition(proj *ProjectCfg, issueType, currentStatus, targetStatus string) error {
	if proj == nil {
		return nil
	}
	it := strings.ToLower(issueType)
	itCfg, ok := proj.IssueTypes[it]
	if !ok {
		return nil // no config for this type — allow anything
	}
	if !proj.Transitions.Rules.NoSkipStates {
		return nil
	}

	wf := itCfg.Workflow
	currentIdx := indexOfStatus(wf, currentStatus)
	targetIdx := indexOfStatus(wf, targetStatus)

	if currentIdx == -1 {
		// Current status not in configured workflow — log warning, allow.
		fmt.Fprintf(os.Stderr, "plugin-jira: warning: current status %q not in workflow for %s — skipping validation\n", currentStatus, issueType)
		return nil
	}
	if targetIdx == -1 {
		return fmt.Errorf("target status %q is not in the workflow for %s (workflow: %s)",
			targetStatus, issueType, strings.Join(wf, " → "))
	}
	if targetIdx == currentIdx {
		return nil // same status — no-op transition
	}
	// Allow backward to any previous. Forward must be adjacent only.
	if targetIdx > currentIdx+1 {
		expected := wf[currentIdx+1]
		return fmt.Errorf("cannot skip from %q to %q — next expected status is %q (workflow: %s)",
			currentStatus, targetStatus, expected, strings.Join(wf, " → "))
	}
	return nil
}

func indexOfStatus(wf []string, status string) int {
	s := strings.ToLower(strings.TrimSpace(status))
	for i, w := range wf {
		if strings.ToLower(strings.TrimSpace(w)) == s {
			return i
		}
	}
	return -1
}

// ── 3.F Gating: story requires epic in_progress ────────────────────────────

// checkEpicGating verifies that the parent epic is "In Progress" before
// allowing a child story to transition forward. Requires a getIssue callback
// to query the parent status (avoids hard dependency on jira.Client here).
func checkEpicGating(proj *ProjectCfg, issueType, parentKey string, getParentStatus func(key string) (string, error)) error {
	if proj == nil || !proj.Transitions.Rules.StoryRequiresEpicInProgress {
		return nil
	}
	if strings.ToLower(issueType) != "story" || parentKey == "" {
		return nil
	}
	status, err := getParentStatus(parentKey)
	if err != nil {
		return fmt.Errorf("check parent epic %s: %w", parentKey, err)
	}
	if strings.ToLower(strings.TrimSpace(status)) != "in progress" {
		return fmt.Errorf("story cannot advance — parent epic %s is %q (must be 'In Progress')", parentKey, status)
	}
	return nil
}

// ── 3.G Required fields on transition ───────────────────────────────────────

// checkRequiredFields validates that the required fields for the target status
// are non-empty. Fields are checked against the provided map of field values.
func checkRequiredFields(proj *ProjectCfg, targetStatus string, fields map[string]string) error {
	if proj == nil || proj.Transitions.RequiredFieldsOnTransit == nil {
		return nil
	}
	required, ok := proj.Transitions.RequiredFieldsOnTransit[targetStatus]
	if !ok {
		return nil
	}
	var missing []string
	for _, f := range required {
		if v, exists := fields[f]; !exists || strings.TrimSpace(v) == "" {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("transition to %q requires non-empty fields: %s", targetStatus, strings.Join(missing, ", "))
	}
	return nil
}
