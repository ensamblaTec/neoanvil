package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTenantIDInjection verifies round-trip: Save → Load → GetByProvider → TenantID. [Épica 265.D]
func TestTenantIDInjection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")

	creds := &Credentials{Version: 1}
	creds.AddEntry("default", "tok-test-123", "tenant-acme")

	if err := Save(creds, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify file has restricted permissions.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode()&0o777 != 0o600 {
		t.Errorf("file perms = %o, want 0600", info.Mode()&0o777)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entry := loaded.GetByProvider("default")
	if entry == nil {
		t.Fatal("GetByProvider returned nil for 'default'")
	}
	if entry.TenantID != "tenant-acme" {
		t.Errorf("TenantID = %q, want %q", entry.TenantID, "tenant-acme")
	}
	if entry.Token != "tok-test-123" {
		t.Errorf("Token = %q, want %q", entry.Token, "tok-test-123")
	}
}

// TestLoad_RedactsTokenBytesOnParseError — regression for the json.Unmarshal
// token-leak vulnerability discovered in the 2026-05-01 audit (SEV 8). When
// the credentials file is corrupted (truncation, partial write, manual
// edit-gone-wrong), the underlying json.SyntaxError / UnmarshalTypeError
// includes a quoted excerpt of the input bytes around the offset. If the
// token line happens to be near the corruption, that excerpt leaks the token
// to any caller that logs the error. The fix replaces the raw error with a
// generic "format invalid" message that NEVER references file contents.
func TestLoad_RedactsTokenBytesOnParseError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	// Build an obviously-corrupt JSON containing an obviously-secret token.
	// If the redaction is broken the test fails because the error message
	// includes the marker byte sequence.
	const secretMarker = "TOK_THIS_MUST_NOT_LEAK_42"
	corrupt := []byte(`{"version":1,"entries":[{"provider":"deepseek","token":"` + secretMarker + `","tenant_id":"tenant-x"  // <-- syntax error: comment in JSON`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load on corrupt JSON should error")
	}
	msg := err.Error()
	if strings.Contains(msg, secretMarker) {
		t.Fatalf("token leak: error message contains the secret marker. err=%q", msg)
	}
	if !strings.Contains(msg, "redacted") {
		t.Errorf("expected error message to advertise redaction; got %q", msg)
	}
}

