package testmock

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// mustDo executes an arbitrary http.Request and t.Fatal-s on transport
// error so the caller can safely defer Body.Close() afterwards.
func mustDo(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http %s %s: %v", req.Method, req.URL.String(), err)
	}
	return resp
}

// ghAuthGet builds a GET request with the default GitHub mock token set.
func ghAuthGet(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer fake-github-token")
	return req
}

// ghAuthPost builds a POST request with the default GitHub mock token set.
func ghAuthPost(t *testing.T, url, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer fake-github-token")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestGitHubMock_ListPRsEmptyByDefault(t *testing.T) {
	m := NewGitHub(t)
	resp := mustDo(t, ghAuthGet(t, m.URL()+"/repos/acme/widgets/pulls"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var got []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len=%d want 0", len(got))
	}
}

func TestGitHubMock_ListPRsReturnsRegistered(t *testing.T) {
	m := NewGitHub(t)
	m.SetPRs("acme", "widgets", []GitHubPR{
		{Number: 42, Title: "Add foo", State: "open", User: "alice", Head: "feature/foo", Base: "main"},
		{Number: 43, Title: "Fix bar", State: "closed", User: "bob", Head: "fix/bar", Base: "main"},
	})

	resp := mustDo(t, ghAuthGet(t, m.URL()+"/repos/acme/widgets/pulls"))
	defer resp.Body.Close()

	var got []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
	if got[0].Number != 42 || got[0].Title != "Add foo" || got[0].User.Login != "alice" {
		t.Errorf("PR[0] decoded incorrectly: %+v", got[0])
	}
	if got[1].State != "closed" || got[1].Head.Ref != "fix/bar" {
		t.Errorf("PR[1] decoded incorrectly: %+v", got[1])
	}
}

func TestGitHubMock_RejectsMissingBearer(t *testing.T) {
	m := NewGitHub(t)
	resp := mustGet(t, m.URL()+"/repos/acme/widgets/pulls")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", resp.StatusCode)
	}
}

func TestGitHubMock_EmptyTokenRejectsAll(t *testing.T) {
	m := NewGitHub(t)
	m.SetToken("")
	resp := mustDo(t, ghAuthGet(t, m.URL()+"/repos/acme/widgets/pulls"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d want 401 with empty expected token", resp.StatusCode)
	}
}

func TestGitHubMock_CreateIssueAssignsNumber(t *testing.T) {
	m := NewGitHub(t)
	resp := mustDo(t, ghAuthPost(t, m.URL()+"/repos/acme/widgets/issues",
		`{"title":"first issue","body":"context","labels":["bug","p1"]}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d want 201", resp.StatusCode)
	}
	var got struct {
		Number  int      `json:"number"`
		Title   string   `json:"title"`
		Labels  []string `json:"labels"`
		State   string   `json:"state"`
		HTMLURL string   `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Number != 1 {
		t.Errorf("number=%d want 1", got.Number)
	}
	if got.Title != "first issue" {
		t.Errorf("title=%q", got.Title)
	}
	if len(got.Labels) != 2 || got.Labels[0] != "bug" {
		t.Errorf("labels=%v", got.Labels)
	}
	if got.State != "open" {
		t.Errorf("state=%q want open", got.State)
	}
	if !strings.Contains(got.HTMLURL, "/acme/widgets/issues/1") {
		t.Errorf("html_url=%q missing path", got.HTMLURL)
	}

	// Second create increments number monotonically.
	resp2 := mustDo(t, ghAuthPost(t, m.URL()+"/repos/acme/widgets/issues", `{"title":"second"}`))
	defer resp2.Body.Close()
	var got2 struct {
		Number int `json:"number"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&got2)
	if got2.Number != 2 {
		t.Errorf("second number=%d want 2", got2.Number)
	}
}

func TestGitHubMock_CreateIssueRequiresTitle(t *testing.T) {
	m := NewGitHub(t)
	resp := mustDo(t, ghAuthPost(t, m.URL()+"/repos/acme/widgets/issues", `{"body":"no title"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}
}

func TestGitHubMock_PRsScopedByOwnerRepo(t *testing.T) {
	m := NewGitHub(t)
	m.SetPRs("acme", "widgets", []GitHubPR{{Number: 1, Title: "acme/widgets", State: "open"}})
	m.SetPRs("other", "repo", []GitHubPR{{Number: 99, Title: "other/repo", State: "open"}})

	resp := mustDo(t, ghAuthGet(t, m.URL()+"/repos/acme/widgets/pulls"))
	defer resp.Body.Close()
	var got []struct {
		Number int `json:"number"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if len(got) != 1 || got[0].Number != 1 {
		t.Errorf("acme/widgets returned %v want [{1}]", got)
	}
}

func TestGitHubMock_CallHistory(t *testing.T) {
	m := NewGitHub(t)
	resp := mustDo(t, ghAuthGet(t, m.URL()+"/repos/acme/widgets/pulls"))
	_ = resp.Body.Close()

	if got := m.CallCount(); got != 1 {
		t.Errorf("CallCount=%d want 1", got)
	}
	calls := m.Calls()
	if len(calls) != 1 || calls[0].Method != http.MethodGet {
		t.Errorf("calls=%+v", calls)
	}
}
