// Package testmock provides in-memory HTTP mocks for the external services
// neoanvil integrates with: Jira Cloud, DeepSeek, Ollama, and GitHub.
//
// Each mock is a self-contained httptest.Server. Tests register fixtures
// via the typed Set* methods, then point the production client at
// mock.URL() instead of the real domain. Call history is captured for
// assertions.
//
// PILAR / Area 3.1 — Integration Test Suite mock infrastructure.
package testmock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// HTTPCall is one captured request against a mock.
type HTTPCall struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// JiraIssue is the fixture shape for GET /issue/{key} responses. The mock
// wraps Description and Comment.Body in a minimal ADF v1 document so the
// production client's RenderADF traversal yields the input text back.
type JiraIssue struct {
	Key         string
	Summary     string
	Status      string
	Description string
	Comments    []JiraComment
}

// JiraComment is the fixture shape for issue comments.
type JiraComment struct {
	Author  string
	Body    string
	Created string
}

// JiraTransition is the fixture shape for transition listings.
type JiraTransition struct {
	ID       string
	Name     string
	ToStatus string
}

// JiraMock is a Jira Cloud REST v3-compatible test server.
//
// Defaults (overridable):
//   - Basic Auth: "test@example.com" / "fake-token"
//   - No fixtures registered (GET requests for unknown keys return 404)
//   - Rate limiting OFF
type JiraMock struct {
	server *httptest.Server

	mu             sync.RWMutex
	expectedEmail  string
	expectedToken  string
	issues         map[string]JiraIssue
	transitions    map[string][]JiraTransition
	rateLimit      bool
	nextCreatedKey int

	callCount int64

	callsMu sync.Mutex
	calls   []HTTPCall
}

// NewJira boots a Jira mock and registers Close as t.Cleanup.
func NewJira(tb testing.TB) *JiraMock {
	tb.Helper()
	m := &JiraMock{
		expectedEmail:  "test@example.com",
		expectedToken:  "fake-token",
		issues:         make(map[string]JiraIssue),
		transitions:    make(map[string][]JiraTransition),
		nextCreatedKey: 1,
	}
	m.server = httptest.NewServer(m.routes())
	tb.Cleanup(m.Close)
	return m
}

// URL returns the mock server's base URL (e.g. http://127.0.0.1:NNNN).
// Production clients should be configured to issue requests against this
// origin instead of the real https://*.atlassian.net domain.
func (m *JiraMock) URL() string { return m.server.URL }

// Close stops the mock server. Safe to call multiple times.
func (m *JiraMock) Close() { m.server.Close() }

// SetCredentials overrides the expected Basic Auth pair.
func (m *JiraMock) SetCredentials(email, token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expectedEmail = email
	m.expectedToken = token
}

// SetIssue registers a fixture for GET /rest/api/3/issue/{key}. When
// issue.Key is empty, the registration key is used.
func (m *JiraMock) SetIssue(key string, issue JiraIssue) {
	if issue.Key == "" {
		issue.Key = key
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.issues[key] = issue
}

// SetTransitions registers the transition list returned by
// GET /rest/api/3/issue/{key}/transitions.
func (m *JiraMock) SetTransitions(key string, ts []JiraTransition) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.transitions[key] = ts
}

// SetRateLimit toggles 429 responses for ALL endpoints.
func (m *JiraMock) SetRateLimit(on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rateLimit = on
}

// CallCount returns the total number of requests received since boot.
func (m *JiraMock) CallCount() int64 { return atomic.LoadInt64(&m.callCount) }

// Calls returns a snapshot of all captured HTTP calls (oldest first).
func (m *JiraMock) Calls() []HTTPCall {
	m.callsMu.Lock()
	defer m.callsMu.Unlock()
	out := make([]HTTPCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// routes wires the four supported endpoints. Go 1.22 ServeMux disambiguates
// /issue from /issue/{key} and /issue/{key}/transitions automatically.
func (m *JiraMock) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /rest/api/3/issue/{key}", m.middleware(m.handleGetIssue))
	mux.HandleFunc("GET /rest/api/3/issue/{key}/transitions", m.middleware(m.handleListTransitions))
	mux.HandleFunc("POST /rest/api/3/issue/{key}/transitions", m.middleware(m.handleDoTransition))
	mux.HandleFunc("POST /rest/api/3/issue", m.middleware(m.handleCreateIssue))
	return mux
}

// maxBodyBytes caps how much of a request body the mock will buffer for
// capture. 1 MiB is well above any realistic Jira REST payload while
// preventing accidental OOM if a malformed test sends a huge body.
const maxBodyBytes = 1 << 20

// middleware captures the request, validates auth, and applies rate limit.
func (m *JiraMock) middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		r.Body = io.NopCloser(bytes.NewReader(body))

		atomic.AddInt64(&m.callCount, 1)
		m.callsMu.Lock()
		m.calls = append(m.calls, HTTPCall{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   body,
		})
		m.callsMu.Unlock()

		m.mu.RLock()
		rateLimited := m.rateLimit
		wantEmail := m.expectedEmail
		wantToken := m.expectedToken
		m.mu.RUnlock()

		if rateLimited {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		email, token, ok := r.BasicAuth()
		if !ok || email != wantEmail || token != wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}

// handleGetIssue: GET /rest/api/3/issue/{key}
func (m *JiraMock) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	m.mu.RLock()
	issue, ok := m.issues[key]
	m.mu.RUnlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jiraIssuePayload(issue))
}

