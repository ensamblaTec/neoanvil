package main

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/time/rate"
	"github.com/ensamblatec/neoanvil/pkg/jira"
)

// clientPool maintains one jira.Client per api_key name with a shared rate
// limiter. Thread-safe — concurrent MCP handlers share the same pool entry.
type clientPool struct {
	mu      sync.RWMutex
	entries map[string]*poolEntry
}

type poolEntry struct {
	client  *jira.Client
	limiter *rate.Limiter
	keyName string
	token   string // track token for rotation detection
}

func newClientPool() *clientPool {
	return &clientPool{entries: make(map[string]*poolEntry)}
}

// getOrCreate returns the cached client for the given api_key name, creating
// it on first access. The rate limiter is shared across all requests to the
// same tenant — prevents burst violations when multiple workspaces map to the
// same api_key.
func (p *clientPool) getOrCreate(keyName string, key *APIKey, token string) (*poolEntry, error) {
	p.mu.RLock()
	if e, ok := p.entries[keyName]; ok {
		// SEV(10) fix: invalidate if token changed (rotation detection).
		if e.token == token {
			p.mu.RUnlock()
			return e, nil
		}
		p.mu.RUnlock()
		p.invalidate(keyName)
	} else {
		p.mu.RUnlock()
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check after acquiring write lock.
	if e, ok := p.entries[keyName]; ok && e.token == token {
		return e, nil
	}

	c, err := jira.NewClient(jira.Config{
		Domain: key.Domain,
		Email:  key.Email,
		Token:  token,
	})
	if err != nil {
		return nil, fmt.Errorf("create client for %q: %w", keyName, err)
	}

	rpm := key.RateLimit.MaxPerMinute
	if rpm <= 0 {
		rpm = 300
	}
	burst := key.RateLimit.Concurrency
	if burst <= 0 {
		burst = 5
	}
	lim := rate.NewLimiter(rate.Limit(float64(rpm)/60.0), burst)

	e := &poolEntry{
		client:  c,
		limiter: lim,
		keyName: keyName,
		token:   token,
	}
	p.entries[keyName] = e
	return e, nil
}

// invalidate removes a tenant entry (e.g. on config reload with changed credentials).
func (p *clientPool) invalidate(keyName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, keyName)
}

// invalidateAll clears the entire pool (e.g. on full config reload).
func (p *clientPool) invalidateAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = make(map[string]*poolEntry)
}

// ── Auth provider ───────────────────────────────────────────────────────────

// AuthProvider resolves credentials for a Jira API call.
type AuthProvider interface {
	Authenticate(key *APIKey) (token string, err error)
}

// PATAuth resolves Personal Access Tokens from inline or token_ref.
type PATAuth struct{}

func (PATAuth) Authenticate(key *APIKey) (string, error) {
	return resolveToken(key)
}

// OAuth2Auth is a stub — not implemented. Logs fatal and returns error.
type OAuth2Auth struct{}

func (OAuth2Auth) Authenticate(_ *APIKey) (string, error) {
	return "", errors.New("OAuth2 authentication is not implemented — use PAT (auth.type: PAT)")
}

// authProviderFor returns the appropriate provider based on auth.type config.
func authProviderFor(key *APIKey) AuthProvider {
	switch key.Auth.Type {
	case "OAUTH2_3LO":
		return OAuth2Auth{}
	default:
		return PATAuth{}
	}
}

// ── Resolver chain ──────────────────────────────────────────────────────────

// resolveCall is the full resolver chain: workspace_id → project → api_key → client.
// Returns the jira.Client, project config, and project name for use by action handlers.
func (s *state) resolveCall(cc callCtx) (*jira.Client, *ProjectCfg, string, error) {
	cfg := s.pluginCfg
	if cfg == nil {
		return s.client, nil, s.activeSpace, nil
	}

	proj, projName, err := cfg.resolveProject(cc.WorkspaceID)
	if err != nil {
		return nil, nil, "", fmt.Errorf("resolve project: %w", err)
	}

	key, ok := cfg.APIKeys[proj.APIKeyRef]
	if !ok {
		return nil, nil, "", fmt.Errorf("api_key %q not found for project %q", proj.APIKeyRef, projName)
	}

	auth := authProviderFor(key)
	token, err := auth.Authenticate(key)
	if err != nil {
		return nil, nil, "", fmt.Errorf("authenticate %q: %w", proj.APIKeyRef, err)
	}

	if s.pool == nil {
		return nil, nil, "", errors.New("client pool not initialized")
	}

	entry, err := s.pool.getOrCreate(proj.APIKeyRef, key, token)
	if err != nil {
		return nil, nil, "", err
	}

	// Apply rate limiting — block until allowed.
	if err := entry.limiter.Wait(s.ctx); err != nil {
		return nil, nil, "", fmt.Errorf("rate limit wait: %w", err)
	}

	return entry.client, proj, projName, nil
}
