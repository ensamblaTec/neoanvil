package jira

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestResolveMasterPlanIDExactMatch verifies a single-result JQL search
// returns the matching ticket key. [134.B.2]
func TestResolveMasterPlanIDExactMatch(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "jql=") {
			t.Errorf("expected jql query param, got %s", r.URL.RawQuery)
		}
		if !strings.Contains(r.URL.RawQuery, "MCPI") {
			t.Errorf("expected projectKey in JQL, got %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[{"key":"MCPI-52","fields":{"summary":"feat: Épica 130 — session portability","status":{"name":"In Progress"}}}]}`))
	}))

	got, err := ResolveMasterPlanID(context.Background(), c, "MCPI", "130", nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if got != "MCPI-52" {
		t.Errorf("expected MCPI-52, got %q", got)
	}
}

// TestResolveMasterPlanIDNotFound verifies an empty result returns ErrNotFound.
func TestResolveMasterPlanIDNotFound(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[]}`))
	}))

	got, err := ResolveMasterPlanID(context.Background(), c, "MCPI", "999.Z.999", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
	if got != "" {
		t.Errorf("expected empty key on not-found, got %q", got)
	}
}

// TestResolveMasterPlanIDAmbiguous verifies multi-result returns the first
// match wrapped in ErrAmbiguous so the caller knows the heuristic was applied.
func TestResolveMasterPlanIDAmbiguous(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[
			{"key":"MCPI-52","fields":{"summary":"feat: 130","status":{"name":"Done"}}},
			{"key":"MCPI-99","fields":{"summary":"feat: 130 followup","status":{"name":"Backlog"}}}
		]}`))
	}))

	got, err := ResolveMasterPlanID(context.Background(), c, "MCPI", "130", nil)
	if got != "MCPI-52" {
		t.Errorf("expected first match MCPI-52, got %q", got)
	}
	if !errors.Is(err, ErrAmbiguous) {
		t.Errorf("expected ErrAmbiguous wrapper, got %v", err)
	}
}

// TestResolveMasterPlanIDCacheHit verifies a populated cache short-circuits
// the HTTP call.
func TestResolveMasterPlanIDCacheHit(t *testing.T) {
	calls := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[]}`))
	}))

	cache := NewMemoryCache()
	cache.Put("130", "MCPI-52")
	got, err := ResolveMasterPlanID(context.Background(), c, "MCPI", "130", cache)
	if err != nil {
		t.Fatalf("cache hit should not error: %v", err)
	}
	if got != "MCPI-52" {
		t.Errorf("expected MCPI-52 from cache, got %q", got)
	}
	if calls != 0 {
		t.Errorf("cache hit must skip HTTP, got %d calls", calls)
	}
}

// TestResolveMasterPlanIDCachePopulatedOnMiss verifies the resolver writes
// the result back to the cache after a successful lookup.
func TestResolveMasterPlanIDCachePopulatedOnMiss(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[{"key":"MCPI-52","fields":{"summary":"X","status":{"name":"Done"}}}]}`))
	}))

	cache := NewMemoryCache()
	if _, err := ResolveMasterPlanID(context.Background(), c, "MCPI", "130", cache); err != nil {
		t.Fatalf("first call failed: %v", err)
	}
	if v, ok := cache.Get("130"); !ok || v != "MCPI-52" {
		t.Errorf("expected cache populated with MCPI-52, got (%q, %v)", v, ok)
	}
}

// TestResolveMasterPlanIDValidation verifies argument validation fails fast.
func TestResolveMasterPlanIDValidation(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	cases := []struct {
		name       string
		project    string
		epic       string
		client     *Client
		wantSubstr string
	}{
		{"empty epic", "MCPI", "  ", c, "epicID is required"},
		{"empty project", "", "130", c, "projectKey is required"},
		{"nil client", "MCPI", "130", nil, "jira client is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveMasterPlanID(context.Background(), tc.client, tc.project, tc.epic, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("expected error containing %q, got %v", tc.wantSubstr, err)
			}
		})
	}
}
