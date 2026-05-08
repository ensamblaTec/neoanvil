package jira

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient wires a Client to a stub HTTP server and returns both.
// Tests must defer srv.Close().
func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Strip the http:// prefix to mimic the "domain only" config the
	// production CLI passes in. Then redirect via custom RoundTripper so
	// the Client builds https://<fake>/... but actually hits srv.URL.
	domain := strings.TrimPrefix(srv.URL, "http://")
	c, err := NewClient(Config{
		Domain: domain,
		Email:  "user@acme.com",
		Token:  "tok",
		HTTP:   srv.Client(),
		MinGap: time.Microsecond, // disable throttle for unit tests
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// Override the URL builder by using a transport that rewrites scheme.
	c.http = &http.Client{Transport: &schemeRewrite{base: srv.Client().Transport}}
	return c, srv
}

// schemeRewrite rewrites https→http on outgoing requests so the test server
// (which is plain HTTP) handles them. RoundTripper-only — no body changes.
type schemeRewrite struct {
	base http.RoundTripper
}

func (s *schemeRewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme == "https" {
		r.URL.Scheme = "http"
	}
	rt := s.base
	if rt == nil {
		rt = http.DefaultTransport
	}
	return rt.RoundTrip(r)
}

// readAllSafe drains the test request body without panicking on EOF.
func readAllSafe(r interface{ Read(p []byte) (int, error) }) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf, nil
}

// happyIssueResponse mimics the Atlassian /rest/api/3/issue/{key} payload
// with description + 5 comments (3 should be kept).
const happyIssueResponse = `{
  "key": "ENG-42",
  "fields": {
    "summary": "Fix the auth bug",
    "status": {"name": "In Progress"},
    "description": {
      "type": "doc",
      "content": [
        {"type":"paragraph","content":[{"type":"text","text":"Steps to reproduce:"}]},
        {"type":"paragraph","content":[{"type":"text","text":"1. Open login page"}]}
      ]
    },
    "comment": {
      "comments": [
        {"author":{"displayName":"Alice"},"body":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Old comment 1"}]}]},"created":"2026-01-01T00:00:00Z"},
        {"author":{"displayName":"Alice"},"body":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Old comment 2"}]}]},"created":"2026-01-02T00:00:00Z"},
        {"author":{"displayName":"Bob"},"body":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Recent 1"}]}]},"created":"2026-04-26T00:00:00Z"},
        {"author":{"displayName":"Bob"},"body":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Recent 2"}]}]},"created":"2026-04-27T00:00:00Z"},
        {"author":{"displayName":"Carol"},"body":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Recent 3"}]}]},"created":"2026-04-28T00:00:00Z"}
      ]
    }
  }
}`

func TestNewClient_RequiredFields(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"missing domain", Config{Email: "x", Token: "y"}},
		{"missing email", Config{Domain: "x", Token: "y"}},
		{"missing token", Config{Domain: "x", Email: "y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewClient(tc.cfg); err == nil {
				t.Errorf("expected error for %q", tc.name)
			}
		})
	}
}

func TestGetIssue_HappyPath(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method=%s want GET", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/rest/api/3/issue/ENG-42") {
			t.Errorf("path=%q", r.URL.Path)
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "user@acme.com" || pass != "tok" {
			t.Errorf("basic auth wrong: ok=%v user=%q", ok, user)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, happyIssueResponse)
	}))

	issue, err := c.GetIssue(context.Background(), "ENG-42")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if issue.Key != "ENG-42" {
		t.Errorf("Key=%q", issue.Key)
	}
	if issue.Summary != "Fix the auth bug" {
		t.Errorf("Summary=%q", issue.Summary)
	}
	if issue.Status != "In Progress" {
		t.Errorf("Status=%q", issue.Status)
	}
	if !strings.Contains(issue.Description, "Steps to reproduce") || !strings.Contains(issue.Description, "1. Open login page") {
		t.Errorf("Description rendering lost ADF text:\n%s", issue.Description)
	}
	if len(issue.Comments) != 3 {
		t.Fatalf("comments=%d want 3 (last 3 kept)", len(issue.Comments))
	}
	if issue.Comments[0].Author != "Bob" || !strings.Contains(issue.Comments[0].Body, "Recent 1") {
		t.Errorf("first kept comment wrong: %+v", issue.Comments[0])
	}
	if issue.Comments[2].Author != "Carol" {
		t.Errorf("last kept comment wrong: %+v", issue.Comments[2])
	}
}

