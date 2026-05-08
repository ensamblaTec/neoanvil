package main

import (
	"context"
	"sync"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/jira"
)

func TestClientPool_GetOrCreate(t *testing.T) {
	pool := newClientPool()
	tok := "test-token"
	key := &APIKey{
		Domain:    "test.atlassian.net",
		Email:     "test@test.com",
		Auth:      AuthConfig{Type: "PAT", Token: &tok},
		RateLimit: RateLimit{MaxPerMinute: 60, Concurrency: 2},
	}

	e1, err := pool.getOrCreate("k1", key, tok)
	if err != nil {
		t.Fatal(err)
	}
	if e1.client == nil {
		t.Fatal("client is nil")
	}

	// Second call returns same entry (cached).
	e2, err := pool.getOrCreate("k1", key, tok)
	if err != nil {
		t.Fatal(err)
	}
	if e1 != e2 {
		t.Error("expected same poolEntry pointer")
	}
}

func TestClientPool_SeparateTenants(t *testing.T) {
	pool := newClientPool()
	tok := "tok"
	k1 := &APIKey{Domain: "a.atlassian.net", Email: "a@a.com", Auth: AuthConfig{Token: &tok}, RateLimit: RateLimit{MaxPerMinute: 60, Concurrency: 2}}
	k2 := &APIKey{Domain: "b.atlassian.net", Email: "b@b.com", Auth: AuthConfig{Token: &tok}, RateLimit: RateLimit{MaxPerMinute: 60, Concurrency: 2}}

	e1, _ := pool.getOrCreate("tenant-a", k1, tok)
	e2, _ := pool.getOrCreate("tenant-b", k2, tok)

	if e1 == e2 {
		t.Error("different tenants should have different pool entries")
	}
	if e1.client == e2.client {
		t.Error("different tenants should have different clients")
	}
}

func TestClientPool_ConcurrentAccess(t *testing.T) {
	pool := newClientPool()
	tok := "tok"
	key := &APIKey{Domain: "test.atlassian.net", Email: "t@t.com", Auth: AuthConfig{Token: &tok}, RateLimit: RateLimit{MaxPerMinute: 300, Concurrency: 5}}

	var wg sync.WaitGroup
	entries := make([]*poolEntry, 20)
	for i := range 20 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			e, err := pool.getOrCreate("shared", key, tok)
			if err != nil {
				t.Errorf("goroutine %d: %v", idx, err)
				return
			}
			entries[idx] = e
		}(i)
	}
	wg.Wait()

	// All should point to same entry.
	for i := 1; i < 20; i++ {
		if entries[i] != entries[0] {
			t.Errorf("entry[%d] differs from entry[0]", i)
		}
	}
}

func TestClientPool_Invalidate(t *testing.T) {
	pool := newClientPool()
	tok := "tok"
	key := &APIKey{Domain: "test.atlassian.net", Email: "t@t.com", Auth: AuthConfig{Token: &tok}, RateLimit: RateLimit{MaxPerMinute: 60, Concurrency: 2}}

	e1, _ := pool.getOrCreate("k1", key, tok)
	pool.invalidate("k1")
	e2, _ := pool.getOrCreate("k1", key, tok)

	if e1 == e2 {
		t.Error("expected new entry after invalidation")
	}
}

func TestClientPool_SharedRateLimiter(t *testing.T) {
	pool := newClientPool()
	tok := "tok"
	key := &APIKey{Domain: "test.atlassian.net", Email: "t@t.com", Auth: AuthConfig{Token: &tok}, RateLimit: RateLimit{MaxPerMinute: 60, Concurrency: 2}}

	e1, _ := pool.getOrCreate("shared", key, tok)
	e2, _ := pool.getOrCreate("shared", key, tok)

	if e1.limiter != e2.limiter {
		t.Error("same tenant should share rate limiter")
	}
}

func TestAuthProviderFor_PAT(t *testing.T) {
	tok := "secret"
	key := &APIKey{Auth: AuthConfig{Type: "PAT", Token: &tok}}
	p := authProviderFor(key)
	if _, ok := p.(PATAuth); !ok {
		t.Errorf("expected PATAuth, got %T", p)
	}
	got, err := p.Authenticate(key)
	if err != nil || got != "secret" {
		t.Errorf("got=%q err=%v", got, err)
	}
}

func TestAuthProviderFor_OAuth2Stub(t *testing.T) {
	key := &APIKey{Auth: AuthConfig{Type: "OAUTH2_3LO"}}
	p := authProviderFor(key)
	if _, ok := p.(OAuth2Auth); !ok {
		t.Errorf("expected OAuth2Auth, got %T", p)
	}
	_, err := p.Authenticate(key)
	if err == nil {
		t.Error("OAuth2 stub should return error")
	}
}

func TestResolveCall_LegacyMode(t *testing.T) {
	c := mustClient(t, "tok")
	st := &state{client: c, activeSpace: "LEGACY", pluginCfg: nil}
	client, proj, name, err := st.resolveCall(callCtx{WorkspaceID: "any"})
	if err != nil {
		t.Fatal(err)
	}
	if client != c {
		t.Error("expected legacy client")
	}
	if proj != nil {
		t.Error("expected nil project in legacy mode")
	}
	if name != "LEGACY" {
		t.Errorf("name = %q", name)
	}
}

func TestResolveCall_MultiTenant(t *testing.T) {
	cfg := minimalConfig()
	st := &state{
		pluginCfg: cfg,
		pool:      newClientPool(),
		ctx:       context.Background(),
	}
	client, proj, name, err := st.resolveCall(callCtx{WorkspaceID: "any"})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil {
		t.Error("expected non-nil client")
	}
	if proj == nil || proj.ProjectKey != "TEST" {
		t.Error("expected TEST project")
	}
	if name != "test" {
		t.Errorf("name = %q", name)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func mustClient(t *testing.T, token string) *jira.Client {
	t.Helper()
	c, err := jira.NewClient(jira.Config{Domain: "test.atlassian.net", Email: "t@t.com", Token: token})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
