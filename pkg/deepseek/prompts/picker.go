// Package prompts provides domain-specific prompt augmentations for the
// DeepSeek red_team_audit action. Each template encodes invariants and
// severity floors specific to a code domain (crypto, storage, auth,
// concurrency, network) so DS gets calibrated context BEFORE the audit
// instead of relying on Claude to pre-engineer every prompt manually.
//
// ÉPICA 151.D / PILAR XXIX. Built from the empirical pattern library
// developed during PILAR XXVI/XXVIII audit sessions:
//   - PILAR XXVI brain code (~7000 LOC) audited without templates → 5 SEV 9/8
//     regressions caught at production-readiness gate, 30% of DS findings
//     hallucinated.
//   - Today's PILAR XXIX session (with manually-written domain prompts)
//     caught 12 regressions with 0 hallucinations after triage. Templates
//     encode that experience so future audits don't depend on the auditor
//     remembering the right prompt scaffolding.
//
// Lifecycle: PickTemplates is called once per red_team_audit invocation
// from cmd/plugin-deepseek/tool_red_team.go. The matched template
// content is prepended to the system prompt, augmenting the
// general-purpose instructions in cache.NewBuilder.
package prompts

import (
	_ "embed"
	"strings"
)

//go:embed crypto.md
var cryptoTemplate string

//go:embed storage.md
var storageTemplate string

//go:embed auth.md
var authTemplate string

//go:embed concurrency.md
var concurrencyTemplate string

//go:embed network.md
var networkTemplate string

// DomainTemplate pairs a name + content with the file-path patterns that
// trigger inclusion. Patterns are case-insensitive substring matches —
// regex would be stricter but adds complexity for marginal gain at the
// scale of "5 templates × 5-10 patterns each".
type DomainTemplate struct {
	Name     string
	Patterns []string
	Content  string
}

// allTemplates is the registry. Order is non-significant; PickTemplates
// returns matches in registry order so output is deterministic.
var allTemplates = []DomainTemplate{
	{
		Name:    "crypto",
		Content: cryptoTemplate,
		Patterns: []string{
			"crypto", "blake2b", "encrypt", "decrypt",
			"signing", "signature", "tls", "cipher",
			"keystore", "kdf", "hkdf", "aead",
		},
	},
	{
		Name:    "storage",
		Content: storageTemplate,
		Patterns: []string{
			"storage/", "/wal", "/bbolt", "/bolt",
			"hnsw_persist", "snapshot", "archive",
			"keystore", "atomic_write",
		},
	},
	{
		Name:    "auth",
		Content: authTemplate,
		Patterns: []string{
			"/auth/", "keystore", "credentials",
			"acl", "/oauth/", "/authz",
			"workspace_id", "permission",
		},
	},
	{
		Name:    "concurrency",
		Content: concurrencyTemplate,
		Patterns: []string{
			"goroutine", "sync.", "atomic.",
			"singleflight", "mutex", "channel",
			"daemon", "process_pool", "watchdog",
			"reaper", "lifecycle",
		},
	},
	{
		Name:    "network",
		Content: networkTemplate,
		Patterns: []string{
			"sse.", "mcp/", "/nexus", "http.",
			"safe_http", "ssrf", "cors",
			"plugin_routing", "dispatch",
		},
	},
}

// PickTemplates returns templates whose patterns match at least one of
// the provided file paths. Matching is case-insensitive substring on
// the path (lowercased). Order matches the registry order — stable
// across calls.
//
// When zero files match any template, returns an empty slice. The
// caller should fall back to the default audit prompt.
func PickTemplates(files []string) []DomainTemplate {
	if len(files) == 0 {
		return nil
	}
	lowered := make([]string, len(files))
	for i, f := range files {
		lowered[i] = strings.ToLower(f)
	}
	var matched []DomainTemplate
	for _, t := range allTemplates {
		if matchesAny(lowered, t.Patterns) {
			matched = append(matched, t)
		}
	}
	return matched
}

// matchesAny returns true when at least one pattern is a substring of
// at least one file path.
func matchesAny(files []string, patterns []string) bool {
	for _, f := range files {
		for _, p := range patterns {
			if strings.Contains(f, p) {
				return true
			}
		}
	}
	return false
}

// AssemblePrefix builds the prompt prefix from matched templates. Each
// template's Content is separated by a newline divider so DS sees them
// as distinct sections. Returns empty string when no templates matched
// — caller appends to system prompt unchanged.
//
// The mechanical_trace requirement (151.B) is appended to every
// template-augmented prompt so even out-of-domain audits get the
// hallucination guard.
func AssemblePrefix(templates []DomainTemplate) string {
	if len(templates) == 0 {
		return mechanicalTraceRequirement()
	}
	var parts []string
	for _, t := range templates {
		parts = append(parts, t.Content)
	}
	parts = append(parts, mechanicalTraceRequirement())
	return strings.Join(parts, "\n\n---\n\n")
}

// mechanicalTraceRequirement (151.B) is the universal hallucination
// guard: every finding must include a mechanical step-by-step trace of
// HOW the impact is reached. If DS cannot trace it, the finding is
// dropped. This pattern empirically reduced hallucination rate from
// ~30% (PILAR XXVI) to <5% (today's audits, post-mechanical-trace prompt).
func mechanicalTraceRequirement() string {
	return `For each finding you report, include a mechanical_trace:
a step-by-step explanation of HOW the failure / attack reaches the
impact. If you cannot trace the impact mechanically, DROP the finding.
"Compose-2-true-premises into false-conclusion" is the dominant
hallucination mode — the trace forces you to verify each step against
the actual code.`
}
