package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCallLinkIssue_RejectsMalformedKeys confirms link_issue applies
// validateTicketID at the dispatch boundary on both from_key and to_key.
// Defense-in-depth: even though pkg/jira/Client.LinkIssue uses a JSON
// body (not URL interpolation), keeping validation symmetric across
// all dispatch handlers prevents future refactors from quietly opening
// a path-injection vector. [Phase E follow-up]
func TestCallLinkIssue_RejectsMalformedKeys(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{
			name: "from_key path traversal",
			args: map[string]any{"from_key": "MCPI-1/../admin", "to_key": "MCPI-2", "link_type": "Blocks"},
			want: "from_key:",
		},
		{
			name: "to_key lowercase",
			args: map[string]any{"from_key": "MCPI-1", "to_key": "abc-1", "link_type": "Blocks"},
			want: "to_key:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &state{}
			resp := s.callLinkIssue(1, tc.args, callCtx{})
			errObj, ok := resp["error"].(map[string]any)
			if !ok {
				t.Fatalf("expected error response, got %v", resp)
			}
			msg, _ := errObj["message"].(string)
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error message %q does not contain %q", msg, tc.want)
			}
		})
	}
}

func TestValidateTicketID(t *testing.T) {
	good := []string{"MCPI-1", "ABC-12345", "TEST-1", "AB-1"}
	bad := []string{
		"",
		"abc-1",            // lowercase
		"A-1",              // single-letter project key (regex requires 2+)
		"ABCDEFGHIJK-1",    // 11-char project key (regex caps at 10)
		"MCPI-",            // missing number
		"MCPI-abc",         // non-digit number
		"MCPI-1/../foo",    // path traversal
		"MCPI-1?evil=true", // query string
		"MCPI-1#frag",      // fragment
		"MCPI 1",           // space
	}
	for _, s := range good {
		if err := validateTicketID(s); err != nil {
			t.Errorf("validateTicketID(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range bad {
		if err := validateTicketID(s); err == nil {
			t.Errorf("validateTicketID(%q) = nil, want error", s)
		}
	}
}

func TestValidateSafeFolderPath_DefaultsToTicketSubdir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	got, err := validateSafeFolderPath("", "MCPI-7")
	if err != nil {
		t.Fatalf("default folder_path: %v", err)
	}
	want := filepath.Join(tmp, ".neo", "jira-docs", "MCPI-7")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Base dir must have been auto-created.
	if _, err := os.Stat(filepath.Dir(got)); err != nil {
		t.Errorf("jira-docs base not created: %v", err)
	}
}

func TestValidateSafeFolderPath_RejectsEscape(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	for _, attack := range []string{
		"/etc/ssh",
		"/etc/passwd",
		"../../../etc",
		"/tmp/anywhere",
	} {
		_, err := validateSafeFolderPath(attack, "MCPI-1")
		if err == nil {
			t.Errorf("attack %q: expected refusal, got success", attack)
		}
		if !strings.Contains(err.Error(), "must live under") {
			t.Errorf("attack %q: error %q does not mention base anchor", attack, err)
		}
	}
}

func TestValidateSafeFolderPath_AcceptsExplicitInsideBase(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	base := filepath.Join(tmp, ".neo", "jira-docs")
	_ = os.MkdirAll(base, 0o755)
	explicit := filepath.Join(base, "custom-subdir")

	got, err := validateSafeFolderPath(explicit, "MCPI-1")
	if err != nil {
		t.Fatalf("explicit safe path rejected: %v", err)
	}
	if got != explicit {
		t.Errorf("got %q, want %q", got, explicit)
	}
}

func TestValidateSafeRepoRoot_RequiresExistingDir(t *testing.T) {
	tmp := t.TempDir()
	got, err := validateSafeRepoRoot(tmp)
	if err != nil {
		t.Fatalf("valid tmp dir rejected: %v", err)
	}
	if got != tmp {
		t.Errorf("got %q, want %q", got, tmp)
	}

	// Non-existent path → error.
	if _, err := validateSafeRepoRoot("/no/such/path/here"); err == nil {
		t.Errorf("nonexistent path accepted")
	}

	// File (not dir) → error.
	f := filepath.Join(tmp, "file.txt")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	if _, err := validateSafeRepoRoot(f); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("file accepted as repo_root: err=%v", err)
	}

	// Empty → error.
	if _, err := validateSafeRepoRoot(""); err == nil {
		t.Errorf("empty repo_root accepted")
	}
}
