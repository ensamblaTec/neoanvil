package auth

// vault.go — Bridge from auth.Backend (FileBackend / KeyringBackend) to the
// pkg/nexus.VaultLookup signature consumed by PluginPool.buildEnv.
// PILAR XXIII / Épica 124.5.
//
// Two factories cover the typical spawn patterns:
//
//   NewLookup(backend, "jira")
//     Single-provider — caller already knows which credential entry to
//     consult. PluginPool can pass spec.Name when each plugin uses one
//     provider.
//
//   NewMultiProviderLookup(backend)
//     Auto-detects the provider from the env var's first segment.
//     Convention: <PROVIDER>_<FIELD> — e.g. JIRA_TOKEN, GITHUB_EMAIL.
//     Used when the plugin pool serves multiple plugins with one shared
//     vault.
//
// Field resolution is suffix-based — see resolveCredField. Recognized
// suffixes: TOKEN, REFRESH_TOKEN, EMAIL, DOMAIN, TENANT_ID. Unknown
// suffixes return (..., false) — plugin authors must use the convention
// or supply a custom VaultLookup.

import (
	"strings"
)

// NewLookup returns a vault resolver bound to a single provider's credentials.
// Suitable when the caller knows which provider's entry to consult — e.g.
// each plugin spec pins one provider, and the spawner builds a fresh
// lookup per spec.
func NewLookup(backend Backend, providerName string) func(envName string) (string, bool) {
	return func(envName string) (string, bool) {
		if backend == nil {
			return "", false
		}
		creds, err := backend.Load()
		if err != nil {
			return "", false
		}
		entry := creds.GetByProvider(providerName)
		if entry == nil {
			return "", false
		}
		return resolveCredField(envName, entry)
	}
}

// NewMultiProviderLookup returns a resolver that picks the provider from
// the env var's prefix. Convention:
//
//	JIRA_TOKEN          → provider=jira,    field=TOKEN
//	GITHUB_REFRESH_TOKEN → provider=github,  field=REFRESH_TOKEN
//	DEEPSEEK_EMAIL      → provider=deepseek, field=EMAIL
//
// Returns (value, false) when no provider entry matches or the field is
// unknown.
func NewMultiProviderLookup(backend Backend) func(envName string) (string, bool) {
	return func(envName string) (string, bool) {
		if backend == nil {
			return "", false
		}
		idx := strings.Index(envName, "_")
		if idx <= 0 {
			return "", false
		}
		provider := strings.ToLower(envName[:idx])
		field := envName[idx+1:]
		creds, err := backend.Load()
		if err != nil {
			return "", false
		}
		entry := creds.GetByProvider(provider)
		if entry == nil {
			return "", false
		}
		return resolveCredField(field, entry)
	}
}

// resolveCredField maps a field-suffix to the corresponding CredEntry
// field. Recognized suffixes (case-insensitive): TOKEN, API_KEY, KEY,
// REFRESH_TOKEN, EMAIL, DOMAIN, TENANT_ID, TENANT. Unknown suffixes return
// (..., false) so the spawner reports "missing vault entries" rather than
// injecting silently-wrong values.
//
// API_KEY and KEY are aliases of TOKEN — different LLM providers use
// different idioms (Atlassian "token", DeepSeek "api_key", OpenAI "api_key",
// Anthropic "api_key"). All map to the same e.Token storage field.
func resolveCredField(envField string, e *CredEntry) (string, bool) {
	if e == nil {
		return "", false
	}
	switch strings.ToUpper(envField) {
	case "TOKEN", "API_KEY", "KEY":
		return nonEmpty(e.Token)
	case "REFRESH_TOKEN":
		return nonEmpty(e.RefreshToken)
	case "EMAIL":
		return nonEmpty(e.Email)
	case "DOMAIN":
		return nonEmpty(e.Domain)
	case "TENANT_ID", "TENANT":
		return nonEmpty(e.TenantID)
	}
	return "", false
}

func nonEmpty(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	return s, true
}

// NewLookupWithContext returns a vault resolver that combines credentials
// (from backend) with the active space (from contextStore) for one
// provider. Falls back to credentials lookup; if that misses, tries the
// space-context fields. Either source may be nil — both being nil yields
// an always-miss resolver.
//
// Recognized space-context suffixes (case-insensitive):
//
//	ACTIVE_SPACE      / SPACE           → Space.SpaceID
//	ACTIVE_SPACE_NAME / SPACE_NAME      → Space.SpaceName
//	ACTIVE_BOARD      / BOARD           → Space.BoardID
//	ACTIVE_BOARD_NAME / BOARD_NAME      → Space.BoardName
//
// Convention: env vars are <PROVIDER>_<FIELD>; the prefix is consumed by
// the surrounding caller (Single-provider lookup is bound to a provider
// already, so envName is the field portion).
func NewLookupWithContext(backend Backend, contextStore *ContextStore, providerName string) func(envName string) (string, bool) {
	credLookup := NewLookup(backend, providerName)
	return func(envName string) (string, bool) {
		if val, ok := credLookup(envName); ok {
			return val, true
		}
		if contextStore == nil {
			return "", false
		}
		active := contextStore.ActiveSpace(providerName)
		if active == nil {
			return "", false
		}
		return resolveSpaceField(envName, active)
	}
}

// NewMultiProviderLookupWithContext combines NewMultiProviderLookup
// (credentials, prefix-detected provider) with active space resolution
// from contextStore. Used by Nexus when serving multiple plugins from one
// shared pool — each plugin's env_from_vault names are matched by the
// PROVIDER_FIELD convention; the resolver looks up credentials first, then
// falls back to active space fields for that provider.
//
// Either argument may be nil. Both nil yields an always-miss resolver.
func NewMultiProviderLookupWithContext(backend Backend, contextStore *ContextStore) func(envName string) (string, bool) {
	credLookup := NewMultiProviderLookup(backend)
	return func(envName string) (string, bool) {
		if val, ok := credLookup(envName); ok {
			return val, true
		}
		if contextStore == nil {
			return "", false
		}
		idx := strings.Index(envName, "_")
		if idx <= 0 {
			return "", false
		}
		provider := strings.ToLower(envName[:idx])
		field := envName[idx+1:]
		active := contextStore.ActiveSpace(provider)
		if active == nil {
			return "", false
		}
		return resolveSpaceField(field, active)
	}
}

// resolveSpaceField maps a field-suffix to the corresponding Space field.
// Recognized suffixes documented on NewLookupWithContext.
func resolveSpaceField(envField string, sp *Space) (string, bool) {
	if sp == nil {
		return "", false
	}
	switch strings.ToUpper(envField) {
	case "ACTIVE_SPACE", "SPACE":
		return nonEmpty(sp.SpaceID)
	case "ACTIVE_SPACE_NAME", "SPACE_NAME":
		return nonEmpty(sp.SpaceName)
	case "ACTIVE_BOARD", "BOARD":
		return nonEmpty(sp.BoardID)
	case "ACTIVE_BOARD_NAME", "BOARD_NAME":
		return nonEmpty(sp.BoardName)
	}
	return "", false
}
