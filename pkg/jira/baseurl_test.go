package jira_test

import (
	"context"
	"testing"

	"github.com/ensamblatec/neoanvil/internal/testmock"
	"github.com/ensamblatec/neoanvil/pkg/jira"
)

// TestBaseURLOverrideRoutesToMock proves that the Area 3.2.A BaseURL
// override actually flows through the URL builders — the production
// client speaks plain HTTP to a test mock when configured to.
func TestBaseURLOverrideRoutesToMock(t *testing.T) {
	mock := testmock.NewJira(t)
	mock.SetIssue("MCPI-100", testmock.JiraIssue{
		Summary: "BaseURL override smoke",
		Status:  "In Progress",
	})

	client, err := jira.NewClient(jira.Config{
		Domain:  "ignored.atlassian.net",
		Email:   "test@example.com",
		Token:   "fake-token",
		BaseURL: mock.URL(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	issue, err := client.GetIssue(context.Background(), "MCPI-100")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if issue.Key != "MCPI-100" {
		t.Errorf("key=%q want MCPI-100", issue.Key)
	}
	if issue.Summary != "BaseURL override smoke" {
		t.Errorf("summary=%q", issue.Summary)
	}
	if issue.Status != "In Progress" {
		t.Errorf("status=%q want In Progress", issue.Status)
	}

	// Mock's call history confirms the request actually hit it.
	if got := mock.CallCount(); got != 1 {
		t.Errorf("mock.CallCount=%d want 1", got)
	}
}

// TestBaseURLValidationRejectsBadScheme proves the defense-in-depth
// guardrail against URL injection — non-http(s) schemes (e.g. file://,
// gopher://) cause NewClient to fail-fast instead of silently routing
// requests to an attacker-controlled host. [DS-AUDIT Finding 1]
func TestBaseURLValidationRejectsBadScheme(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"file_scheme", "file:///etc/passwd"},
		{"gopher_scheme", "gopher://attacker.example/9"},
		{"no_scheme_no_host", "not-a-url"},
		{"empty_host", "https:///path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := jira.NewClient(jira.Config{
				Domain:  "acme.atlassian.net",
				Email:   "u@example.com",
				Token:   "tok",
				BaseURL: tc.url,
			})
			if err == nil {
				t.Errorf("NewClient with BaseURL=%q did not fail; want validation error", tc.url)
			}
		})
	}
}

// TestBaseURLValidationTrimsWhitespace verifies the helper trims
// surrounding whitespace before validating, so a sloppy operator config
// doesn't surface as a confusing parse error downstream.
func TestBaseURLValidationTrimsWhitespace(t *testing.T) {
	mock := testmock.NewJira(t)
	client, err := jira.NewClient(jira.Config{
		Domain:  "acme.atlassian.net",
		Email:   "u@example.com",
		Token:   "tok",
		BaseURL: "  " + mock.URL() + "/  ",
	})
	if err != nil {
		t.Fatalf("trimmed BaseURL rejected: %v", err)
	}
	if client == nil {
		t.Fatalf("nil client")
	}
}

// TestEmptyBaseURLFallsBackToDomain documents the legacy contract: when
// BaseURL is empty, the client builds https://{Domain}/... — preserving
// production behavior. We can't actually reach Atlassian here, but we
// verify that constructing the client succeeds (no validation requires
// BaseURL).
func TestEmptyBaseURLFallsBackToDomain(t *testing.T) {
	client, err := jira.NewClient(jira.Config{
		Domain: "acme.atlassian.net",
		Email:  "u@example.com",
		Token:  "tok",
	})
	if err != nil {
		t.Fatalf("NewClient with empty BaseURL: %v", err)
	}
	if client == nil {
		t.Fatalf("nil client returned")
	}
}
