package main

import (
	"strings"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/jira"
)

func TestFormatIssueMarkdown_Full(t *testing.T) {
	issue := &jira.Issue{
		Key:         "ENG-42",
		Summary:     "Auth bug",
		Status:      "In Progress",
		Description: "Steps to reproduce:\n1. Open login",
		Comments: []jira.Comment{
			{Author: "Alice", Body: "Reproduced", Created: "2026-04-26T00:00:00Z"},
			{Author: "Bob", Body: "Working on it", Created: "2026-04-27T00:00:00Z"},
		},
	}
	got := formatIssueMarkdown(issue, "ENG", "15")

	mustContain := []string{
		"## ENG-42 — Auth bug",
		"**Status:** In Progress",
		"### Description",
		"Steps to reproduce",
		"### Last 2 comments",
		"**Alice**",
		"Reproduced",
		"**Bob**",
		"space=ENG",
		"board=15",
	}
	for _, s := range mustContain {
		if !strings.Contains(got, s) {
			t.Errorf("output missing %q\n--- got ---\n%s", s, got)
		}
	}
}

func TestFormatIssueMarkdown_NoContext(t *testing.T) {
	issue := &jira.Issue{Key: "X-1", Summary: "s", Status: "Open"}
	got := formatIssueMarkdown(issue, "", "")
	if strings.Contains(got, "context") {
		t.Errorf("should not include context preamble when both empty:\n%s", got)
	}
}

func TestFormatIssueMarkdown_NoCommentsOrDescription(t *testing.T) {
	issue := &jira.Issue{Key: "X-1", Summary: "s", Status: "Open"}
	got := formatIssueMarkdown(issue, "ENG", "")
	if strings.Contains(got, "### Description") {
		t.Error("should not render Description section when empty")
	}
	if strings.Contains(got, "### Last") {
		t.Error("should not render comments section when empty")
	}
	if !strings.Contains(got, "space=ENG") {
		t.Error("space should still be in preamble")
	}
}

func TestSummarizeTransitions_Empty(t *testing.T) {
	if got := summarizeTransitions(nil); !strings.Contains(got, "none") {
		t.Errorf("expected 'none' fallback, got %q", got)
	}
}

func TestSummarizeTransitions_Listed(t *testing.T) {
	got := summarizeTransitions([]jira.Transition{
		{ID: "11", Name: "Start", ToStatus: "In Progress"},
		{ID: "31", Name: "Mark Done", ToStatus: "Done"},
	})
	for _, must := range []string{"Start", "In Progress", "Mark Done", "Done"} {
		if !strings.Contains(got, must) {
			t.Errorf("output missing %q: %s", must, got)
		}
	}
}

func TestFormatTransitionResult_NoComment(t *testing.T) {
	t1 := &jira.Transition{ID: "31", Name: "Mark Done", ToStatus: "Done"}
	got := formatTransitionResult("ENG-42", t1, "")
	if !strings.Contains(got, "ENG-42") || !strings.Contains(got, "Mark Done") || !strings.Contains(got, "Done") {
		t.Errorf("missing fields: %s", got)
	}
	if strings.Contains(got, "Comment added") {
		t.Errorf("should not show comment section when empty: %s", got)
	}
	if !strings.Contains(got, "audit-jira.log") {
		t.Errorf("should mention audit log: %s", got)
	}
}

func TestFormatTransitionResult_WithComment(t *testing.T) {
	t1 := &jira.Transition{ID: "31", Name: "Mark Done", ToStatus: "Done"}
	got := formatTransitionResult("ENG-42", t1, "fixed in PR #99")
	if !strings.Contains(got, "Comment added") || !strings.Contains(got, "fixed in PR #99") {
		t.Errorf("comment section missing: %s", got)
	}
}

func TestPluginAuditPath_FollowsHome(t *testing.T) {
	t.Setenv("HOME", "/tmp/fake-home-test")
	got := pluginAuditPath()
	if !strings.HasSuffix(got, ".neo/audit-jira.log") {
		t.Errorf("path=%q should end with .neo/audit-jira.log", got)
	}
}

func TestFormatIssueMarkdown_BoardOnly(t *testing.T) {
	issue := &jira.Issue{Key: "X-1", Summary: "s", Status: "Open"}
	got := formatIssueMarkdown(issue, "", "15")
	if !strings.Contains(got, "board=15") {
		t.Errorf("board-only context lost: %s", got)
	}
	if strings.Contains(got, "space=") {
		t.Errorf("should not include space when empty: %s", got)
	}
}
