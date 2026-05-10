package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewClient_RequiresToken(t *testing.T) {
	if _, err := NewClient(Config{Token: ""}); err == nil {
		t.Errorf("expected error for empty token")
	}
}

func TestNewClient_DefaultsToAPIGitHub(t *testing.T) {
	c, err := NewClient(Config{Token: "ghp_test"})
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://api.github.com" {
		t.Errorf("BaseURL = %q, want https://api.github.com", c.BaseURL)
	}
}

func TestNewClient_RejectsInvalidBaseURL(t *testing.T) {
	if _, err := NewClient(Config{Token: "ghp", BaseURL: "://broken"}); err == nil {
		t.Errorf("expected error for invalid base_url")
	}
}

func TestListPRs_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Auth + headers expected
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("missing Authorization header: %q", got)
		}
		if r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Errorf("missing X-GitHub-Api-Version header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[
			{"number":1,"title":"Fix login","state":"open","html_url":"https://gh/1","user":{"login":"alice"},"head":{"ref":"feat"},"base":{"ref":"main"}},
			{"number":2,"title":"Bump deps","state":"open","html_url":"https://gh/2","user":{"login":"bob"},"head":{"ref":"deps"},"base":{"ref":"main"}}
		]`))
	}))
	defer srv.Close()

	c, _ := NewClient(Config{BaseURL: srv.URL, Token: "ghp_test"})
	c.HTTP = srv.Client()
	prs, err := c.ListPRs(context.Background(), "owner", "repo", "open")
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 2 {
		t.Fatalf("got %d prs, want 2", len(prs))
	}
	if prs[0].Number != 1 || prs[0].User.Login != "alice" {
		t.Errorf("PR shape decode failed: %+v", prs[0])
	}
}

func TestListIssues_FiltersPRs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Mix issues + PRs (PRs have a pull_request key per GitHub API)
		w.Write([]byte(`[
			{"number":10,"title":"Bug: crash","state":"open","html_url":"https://gh/10","user":{"login":"alice"}},
			{"number":11,"title":"PR: fix","state":"open","html_url":"https://gh/11","user":{"login":"bob"},"pull_request":{"url":"https://gh/pulls/11"}},
			{"number":12,"title":"Feat: search","state":"open","html_url":"https://gh/12","user":{"login":"carol"}}
		]`))
	}))
	defer srv.Close()

	c, _ := NewClient(Config{BaseURL: srv.URL, Token: "ghp_test"})
	c.HTTP = srv.Client()
	issues, err := c.ListIssues(context.Background(), "owner", "repo", "open")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 {
		t.Errorf("got %d issues, want 2 (PR should be filtered)", len(issues))
	}
}

func TestDo_RetriesOn5xx(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"err":"transient"}`))
			return
		}
		w.Write([]byte(`{"login":"alice"}`))
	}))
	defer srv.Close()

	c, _ := NewClient(Config{BaseURL: srv.URL, Token: "ghp_test"})
	c.HTTP = srv.Client()
	// Use very short retry delays for the test by invoking via a
	// 1-second context — first 2 attempts fail, 3rd succeeds.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	login, err := c.GetUser(ctx)
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if login != "alice" {
		t.Errorf("login = %q, want alice", login)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestDo_4xxIsNotRetried(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	c, _ := NewClient(Config{BaseURL: srv.URL, Token: "ghp_test"})
	c.HTTP = srv.Client()
	_, err := c.GetUser(context.Background())
	if err == nil {
		t.Fatal("expected error on 401")
	}
	apiErr := &APIError{}
	if !errors.As(err, &apiErr) || apiErr.Status != 401 {
		t.Errorf("expected APIError 401, got %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("4xx should not retry; attempts = %d", got)
	}
}

// ── Read-side endpoint tests (Area 2.2.E) ────────────────────────────

func TestGetPR_ReturnsFullDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/foo/bar/pulls/42" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":42,"title":"Big PR","state":"open","mergeable":true,"body":"changelog\nlines","html_url":"https://gh/42"}`))
	}))
	defer srv.Close()
	c, _ := NewClient(Config{BaseURL: srv.URL, Token: "ghp_test"})
	c.HTTP = srv.Client()
	pr, err := c.GetPR(context.Background(), "foo", "bar", 42)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if pr["title"].(string) != "Big PR" {
		t.Errorf("title=%q want Big PR", pr["title"])
	}
	if pr["mergeable"].(bool) != true {
		t.Errorf("mergeable=%v want true", pr["mergeable"])
	}
}

func TestGetPR_RejectsZeroNumber(t *testing.T) {
	c, _ := NewClient(Config{BaseURL: "https://api.github.com", Token: "ghp_test"})
	if _, err := c.GetPR(context.Background(), "foo", "bar", 0); err == nil {
		t.Errorf("expected error for number=0")
	}
}

func TestGetIssue_ReturnsTypedStruct(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/foo/bar/issues/7" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":7,"title":"Bug X","state":"open","body":"steps","html_url":"https://gh/7","user":{"login":"alice"}}`))
	}))
	defer srv.Close()
	c, _ := NewClient(Config{BaseURL: srv.URL, Token: "ghp_test"})
	c.HTTP = srv.Client()
	iss, err := c.GetIssue(context.Background(), "foo", "bar", 7)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if iss.Title != "Bug X" || iss.User.Login != "alice" {
		t.Errorf("got %+v", iss)
	}
}