// TestSave_SymlinkAtTargetIsAtomicallyReplaced — regression for the symlink
// traversal vulnerability discovered in the 2026-05-01 audit (SEV 9). An
// adversary with write access to ~/.neo/ can pre-place a symlink at
// credentials.json pointing to /etc/passwd or any sensitive file. The pre-
// fix Save() used os.OpenFile with O_TRUNC, which followed the symlink and
// wrote the credentials to the attacker's chosen target. The fixed Save()
// uses write-tmp + atomic rename: os.Rename does NOT follow symlinks at the
// destination, so the symlink is replaced atomically by the regular file
// holding our credentials, and the symlink target file is never touched.
func TestSave_SymlinkAtTargetIsAtomicallyReplaced(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "credentials.json")
	bystander := filepath.Join(dir, "bystander.txt")
	if err := os.WriteFile(bystander, []byte("must-survive"), 0o600); err != nil {
		t.Fatalf("seed bystander: %v", err)
	}
	if err := os.Symlink(bystander, credsPath); err != nil {
		t.Skipf("os.Symlink unsupported on this filesystem: %v", err)
	}
	creds := &Credentials{Version: 1}
	creds.AddEntry("default", "secret-token", "tenant-x")
	if err := Save(creds, credsPath); err != nil {
		t.Fatalf("Save: %v", err)
	}
	bystanderAfter, err := os.ReadFile(bystander)
	if err != nil {
		t.Fatalf("read bystander: %v", err)
	}
	if string(bystanderAfter) != "must-survive" {
		t.Errorf("symlink target was overwritten — Save followed the symlink. content=%q", bystanderAfter)
	}
	info, err := os.Lstat(credsPath)
	if err != nil {
		t.Fatalf("lstat credsPath: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("credsPath is still a symlink after Save — atomic replace did not happen")
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("credsPath perm = %o, want 0600", info.Mode().Perm())
	}
	loaded, err := Load(credsPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if e := loaded.GetByProvider("default"); e == nil || e.Token != "secret-token" {
		t.Errorf("round-trip mismatch: got entry %+v", e)
	}
}

// TestCredentialsAddEntry verifies AddEntry updates existing provider entry.
func TestCredentialsAddEntry(t *testing.T) {
	creds := &Credentials{Version: 1}
	creds.AddEntry("default", "old-token", "tenant-1")
	creds.AddEntry("default", "new-token", "tenant-2")

	if len(creds.Entries) != 1 {
		t.Errorf("expected 1 entry after upsert, got %d", len(creds.Entries))
	}
	e := creds.GetByProvider("default")
	if e == nil {
		t.Fatal("entry missing after upsert")
	}
	if e.Token != "new-token" {
		t.Errorf("Token = %q, want %q", e.Token, "new-token")
	}
	if e.TenantID != "tenant-2" {
		t.Errorf("TenantID = %q, want %q", e.TenantID, "tenant-2")
	}
	if e.Type != CredTypeAPIToken {
		t.Errorf("Type = %q, want %q (legacy AddEntry should default to API token)", e.Type, CredTypeAPIToken)
	}
}

// TestCredentials_Add_FullEntry verifies the new struct-based Add method
// preserves OAuth fields and stamps CreatedAt automatically.
func TestCredentials_Add_FullEntry(t *testing.T) {
	creds := &Credentials{Version: 1}
	expires := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	creds.Add(CredEntry{
		Provider:     "jira",
		Type:         CredTypeOAuth2,
		Token:        "access-tok",
		RefreshToken: "refresh-tok",
		Scopes:       []string{"read:jira-work", "write:jira-work"},
		Email:        "user@example.com",
		Domain:       "acme.atlassian.net",
		ExpiresAt:    expires,
	})

	e := creds.GetByProvider("jira")
	if e == nil {
		t.Fatal("entry not found")
	}
	if e.RefreshToken != "refresh-tok" || len(e.Scopes) != 2 {
		t.Errorf("oauth fields lost: %+v", e)
	}
	if e.Email != "user@example.com" || e.Domain != "acme.atlassian.net" {
		t.Errorf("provider-specific fields lost: %+v", e)
	}
	if e.CreatedAt == "" {
		t.Error("CreatedAt should be auto-stamped")
	}
	if e.ExpiresAt != expires {
		t.Errorf("ExpiresAt = %q, want %q", e.ExpiresAt, expires)
	}
}

func TestCredentials_Add_UpsertReplaces(t *testing.T) {
	creds := &Credentials{Version: 1}
	creds.Add(CredEntry{Provider: "jira", Token: "v1"})
	creds.Add(CredEntry{Provider: "jira", Token: "v2", Type: CredTypeOAuth2})

	if len(creds.Entries) != 1 {
		t.Fatalf("entries=%d want 1 after upsert", len(creds.Entries))
	}
	if e := creds.GetByProvider("jira"); e == nil || e.Token != "v2" {
		t.Errorf("upsert failed: %+v", e)
	}
}

func TestCredEntry_IsExpired(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		expiresAt string
		want      bool
	}{
		{"empty", "", false},
		{"future", now.Add(48 * time.Hour).UTC().Format(time.RFC3339), false},
		{"past", now.Add(-1 * time.Hour).UTC().Format(time.RFC3339), true},
		{"malformed", "not-a-timestamp", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &CredEntry{ExpiresAt: tc.expiresAt}
			if got := e.IsExpired(now); got != tc.want {
				t.Errorf("IsExpired() = %v, want %v", got, tc.want)
			}
		})
	}
	var nilEntry *CredEntry
	if nilEntry.IsExpired(now) {
		t.Error("nil receiver should not be expired")
	}
}

func TestCredEntry_ExpiresIn(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	future := &CredEntry{ExpiresAt: now.Add(2 * time.Hour).UTC().Format(time.RFC3339)}
	if d := future.ExpiresIn(now); d <= 0 {
		t.Errorf("future ExpiresIn = %v, want positive", d)
	}

	past := &CredEntry{ExpiresAt: now.Add(-30 * time.Minute).UTC().Format(time.RFC3339)}
	if d := past.ExpiresIn(now); d >= 0 {
		t.Errorf("past ExpiresIn = %v, want negative", d)
	}

	none := &CredEntry{}
	if d := none.ExpiresIn(now); d != 0 {
		t.Errorf("empty ExpiresIn = %v, want 0", d)
	}
}

// TestCredEntry_BackwardCompat ensures legacy PILAR-XXXIII JSON (no
// Type/ExpiresAt/RefreshToken/Scopes/Email/Domain) parses cleanly.
func TestCredEntry_BackwardCompat(t *testing.T) {
	legacy := `{
		"version": 1,
		"entries": [
			{"provider": "openai", "token": "sk-old", "tenant_id": "t1", "created_at": "2025-01-01T00:00:00Z"}
		]
	}`
	var creds Credentials
	if err := json.Unmarshal([]byte(legacy), &creds); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	e := creds.GetByProvider("openai")
	if e == nil {
		t.Fatal("legacy entry not parsed")
	}
	if e.Type != "" || e.ExpiresAt != "" || e.RefreshToken != "" {
		t.Errorf("expected zero values for new fields on legacy entry: %+v", e)
	}
	if e.IsExpired(time.Now()) {
		t.Error("legacy entry without ExpiresAt should never be expired")
	}
}
