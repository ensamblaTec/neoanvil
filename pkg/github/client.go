// Package github is the minimal REST v3 client used by
// cmd/plugin-github. Mirrors the shape of pkg/jira/client.go: PAT
// auth, configurable base URL (Enterprise support), 429/5xx retry
// with exponential backoff, pagination helpers.
//
// We don't use go-github (Google) because it pulls 25+ transitive
// deps for surface we don't need. The handful of endpoints the
// plugin uses (PRs, issues, checks, branches) are easy to call via
// stdlib net/http.
//
// [Area 2.1.D]

package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// Client is the per-PAT GitHub REST v3 client. Construct via
// NewClient. Goroutine-safe; reuse across requests.
type Client struct {
	BaseURL string // "https://api.github.com" or self-hosted Enterprise URL
	Token   string // PAT (raw, no "Bearer " prefix)
	HTTP    *http.Client
}

// Config is the wire format consumed by NewClient.
type Config struct {
	BaseURL    string
	Token      string
	UserAgent  string // GitHub recommends a UA per their docs
	MaxRetries int    // default 3
}

// NewClient validates the config and returns a ready-to-use Client.
// Token is required. BaseURL defaults to api.github.com when empty.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Token == "" {
		return nil, errors.New("token is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	base = strings.TrimRight(base, "/")
	if _, err := url.ParseRequestURI(base); err != nil {
		return nil, fmt.Errorf("invalid base_url %q: %w", base, err)
	}
	return &Client{
		BaseURL: base,
		Token:   cfg.Token,
		HTTP:    sre.SafeHTTPClient(),
	}, nil
}

// Do executes a request with auth + retry. The body argument is
// JSON-marshalled; pass nil for GET-style calls. The response body
// is decoded into out (also nil-tolerant for empty 2xx).
//
// Retries: 3 attempts on 429 + 5xx with exponential backoff (1s, 2s, 4s).
func (c *Client) Do(ctx context.Context, method, path string, body any, out any) error {
	const maxRetries = 3
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		err := c.do(ctx, method, path, body, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !shouldRetry(err) {
			return err
		}
	}
	return fmt.Errorf("after %d retries: %w", maxRetries, lastErr)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024)) // 4MB cap
	if resp.StatusCode >= 400 {
		return &APIError{
			Status: resp.StatusCode,
			Body:   string(respBody),
		}
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w (body=%s)", err, truncate(string(respBody), 200))
		}
	}
	return nil
}

// APIError is returned for non-2xx GitHub responses. Callers can
// type-assert to inspect the status (e.g., 404 → "not found").
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github api %d: %s", e.Status, truncate(e.Body, 240))
}

// shouldRetry decides whether a transient failure deserves another
// attempt. Retries 429 (rate limited) + 5xx + network errors.
func shouldRetry(err error) bool {
	if err == nil {
		return false
	}
	apiErr := &APIError{}
	if errors.As(err, &apiErr) {
		return apiErr.Status == 429 || apiErr.Status >= 500
	}
	// Network-level error — retry.
	return true
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// ── Endpoint wrappers ────────────────────────────────────────────────

// PullRequest is the subset of fields we surface in MCP responses.
type PullRequest struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// Issue is the subset of fields we surface in MCP responses for
// list_issues + get_issue.
type Issue struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
}

// listPagesCap bounds total pagination depth so a misconfigured
// repo (or attacker forcing huge result sets) doesn't pin a worker
// indefinitely. 10 pages × 100 items = 1000 results — generous for
// real workflows, hard ceiling against DoS. [DS-AUDIT pagination Finding 1]
const listPagesCap = 10

