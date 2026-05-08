// Package jira is a thin Atlassian Jira Cloud REST client tuned for the
// neoanvil plugin host. PILAR XXIII / Épica 125.3.
//
// Scope: read tickets, list transitions, perform transitions with comment.
// Out of scope: project administration, Confluence, complex JQL search
// (use Atlassian's full SDKs for those — neo's plugin only needs the
// orchestration primitives).
//
// Auth: HTTP Basic with (email, api_token). API tokens follow the 2024-12-15
// rotation policy — see ADR-006.
//
// Rate limiting: enforced client-side via a minimum gap between requests
// (default 100 ms = 10 req/s). Atlassian's official limits are vendor-side
// only — we keep a conservative client-side throttle to avoid 429s.
package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultMinGap   = 100 * time.Millisecond // 10 req/s
	defaultTimeout  = 30 * time.Second
	maxCommentsKept = 3 // ADR-006: get_context returns last 3 comments
)

// ErrNotFound is returned when an issue (or other resource) does not exist.
var ErrNotFound = errors.New("not found")

// ErrAuth is returned on 401/403 responses — caller should re-prompt for
// credentials via `neo login`.
var ErrAuth = errors.New("authentication failed")

// ErrRateLimited is returned on 429 responses. The error message includes
// the Retry-After header value when present.
var ErrRateLimited = errors.New("rate limited")

// Issue is the slim projection of /rest/api/3/issue/{key} that the plugin
// surfaces to the LLM. Description and Comment bodies are rendered to
// plaintext (best-effort ADF traversal) — the LLM consumes prose, not
// Atlassian Document Format JSON.
type Issue struct {
	Key         string
	Summary     string
	Status      string
	Description string
	Comments    []Comment
}

// Comment is one comment on an issue.
type Comment struct {
	Author  string
	Body    string
	Created string
}

// Config controls Client construction.
type Config struct {
	Domain string        // e.g. "acme.atlassian.net" — no scheme
	Email  string        // Atlassian account email
	Token  string        // API token from id.atlassian.com
	HTTP   *http.Client  // optional override (nil = default with 30s timeout)
	MinGap time.Duration // optional throttle gap (0 = 100ms default)

	// BaseURL overrides the default `https://{Domain}` prefix used to build
	// REST URLs. Required for integration tests that need to point the
	// client at an http://127.0.0.1:NNNN testmock instead of Atlassian's
	// production HTTPS host. When empty, falls back to the legacy
	// `https://{Domain}` shape so production callers see no behavior change.
	// [Area 3.2.A]
	BaseURL string
}

// Client is a Jira Cloud REST client. Safe for concurrent use — request
// throttling is mutex-serialized; HTTP is delegated to the embedded client.
type Client struct {
	domain  string
	email   string
	token   string
	http    *http.Client
	baseURL string // resolved at New: BaseURL if set, else "https://"+Domain

	mu      sync.Mutex
	lastReq time.Time
	minGap  time.Duration
}

// NewClient validates required fields and returns a Client. Use Config.HTTP
// to inject a test stub or sre.SafeHTTPClient.
func NewClient(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Domain) == "" {
		return nil, errors.New("domain is required")
	}
	if strings.TrimSpace(cfg.Email) == "" {
		return nil, errors.New("email is required")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("token is required")
	}
	httpClient := cfg.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	gap := cfg.MinGap
	if gap == 0 {
		gap = defaultMinGap
	}
	baseURL, err := resolveBaseURL(cfg.BaseURL, cfg.Domain)
	if err != nil {
		return nil, err
	}
	return &Client{
		domain:  cfg.Domain,
		email:   cfg.Email,
		token:   cfg.Token,
		http:    httpClient,
		baseURL: baseURL,
		minGap:  gap,
	}, nil
}