func TestGetIssue_404IsErrNotFound(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	_, err := c.GetIssue(context.Background(), "GHOST-1")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err=%v want ErrNotFound", err)
	}
}

func TestGetIssue_401IsErrAuth(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	_, err := c.GetIssue(context.Background(), "X-1")
	if !errors.Is(err, ErrAuth) {
		t.Errorf("err=%v want ErrAuth", err)
	}
}

func TestGetIssue_429IsErrRateLimited(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	_, err := c.GetIssue(context.Background(), "X-1")
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("err=%v want ErrRateLimited", err)
	}
	if !strings.Contains(err.Error(), "30") {
		t.Errorf("err should include Retry-After value, got %v", err)
	}
}

func TestGetIssue_5xxIsUpstream(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	_, err := c.GetIssue(context.Background(), "X-1")
	if err == nil || !strings.Contains(err.Error(), "upstream") {
		t.Errorf("err=%v want upstream error", err)
	}
}

func TestGetIssue_EmptyKeyFails(t *testing.T) {
	c, _ := newTestClient(t, http.NotFoundHandler())
	if _, err := c.GetIssue(context.Background(), ""); err == nil {
		t.Error("empty key should fail")
	}
}

func TestThrottle_EnforcesMinGap(t *testing.T) {
	c, err := NewClient(Config{
		Domain: "x", Email: "e", Token: "t",
		MinGap: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.lastReq = time.Now() // pretend we just made a request
	start := time.Now()
	c.throttle()
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("throttle slept %s, expected >= 40ms", elapsed)
	}
}

func TestRenderADF_PlainString(t *testing.T) {
	if got := RenderADF("hello"); got != "hello" {
		t.Errorf("got %q want hello", got)
	}
}

func TestRenderADF_NilEmpty(t *testing.T) {
	if got := RenderADF(nil); got != "" {
		t.Errorf("got %q want empty", got)
	}
}

// transitionsResponse mimics /rest/api/3/issue/{key}/transitions.
const transitionsResponse = `{
  "transitions": [
    {"id":"11","name":"Start Progress","to":{"name":"In Progress"}},
    {"id":"31","name":"Mark Done","to":{"name":"Done"}},
    {"id":"41","name":"Reopen","to":{"name":"To Do"}}
  ]
}`

func TestListTransitions_HappyPath(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/transitions") {
			t.Errorf("path=%q want suffix /transitions", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method=%s want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, transitionsResponse)
	}))

	transitions, err := c.ListTransitions(context.Background(), "ENG-42")
	if err != nil {
		t.Fatalf("ListTransitions: %v", err)
	}
	if len(transitions) != 3 {
		t.Fatalf("count=%d want 3", len(transitions))
	}
	if transitions[1].ID != "31" || transitions[1].ToStatus != "Done" || transitions[1].Name != "Mark Done" {
		t.Errorf("Done transition mismatched: %+v", transitions[1])
	}
}

func TestListTransitions_404IsErrNotFound(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	_, err := c.ListTransitions(context.Background(), "GHOST-1")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err=%v want ErrNotFound", err)
	}
}

func TestDoTransition_HappyPathWithComment(t *testing.T) {
	var capturedBody []byte
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s want POST", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/transitions") {
			t.Errorf("path=%q", r.URL.Path)
		}
		body, _ := readAllSafe(r.Body)
		capturedBody = body
		w.WriteHeader(http.StatusNoContent) // Jira returns 204 on transition
	}))

	err := c.DoTransition(context.Background(), "ENG-42", "31", "Resolved by neo-mcp")
	if err != nil {
		t.Fatalf("DoTransition: %v", err)
	}
	if !strings.Contains(string(capturedBody), `"id":"31"`) {
		t.Errorf("payload missing transition id: %s", capturedBody)
	}
	if !strings.Contains(string(capturedBody), "Resolved by neo-mcp") {
		t.Errorf("payload missing comment: %s", capturedBody)
	}
	if !strings.Contains(string(capturedBody), `"type":"doc"`) {
		t.Errorf("comment should be ADF-wrapped: %s", capturedBody)
	}
}

