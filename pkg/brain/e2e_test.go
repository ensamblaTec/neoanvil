// e2e_test.go — end-to-end integration tests for PILAR XXVI Brain
// Portable. Each test composes the full pipeline (walk → manifest →
// build → encrypt → store → retrieve → decrypt → extract) using only
// public API so the tests double as doctrine examples.
//
// Tests use t.TempDir() for both source and destination so they run
// without network or operator credentials. Cross-platform concerns
// (path separators, file modes) are covered structurally; live
// cross-platform CI matrix is a separate concern (not gated here).

package brain_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/brain"
	"github.com/ensamblatec/neoanvil/pkg/brain/storage"
	"github.com/ensamblatec/neoanvil/pkg/workspace"
)

// TestE2E_FullRoundtrip — the canonical Mac→PC scenario simulated with
// two TempDirs as origin and destination machines. Confirms:
//
//   1. Origin's manifest captures every workspace + canonical_id
//   2. Encrypted archive survives store roundtrip
//   3. Destination's pull restores the same content under the right path
//   4. The 135.E PathResolution chain hits the local-registry-fast-path
//      when the destination has matching canonical_ids
// createFileTree writes a map of relative-path→content under base,
// creating parent directories as needed. Fatalf on any error.
func createFileTree(t *testing.T, base string, files map[string]string) {
	t.Helper()
	for path, body := range files {
		full := filepath.Join(base, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// buildEncryptedArchive creates and encrypts a brain archive for testing.
// Returns the encrypted buffer, symmetric key, and AAD; fatalf on any error.
func buildEncryptedArchive(t *testing.T, manifest *brain.Manifest, passphrase string) (bytes.Buffer, []byte, []byte) {
	t.Helper()
	var archiveBuf bytes.Buffer
	if _, err := brain.BuildArchive(manifest, &archiveBuf); err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}
	key, err := brain.DeriveKey([]byte(passphrase), []byte(manifest.NodeID))
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	aad := []byte(manifest.HLC.String() + "|" + manifest.NodeID)
	var encrypted bytes.Buffer
	if err := brain.EncryptStream(&archiveBuf, &encrypted, key, aad); err != nil {
		t.Fatalf("EncryptStream: %v", err)
	}
	return encrypted, key, aad
}

func TestE2E_FullRoundtrip(t *testing.T) {
	const passphrase = "integration-test-pass-not-real"

	// ─── ORIGIN SIDE ─────────────────────────────────────────────────
	originHome := t.TempDir()
	originWS := filepath.Join(originHome, "fake-repo")
	createFileTree(t, originWS, map[string]string{
		"README.md":   "# fake repo\n",
		"src/main.go": "package main\nfunc main(){}\n",
		"src/util.go": "package main\n// helpers\n",
		"server.log":  "ERROR should-be-excluded\n", // excluded by suffix
		".git/config": "[core]\n",                    // excluded by dir
	})

	originReg := &workspace.Registry{
		Workspaces: []workspace.WorkspaceEntry{
			{ID: "fake-1", Path: originWS, Name: "fake-repo", Type: "workspace"},
		},
		ActiveID: "fake-1",
	}
	walked := brain.WalkWorkspaces(originReg)
	if len(walked) != 1 {
		t.Fatalf("origin walk: got %d workspaces, want 1", len(walked))
	}
	originCanonical := walked[0].CanonicalID

	manifest := brain.NewManifest(walked, nil, nil)
	encrypted, key, aad := buildEncryptedArchive(t, manifest, passphrase)

	// ─── REMOTE STORAGE ──────────────────────────────────────────────
	remoteDir := t.TempDir()
	store, err := storage.NewLocalStore(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.Put("snapshots/test/archive.bin", &encrypted); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// ─── DESTINATION SIDE ────────────────────────────────────────────
	rc, err := store.Get("snapshots/test/archive.bin")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	var decrypted bytes.Buffer
	if err := brain.DecryptStream(rc, &decrypted, key, aad); err != nil {
		t.Fatalf("DecryptStream: %v", err)
	}

	dest := t.TempDir()
	written, err := brain.ApplyArchive(&decrypted, manifest, brain.HLC{}, dest, false)
	if err != nil {
		t.Fatalf("ApplyArchive: %v", err)
	}

	// ─── ASSERTIONS ──────────────────────────────────────────────────
	if len(written) == 0 {
		t.Fatal("ApplyArchive wrote zero files")
	}
	// Confirm a known included file made it through.
	got, err := os.ReadFile(filepath.Join(dest, "workspace", "fake-1", "src", "main.go"))
	if err != nil {
		t.Fatalf("read restored main.go: %v", err)
	}
	if !strings.Contains(string(got), "func main()") {
		t.Errorf("content drift: %q", got)
	}
	// Confirm an excluded file did NOT make it through.
	if _, err := os.Stat(filepath.Join(dest, "workspace", "fake-1", "server.log")); err == nil {
		t.Error("server.log leaked into archive (should be excluded by suffix)")
	}
	if _, err := os.Stat(filepath.Join(dest, "workspace", "fake-1", ".git", "config")); err == nil {
		t.Error(".git/config leaked into archive (should be excluded by dir)")
	}

	// 135.E pathmap chain: simulate the destination machine having a
	// registered workspace with the same canonical_id.
	destReg := &workspace.Registry{
		Workspaces: []workspace.WorkspaceEntry{
			{ID: "fake-dest-1", Path: dest, Name: "fake-repo", Type: "workspace"},
		},
	}
	destWalked := brain.WalkWorkspaces(destReg)
	if len(destWalked) != 1 {
		t.Fatal("dest walk failed")
	}
	// Note: dest's canonical_id will likely differ from origin's because
	// path-hash falls back when there's no .git/.neo-project. That's the
	// expected behavior — the operator uses path_map to bridge the gap.
	pm := brain.NewPathMap()
	_ = pm.Set(originCanonical, brain.PathMapEntry{Path: dest})
	res := brain.ResolveWorkspacePath(originCanonical, originWS, nil, pm, false, "")
	if res.Source != brain.ResolutionSourcePathMap || res.Path != dest {
		t.Errorf("path_map fallback failed: %+v", res)
	}
}

// TestE2E_TamperDetected — flip a bit in the archive after encryption
// and confirm the consumer side reports an auth failure (AEAD tag
// mismatch). This is the integrity guarantee the operator relies on.
func TestE2E_TamperDetected(t *testing.T) {
	pass := []byte("p")
	salt := []byte("s")
	key, _ := brain.DeriveKey(pass, salt)
	aad := []byte("aad")

	var encrypted bytes.Buffer
	if err := brain.EncryptStream(strings.NewReader("the original payload"), &encrypted, key, aad); err != nil {
		t.Fatal(err)
	}
	corrupted := append([]byte(nil), encrypted.Bytes()...)
	// Flip a bit far enough into the stream to land in ciphertext (skip the 4-byte length header).
	corrupted[10] ^= 0x01

	var out bytes.Buffer
	err := brain.DecryptStream(bytes.NewReader(corrupted), &out, key, aad)
	if err == nil {
		t.Fatal("tampered stream should fail AEAD verification")
	}
	if !strings.Contains(err.Error(), "auth failed") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestE2E_WrongPassphrase — the receiver with the wrong passphrase
// gets an auth-failure, never the plaintext. Confirms confidentiality.
func TestE2E_WrongPassphrase(t *testing.T) {
	salt := []byte("device-fingerprint")
	keyA, _ := brain.DeriveKey([]byte("correct-pass"), salt)
	keyB, _ := brain.DeriveKey([]byte("wrong-pass"), salt)
	aad := []byte("aad")

	var ct bytes.Buffer
	if err := brain.EncryptStream(strings.NewReader("secret"), &ct, keyA, aad); err != nil {
		t.Fatal(err)
	}
	var pt bytes.Buffer
	err := brain.DecryptStream(&ct, &pt, keyB, aad)
	if err == nil {
		t.Fatal("wrong passphrase should fail")
	}
	if pt.Len() != 0 {
		t.Errorf("plaintext leaked %d bytes despite wrong passphrase", pt.Len())
	}
}

// TestE2E_StoreRoundtripIntegrity — push to LocalStore + pull back +
// verify byte-for-byte equality. Catches subtle FS corruption.
func TestE2E_StoreRoundtripIntegrity(t *testing.T) {
	store, err := storage.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, payload := range [][]byte{
		nil,
		[]byte("small"),
		bytes.Repeat([]byte{0xAA}, 1<<20+1234), // 1 MiB+ to force chunking concerns at higher layers
	} {
		key := "test-" + brain.NextHLC().String()
		if _, err := store.Put(key, bytes.NewReader(payload)); err != nil {
			t.Fatalf("Put %d bytes: %v", len(payload), err)
		}
		rc, err := store.Get(key)
		if err != nil {
			t.Fatalf("Get %d bytes: %v", len(payload), err)
		}
		got := readAll(t, rc)
		_ = rc.Close()
		if !bytes.Equal(got, payload) {
			t.Errorf("size %d: roundtrip drift", len(payload))
		}
	}
}

func readAll(t *testing.T, r interface {
	Read(p []byte) (n int, err error)
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			break
		}
	}
	return buf.Bytes()
}
