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

// ListPRs returns PRs for owner/repo. state ∈ {"open", "closed", "all"}.
func (c *Client) ListPRs(ctx context.Context, owner, repo, state string) ([]PullRequest, error) {
	if state == "" {
		state = "open"
	}
	var out []PullRequest
	path := fmt.Sprintf("/repos/%s/%s/pulls?state=%s&per_page=50", url.PathEscape(owner), url.PathEscape(repo), url.QueryEscape(state))
	if err := c.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListIssues returns issues for owner/repo (excludes PRs — GitHub
// returns both via /issues; we filter here).
func (c *Client) ListIssues(ctx context.Context, owner, repo, state string) ([]Issue, error) {
	if state == "" {
		state = "open"
	}
	var raw []map[string]any
	path := fmt.Sprintf("/repos/%s/%s/issues?state=%s&per_page=50", url.PathEscape(owner), url.PathEscape(repo), url.QueryEscape(state))
	if err := c.Do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(raw))
	for _, r := range raw {
		if _, isPR := r["pull_request"]; isPR {
			continue // skip PRs (also returned by /issues endpoint)
		}
		var i Issue
		b, _ := json.Marshal(r)
		_ = json.Unmarshal(b, &i)
		out = append(out, i)
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
