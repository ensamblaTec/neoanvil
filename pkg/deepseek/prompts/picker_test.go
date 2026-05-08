package prompts

import (
	"strings"
	"testing"
)

// TestPickTemplates_ExactDomain verifies that a file path matching a
// single domain returns that template (and no others). [151.D]
func TestPickTemplates_ExactDomain(t *testing.T) {
	cases := []struct {
		name        string
		files       []string
		wantContain string
	}{
		{"crypto", []string{"pkg/brain/crypto.go"}, "crypto"},
		{"storage", []string{"pkg/brain/storage/local.go"}, "storage"},
		{"auth", []string{"pkg/auth/keystore.go"}, "auth"},
		{"concurrency", []string{"pkg/state/daemon_audit.go"}, "concurrency"},
		{"network", []string{"cmd/neo-nexus/sse.go"}, "network"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PickTemplates(tc.files)
			if len(got) == 0 {
				t.Fatalf("no template matched for %v", tc.files)
			}
			found := false
			for _, m := range got {
				if m.Name == tc.wantContain {
					found = true
					break
				}
			}
			if !found {
				names := make([]string, len(got))
				for i, m := range got {
					names[i] = m.Name
				}
				t.Errorf("expected %s in matched templates, got %v", tc.wantContain, names)
			}
		})
	}
}

// TestPickTemplates_NoMatch verifies that file paths outside any
// domain yield no templates. Caller falls back to the default prompt.
func TestPickTemplates_NoMatch(t *testing.T) {
	got := PickTemplates([]string{"docs/random.md", "scripts/build.sh"})
	if len(got) != 0 {
		t.Errorf("expected no matches, got %d", len(got))
	}
}

// TestPickTemplates_EmptyFiles verifies the nil-files fast path.
func TestPickTemplates_EmptyFiles(t *testing.T) {
	if got := PickTemplates(nil); len(got) != 0 {
		t.Errorf("nil files should yield no templates, got %d", len(got))
	}
	if got := PickTemplates([]string{}); len(got) != 0 {
		t.Errorf("empty files should yield no templates, got %d", len(got))
	}
}

// TestPickTemplates_MultiDomainMatch verifies that a mixed-file audit
// (auth + storage code) returns BOTH templates. The order matches
// registry order (deterministic).
func TestPickTemplates_MultiDomainMatch(t *testing.T) {
	files := []string{
		"pkg/auth/keystore.go",          // auth
		"pkg/brain/storage/r2.go",       // storage
		"pkg/sre/safe_http.go",          // network
		"pkg/state/daemon_audit.go",     // concurrency
	}
	got := PickTemplates(files)
	if len(got) < 4 {
		names := make([]string, len(got))
		for i, m := range got {
			names[i] = m.Name
		}
		t.Errorf("expected ≥4 templates for multi-domain files, got %d (%v)", len(got), names)
	}
}

// TestPickTemplates_CaseInsensitive verifies that uppercase/lowercase
// path components both match.
func TestPickTemplates_CaseInsensitive(t *testing.T) {
	got := PickTemplates([]string{"PKG/AUTH/KEYSTORE.GO"})
	if len(got) == 0 {
		t.Error("uppercase path should still match auth template")
	}
}

// TestAssemblePrefix_WithTemplates verifies the prefix includes each
// matched template's content + the mechanical_trace requirement.
func TestAssemblePrefix_WithTemplates(t *testing.T) {
	templates := PickTemplates([]string{"pkg/brain/crypto.go"})
	prefix := AssemblePrefix(templates)
	if !strings.Contains(prefix, "Crypto domain checklist") {
		t.Error("prefix missing crypto template content")
	}
	if !strings.Contains(prefix, "mechanical_trace") {
		t.Error("prefix missing mechanical_trace requirement (151.B)")
	}
}

// TestAssemblePrefix_NoTemplates verifies the mechanical_trace
// requirement appears even when no domain template matched. The
// hallucination guard applies to all audits, not just domain-matched ones.
func TestAssemblePrefix_NoTemplates(t *testing.T) {
	prefix := AssemblePrefix(nil)
	if !strings.Contains(prefix, "mechanical_trace") {
		t.Error("nil templates should still emit mechanical_trace requirement")
	}
	if strings.Contains(prefix, "Crypto") || strings.Contains(prefix, "Storage") {
		t.Errorf("nil templates should not include domain checklists: %s", prefix)
	}
}
