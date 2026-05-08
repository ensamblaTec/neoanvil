package auth

// provider.go — Provider interface for credential lifecycle (refresh +
// validation). PILAR XXIII / Épica 124.3.
//
// Each credential Type (api_token, oauth2, vendor-specific) registers a
// Provider that knows how to:
//   - Validate that an entry has the fields required by that Type.
//   - Refresh the entry — for OAuth, exchange RefreshToken for a fresh
//     access token; for API tokens, signal that manual rotation is required.
//
// The ProviderRegistry maps Type → Provider and dispatches at call time.
// Adds new types (SAML, webhook secret, vendor-issued JWT) without changing
// any caller.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ErrManualRefreshRequired signals that the credential cannot be rotated
// programmatically — the operator must re-issue and re-store, typically
// via `neo login --provider <name>`.
var ErrManualRefreshRequired = errors.New("manual refresh required for this credential type")

// ErrNoProviderRegistered is returned when the entry's Type has no
// registered Provider in the registry.
var ErrNoProviderRegistered = errors.New("no provider registered for credential type")

// Provider implements credential lifecycle operations specific to a Type.
// All methods must be safe for concurrent use — the registry may dispatch
// from multiple goroutines.
type Provider interface {
	// Type returns the credential type this Provider handles. One of
	// CredTypeAPIToken, CredTypeOAuth2, or a caller-defined extension.
	Type() string

	// Refresh rotates the credential, mutating entry in place. For
	// api_token, returns ErrManualRefreshRequired. For oauth2, exchanges
	// the refresh token for a fresh access token and updates Token,
	// RefreshToken (if rotated by IdP), and ExpiresAt.
	Refresh(ctx context.Context, entry *CredEntry) error

	// Validate checks that the entry has the fields required by this Type.
	// Called before Save and at boot to surface schema drift early.
	Validate(entry *CredEntry) error
}

// ProviderRegistry maps Type → Provider. Goroutine-safe.
type ProviderRegistry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewProviderRegistry returns an empty registry. Use Register to populate.
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{providers: make(map[string]Provider)}
}

// DefaultProviderRegistry returns a registry preloaded with built-in
// providers. Currently: APITokenProvider for "api_token". OAuth2 lands when
// the first OAuth-based plugin (e.g. Jira 3LO) is wired.
func DefaultProviderRegistry() *ProviderRegistry {
	r := NewProviderRegistry()
	_ = r.Register(&APITokenProvider{})
	return r
}

// Register adds a Provider. Returns an error when another Provider is
// already registered for the same Type — duplicate registrations are
// almost always programming errors.
func (r *ProviderRegistry) Register(p Provider) error {
	if p == nil {
		return errors.New("nil Provider")
	}
	typ := strings.TrimSpace(p.Type())
	if typ == "" {
		return errors.New("Provider returned empty Type()")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[typ]; exists {
		return fmt.Errorf("provider for type %q already registered", typ)
	}
	r.providers[typ] = p
	return nil
}

// Get returns the Provider for typ, or (nil, false).
func (r *ProviderRegistry) Get(typ string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[typ]
	return p, ok
}

// Refresh dispatches to the Provider for entry.Type. Returns
// ErrNoProviderRegistered when the type has no Provider.
func (r *ProviderRegistry) Refresh(ctx context.Context, entry *CredEntry) error {
	if entry == nil {
		return errors.New("nil entry")
	}
	p, ok := r.Get(entry.Type)
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoProviderRegistered, entry.Type)
	}
	return p.Refresh(ctx, entry)
}

// Validate dispatches to the Provider for entry.Type. Returns
// ErrNoProviderRegistered when the type has no Provider.
func (r *ProviderRegistry) Validate(entry *CredEntry) error {
	if entry == nil {
		return errors.New("nil entry")
	}
	p, ok := r.Get(entry.Type)
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoProviderRegistered, entry.Type)
	}
	return p.Validate(entry)
}

// APITokenProvider handles simple bearer / Basic-auth tokens (Atlassian
// API token, GitHub PAT, etc.). Refresh is always manual — the operator
// re-issues at the provider's web UI and updates via `neo login`.
type APITokenProvider struct{}

// Type implements Provider.
func (*APITokenProvider) Type() string { return CredTypeAPIToken }

// Refresh always returns ErrManualRefreshRequired. API tokens cannot rotate
// programmatically; the operator must re-issue the token and re-store it.
func (*APITokenProvider) Refresh(_ context.Context, _ *CredEntry) error {
	return ErrManualRefreshRequired
}

// Validate enforces the minimal contract: Provider name and Token must
// be set. ExpiresAt is optional (legacy entries don't have one).
func (*APITokenProvider) Validate(entry *CredEntry) error {
	if entry == nil {
		return errors.New("nil entry")
	}
	if strings.TrimSpace(entry.Provider) == "" {
		return errors.New("api_token: Provider is required")
	}
	if strings.TrimSpace(entry.Token) == "" {
		return errors.New("api_token: Token is required")
	}
	return nil
}
