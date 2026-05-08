package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/auth"
)

// runLogin executes the login cobra command in a sandboxed HOME and returns
// the path used for credentials.json so callers can assert post-conditions.
func runLogin(t *testing.T, args []string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	cmd := loginCmd()
	cmd.SetArgs(args)
	cmd.SetOut(os.Stderr) // silence stdout
	cmd.SetErr(os.Stderr)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return filepath.Join(home, ".neo", "credentials.json")
}

func TestLoginCmd_BasicAPIToken(t *testing.T) {
	credsPath := runLogin(t, []string{
		"--provider", "github",
		"--token", "ghp_test_xxx",
		"--tenant", "",
	})

	creds, err := auth.Load(credsPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e := creds.GetByProvider("github")
	if e == nil {
		t.Fatal("entry not found")
	}
	if e.Token != "ghp_test_xxx" {
		t.Errorf("Token=%q want ghp_test_xxx", e.Token)
	}
	if e.Type != auth.CredTypeAPIToken {
		t.Errorf("Type=%q want %q (default)", e.Type, auth.CredTypeAPIToken)
	}
}

func TestLoginCmd_FullFlags(t *testing.T) {
	credsPath := runLogin(t, []string{
		"--provider", "jira",
		"--token", "atl_token",
		"--type", "api_token",
		"--email", "user@acme.com",
		"--domain", "acme.atlassian.net",
		"--expires", "2027-04-28T00:00:00Z",
		"--tenant", "tA",
	})

	creds, err := auth.Load(credsPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e := creds.GetByProvider("jira")
	if e == nil {
		t.Fatal("entry not found")
	}
	if e.Email != "user@acme.com" || e.Domain != "acme.atlassian.net" {
		t.Errorf("identity fields lost: %+v", e)
	}
	if e.ExpiresAt != "2027-04-28T00:00:00Z" {
		t.Errorf("ExpiresAt=%q", e.ExpiresAt)
	}
	if e.TenantID != "tA" {
		t.Errorf("TenantID=%q", e.TenantID)
	}
}

func TestLoginCmd_OAuth2Entry(t *testing.T) {
	credsPath := runLogin(t, []string{
		"--provider", "future-jira",
		"--token", "access",
		"--refresh-token", "refresh",
		"--type", "oauth2",
		"--tenant", "",
	})
	creds, _ := auth.Load(credsPath)
	e := creds.GetByProvider("future-jira")
	if e == nil {
		t.Fatal("entry not found")
	}
	if e.Type != auth.CredTypeOAuth2 || e.RefreshToken != "refresh" {
		t.Errorf("oauth fields wrong: %+v", e)
	}
}

func TestLoginCmd_AuditLogAppended(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cmd := loginCmd()
	cmd.SetArgs([]string{"--provider", "github", "--token", "x", "--tenant", ""})
	cmd.SetOut(os.Stderr)
	cmd.SetErr(os.Stderr)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	auditPath := filepath.Join(home, ".neo", "audit.log")
	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("audit log is empty")
	}
	line := strings.TrimSpace(strings.Split(string(raw), "\n")[0])
	var entry auth.AuditEntry
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("unmarshal audit entry: %v", err)
	}
	if entry.Kind != "credential_added" || entry.Provider != "github" {
		t.Errorf("unexpected audit entry: %+v", entry)
	}

	// Verify the audit log chain.
	logger, err := auth.OpenAuditLog(auditPath)
	if err != nil {
		t.Fatalf("OpenAuditLog: %v", err)
	}
	defer logger.Close()
	if err := logger.Verify(); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestLoginCmd_ValidationRejectsEmptyTokenInteractiveDisabled(t *testing.T) {
	// Cobra Execute with --token "" but flag changed -> interactive prompt
	// would normally fire. We stub stdin to empty. Skipped: non-trivial to
	// drive the interactive scanner reliably from a unit test. The
	// validation path is covered by TestAPITokenProvider_Validate in
	// pkg/auth/provider_test.go.
	t.Skip("interactive prompt path; validation covered by pkg/auth/provider tests")
}

func TestLoginCmd_UpsertReplaces(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for i, tok := range []string{"v1", "v2"} {
		cmd := loginCmd()
		cmd.SetArgs([]string{"--provider", "github", "--token", tok, "--tenant", ""})
		cmd.SetOut(os.Stderr)
		cmd.SetErr(os.Stderr)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("login %d: %v", i, err)
		}
	}

	credsPath := filepath.Join(home, ".neo", "credentials.json")
	creds, _ := auth.Load(credsPath)
	if len(creds.Entries) != 1 {
		t.Errorf("entries=%d want 1 after upsert", len(creds.Entries))
	}
	if e := creds.GetByProvider("github"); e == nil || e.Token != "v2" {
		t.Errorf("upsert failed: %+v", e)
	}
}