// resolveBaseURL turns the operator-supplied BaseURL into the final prefix
// used for REST URL construction. Validates the scheme is http/https,
// trims whitespace and trailing slashes, and falls back to
// `https://{domain}` when override is empty.
//
// Defense-in-depth: a malformed or non-http(s) URL fails fast at boot
// rather than silently routing requests to an attacker-controlled host.
// [Area 3.2.A — DS-AUDIT Finding 1]
func resolveBaseURL(override, domain string) (string, error) {
	override = strings.TrimSpace(override)
	override = strings.TrimRight(override, "/")
	if override == "" {
		return "https://" + domain, nil
	}
	parsed, err := url.Parse(override)
	if err != nil {
		return "", fmt.Errorf("BaseURL %q is malformed: %w", override, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("BaseURL %q must use http or https scheme (got %q)", override, parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("BaseURL %q is missing a host", override)
	}
	return override, nil
}

// GetIssue fetches the issue identified by key (e.g. "ENG-123") with up to
// the last maxCommentsKept comments rendered as plaintext.
func (c *Client) GetIssue(ctx context.Context, key string) (*Issue, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("issue key is required")
	}
	c.throttle()

	u := fmt.Sprintf("%s/rest/api/3/issue/%s?fields=summary,status,description,comment",
		c.baseURL, url.PathEscape(key))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.SetBasicAuth(c.email, c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if err := mapStatus(resp); err != nil {
		return nil, err
	}
	return decodeIssue(resp.Body)
}

// SearchIssues runs a JQL query against /rest/api/3/search and returns up to
// `limit` matches with summary + status only. Description is omitted to keep
// payloads small — most callers (e.g. the master-plan resolver) only need
// the issue key.
//
// JQL example: `project = MCPI AND summary ~ "\"134.A.1\""`. The double
// quotes inside the value force literal substring match (unquoted terms
// are tokenised by Lucene).
func (c *Client) SearchIssues(ctx context.Context, jql string, limit int) ([]Issue, error) {
	jql = strings.TrimSpace(jql)
	if jql == "" {
		return nil, errors.New("jql is required")
	}
	if limit <= 0 {
		limit = 10
	}
	c.throttle()

	u := fmt.Sprintf("%s/rest/api/3/search?jql=%s&fields=summary,status&maxResults=%d",
		c.baseURL, url.QueryEscape(jql), limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.SetBasicAuth(c.email, c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if err := mapStatus(resp); err != nil {
		return nil, err
	}
	var raw struct {
		Issues []struct {
			Key    string `json:"key"`
			Fields struct {
				Summary string `json:"summary"`
				Status  struct {
					Name string `json:"name"`
				} `json:"status"`
			} `json:"fields"`
		} `json:"issues"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	out := make([]Issue, 0, len(raw.Issues))
	for _, i := range raw.Issues {
		out = append(out, Issue{Key: i.Key, Summary: i.Fields.Summary, Status: i.Fields.Status.Name})
	}
	return out, nil
}

// Transition is one edge in a Jira workflow — a directed move from the
// issue's current status to a target status, addressable by ID.
//
// Jira concept reminder:
//   - Status (e.g. "In Progress") is just a label; you cannot PATCH it.
//   - Workflow is a state machine; valid transitions depend on the
//     project's workflow design and the current status.
//   - Transitions can also have side effects (assignment, fields updates)
//     configured in the workflow editor. We only execute by ID + comment.
type Transition struct {
	ID       string
	Name     string // operator-facing label, e.g. "Mark Done"
	ToStatus string // target status name, e.g. "Done"
}

// ListTransitions returns the transitions currently available on the issue.
// Available = the workflow edges from the issue's current status that the
// authenticated user has permission to execute.
func (c *Client) ListTransitions(ctx context.Context, key string) ([]Transition, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("issue key is required")
	}
	c.throttle()

	u := fmt.Sprintf("%s/rest/api/3/issue/%s/transitions", c.baseURL, url.PathEscape(key))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.SetBasicAuth(c.email, c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if err := mapStatus(resp); err != nil {
		return nil, err
	}
	return decodeTransitions(resp.Body)
}

func decodeTransitions(body io.Reader) ([]Transition, error) {
	var raw struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	out := make([]Transition, 0, len(raw.Transitions))
	for _, t := range raw.Transitions {
		out = append(out, Transition{ID: t.ID, Name: t.Name, ToStatus: t.To.Name})
	}
	return out, nil
}

// DoTransition executes the named transition on the issue. When comment
// is non-empty, it is wrapped as ADF and added atomically with the
// transition (single round-trip). Returns nil on success (Jira responds
// 204 No Content).
func (c *Client) DoTransition(ctx context.Context, key, transitionID, comment string) error {
	key = strings.TrimSpace(key)
	transitionID = strings.TrimSpace(transitionID)
	if key == "" || transitionID == "" {
		return errors.New("key and transitionID are required")
	}
	c.throttle()

	payload := map[string]any{
		"transition": map[string]any{"id": transitionID},
	}
	if strings.TrimSpace(comment) != "" {
		payload["update"] = map[string]any{
			"comment": []map[string]any{
				{"add": map[string]any{"body": PlainToADF(comment)}},
			},
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	u := fmt.Sprintf("%s/rest/api/3/issue/%s/transitions", c.baseURL, url.PathEscape(key))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.SetBasicAuth(c.email, c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	return mapStatus(resp)
}

// FindTransitionByStatus returns the first Transition whose ToStatus or
// Name case-insensitively matches target. Some workflows label transitions
// distinctly from target status names ("Mark Complete" → "Done"); this
// helper accepts either to match operator intent.
func FindTransitionByStatus(transitions []Transition, target string) *Transition {
	t := strings.ToLower(strings.TrimSpace(target))
	if t == "" {
		return nil
	}
	for i := range transitions {
		if strings.ToLower(transitions[i].ToStatus) == t {
			return &transitions[i]
		}
	}
	for i := range transitions {
		if strings.ToLower(transitions[i].Name) == t {
			return &transitions[i]
		}
	}
	return nil
}

// PlainToADF wraps a plaintext string as a minimal ADF document for
// Atlassian endpoints that require ADF input (comment bodies,
// descriptions). Counterpart to RenderADF.
func PlainToADF(text string) map[string]any {
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

// MarkdownToADF builds a richer ADF document from a Markdown-ish string,
// recognizing H2 (`## `) and H3 (`### `) headings as ADF heading nodes,
// and treating other lines as paragraphs. Bullet lines (`* ` or `- `) are
// emitted as bulletList items. Best-effort — not a full Markdown parser;
// good enough for templated Epic/Story descriptions.
func MarkdownToADF(md string) map[string]any {
	lines := strings.Split(md, "\n")
	content := make([]map[string]any, 0, len(lines))

	flushBullets := func(items []map[string]any) []map[string]any {
		if len(items) == 0 {
			return nil
		}
		listContent := make([]map[string]any, 0, len(items))
		listContent = append(listContent, items...)
		content = append(content, map[string]any{
			"type":    "bulletList",
			"content": listContent,
		})
		return nil
	}

	var bulletBuf []map[string]any
	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		if line == "" {
			bulletBuf = flushBullets(bulletBuf)
			continue
		}
		switch {
		case strings.HasPrefix(line, "## "):
			bulletBuf = flushBullets(bulletBuf)
			content = append(content, adfHeading(2, strings.TrimPrefix(line, "## ")))
		case strings.HasPrefix(line, "### "):
			bulletBuf = flushBullets(bulletBuf)
			content = append(content, adfHeading(3, strings.TrimPrefix(line, "### ")))
		case strings.HasPrefix(line, "# "):
			bulletBuf = flushBullets(bulletBuf)
			content = append(content, adfHeading(1, strings.TrimPrefix(line, "# ")))
		case strings.HasPrefix(line, "* ") || strings.HasPrefix(line, "- "):
			bulletBuf = append(bulletBuf, adfListItem(strings.TrimSpace(line[2:])))
		default:
			bulletBuf = flushBullets(bulletBuf)
			content = append(content, adfParagraph(line))
		}
	}
	bulletBuf = flushBullets(bulletBuf)
	_ = bulletBuf

	if len(content) == 0 {
		// Fall back to a single empty paragraph so Atlassian accepts the doc.
		content = []map[string]any{adfParagraph("")}
	}
	return map[string]any{
		"type":    "doc",
		"version": 1,
		"content": content,
	}
}

func adfHeading(level int, text string) map[string]any {
	return map[string]any{
		"type":  "heading",
		"attrs": map[string]any{"level": level},
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	}
}

func adfParagraph(text string) map[string]any {
	if text == "" {
		return map[string]any{"type": "paragraph"}
	}
	return map[string]any{
		"type": "paragraph",
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	}
}

func adfListItem(text string) map[string]any {
	return map[string]any{
		"type": "listItem",
		"content": []map[string]any{
			adfParagraph(text),
		},
	}
}

// CreateIssueInput captures the fields needed to create an Epic or Story
// (or any issuetype) via POST /rest/api/3/issue. Optional fields are
// omitted from the request when zero-valued.
//
// For Epic→Story relationships in Jira Cloud (post-2022): set ParentKey
// on the Story to the Epic's key. Older Jira instances used
// "Epic Link" custom field — not supported here.
type CreateIssueInput struct {
	ProjectKey        string   // required: e.g. "MCPI"
	IssueType         string   // required: "Epic", "Story", "Bug", "Task", ...
	Summary           string   // required
	Description       string   // optional plaintext or basic Markdown — wrapped to ADF
	ParentKey         string   // optional: for Story, the Epic's key
	AssigneeAccountID string   // optional: Atlassian accountId (use LookupAccountByEmail)
	ReporterAccountID string   // optional
	Labels            []string // optional
	DueDate           string   // optional: YYYY-MM-DD (Jira "due date" = end date)
	StartDate         string   // optional: YYYY-MM-DD (custom field, requires StartDateField)
	StartDateField    string   // override custom field ID (default "customfield_10015"; varies per instance)
	StoryPoints       float64  // 0 = not set
	// StoryPointsFields lists every custom field ID to populate with
	// StoryPoints. Defaults to {"customfield_10016", "customfield_10038"}
	// — Jira Cloud sometimes ships BOTH "Story point estimate" (modern,
	// 10016) and "Story Points" (legacy/Asana-style, 10038), and which
	// one the UI surfaces depends on instance config. Writing to both
	// keeps the value visible regardless of which field the screen
	// scheme exposes.
	StoryPointsFields []string
}

// CreateIssueOutput is the slim view of POST /rest/api/3/issue response.
type CreateIssueOutput struct {
	Key  string
	ID   string
	Self string
}

// CreateIssue posts a new issue to /rest/api/3/issue. Returns 201 Created
// on success. Maps 4xx to typed errors. Field assembly delegated to
// buildIssueFields to keep this orchestrator under the CC cap.
func (c *Client) CreateIssue(ctx context.Context, in CreateIssueInput) (*CreateIssueOutput, error) {
	if err := validateCreateIssueInput(in); err != nil {
		return nil, err
	}
	c.throttle()

	body, err := json.Marshal(map[string]any{"fields": buildIssueFields(in)})
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	u := fmt.Sprintf("%s/rest/api/3/issue", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.SetBasicAuth(c.email, c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if err := mapStatus(resp); err != nil {
		return nil, err
	}
	var raw struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Self string `json:"self"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &CreateIssueOutput{Key: raw.Key, ID: raw.ID, Self: raw.Self}, nil
}

// validateCreateIssueInput enforces required-field invariants for CreateIssue.
func validateCreateIssueInput(in CreateIssueInput) error {
	if strings.TrimSpace(in.ProjectKey) == "" {
		return errors.New("projectKey is required")
	}
	if strings.TrimSpace(in.IssueType) == "" {
		return errors.New("issueType is required")
	}
	if strings.TrimSpace(in.Summary) == "" {
		return errors.New("summary is required")
	}
	return nil
}

// buildIssueFields assembles the `fields` map of the create-issue payload,
// omitting optional fields when empty. Custom field IDs (start_date,
// story_points) default to common Jira Cloud values; CreateIssueInput
// allows overrides per-instance.
func buildIssueFields(in CreateIssueInput) map[string]any {
	fields := map[string]any{
		"project":   map[string]any{"key": in.ProjectKey},
		"issuetype": map[string]any{"name": in.IssueType},
		"summary":   in.Summary,
	}
	if strings.TrimSpace(in.Description) != "" {
		fields["description"] = MarkdownToADF(in.Description)
	}
	if strings.TrimSpace(in.ParentKey) != "" {
		fields["parent"] = map[string]any{"key": in.ParentKey}
	}
	if strings.TrimSpace(in.AssigneeAccountID) != "" {
		fields["assignee"] = map[string]any{"accountId": in.AssigneeAccountID}
	}
	if strings.TrimSpace(in.ReporterAccountID) != "" {
		fields["reporter"] = map[string]any{"accountId": in.ReporterAccountID}
	}
	if len(in.Labels) > 0 {
		fields["labels"] = in.Labels
	}
	if strings.TrimSpace(in.DueDate) != "" {
		fields["duedate"] = in.DueDate
	}
	addCustomFields(fields, in)
	return fields
}

// addCustomFields applies start-date and story-points custom fields with
// defaults that match the most common Jira Cloud configurations.
//
// Story points: writes to every ID in StoryPointsFields (default
// "customfield_10016" + "customfield_10038"). See CreateIssueInput
// docstring for why dual-write.
func addCustomFields(fields map[string]any, in CreateIssueInput) {
	if strings.TrimSpace(in.StartDate) != "" {
		fld := in.StartDateField
		if fld == "" {
			fld = "customfield_10015"
		}
		fields[fld] = in.StartDate
	}
	if in.StoryPoints > 0 {
		ids := in.StoryPointsFields
		if len(ids) == 0 {
			ids = []string{"customfield_10016", "customfield_10038"}
		}
		for _, fld := range ids {
			fields[fld] = in.StoryPoints
		}
	}
}

// UpdateIssueInput drives PUT /rest/api/3/issue/{key}. Empty string fields
// are OMITTED from the payload — clean PATCH semantics where you only send
// what you intend to change. Labels is the exception: a non-nil slice
// (even empty) replaces the entire labels array on the ticket.
//
// AssigneeAccountID expects an Atlassian accountId. Callers that have an
// email should resolve via LookupAccountByEmail first (same contract as
// CreateIssueInput).
//
// To CLEAR a field (set to null), the Jira API requires a different
// payload shape (using `update` instead of `fields` with `set:null`). This
// helper does NOT cover the clear case — for that, call PUT directly with
// a custom body. Use case is rare; backfilling is the common need.
type UpdateIssueInput struct {
	Summary           string   // empty = skip
	Description       string   // empty = skip; non-empty wrapped via MarkdownToADF
	Labels            []string // nil = skip, non-nil = replace (incl. empty slice clears all)
	AssigneeAccountID string   // empty = skip
	DueDate           string   // YYYY-MM-DD; empty = skip
	StartDate         string   // YYYY-MM-DD; empty = skip
	StartDateField    string   // override custom field ID (default "customfield_10015")
}

// UpdateIssue patches the issue identified by key. Returns nil on 204
// No Content; ErrNotFound on 404; ErrAuth on 401/403; other errors wrapped.
//
// At least one mutable field must be set in `in` — otherwise we'd send an
// empty payload, which Jira accepts but is wasteful round-trip; we reject
// early with a clear error to surface caller bugs.
func (c *Client) UpdateIssue(ctx context.Context, key string, in UpdateIssueInput) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("issue key is required")
	}
	fields := buildUpdateFields(in)
	if len(fields) == 0 {
		return errors.New("at least one field must be set in UpdateIssueInput")
	}
	c.throttle()

	body, err := json.Marshal(map[string]any{"fields": fields})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	u := fmt.Sprintf("%s/rest/api/3/issue/%s", c.baseURL, url.PathEscape(key))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.SetBasicAuth(c.email, c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	// Jira PUT returns 204 No Content on success. mapStatus handles
	// 401/403/404/429 → typed errors; other 4xx/5xx → generic error
	// containing the response body for debug.
	return mapStatus(resp)
}

// buildUpdateFields assembles the partial-update payload, omitting
// fields that the caller didn't set. Mirrors buildIssueFields but skips
// project/issuetype (immutable post-create) and treats Labels with non-nil
// vs nil semantics so callers can explicitly clear vs preserve.
func buildUpdateFields(in UpdateIssueInput) map[string]any {
	fields := map[string]any{}
	if strings.TrimSpace(in.Summary) != "" {
		fields["summary"] = in.Summary
	}
	if strings.TrimSpace(in.Description) != "" {
		fields["description"] = MarkdownToADF(in.Description)
	}
	if in.Labels != nil {
		// Pass through whatever slice the caller gave us (including empty,
		// which clears the labels). nil distinguishes "don't touch".
		fields["labels"] = in.Labels
	}
	if strings.TrimSpace(in.AssigneeAccountID) != "" {
		fields["assignee"] = map[string]any{"accountId": in.AssigneeAccountID}
	}
	if strings.TrimSpace(in.DueDate) != "" {
		fields["duedate"] = in.DueDate
	}
	if strings.TrimSpace(in.StartDate) != "" {
		fld := in.StartDateField
		if fld == "" {
			fld = "customfield_10015"
		}
		fields[fld] = in.StartDate
	}
	return fields
}

// LinkIssue creates an issue link between two existing tickets. Common
// linkType values: "Relates", "Blocks", "Duplicates", "Cloners". Validate
// against your instance's available link types if unsure.
//
// This is for ARBITRARY issue links (relates, blocks, etc.). For
// Epic→Story parent relationships, set ParentKey at Story creation time
// in CreateIssueInput — that uses Jira Cloud's modern hierarchy API.
func (c *Client) LinkIssue(ctx context.Context, fromKey, toKey, linkType string) error {
	if strings.TrimSpace(fromKey) == "" || strings.TrimSpace(toKey) == "" || strings.TrimSpace(linkType) == "" {
		return errors.New("fromKey, toKey, and linkType are required")
	}
	c.throttle()

	payload := map[string]any{
		"type":         map[string]any{"name": linkType},
		"inwardIssue":  map[string]any{"key": fromKey},
		"outwardIssue": map[string]any{"key": toKey},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	u := fmt.Sprintf("%s/rest/api/3/issueLink", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.SetBasicAuth(c.email, c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	return mapStatus(resp)
}

// AttachFile uploads a single file to an issue via POST
// /rest/api/3/issue/{key}/attachments. Multipart upload with the
// X-Atlassian-Token: no-check header (Atlassian-mandated for attachment
// endpoints to bypass XSRF). Returns the count of attachments registered.
//
// Use AttachZipFolder for the operator flow: zip a local folder and
// upload it as a single artifact.
func (c *Client) AttachFile(ctx context.Context, key, filePath string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("issue key is required")
	}
	if strings.TrimSpace(filePath) == "" {
		return errors.New("filePath is required")
	}
	c.throttle()

	body, contentType, err := buildAttachmentBody(filePath)
	if err != nil {
		return fmt.Errorf("build multipart: %w", err)
	}
	u := fmt.Sprintf("%s/rest/api/3/issue/%s/attachments", c.baseURL, url.PathEscape(key))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.SetBasicAuth(c.email, c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Atlassian-Token", "no-check")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	return mapStatus(resp)
}

// LookupAccountByEmail returns the Atlassian accountId for the given
// email address, used to set assignee/reporter at issue creation. Returns
// ErrNotFound when no user matches.
func (c *Client) LookupAccountByEmail(ctx context.Context, email string) (string, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return "", errors.New("email is required")
	}
	c.throttle()

	u := fmt.Sprintf("%s/rest/api/3/user/search?query=%s", c.baseURL, url.QueryEscape(email))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.SetBasicAuth(c.email, c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if err := mapStatus(resp); err != nil {
		return "", err
	}
	var users []struct {
		AccountID    string `json:"accountId"`
		EmailAddress string `json:"emailAddress"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if len(users) == 0 {
		return "", ErrNotFound
	}
	return users[0].AccountID, nil
}

// throttle blocks until at least minGap has elapsed since the previous
// request. Best-effort client-side rate limit.
func (c *Client) throttle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	elapsed := time.Since(c.lastReq)
	if elapsed < c.minGap {
		time.Sleep(c.minGap - elapsed)
	}
	c.lastReq = time.Now()
}

// mapStatus converts non-2xx HTTP responses into typed errors. Caller can
// errors.Is against ErrNotFound / ErrAuth / ErrRateLimited. Accepts any
// 2xx as success — Jira returns 204 No Content for transitions.
func mapStatus(resp *http.Response) error {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: status %d", ErrAuth, resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%w: Retry-After=%s", ErrRateLimited, resp.Header.Get("Retry-After"))
	case resp.StatusCode >= 500:
		return fmt.Errorf("upstream error: status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("api error %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// decodeIssue parses the slim view of /rest/api/3/issue/{key}. Trims comments
// to the last maxCommentsKept entries.
func decodeIssue(body io.Reader) (*Issue, error) {
	var raw struct {
		Key    string `json:"key"`
		Fields struct {
			Summary string `json:"summary"`
			Status  struct {
				Name string `json:"name"`
			} `json:"status"`
			Description any `json:"description"`
			Comment     struct {
				Comments []struct {
					Author struct {
						DisplayName string `json:"displayName"`
					} `json:"author"`
					Body    any    `json:"body"`
					Created string `json:"created"`
				} `json:"comments"`
			} `json:"comment"`
		} `json:"fields"`
	}
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	issue := &Issue{
		Key:         raw.Key,
		Summary:     raw.Fields.Summary,
		Status:      raw.Fields.Status.Name,
		Description: RenderADF(raw.Fields.Description),
	}
	comments := raw.Fields.Comment.Comments
	if len(comments) > maxCommentsKept {
		comments = comments[len(comments)-maxCommentsKept:]
	}
	for _, com := range comments {
		issue.Comments = append(issue.Comments, Comment{
			Author:  com.Author.DisplayName,
			Body:    RenderADF(com.Body),
			Created: com.Created,
		})
	}
	return issue, nil
}

// RenderADF converts Atlassian Document Format to plaintext, best-effort.
// Handles text nodes and recursive `content` arrays. Adds newlines after
// `paragraph` and `heading` blocks. Unknown node types are traversed for
// their text content but do not add structure. Strings are returned as-is.
//
// Exported because the plugin formatter needs it too.
func RenderADF(node any) string {
	if node == nil {
		return ""
	}
	if s, ok := node.(string); ok {
		return s
	}
	var sb strings.Builder
	renderADFNode(node, &sb)
	return strings.TrimRight(sb.String(), "\n")
}

func renderADFNode(node any, sb *strings.Builder) {
	obj, ok := node.(map[string]any)
	if !ok {
		return
	}
	if t, _ := obj["type"].(string); t == "text" {
		if text, _ := obj["text"].(string); text != "" {
			sb.WriteString(text)
		}
		return
	}
	if content, ok := obj["content"].([]any); ok {
		for _, child := range content {
			renderADFNode(child, sb)
		}
	}
	if t, _ := obj["type"].(string); t == "paragraph" || t == "heading" {
		sb.WriteString("\n")
	}
}