func TestDoTransition_HappyPathNoComment(t *testing.T) {
	var capturedBody []byte
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAllSafe(r.Body)
		capturedBody = body
		w.WriteHeader(http.StatusNoContent)
	}))

	if err := c.DoTransition(context.Background(), "ENG-42", "31", ""); err != nil {
		t.Fatalf("DoTransition: %v", err)
	}
	if strings.Contains(string(capturedBody), `"comment"`) {
		t.Errorf("empty comment should not appear in payload: %s", capturedBody)
	}
	if !strings.Contains(string(capturedBody), `"id":"31"`) {
		t.Errorf("payload missing transition id: %s", capturedBody)
	}
}

func TestDoTransition_RequiresKeyAndTransitionID(t *testing.T) {
	c, _ := newTestClient(t, http.NotFoundHandler())
	if err := c.DoTransition(context.Background(), "", "31", ""); err == nil {
		t.Error("empty key should fail")
	}
	if err := c.DoTransition(context.Background(), "X-1", "", ""); err == nil {
		t.Error("empty transitionID should fail")
	}
}

func TestFindTransitionByStatus_MatchesTo(t *testing.T) {
	transitions := []Transition{
		{ID: "11", Name: "Start", ToStatus: "In Progress"},
		{ID: "31", Name: "Mark Done", ToStatus: "Done"},
	}
	got := FindTransitionByStatus(transitions, "Done")
	if got == nil || got.ID != "31" {
		t.Errorf("FindTransitionByStatus(Done) = %+v, want id=31", got)
	}
}

func TestFindTransitionByStatus_MatchesName(t *testing.T) {
	transitions := []Transition{
		{ID: "31", Name: "Mark Done", ToStatus: "Closed"},
	}
	got := FindTransitionByStatus(transitions, "Mark Done")
	if got == nil || got.ID != "31" {
		t.Errorf("FindTransitionByStatus by name failed: %+v", got)
	}
}

func TestFindTransitionByStatus_CaseInsensitive(t *testing.T) {
	transitions := []Transition{
		{ID: "31", Name: "Done", ToStatus: "DONE"},
	}
	if got := FindTransitionByStatus(transitions, "done"); got == nil {
		t.Error("case-insensitive match should hit")
	}
}

func TestFindTransitionByStatus_NoMatch(t *testing.T) {
	transitions := []Transition{
		{ID: "31", Name: "Done", ToStatus: "Done"},
	}
	if got := FindTransitionByStatus(transitions, "Reopen"); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
	if got := FindTransitionByStatus(transitions, ""); got != nil {
		t.Errorf("empty target should not match")
	}
}

func TestPlainToADF_Shape(t *testing.T) {
	adf := PlainToADF("hello world")
	if t1, _ := adf["type"].(string); t1 != "doc" {
		t.Errorf("type=%v want doc", adf["type"])
	}
	content, _ := adf["content"].([]map[string]any)
	if len(content) != 1 {
		t.Fatalf("content len=%d want 1", len(content))
	}
	para := content[0]
	if t2, _ := para["type"].(string); t2 != "paragraph" {
		t.Errorf("paragraph type=%v", para["type"])
	}
	inner, _ := para["content"].([]map[string]any)
	if len(inner) != 1 || inner[0]["text"] != "hello world" {
		t.Errorf("text node lost: %+v", inner)
	}
}

func TestRenderADF_NestedDoc(t *testing.T) {
	doc := map[string]any{
		"type": "doc",
		"content": []any{
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{"type": "text", "text": "Line 1"},
				},
			},
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{"type": "text", "text": "Line 2"},
				},
			},
		},
	}
	got := RenderADF(doc)
	if !strings.Contains(got, "Line 1") || !strings.Contains(got, "Line 2") {
		t.Errorf("RenderADF lost text: %q", got)
	}
}
