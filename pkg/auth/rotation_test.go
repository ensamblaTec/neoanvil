package auth

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOAuth2Provider simulates an OAuth 2.0 refresh exchange without
// hitting a real authorization server. Production OAuth implementations
// live in plugin-specific packages (e.g. cmd/plugin-jira/) — this fake
// exists only to drive end-to-end rotation tests in CI.
type fakeOAuth2Provider struct {
	callCount  atomic.Int64
	expiryHint time.Duration // 0 = default 1h
}

func (*fakeOAuth2Provider) Type() string { return CredTypeOAuth2 }

func (p *fakeOAuth2Provider) Refresh(_ context.Context, e *CredEntry) error {
	if e == nil {
		return errors.New("nil entry")
	}
	if strings.TrimSpace(e.RefreshToken) == "" {
		return errors.New("oauth2: RefreshToken is required for refresh")
	}
	n := p.callCount.Add(1)
	e.Token = fmt.Sprintf("access-rotated-%d", n)
	e.RefreshToken = fmt.Sprintf("refresh-rotated-%d", n)
	expiry := p.expiryHint
	if expiry == 0 {
		expiry = time.Hour
	}
	e.ExpiresAt = time.Now().Add(expiry).UTC().Format(time.RFC3339)
	return nil
}

func (*fakeOAuth2Provider) Validate(e *CredEntry) error {
	if e == nil {
		return errors.New("nil entry")
	}
	if strings.TrimSpace(e.Token) == "" {
		return errors.New("oauth2: Token required")
	}
	if strings.TrimSpace(e.RefreshToken) == "" {
		return errors.New("oauth2: RefreshToken required")
	}
	return nil
}

// TestRotation_EndToEnd_FileBackend covers the full lifecycle:
//
//	save expired entry → load → refresh via registry dispatch → save back
//	→ reload → assert Token/RefreshToken/ExpiresAt rotated
//
// Uses FileBackend so no Keychain access during go test.
func TestRotation_EndToEnd_FileBackend(t *testing.T) {
	backend := NewFileBackend(filepath.Join(t.TempDir(), "creds.json"))
	fakeP := &fakeOAuth2Provider{}
	registry := NewProviderRegistry()
	if err := registry.Register(fakeP); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// 1. Save expired entry.
	creds := &Credentials{Version: 1}
	creds.Add(CredEntry{
		Provider:     "jira",
		Type:         CredTypeOAuth2,
		Token:        "access-original",
		RefreshToken: "refresh-original",
		ExpiresAt:    time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	})
	if err := backend.Save(creds); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// 2. Reload + Refresh via registry.
	loaded, err := backend.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entry := loaded.GetByProvider("jira")
	if entry == nil {
		t.Fatal("entry not found after reload")
	}
	if !entry.IsExpired(time.Now()) {
		t.Fatal("entry should be expired before refresh")
	}
	if err := registry.Refresh(context.Background(), entry); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// 3. Save rotated entry back (Add upserts by provider key).
	loaded.Add(*entry)
	if err := backend.Save(loaded); err != nil {
		t.Fatalf("Save rotated: %v", err)
	}

	// 4. Reload + assert rotation persisted.
	final, err := backend.Load()
	if err != nil {
		t.Fatalf("final Load: %v", err)
	}
	e := final.GetByProvider("jira")
	if e.Token != "access-rotated-1" {
		t.Errorf("Token=%q want access-rotated-1", e.Token)
	}
	if e.RefreshToken != "refresh-rotated-1" {
		t.Errorf("RefreshToken=%q want refresh-rotated-1", e.RefreshToken)
	}
	if e.IsExpired(time.Now()) {
		t.Error("rotated entry should not be expired")
	}
	if e.ExpiresIn(time.Now()) <= 0 {
		t.Error("ExpiresIn should be positive after rotation")
	}
	if got := fakeP.callCount.Load(); got != 1 {
		t.Errorf("provider call count=%d want 1", got)
	}
}

