package jira

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestUpdateIssue_DescriptionOnly verifies a description-only update sends the
// PUT to /rest/api/3/issue/{key} with ADF-wrapped description and nothing
// else in the fields map. [140.2a]
func TestUpdateIssue_DescriptionOnly(t *testing.T) {
	var capturedMethod, capturedPath string
	var capturedBody []byte
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		capturedBody, _ = readAllSafe(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))

	err := c.UpdateIssue(context.Background(), "MCPI-57", UpdateIssueInput{
		Description: "## New body\n\nUpdated retroactively.",
	})
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	if capturedMethod != http.MethodPut {
		t.Errorf("method=%s want PUT", capturedMethod)
	}
	if !strings.HasSuffix(capturedPath, "/rest/api/3/issue/MCPI-57") {
		t.Errorf("path=%q want suffix /rest/api/3/issue/MCPI-57", capturedPath)
	}

	var raw map[string]any
	if err := json.Unmarshal(capturedBody, &raw); err != nil {
		t.Fatalf("decode body: %v\nbody=%s", err, capturedBody)
	}
	fields, ok := raw["fields"].(map[string]any)
	if !ok {
		t.Fatalf("expected fields map, got %T", raw["fields"])
	}
	// Description must be present as ADF — at minimum a "type":"doc" wrapper.
	desc, hasDesc := fields["description"]
	if !hasDesc {
		t.Errorf("description missing from payload")
	}
	if descMap, ok := desc.(map[string]any); !ok || descMap["type"] != "doc" {
		t.Errorf("description not ADF-wrapped: %v", desc)
	}
	// summary, labels, assignee must be ABSENT (we didn't set them).
	for _, k := range []string{"summary", "labels", "assignee", "duedate"} {
		if _, present := fields[k]; present {
			t.Errorf("unexpected field %q present (should be omitted)", k)
		}
	}
}

// TestUpdateIssue_SummaryOnly verifies summary string passes through as-is
// (no ADF conversion applied — Jira summary is plain text). [140.2b]
func TestUpdateIssue_SummaryOnly(t *testing.T) {
	var capturedBody []byte
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = readAllSafe(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))

	err := c.UpdateIssue(context.Background(), "MCPI-58", UpdateIssueInput{
		Summary: "[FEATURE][PLANIFICADOR] revised title",
	})
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	var raw map[string]any
	_ = json.Unmarshal(capturedBody, &raw)
	fields := raw["fields"].(map[string]any)
	if fields["summary"] != "[FEATURE][PLANIFICADOR] revised title" {
		t.Errorf("summary mismatch: %v", fields["summary"])
	}
}

// TestUpdateIssue_LabelsReplace verifies non-nil labels slice replaces the
// entire array (Jira's PUT semantic). [140.2c]
func TestUpdateIssue_LabelsReplace(t *testing.T) {
	var capturedBody []byte
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = readAllSafe(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))

	err := c.UpdateIssue(context.Background(), "MCPI-59", UpdateIssueInput{
		Labels: []string{"FEATURE", "API"},
	})
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	var raw map[string]any
	_ = json.Unmarshal(capturedBody, &raw)
	fields := raw["fields"].(map[string]any)
	labels, ok := fields["labels"].([]any)
	if !ok || len(labels) != 2 {
		t.Errorf("labels mismatch: %v", fields["labels"])
	}
}

// TestUpdateIssue_NotFound verifies 404 maps to ErrNotFound. [140.2d]
func TestUpdateIssue_NotFound(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	err := c.UpdateIssue(context.Background(), "GHOST-1", UpdateIssueInput{Summary: "x"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err=%v want ErrNotFound", err)
	}
}

// TestUpdateIssue_AuthFail verifies 401 maps to ErrAuth. [140.2e]
func TestUpdateIssue_AuthFail(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	err := c.UpdateIssue(context.Background(), "MCPI-1", UpdateIssueInput{Summary: "x"})
	if !errors.Is(err, ErrAuth) {
		t.Errorf("err=%v want ErrAuth", err)
	}
}

// TestUpdateIssue_EmptyKey verifies key validation. [140.2 extra]
func TestUpdateIssue_EmptyKey(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("server must NOT be hit when key is empty")
	}))
	err := c.UpdateIssue(context.Background(), "  ", UpdateIssueInput{Summary: "x"})
	if err == nil || !strings.Contains(err.Error(), "key is required") {
		t.Errorf("err=%v want 'key is required'", err)
	}
}

// TestUpdateIssue_NoFieldsRejected verifies that calling with all-empty input
// is rejected with a clear error (early validation, no HTTP roundtrip).
func TestUpdateIssue_NoFieldsRejected(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("server must NOT be hit when no fields set")
	}))
	err := c.UpdateIssue(context.Background(), "MCPI-1", UpdateIssueInput{})
	if err == nil || !strings.Contains(err.Error(), "at least one field") {
		t.Errorf("err=%v want 'at least one field' message", err)
	}
}

// TestUpdateIssue_LabelsClearWithEmptySlice verifies that a non-nil EMPTY
// slice clears all labels (vs nil which leaves them alone).
func TestUpdateIssue_LabelsClearWithEmptySlice(t *testing.T) {
	var capturedBody []byte
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = readAllSafe(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))

	err := c.UpdateIssue(context.Background(), "MCPI-1", UpdateIssueInput{
		Labels: []string{}, // explicit empty: clear
	})
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	var raw map[string]any
	_ = json.Unmarshal(capturedBody, &raw)
	fields := raw["fields"].(map[string]any)
	labels, present := fields["labels"]
	if !present {
		t.Errorf("expected labels field present (empty array clears)")
	}
	arr, ok := labels.([]any)
	if !ok || len(arr) != 0 {
		t.Errorf("expected empty labels array, got %v", labels)
	}
}
