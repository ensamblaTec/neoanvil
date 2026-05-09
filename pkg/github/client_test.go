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