// TestRotation_EndToEnd_KeyringBackend mirrors the FileBackend test but
// uses the deterministic file-mode of 99designs/keyring — exercises the
// keyring code path without touching the developer's real Keychain.
func TestRotation_EndToEnd_KeyringBackend(t *testing.T) {
	kb := newTestKeyring(t)
	fakeP := &fakeOAuth2Provider{}
	registry := NewProviderRegistry()
	if err := registry.Register(fakeP); err != nil {
		t.Fatalf("Register: %v", err)
	}

	creds := &Credentials{Version: 1}
	creds.Add(CredEntry{
		Provider:     "jira",
		Type:         CredTypeOAuth2,
		Token:        "access-original",
		RefreshToken: "refresh-original",
	})
	if err := kb.Save(creds); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, _ := kb.Load()
	entry := loaded.GetByProvider("jira")
	if err := registry.Refresh(context.Background(), entry); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	loaded.Add(*entry)
	if err := kb.Save(loaded); err != nil {
		t.Fatalf("Save rotated: %v", err)
	}

	final, _ := kb.Load()
	e := final.GetByProvider("jira")
	if e.Token != "access-rotated-1" {
		t.Errorf("Token=%q want access-rotated-1", e.Token)
	}
	if e.RefreshToken != "refresh-rotated-1" {
		t.Errorf("RefreshToken=%q want refresh-rotated-1", e.RefreshToken)
	}
}

// TestRotation_RefreshTokenRequired ensures the fake returns a clear error
// when RefreshToken is missing — covers the precondition contract.
func TestRotation_RefreshTokenRequired(t *testing.T) {
	fakeP := &fakeOAuth2Provider{}
	entry := &CredEntry{Provider: "jira", Type: CredTypeOAuth2, Token: "access"}
	err := fakeP.Refresh(context.Background(), entry)
	if err == nil {
		t.Fatal("expected error for missing RefreshToken")
	}
	if !strings.Contains(err.Error(), "RefreshToken is required") {
		t.Errorf("err=%q want contains 'RefreshToken is required'", err)
	}
}

// TestRotation_MultipleRefreshes verifies the call counter increments and
// the rotated values reflect each call (no caching).
func TestRotation_MultipleRefreshes(t *testing.T) {
	fakeP := &fakeOAuth2Provider{expiryHint: 30 * time.Minute}
	entry := &CredEntry{
		Provider:     "jira",
		Type:         CredTypeOAuth2,
		Token:        "v0",
		RefreshToken: "rt",
	}
	for i := 1; i <= 3; i++ {
		if err := fakeP.Refresh(context.Background(), entry); err != nil {
			t.Fatalf("Refresh #%d: %v", i, err)
		}
		if entry.Token != fmt.Sprintf("access-rotated-%d", i) {
			t.Errorf("after refresh #%d Token=%q", i, entry.Token)
		}
	}
}

// TestRotation_AuditLogged demonstrates the expected wiring pattern:
// callers append a credential_rotated event to the audit log on success.
// Verifies the chain stays valid after the event.
func TestRotation_AuditLogged(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.log")
	log, err := OpenAuditLog(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	fakeP := &fakeOAuth2Provider{}
	entry := &CredEntry{
		Provider:     "jira",
		Type:         CredTypeOAuth2,
		Token:        "old",
		RefreshToken: "rt",
	}
	if err := fakeP.Refresh(context.Background(), entry); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, err := log.Append(Event{
		Kind:     "credential_rotated",
		Actor:    "test",
		Provider: entry.Provider,
		Details:  map[string]any{"type": entry.Type, "previous_expires_at": ""},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := log.Verify(); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// TestRotation_ValidationViaRegistry verifies the registry dispatch path
// for Validate matches the fake's contract.
func TestRotation_ValidationViaRegistry(t *testing.T) {
	fakeP := &fakeOAuth2Provider{}
	registry := NewProviderRegistry()
	_ = registry.Register(fakeP)

	good := &CredEntry{Provider: "jira", Type: CredTypeOAuth2, Token: "t", RefreshToken: "r"}
	if err := registry.Validate(good); err != nil {
		t.Errorf("Validate good: %v", err)
	}

	bad := &CredEntry{Provider: "jira", Type: CredTypeOAuth2, Token: "t"}
	if err := registry.Validate(bad); err == nil {
		t.Error("Validate without RefreshToken should fail")
	}
}
