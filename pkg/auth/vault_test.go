package auth

import (
	"errors"
	"path/filepath"
	"testing"
)

// failingBackend always errors on Load — exercises the error branch of
// the lookup factories.
type failingBackend struct{}

func (*failingBackend) Save(*Credentials) error    { return errors.New("not implemented") }
func (*failingBackend) Load() (*Credentials, error) { return nil, errors.New("read failed") }

func makePopulatedBackend(t *testing.T) Backend {
	t.Helper()
	fb := NewFileBackend(filepath.Join(t.TempDir(), "creds.json"))
	creds := &Credentials{Version: 1}
	creds.Add(CredEntry{
		Provider: "jira",
		Type:     CredTypeAPIToken,
		Token:    "jira-tok",
		Email:    "user@acme.com",
		Domain:   "acme.atlassian.net",
		TenantID: "t-jira",
	})
	creds.Add(CredEntry{
		Provider:     "github",
		Type:         CredTypeOAuth2,
		Token:        "gh-access",
		RefreshToken: "gh-refresh",
	})
	if err := fb.Save(creds); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return fb
}

func TestNewLookup_HappyPath(t *testing.T) {
	backend := makePopulatedBackend(t)
	lookup := NewLookup(backend, "jira")

	cases := []struct {
		envName string
		want    string
	}{
		{"TOKEN", "jira-tok"},
		{"EMAIL", "user@acme.com"},
		{"DOMAIN", "acme.atlassian.net"},
		{"TENANT_ID", "t-jira"},
		{"TENANT", "t-jira"}, // alias
	}
	for _, tc := range cases {
		t.Run(tc.envName, func(t *testing.T) {
			got, ok := lookup(tc.envName)
			if !ok {
				t.Fatalf("lookup(%q) ok=false", tc.envName)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestNewLookup_UnknownProvider(t *testing.T) {
	backend := makePopulatedBackend(t)
	lookup := NewLookup(backend, "saml")
	if _, ok := lookup("TOKEN"); ok {
		t.Error("unknown provider should return ok=false")
	}
}

// TestNewLookup_APIKeyAliasesToken verifies API_KEY and KEY are recognized
// aliases of TOKEN (different LLM providers use different idioms — DeepSeek,
// OpenAI, Anthropic prefer "api_key"; Atlassian uses "token"). Both surface
// the same e.Token storage field. [Épica 143 vault hardening]
func TestNewLookup_APIKeyAliasesToken(t *testing.T) {
	backend := makePopulatedBackend(t)
	lookup := NewLookup(backend, "jira")
	for _, field := range []string{"API_KEY", "KEY", "TOKEN"} {
		v, ok := lookup(field)
		if !ok {
			t.Errorf("%s alias should resolve, got ok=false", field)
		}
		if v == "" {
			t.Errorf("%s alias should return non-empty token, got empty", field)
		}
	}
}

func TestNewLookup_UnknownField(t *testing.T) {
	backend := makePopulatedBackend(t)
	lookup := NewLookup(backend, "jira")
	// BOGUS_FIELD is genuinely unrecognized; resolveCredField must return
	// ok=false so the spawner can flag missing vault entries explicitly
	// (instead of injecting silent zeros).
	if _, ok := lookup("BOGUS_FIELD"); ok {
		t.Error("unknown field should return ok=false")
	}
}

func TestNewLookup_NilBackend(t *testing.T) {
	lookup := NewLookup(nil, "jira")
	if _, ok := lookup("TOKEN"); ok {
		t.Error("nil backend should return ok=false")
	}
}

func TestNewLookup_BackendError(t *testing.T) {
	lookup := NewLookup(&failingBackend{}, "jira")
	if _, ok := lookup("TOKEN"); ok {
		t.Error("backend Load error should produce ok=false")
	}
}

func TestNewLookup_EmptyFieldValue(t *testing.T) {
	backend := makePopulatedBackend(t)
	lookup := NewLookup(backend, "github") // github has no Email
	if _, ok := lookup("EMAIL"); ok {
		t.Error("empty Email should return ok=false")
	}
}

func TestNewMultiProviderLookup_PrefixSplit(t *testing.T) {
	backend := makePopulatedBackend(t)
	lookup := NewMultiProviderLookup(backend)

	cases := []struct {
		envName string
		want    string
	}{
		{"JIRA_TOKEN", "jira-tok"},
		{"JIRA_EMAIL", "user@acme.com"},
		{"GITHUB_TOKEN", "gh-access"},
		{"GITHUB_REFRESH_TOKEN", "gh-refresh"},
	}
	for _, tc := range cases {
		t.Run(tc.envName, func(t *testing.T) {
			got, ok := lookup(tc.envName)
			if !ok {
				t.Fatalf("lookup(%q) ok=false", tc.envName)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestNewMultiProviderLookup_NoUnderscore(t *testing.T) {
	backend := makePopulatedBackend(t)
	lookup := NewMultiProviderLookup(backend)
	if _, ok := lookup("JIRATOKEN"); ok {
		t.Error("no underscore should return ok=false")
	}
}

func TestNewMultiProviderLookup_LeadingUnderscore(t *testing.T) {
	backend := makePopulatedBackend(t)
	lookup := NewMultiProviderLookup(backend)
	if _, ok := lookup("_TOKEN"); ok {
		t.Error("leading underscore (no provider prefix) should return ok=false")
	}
}

func TestNewMultiProviderLookup_UnknownProvider(t *testing.T) {
	backend := makePopulatedBackend(t)
	lookup := NewMultiProviderLookup(backend)
	if _, ok := lookup("SAML_TOKEN"); ok {
		t.Error("unknown provider prefix should return ok=false")
	}
}

func TestNewMultiProviderLookup_NilBackend(t *testing.T) {
	lookup := NewMultiProviderLookup(nil)
	if _, ok := lookup("JIRA_TOKEN"); ok {
		t.Error("nil backend should return ok=false")
	}
}

func TestResolveCredField_NilEntry(t *testing.T) {
	if _, ok := resolveCredField("TOKEN", nil); ok {
		t.Error("nil entry should return ok=false")
	}
}

func TestNewLookupWithContext_CredAndSpaceFallback(t *testing.T) {
	backend := makePopulatedBackend(t)
	store := &ContextStore{Version: 1, Active: map[string]string{}}
	store.Set(Space{
		Provider:  "jira",
		SpaceID:   "ENG",
		SpaceName: "Engineering",
		BoardID:   "15",
		BoardName: "Sprint Board",
	})
	if err := store.Use("jira", "ENG"); err != nil {
		t.Fatal(err)
	}

	lookup := NewLookupWithContext(backend, store, "jira")

	// Credentials path
	if v, ok := lookup("TOKEN"); !ok || v != "jira-tok" {
		t.Errorf("TOKEN lookup: ok=%v v=%q want jira-tok", ok, v)
	}
	if v, ok := lookup("DOMAIN"); !ok || v != "acme.atlassian.net" {
		t.Errorf("DOMAIN lookup: ok=%v v=%q", ok, v)
	}

	// Context path
	cases := []struct{ env, want string }{
		{"ACTIVE_SPACE", "ENG"},
		{"SPACE", "ENG"},
		{"SPACE_NAME", "Engineering"},
		{"ACTIVE_BOARD", "15"},
		{"BOARD_NAME", "Sprint Board"},
	}
	for _, tc := range cases {
		got, ok := lookup(tc.env)
		if !ok || got != tc.want {
			t.Errorf("lookup(%q)=%q ok=%v want %q", tc.env, got, ok, tc.want)
		}
	}
}

func TestNewLookupWithContext_NilContextStore(t *testing.T) {
	backend := makePopulatedBackend(t)
	lookup := NewLookupWithContext(backend, nil, "jira")
	if v, ok := lookup("TOKEN"); !ok || v != "jira-tok" {
		t.Errorf("credentials should still resolve: %q ok=%v", v, ok)
	}
	if _, ok := lookup("ACTIVE_SPACE"); ok {
		t.Error("nil context store should not resolve space fields")
	}
}

func TestNewLookupWithContext_NoActiveSpace(t *testing.T) {
	backend := makePopulatedBackend(t)
	store := &ContextStore{Version: 1}
	store.Set(Space{Provider: "jira", SpaceID: "ENG"})
	// Note: did NOT call store.Use — no active space.

	lookup := NewLookupWithContext(backend, store, "jira")
	if _, ok := lookup("ACTIVE_SPACE"); ok {
		t.Error("no Use() called → ACTIVE_SPACE should not resolve")
	}
}

func TestNewMultiProviderLookupWithContext_BothSources(t *testing.T) {
	backend := makePopulatedBackend(t)
	store := &ContextStore{Version: 1, Active: map[string]string{}}
	store.Set(Space{Provider: "jira", SpaceID: "ENG", BoardID: "15"})
	_ = store.Use("jira", "ENG")

	lookup := NewMultiProviderLookupWithContext(backend, store)

	// Credential path (provider auto-detected from prefix)
	if v, ok := lookup("JIRA_TOKEN"); !ok || v != "jira-tok" {
		t.Errorf("JIRA_TOKEN: ok=%v v=%q", ok, v)
	}
	// Context path
	if v, ok := lookup("JIRA_ACTIVE_SPACE"); !ok || v != "ENG" {
		t.Errorf("JIRA_ACTIVE_SPACE: ok=%v v=%q", ok, v)
	}
	if v, ok := lookup("JIRA_ACTIVE_BOARD"); !ok || v != "15" {
		t.Errorf("JIRA_ACTIVE_BOARD: ok=%v v=%q", ok, v)
	}
	// Unknown provider misses cleanly
	if _, ok := lookup("UNKNOWN_TOKEN"); ok {
		t.Error("unknown provider should miss")
	}
}

func TestNewMultiProviderLookupWithContext_BothNilOK(t *testing.T) {
	lookup := NewMultiProviderLookupWithContext(nil, nil)
	if _, ok := lookup("ANY_TOKEN"); ok {
		t.Error("both nil should always miss")
	}
}

func TestResolveSpaceField_NilSpace(t *testing.T) {
	if _, ok := resolveSpaceField("ACTIVE_SPACE", nil); ok {
		t.Error("nil Space should return ok=false")
	}
}

func TestResolveCredField_CaseInsensitive(t *testing.T) {
	e := &CredEntry{Token: "tok"}
	for _, name := range []string{"token", "Token", "TOKEN"} {
		if v, ok := resolveCredField(name, e); !ok || v != "tok" {
			t.Errorf("%q: ok=%v v=%q", name, ok, v)
		}
	}
}