// ListPRs returns PRs for owner/repo. state ∈ {"open", "closed", "all"}.
// Paginates server-side up to listPagesCap pages. [DS-AUDIT Finding 1]
func (c *Client) ListPRs(ctx context.Context, owner, repo, state string) ([]PullRequest, error) {
	if state == "" {
		state = "open"
	}
	var out []PullRequest
	for page := 1; page <= listPagesCap; page++ {
		var batch []PullRequest
		path := fmt.Sprintf("/repos/%s/%s/pulls?state=%s&per_page=100&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), url.QueryEscape(state), page)
		if err := c.Do(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < 100 {
			break // last page
		}
	}
	return out, nil
}

// ListIssues returns issues for owner/repo (excludes PRs — GitHub
// returns both via /issues; we filter here). Paginates per Finding 1.
func (c *Client) ListIssues(ctx context.Context, owner, repo, state string) ([]Issue, error) {
	if state == "" {
		state = "open"
	}
	out := make([]Issue, 0, 100)
	for page := 1; page <= listPagesCap; page++ {
		var raw []map[string]any
		path := fmt.Sprintf("/repos/%s/%s/issues?state=%s&per_page=100&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), url.QueryEscape(state), page)
		if err := c.Do(ctx, http.MethodGet, path, nil, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			if _, isPR := r["pull_request"]; isPR {
				continue // skip PRs (also returned by /issues endpoint)
			}
			var i Issue
			b, _ := json.Marshal(r)
			_ = json.Unmarshal(b, &i)
			out = append(out, i)
		}
		if len(raw) < 100 {
			break
		}
	}
	return out, nil
}

// GetUser returns the authenticated user (used at boot for
// connectivity check). 401 from this endpoint = bad PAT.
func (c *Client) GetUser(ctx context.Context) (string, error) {
	var out struct {
		Login string `json:"login"`
	}
	if err := c.Do(ctx, http.MethodGet, "/user", nil, &out); err != nil {
		return "", err
	}
	return out.Login, nil
}

// CreatePR opens a new pull request. base is the target branch
// (typically "main"); head is the source. Body may be empty.
// [Area 2.2.A]
func (c *Client) CreatePR(ctx context.Context, owner, repo, title, body, head, base string) (*PullRequest, error) {
	if title == "" || head == "" || base == "" {
		return nil, errors.New("title, head, base are required")
	}
	payload := map[string]any{
		"title": title,
		"body":  body,
		"head":  head,
		"base":  base,
	}
	var out PullRequest
	path := fmt.Sprintf("/repos/%s/%s/pulls", url.PathEscape(owner), url.PathEscape(repo))
	if err := c.Do(ctx, http.MethodPost, path, payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MergePR merges an open PR. mergeMethod ∈ {"merge", "squash", "rebase"}.
// [Area 2.2.A]
func (c *Client) MergePR(ctx context.Context, owner, repo string, number int, mergeMethod, commitTitle string) error {
	if mergeMethod == "" {
		mergeMethod = "merge"
	}
	payload := map[string]any{
		"merge_method": mergeMethod,
	}
	if commitTitle != "" {
		payload["commit_title"] = commitTitle
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", url.PathEscape(owner), url.PathEscape(repo), number)
	return c.Do(ctx, http.MethodPut, path, payload, nil)
}

// ClosePR closes a PR without merging. [Area 2.2.B]
func (c *Client) ClosePR(ctx context.Context, owner, repo string, number int) error {
	payload := map[string]any{"state": "closed"}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(repo), number)
	return c.Do(ctx, http.MethodPatch, path, payload, nil)
}

// PRComment is one item from `GET /repos/.../issues/{n}/comments` —
// the API returns this same shape for both PR and issue comments.
type PRComment struct {
	ID      int64  `json:"id"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
	CreatedAt string `json:"created_at"`
}

// PRComments returns the comments on a PR (or issue — same endpoint).
// Paginates per Finding 1. [Area 2.2.B + DS-AUDIT]
func (c *Client) PRComments(ctx context.Context, owner, repo string, number int) ([]PRComment, error) {
	var out []PRComment
	for page := 1; page <= listPagesCap; page++ {
		var batch []PRComment
		path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), number, page)
		if err := c.Do(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return out, nil
}

// CreateReview posts a PR review. event ∈ {"APPROVE", "REQUEST_CHANGES", "COMMENT"}.
// [Area 2.2.B]
func (c *Client) CreateReview(ctx context.Context, owner, repo string, number int, event, body string) error {
	payload := map[string]any{
		"event": event,
		"body":  body,
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", url.PathEscape(owner), url.PathEscape(repo), number)
	return c.Do(ctx, http.MethodPost, path, payload, nil)
}

// CreateIssue opens a new issue. labels is optional. [Area 2.2.C]
func (c *Client) CreateIssue(ctx context.Context, owner, repo, title, body string, labels []string) (*Issue, error) {
	if title == "" {
		return nil, errors.New("title is required")
	}
	payload := map[string]any{
		"title": title,
		"body":  body,
	}
	if len(labels) > 0 {
		payload["labels"] = labels
	}
	var out Issue
	path := fmt.Sprintf("/repos/%s/%s/issues", url.PathEscape(owner), url.PathEscape(repo))
	if err := c.Do(ctx, http.MethodPost, path, payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateIssue patches an issue. Pass state="closed" to close. [Area 2.2.C]
func (c *Client) UpdateIssue(ctx context.Context, owner, repo string, number int, fields map[string]any) error {
	if len(fields) == 0 {
		return errors.New("update fields are required")
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d", url.PathEscape(owner), url.PathEscape(repo), number)
	return c.Do(ctx, http.MethodPatch, path, fields, nil)
}

// CheckRun mirrors the GitHub Actions check_run shape we surface.
type CheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`     // queued|in_progress|completed
	Conclusion string `json:"conclusion"` // success|failure|neutral|cancelled|...
	HTMLURL    string `json:"html_url"`
}

// GetChecks returns all check runs for a commit ref (typically a PR
// head SHA or branch name). [Area 2.2.D]
func (c *Client) GetChecks(ctx context.Context, owner, repo, ref string) ([]CheckRun, error) {
	if ref == "" {
		return nil, errors.New("ref is required")
	}
	var raw struct {
		CheckRuns []CheckRun `json:"check_runs"`
	}
	path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(ref))
	if err := c.Do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	return raw.CheckRuns, nil
}

// Branch represents one repo branch in list responses.
type Branch struct {
	Name      string `json:"name"`
	Protected bool   `json:"protected"`
	Commit    struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

// ListBranches enumerates branches for owner/repo. Paginates per
// Finding 1. [Area 2.2.D + DS-AUDIT]
func (c *Client) ListBranches(ctx context.Context, owner, repo string) ([]Branch, error) {
	var out []Branch
	for page := 1; page <= listPagesCap; page++ {
		var batch []Branch
		path := fmt.Sprintf("/repos/%s/%s/branches?per_page=100&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), page)
		if err := c.Do(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return out, nil
}

// CompareCommits returns the diff metadata for base...head. The
// returned Commits slice is bounded server-side to ~250.
// [Area 2.2.D]
func (c *Client) CompareCommits(ctx context.Context, owner, repo, base, head string) (map[string]any, error) {
	if base == "" || head == "" {
		return nil, errors.New("base and head are required")
	}
	var out map[string]any
	path := fmt.Sprintf("/repos/%s/%s/compare/%s...%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(base), url.PathEscape(head))
	if err := c.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
