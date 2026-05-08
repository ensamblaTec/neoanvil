package testmock

import (
	"strings"
	"testing"
)

// Harness composes one instance of each mock in this package and exposes
// helpers that integration tests use to spawn production binaries with
// the mock URLs / fake credentials wired in. All four mocks register
// their own t.Cleanup; the Harness adds none of its own.
//
// Layout:
//
//	h := testmock.NewHarness(t)
//	h.Jira.SetIssue("MCPI-1", testmock.JiraIssue{...})
//	h.DeepSeek.SetReply(testmock.DeepSeekReply{...})
//	env := h.Env()  // pass to exec.Cmd to override base URLs + credentials
//
// The Env map is intentionally aligned with the env_from_vault entries
// declared in plugins.yaml.example, plus the BASE_URL overrides that
// production clients will honor once Area 3.2.A lands. Until that
// production fix exists, only the credentials are picked up by the
// plugins; the BASE_URL keys are inert but pre-wired for the cutover.
type Harness struct {
	Jira     *JiraMock
	DeepSeek *DeepSeekMock
	Ollama   *OllamaMock
	GitHub   *GitHubMock
}

// NewHarness boots all four mocks. Each mock registers its own t.Cleanup;
// the call returns once they are all listening. Safe to call once per
// test (servers are not reusable across runs).
func NewHarness(tb testing.TB) *Harness {
	tb.Helper()
	return &Harness{
		Jira:     NewJira(tb),
		DeepSeek: NewDeepSeek(tb),
		Ollama:   NewOllama(tb),
		GitHub:   NewGitHub(tb),
	}
}

// Env returns the env-var bag to inject into plugin subprocesses. Keys
// match what plugins.yaml.example declares under env_from_vault, with
// BASE_URL companions for the integration tests that point at the mocks.
//
// Caveats:
//   - JIRA_DOMAIN omits the scheme (host:port only) because pkg/jira's
//     Client builds "https://%s/...". Tests must use Area 3.2.A's
//     JIRA_BASE_URL override (or pkg/jira would still fail TLS handshake
//     against an http mock).
//   - OLLAMA_URL is a full URL (used by neo.yaml ai.base_url).
func (h *Harness) Env() map[string]string {
	return map[string]string{
		// DeepSeek
		"DEEPSEEK_API_KEY": "fake-deepseek-token",
		"DEEPSEEK_BASE_URL": h.DeepSeek.URL(),

		// Jira
		"JIRA_TOKEN":              "fake-token",
		"JIRA_EMAIL":              "test@example.com",
		"JIRA_DOMAIN":             stripScheme(h.Jira.URL()),
		"JIRA_BASE_URL":           h.Jira.URL(),
		"JIRA_ACTIVE_SPACE":       "MCPI",
		"JIRA_ACTIVE_SPACE_NAME":  "MCP Integration",
		"JIRA_ACTIVE_BOARD":       "1",
		"JIRA_ACTIVE_BOARD_NAME":  "MCP Sprint",

		// GitHub (forward-compat, plugin-github not built yet)
		"GITHUB_TOKEN":    "fake-github-token",
		"GITHUB_BASE_URL": h.GitHub.URL(),

		// Ollama
		"OLLAMA_URL":         h.Ollama.URL(),
		"OLLAMA_EMBED_HOST":  h.Ollama.URL(),
	}
}

// VaultLookup returns a function compatible with pkg/nexus.VaultLookup.
// PluginPool calls this when resolving a plugin spec's env_from_vault
// names; the harness's lookup answers the same set of names that Env()
// covers.
//
// Returning a function (rather than implementing the type directly) means
// callers can pass `h.VaultLookup()` straight into NewPluginPool without
// importing pkg/nexus from the testmock package.
func (h *Harness) VaultLookup() func(name string) (string, bool) {
	bag := h.Env()
	return func(name string) (string, bool) {
		v, ok := bag[name]
		return v, ok
	}
}

// EnvSlice returns the Env map as a []string in "KEY=VALUE" form, ready
// for exec.Cmd.Env. Keys are emitted in deterministic order so tests
// can golden-compare the slice. Order: DeepSeek → Jira → GitHub → Ollama.
func (h *Harness) EnvSlice() []string {
	keys := []string{
		"DEEPSEEK_API_KEY", "DEEPSEEK_BASE_URL",
		"JIRA_TOKEN", "JIRA_EMAIL", "JIRA_DOMAIN", "JIRA_BASE_URL",
		"JIRA_ACTIVE_SPACE", "JIRA_ACTIVE_SPACE_NAME",
		"JIRA_ACTIVE_BOARD", "JIRA_ACTIVE_BOARD_NAME",
		"GITHUB_TOKEN", "GITHUB_BASE_URL",
		"OLLAMA_URL", "OLLAMA_EMBED_HOST",
	}
	bag := h.Env()
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+bag[k])
	}
	return out
}

// stripScheme returns the URL without "http://" or "https://" prefix —
// pkg/jira/Client expects Domain in host:port form.
func stripScheme(u string) string {
	for _, prefix := range []string{"http://", "https://"} {
		if strings.HasPrefix(u, prefix) {
			return u[len(prefix):]
		}
	}
	return u
}
