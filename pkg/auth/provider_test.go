package auth

import (
	"context"
	"errors"
	"testing"
)

// Compile-time check: APITokenProvider satisfies Provider.
var _ Provider = (*APITokenProvider)(nil)

func TestProviderRegistry_RegisterAndGet(t *testing.T) {
	r := NewProviderRegistry()
	if err := r.Register(&APITokenProvider{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	p, ok := r.Get(CredTypeAPIToken)
	if !ok {
		t.Fatal("Get returned ok=false for registered type")
	}
	if p.Type() != CredTypeAPIToken {
		t.Errorf("Type=%q want %q", p.Type(), CredTypeAPIToken)
	}
}

func TestProviderRegistry_DuplicateRegisterFails(t *testing.T) {
	r := NewProviderRegistry()
	if err := r.Register(&APITokenProvider{}); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := r.Register(&APITokenProvider{}); err == nil {
		t.Error("duplicate Register should fail")
	}
}

func TestProviderRegistry_RegisterNilFails(t *testing.T) {
	r := NewProviderRegistry()
	if err := r.Register(nil); err == nil {
		t.Error("Register(nil) should fail")
	}
}

// emptyTypeProvider returns "" from Type() — registry must reject it.
type emptyTypeProvider struct{}

func (*emptyTypeProvider) Type() string                                  { return "" }
func (*emptyTypeProvider) Refresh(context.Context, *CredEntry) error     { return nil }
func (*emptyTypeProvider) Validate(*CredEntry) error                     { return nil }

func TestProviderRegistry_RegisterEmptyTypeFails(t *testing.T) {
	r := NewProviderRegistry()
	if err := r.Register(&emptyTypeProvider{}); err == nil {
		t.Error("Register with empty Type() should fail")
	}
}

func TestDefaultProviderRegistry_HasAPIToken(t *testing.T) {
	r := DefaultProviderRegistry()
	if _, ok := r.Get(CredTypeAPIToken); !ok {
		t.Error("default registry missing api_token provider")
	}
}

func TestProviderRegistry_RefreshDispatches(t *testing.T) {
	r := DefaultProviderRegistry()
	entry := &CredEntry{Provider: "github", Type: CredTypeAPIToken, Token: "x"}
	err := r.Refresh(context.Background(), entry)
	if !errors.Is(err, ErrManualRefreshRequired) {
		t.Errorf("api_token refresh: err=%v want ErrManualRefreshRequired", err)
	}
}

func TestProviderRegistry_RefreshUnknownType(t *testing.T) {
	r := NewProviderRegistry()
	entry := &CredEntry{Type: "saml"}
	err := r.Refresh(context.Background(), entry)
	if !errors.Is(err, ErrNoProviderRegistered) {
		t.Errorf("err=%v want ErrNoProviderRegistered", err)
	}
}

func TestProviderRegistry_RefreshNilEntry(t *testing.T) {
	r := DefaultProviderRegistry()
	if err := r.Refresh(context.Background(), nil); err == nil {
		t.Error("Refresh(nil) should fail")
	}
}

func TestProviderRegistry_ValidateDispatches(t *testing.T) {
	r := DefaultProviderRegistry()
	entry := &CredEntry{Provider: "github", Type: CredTypeAPIToken, Token: "tok"}
	if err := r.Validate(entry); err != nil {
		t.Errorf("Validate happy path: %v", err)
	}
}

func TestProviderRegistry_ValidateUnknownType(t *testing.T) {
	r := NewProviderRegistry()
	entry := &CredEntry{Type: "saml"}
	if err := r.Validate(entry); !errors.Is(err, ErrNoProviderRegistered) {
		t.Errorf("err=%v want ErrNoProviderRegistered", err)
	}
}

func TestAPITokenProvider_Type(t *testing.T) {
	p := &APITokenProvider{}
	if p.Type() != CredTypeAPIToken {
		t.Errorf("Type=%q want %q", p.Type(), CredTypeAPIToken)
	}
}

func TestAPITokenProvider_Refresh(t *testing.T) {
	p := &APITokenProvider{}
	err := p.Refresh(context.Background(), &CredEntry{Provider: "x", Token: "y"})
	if !errors.Is(err, ErrManualRefreshRequired) {
		t.Errorf("err=%v want ErrManualRefreshRequired", err)
	}
}

func TestAPITokenProvider_Validate(t *testing.T) {
	p := &APITokenProvider{}

	cases := []struct {
		name    string
		entry   *CredEntry
		wantErr bool
	}{
		{"nil entry", nil, true},
		{"missing provider", &CredEntry{Token: "tok"}, true},
		{"missing token", &CredEntry{Provider: "x"}, true},
		{"whitespace token", &CredEntry{Provider: "x", Token: "   "}, true},
		{"happy", &CredEntry{Provider: "x", Token: "tok"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := p.Validate(tc.entry)
			if (err != nil) != tc.wantErr {
				t.Errorf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