// handleListTransitions: GET /rest/api/3/issue/{key}/transitions
func (m *JiraMock) handleListTransitions(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	m.mu.RLock()
	ts := m.transitions[key]
	m.mu.RUnlock()
	out := struct {
		Transitions []map[string]any `json:"transitions"`
	}{Transitions: make([]map[string]any, 0, len(ts))}
	for _, t := range ts {
		out.Transitions = append(out.Transitions, map[string]any{
			"id":   t.ID,
			"name": t.Name,
			"to":   map[string]any{"name": t.ToStatus},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleDoTransition: POST /rest/api/3/issue/{key}/transitions
//
// Validates that the body's transition.id exists in the registered
// transition list when one is set via SetTransitions — mirrors real Jira,
// which rejects unknown IDs with 400. When no transitions are registered
// for the key, any non-empty ID is accepted (test convenience).
func (m *JiraMock) handleDoTransition(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	m.mu.RLock()
	_, exists := m.issues[key]
	registered := m.transitions[key]
	m.mu.RUnlock()
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var body struct {
		Transition struct {
			ID string `json:"id"`
		} `json:"transition"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.Transition.ID == "" {
		writeJSONError(w, http.StatusBadRequest, "transition.id is required")
		return
	}
	target := lookupTransition(registered, body.Transition.ID)
	if len(registered) > 0 && target == nil {
		writeJSONError(w, http.StatusBadRequest,
			fmt.Sprintf("transition id %q is not registered for issue %s", body.Transition.ID, key))
		return
	}
	// Mirror real Jira: a successful transition flips the issue's status
	// to the transition's ToStatus. Only applies when transitions were
	// registered (otherwise we have no mapping from id → status).
	if target != nil && target.ToStatus != "" {
		m.mu.Lock()
		issue := m.issues[key]
		issue.Status = target.ToStatus
		m.issues[key] = issue
		m.mu.Unlock()
	}
	w.WriteHeader(http.StatusNoContent)
}

// lookupTransition returns the registered transition matching id, or nil.
func lookupTransition(ts []JiraTransition, id string) *JiraTransition {
	for i := range ts {
		if ts[i].ID == id {
			return &ts[i]
		}
	}
	return nil
}

// handleCreateIssue: POST /rest/api/3/issue
func (m *JiraMock) handleCreateIssue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Fields struct {
			Project struct {
				Key string `json:"key"`
			} `json:"project"`
			IssueType struct {
				Name string `json:"name"`
			} `json:"issuetype"`
			Summary string `json:"summary"`
		} `json:"fields"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.Fields.Project.Key == "" || body.Fields.IssueType.Name == "" || body.Fields.Summary == "" {
		writeJSONError(w, http.StatusBadRequest, "fields.project.key, fields.issuetype.name and fields.summary are required")
		return
	}

	m.mu.Lock()
	n := m.nextCreatedKey
	m.nextCreatedKey++
	newKey := fmt.Sprintf("%s-%d", body.Fields.Project.Key, n)
	// Auto-register the created issue so a follow-up GET round-trips
	// without the test having to call SetIssue manually. Status defaults
	// to "To Do" — tests can override via SetIssue post-create.
	m.issues[newKey] = JiraIssue{
		Key:     newKey,
		Summary: body.Fields.Summary,
		Status:  "To Do",
	}
	m.mu.Unlock()

	out := map[string]any{
		"id":   fmt.Sprintf("%d", 10000+n),
		"key":  newKey,
		"self": m.URL() + "/rest/api/3/issue/" + newKey,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(out)
}

// writeJSONError emits a Jira-style error envelope.
func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errorMessages": []string{msg},
	})
}

// jiraIssuePayload builds the JSON shape decodeIssue (pkg/jira/client.go)
// expects: {key, fields: {summary, status:{name}, description, comment:{comments}}}.
func jiraIssuePayload(issue JiraIssue) map[string]any {
	fields := map[string]any{
		"summary": issue.Summary,
		"status":  map[string]any{"name": issue.Status},
	}
	if issue.Description != "" {
		fields["description"] = adfDoc(issue.Description)
	}
	comments := make([]map[string]any, 0, len(issue.Comments))
	for _, c := range issue.Comments {
		comments = append(comments, map[string]any{
			"author":  map[string]any{"displayName": c.Author},
			"body":    adfDoc(c.Body),
			"created": c.Created,
		})
	}
	fields["comment"] = map[string]any{"comments": comments}
	return map[string]any{
		"key":    issue.Key,
		"fields": fields,
	}
}

// adfDoc wraps plaintext in a minimal ADF v1 document — a single paragraph
// holding one text node. This is sufficient for round-tripping plain
// fixtures through pkg/jira's RenderADF, but does NOT exercise marks
// (bold/links), bullet/ordered lists, or nested blocks. Tests that need
// richer ADF should not rely on this helper.
func adfDoc(text string) map[string]any {
	return map[string]any{
		"type":    "doc",
		"version": 1,
		"content": []map[string]any{
			{
				"type": "paragraph",
				"content": []map[string]any{
					{"type": "text", "text": text},
				},
			},
		},
	}
}
