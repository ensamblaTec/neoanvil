package auth

import (
	"path/filepath"
	"testing"

	"github.com/99designs/keyring"
)

// newTestKeyring opens a deterministic file-based keyring under t.TempDir().
// Avoids touching the developer's real Keychain / libsecret during go test.
func newTestKeyring(t *testing.T) *KeyringBackend {
	t.Helper()
	kb, err := OpenKeyring(KeyringConfig{
		ServiceName:     "neoanvil-test",
		AllowedBackends: []keyring.BackendType{keyring.FileBackend},
		FileDir:         t.TempDir(),
		PasswordFunc:    func(string) (string, error) { return "test-pass", nil },
	})
	if err != nil {
		t.Fatalf("OpenKeyring: %v", err)
	}
	return kb
}

func TestKeyringBackend_RoundTrip(t *testing.T) {
	kb := newTestKeyring(t)

	creds := &Credentials{Version: 1}
	creds.AddEntry("jira", "secret-token", "tenant-1")

	if err := kb.Save(creds); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := kb.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entry := loaded.GetByProvider("jira")
	if entry == nil {
		t.Fatal("entry not found after round-trip")
	}
	if entry.Token != "secret-token" {
		t.Errorf("token=%q want secret-token", entry.Token)
	}
	if entry.TenantID != "tenant-1" {
		t.Errorf("tenant=%q want tenant-1", entry.TenantID)
	}
}

func TestKeyringBackend_LoadEmpty(t *testing.T) {
	kb := newTestKeyring(t)

	creds, err := kb.Load()
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if len(creds.Entries) != 0 {
		t.Errorf("entries=%d want 0", len(creds.Entries))
	}
	if creds.Version != 1 {
		t.Errorf("version=%d want 1", creds.Version)
	}
}

func TestKeyringBackend_SaveNilFails(t *testing.T) {
	kb := newTestKeyring(t)
	if err := kb.Save(nil); err == nil {
		t.Error("Save(nil) should fail")
	}
}

func TestKeyringBackend_OverwriteEntry(t *testing.T) {
	kb := newTestKeyring(t)

	first := &Credentials{Version: 1}
	first.AddEntry("github", "v1-token", "")
	if err := kb.Save(first); err != nil {
		t.Fatalf("Save first: %v", err)
	}

	second := &Credentials{Version: 1}
	second.AddEntry("github", "v2-token", "")
	if err := kb.Save(second); err != nil {
		t.Fatalf("Save second: %v", err)
	}

	loaded, err := kb.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if e := loaded.GetByProvider("github"); e == nil || e.Token != "v2-token" {
		t.Errorf("expected v2-token after overwrite, got %+v", e)
	}
}

func TestFileBackend_RoundTrip(t *testing.T) {
	fb := NewFileBackend(filepath.Join(t.TempDir(), "creds.json"))

	creds := &Credentials{Version: 1}
	creds.AddEntry("github", "gh-token", "")

	if err := fb.Save(creds); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := fb.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if e := loaded.GetByProvider("github"); e == nil || e.Token != "gh-token" {
		t.Errorf("FileBackend round-trip failed: %+v", loaded)
	}
}

func TestNewFileBackend_DefaultPath(t *testing.T) {
	fb := NewFileBackend("")
	if fb.Path == "" {
		t.Error("default path should resolve to non-empty")
	}
}

// TestBackendInterface verifies both implementations satisfy the Backend
// contract — caught at compile time, but documents the intent.
func TestBackendInterface(t *testing.T) {
	var _ Backend = (*FileBackend)(nil)
	var _ Backend = (*KeyringBackend)(nil)
}
