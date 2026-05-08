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

// GitHubPR is the fixture shape for /pulls list entries.
type GitHubPR struct {
	Number int
	Title  string
	State  string // "open" | "closed"
	User   string
	Head   string // branch name
	Base   string
}

// GitHubMock is a minimal stub of the GitHub REST v3 API.
//
// Currently exposes only the two endpoints the future plugin-github
// (Area 2) will need first. More endpoints (issues, reviews, checks)
// will be added when the plugin starts taking shape.
//
//	GET  /repos/{owner}/{repo}/pulls    — list PRs (configurable fixtures)
//	POST /repos/{owner}/{repo}/issues   — create an issue (auto-assigned id)
//
// Defaults (overridable):
//   - Bearer token: "fake-github-token"
//   - No PRs registered (empty list returned)
type GitHubMock struct {
	server *httptest.Server

	mu              sync.RWMutex
	expectedToken   string
	prs             map[string][]GitHubPR // key "owner/repo"
	nextIssueNumber int

	callCount int64

	callsMu sync.Mutex
	calls   []HTTPCall
}

// NewGitHub boots a GitHub mock and registers Close as t.Cleanup.
func NewGitHub(tb testing.TB) *GitHubMock {
	tb.Helper()
	m := &GitHubMock{
		expectedToken:   "fake-github-token",
		prs:             make(map[string][]GitHubPR),
		nextIssueNumber: 1,
	}
	m.server = httptest.NewServer(m.routes())
	tb.Cleanup(m.Close)
	return m
}

// URL returns the mock server's base URL. Configure clients to hit this
// URL instead of https://api.github.com.
func (m *GitHubMock) URL() string { return m.server.URL }

// Close stops the mock server. Safe to call multiple times.
func (m *GitHubMock) Close() { m.server.Close() }

// SetToken overrides the expected Bearer token. Empty string causes
// checkBearer to reject ALL requests (auth-bypass guardrail mirrored
// from DeepSeekMock).
func (m *GitHubMock) SetToken(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expectedToken = token
}

// SetPRs registers fixture pull-request entries returned by
// GET /repos/{owner}/{repo}/pulls.
func (m *GitHubMock) SetPRs(owner, repo string, prs []GitHubPR) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prs[ownerRepoKey(owner, repo)] = append([]GitHubPR(nil), prs...)
}

// CallCount returns the total number of requests received since boot.
func (m *GitHubMock) CallCount() int64 { return atomic.LoadInt64(&m.callCount) }

// Calls returns a snapshot of all captured HTTP calls (oldest first).
func (m *GitHubMock) Calls() []HTTPCall {
	m.callsMu.Lock()
	defer m.callsMu.Unlock()
	out := make([]HTTPCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// routes wires the two stub endpoints. Order is irrelevant — Go 1.22
// path templates disambiguate by specificity.
func (m *GitHubMock) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls", m.middleware(m.handleListPRs))
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues", m.middleware(m.handleCreateIssue))
	return mux
}

// middleware captures the request, checks Bearer auth, and records history.
// Mirrors the DeepSeek middleware shape — the auth model is identical.
func (m *GitHubMock) middleware(next http.HandlerFunc) http.HandlerFunc {
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
		wantToken := m.expectedToken
		m.mu.RUnlock()

		if !checkBearer(r.Header.Get("Authorization"), wantToken) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// handleListPRs: GET /repos/{owner}/{repo}/pulls
func (m *GitHubMock) handleListPRs(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repo := r.PathValue("repo")
	m.mu.RLock()
	prs := append([]GitHubPR(nil), m.prs[ownerRepoKey(owner, repo)]...)
	m.mu.RUnlock()

	out := make([]map[string]any, 0, len(prs))
	for _, pr := range prs {
		out = append(out, map[string]any{
			"number": pr.Number,
			"title":  pr.Title,
			"state":  pr.State,
			"user":   map[string]any{"login": pr.User},
			"head":   map[string]any{"ref": pr.Head},
			"base":   map[string]any{"ref": pr.Base},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleCreateIssue: POST /repos/{owner}/{repo}/issues
func (m *GitHubMock) handleCreateIssue(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repo := r.PathValue("repo")

	var body struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writePlainError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.Title == "" {
		writePlainError(w, http.StatusBadRequest, "title is required")
		return
	}

	m.mu.Lock()
	n := m.nextIssueNumber
	m.nextIssueNumber++
	m.mu.Unlock()

	out := map[string]any{
		"number":   n,
		"title":    body.Title,
		"body":     body.Body,
		"labels":   body.Labels,
		"state":    "open",
		"html_url": fmt.Sprintf("%s/%s/%s/issues/%d", m.URL(), owner, repo, n),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(out)
}

// ownerRepoKey returns the canonical "owner/repo" identifier for the PR
// fixture map. Trims slashes so callers don't have to.
func ownerRepoKey(owner, repo string) string { return owner + "/" + repo }