func TestAddIssueComment_PostsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s want POST", r.Method)
		}
		if r.URL.Path != "/repos/foo/bar/issues/7/comments" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":12345,"body":"thanks","html_url":"https://gh/cmt/12345","user":{"login":"alice"},"created_at":"2026-05-10T00:00:00Z"}`))
	}))
	defer srv.Close()
	c, _ := NewClient(Config{BaseURL: srv.URL, Token: "ghp_test"})
	c.HTTP = srv.Client()
	cmt, err := c.AddIssueComment(context.Background(), "foo", "bar", 7, "thanks")
	if err != nil {
		t.Fatalf("AddIssueComment: %v", err)
	}
	if cmt.ID != 12345 {
		t.Errorf("ID=%d want 12345", cmt.ID)
	}
}

func TestAddIssueComment_RejectsEmptyBody(t *testing.T) {
	c, _ := NewClient(Config{BaseURL: "https://api.github.com", Token: "ghp_test"})
	if _, err := c.AddIssueComment(context.Background(), "foo", "bar", 1, ""); err == nil {
		t.Errorf("expected error for empty body")
	}
}

func TestListFiles_DirReturnsArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/foo/bar/contents/src" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("ref") != "main" {
			t.Errorf("ref=%q want main", r.URL.Query().Get("ref"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"name":"a.go","path":"src/a.go","type":"file","size":120,"sha":"abc"},
			{"name":"sub","path":"src/sub","type":"dir","size":0,"sha":"def"}
		]`))
	}))
	defer srv.Close()
	c, _ := NewClient(Config{BaseURL: srv.URL, Token: "ghp_test"})
	c.HTTP = srv.Client()
	entries, err := c.ListFiles(context.Background(), "foo", "bar", "src", "main")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(entries) != 2 || entries[0].Type != "file" || entries[1].Type != "dir" {
		t.Errorf("entries=%+v", entries)
	}
}

func TestListFiles_SingleFileWrapsAsList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"README.md","path":"README.md","type":"file","size":42,"sha":"123"}`))
	}))
	defer srv.Close()
	c, _ := NewClient(Config{BaseURL: srv.URL, Token: "ghp_test"})
	c.HTTP = srv.Client()
	entries, err := c.ListFiles(context.Background(), "foo", "bar", "README.md", "")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "README.md" {
		t.Errorf("expected single-file wrap, got %+v", entries)
	}
}

func TestGetFile_DecodesBase64(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// base64("hello\nworld\n")
		_, _ = w.Write([]byte(`{"name":"f.txt","path":"f.txt","type":"file","size":12,"encoding":"base64","content":"aGVsbG8KdW9ybGQK"}`))
	}))
	defer srv.Close()
	c, _ := NewClient(Config{BaseURL: srv.URL, Token: "ghp_test"})
	c.HTTP = srv.Client()
	content, err := c.GetFile(context.Background(), "foo", "bar", "f.txt", "")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if !strings.Contains(content, "hello") {
		t.Errorf("decoded content=%q want contains hello", content)
	}
}

func TestGetFile_RejectsLargeFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"big.bin","path":"big.bin","type":"file","size":2000000,"encoding":"none","content":""}`))
	}))
	defer srv.Close()
	c, _ := NewClient(Config{BaseURL: srv.URL, Token: "ghp_test"})
	c.HTTP = srv.Client()
	_, err := c.GetFile(context.Background(), "foo", "bar", "big.bin", "")
	if err == nil {
		t.Errorf("expected error for encoding=none (file too large)")
	}
}

func TestGetFile_RejectsDirPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"src","path":"src","type":"dir","size":0,"sha":"abc"}`))
	}))
	defer srv.Close()
	c, _ := NewClient(Config{BaseURL: srv.URL, Token: "ghp_test"})
	c.HTTP = srv.Client()
	_, err := c.GetFile(context.Background(), "foo", "bar", "src", "")
	if err == nil {
		t.Errorf("expected error when path resolves to a dir")
	}
}

func TestSearchCode_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/code" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("q") != "auth.Load language:go" {
			t.Errorf("q=%q", r.URL.Query().Get("q"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_count":2,"items":[
			{"path":"pkg/auth/keystore.go","name":"keystore.go","html_url":"https://gh/x","score":1.5,"repository":{"full_name":"foo/bar"}},
			{"path":"cmd/neo/main.go","name":"main.go","html_url":"https://gh/y","score":1.2,"repository":{"full_name":"foo/bar"}}
		]}`))
	}))
	defer srv.Close()
	c, _ := NewClient(Config{BaseURL: srv.URL, Token: "ghp_test"})
	c.HTTP = srv.Client()
	hits, err := c.SearchCode(context.Background(), "auth.Load language:go")
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits=%d want 2", len(hits))
	}
	if hits[0].Repository.FullName != "foo/bar" {
		t.Errorf("repo=%q want foo/bar", hits[0].Repository.FullName)
	}
}

func TestListCommits_AppliesBranchQueryParam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sha") != "develop" {
			t.Errorf("sha=%q want develop", r.URL.Query().Get("sha"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"sha":"abc1234567","html_url":"https://gh/c1","commit":{"message":"feat: x\n\nbody","author":{"name":"alice","email":"a@x.com","date":"2026-05-10T00:00:00Z"}}}
		]`))
	}))
	defer srv.Close()
	c, _ := NewClient(Config{BaseURL: srv.URL, Token: "ghp_test"})
	c.HTTP = srv.Client()
	commits, err := c.ListCommits(context.Background(), "foo", "bar", "develop")
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	if len(commits) != 1 || commits[0].SHA != "abc1234567" {
		t.Errorf("commits=%+v", commits)
	}
}

// Suppress unused-import warning when the test file doesn't reference
// these directly (they're used elsewhere in the file).
var _ = errors.New
var _ atomic.Int64
var _ time.Time
