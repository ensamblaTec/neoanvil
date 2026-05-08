package testmock

import (
	"strings"
	"testing"
)

func TestHarness_BootsAllFourMocks(t *testing.T) {
	h := NewHarness(t)
	if h.Jira == nil || h.DeepSeek == nil || h.Ollama == nil || h.GitHub == nil {
		t.Fatalf("harness has nil mocks: %+v", h)
	}
	if h.Jira.URL() == "" || h.DeepSeek.URL() == "" || h.Ollama.URL() == "" || h.GitHub.URL() == "" {
		t.Errorf("one or more mock URLs empty")
	}
}

func TestHarness_EnvHasAllExpectedKeys(t *testing.T) {
	h := NewHarness(t)
	env := h.Env()
	expected := []string{
		"DEEPSEEK_API_KEY", "DEEPSEEK_BASE_URL",
		"JIRA_TOKEN", "JIRA_EMAIL", "JIRA_DOMAIN", "JIRA_BASE_URL",
		"JIRA_ACTIVE_SPACE", "JIRA_ACTIVE_SPACE_NAME",
		"JIRA_ACTIVE_BOARD", "JIRA_ACTIVE_BOARD_NAME",
		"GITHUB_TOKEN", "GITHUB_BASE_URL",
		"OLLAMA_URL", "OLLAMA_EMBED_HOST",
	}
	for _, k := range expected {
		if v, ok := env[k]; !ok || v == "" {
			t.Errorf("env[%q] empty or missing (value=%q ok=%v)", k, v, ok)
		}
	}
}

func TestHarness_EnvBaseURLsPointAtMocks(t *testing.T) {
	h := NewHarness(t)
	env := h.Env()
	cases := []struct {
		key  string
		want string
	}{
		{"DEEPSEEK_BASE_URL", h.DeepSeek.URL()},
		{"JIRA_BASE_URL", h.Jira.URL()},
		{"GITHUB_BASE_URL", h.GitHub.URL()},
		{"OLLAMA_URL", h.Ollama.URL()},
		{"OLLAMA_EMBED_HOST", h.Ollama.URL()},
	}
	for _, tc := range cases {
		if env[tc.key] != tc.want {
			t.Errorf("env[%q]=%q want %q", tc.key, env[tc.key], tc.want)
		}
	}
}

func TestHarness_JiraDomainStripsScheme(t *testing.T) {
	h := NewHarness(t)
	dom := h.Env()["JIRA_DOMAIN"]
	if strings.HasPrefix(dom, "http://") || strings.HasPrefix(dom, "https://") {
		t.Errorf("JIRA_DOMAIN=%q still has scheme", dom)
	}
	if dom != strings.TrimPrefix(h.Jira.URL(), "http://") {
		t.Errorf("JIRA_DOMAIN=%q does not match host:port of mock URL %q", dom, h.Jira.URL())
	}
}

func TestHarness_VaultLookupResolvesAllEnv(t *testing.T) {
	h := NewHarness(t)
	lookup := h.VaultLookup()
	for k, v := range h.Env() {
		got, ok := lookup(k)
		if !ok {
			t.Errorf("VaultLookup(%q) reported missing", k)
			continue
		}
		if got != v {
			t.Errorf("VaultLookup(%q)=%q want %q", k, got, v)
		}
	}
}

func TestHarness_VaultLookupRejectsUnknownKey(t *testing.T) {
	h := NewHarness(t)
	lookup := h.VaultLookup()
	if v, ok := lookup("NONEXISTENT_VAULT_KEY"); ok {
		t.Errorf("lookup of unknown key returned ok=true value=%q", v)
	}
}

func TestHarness_EnvSliceMatchesEnvAndIsDeterministic(t *testing.T) {
	h := NewHarness(t)
	slice1 := h.EnvSlice()
	slice2 := h.EnvSlice()
	if len(slice1) != len(slice2) {
		t.Fatalf("EnvSlice not deterministic: len %d vs %d", len(slice1), len(slice2))
	}
	for i := range slice1 {
		if slice1[i] != slice2[i] {
			t.Fatalf("EnvSlice not deterministic at [%d]: %q vs %q", i, slice1[i], slice2[i])
		}
	}
	if len(slice1) != len(h.Env()) {
		t.Errorf("EnvSlice len=%d Env() len=%d (drift)", len(slice1), len(h.Env()))
	}
	// Each entry has KEY=VALUE shape and the key resolves via Env().
	env := h.Env()
	for _, kv := range slice1 {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			t.Errorf("malformed entry %q", kv)
			continue
		}
		key, val := kv[:eq], kv[eq+1:]
		if env[key] != val {
			t.Errorf("entry %q diverges from Env()[%q]=%q", kv, key, env[key])
		}
	}
}

func TestHarness_MocksRespondToHTTP(t *testing.T) {
	// Smoke test that each mock is actually listening (defends against the
	// harness somehow forgetting to wire one of them up).
	h := NewHarness(t)
	h.Jira.SetIssue("MCPI-99", JiraIssue{Summary: "harness smoke", Status: "Open"})

	resp := mustGet(t, h.Jira.URL()+"/rest/api/3/issue/MCPI-99")
	defer resp.Body.Close()
	// 401 because no auth — but the connection SUCCEEDED, which is the
	// liveness signal we care about.
	if resp.StatusCode != 401 {
		t.Errorf("jira mock got %d (expected 401 unauth)", resp.StatusCode)
	}
}
