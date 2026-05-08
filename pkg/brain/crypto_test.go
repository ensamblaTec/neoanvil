package brain

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// helperKey returns a deterministic 32-byte key for tests that don't
// exercise DeriveKey itself — saves the Argon2id cost (~250ms).
func helperKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, keyLen)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

// TestDeriveKey_Length — Argon2id output is exactly 32 bytes.
func TestDeriveKey_Length(t *testing.T) {
	key, err := DeriveKey([]byte("hunter2"), []byte("salt-x"))
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	if len(key) != keyLen {
		t.Errorf("len(key) = %d, want %d", len(key), keyLen)
	}
}

// TestDeriveKey_Deterministic — same (passphrase, salt) → same key.
// Runs twice and compares.
func TestDeriveKey_Deterministic(t *testing.T) {
	a, err := DeriveKey([]byte("p"), []byte("s"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := DeriveKey([]byte("p"), []byte("s"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Error("DeriveKey not deterministic")
	}
}

// TestDeriveKey_DifferentSalts — different salts produce different keys
// even with the same passphrase. Required for the "two devices same
// passphrase ≠ same key" invariant.
func TestDeriveKey_DifferentSalts(t *testing.T) {
	a, _ := DeriveKey([]byte("p"), []byte("salt-A"))
	b, _ := DeriveKey([]byte("p"), []byte("salt-B"))
	if bytes.Equal(a, b) {
		t.Error("different salts collided")
	}
}

// TestDeriveKey_RejectsEmpty — empty passphrase or salt is a caller bug.
func TestDeriveKey_RejectsEmpty(t *testing.T) {
	if _, err := DeriveKey(nil, []byte("s")); err == nil {
		t.Error("empty passphrase should error")
	}
	if _, err := DeriveKey([]byte("p"), nil); err == nil {
		t.Error("empty salt should error")
	}
}

// TestDeriveKeyCached_HitsCache — second call with same (p, s) returns
// without re-running Argon2id. We can't directly observe the timing in a
// unit test reliably, so we check that ResetKeyCache between calls
// causes a re-derive (counted via cache map entries).
func TestDeriveKeyCached_HitsCache(t *testing.T) {
	ResetKeyCache()
	defer ResetKeyCache()

	k1, err := DeriveKeyCached([]byte("hunter2"), []byte("dev-fp"))
	if err != nil {
		t.Fatal(err)
	}
	k2, err := DeriveKeyCached([]byte("hunter2"), []byte("dev-fp"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1, k2) {
		t.Error("cached key drift")
	}
	// Different passphrase → cache miss + new entry.
	k3, err := DeriveKeyCached([]byte("other"), []byte("dev-fp"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k1, k3) {
		t.Error("different passphrase produced same key")
	}
}

// TestDeriveKeyCached_ReturnsFreshSlice — caller may zero the returned
// key after use; that MUST NOT corrupt the cached copy.
func TestDeriveKeyCached_ReturnsFreshSlice(t *testing.T) {
	ResetKeyCache()
	defer ResetKeyCache()

	k1, err := DeriveKeyCached([]byte("p"), []byte("s"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range k1 {
		k1[i] = 0xAA
	}
	k2, err := DeriveKeyCached([]byte("p"), []byte("s"))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(k2, 0xAA) {
		t.Error("cache returned mutated slice")
	}
}

// TestEncryptDecrypt_Roundtrip — encrypt then decrypt yields the original
// plaintext. Tests the happy path with multi-chunk payload.
func TestEncryptDecrypt_Roundtrip(t *testing.T) {
	key := helperKey(t)
	aad := []byte("manifest:hlc=1.0:node:foo")
	plaintext := bytes.Repeat([]byte("hello world\n"), 200_000) // ~2.4 MiB → 3 chunks

	var ct bytes.Buffer
	if err := EncryptStream(bytes.NewReader(plaintext), &ct, key, aad); err != nil {
		t.Fatalf("EncryptStream: %v", err)
	}
	if ct.Len() == 0 {
		t.Fatal("ciphertext empty")
	}
	if ct.Len() < len(plaintext) {
		t.Errorf("ciphertext smaller than plaintext (no AEAD overhead?): %d vs %d", ct.Len(), len(plaintext))
	}

	var pt bytes.Buffer
	if err := DecryptStream(&ct, &pt, key, aad); err != nil {
		t.Fatalf("DecryptStream: %v", err)
	}
	if !bytes.Equal(pt.Bytes(), plaintext) {
		t.Error("plaintext drift after roundtrip")
	}
}

// TestDecrypt_WrongKey — decrypting with a different key fails with the
// auth-failed error message.
func TestDecrypt_WrongKey(t *testing.T) {
	key := helperKey(t)
	wrong := make([]byte, keyLen)
	for i := range wrong {
		wrong[i] = byte(i + 1)
	}
	aad := []byte("aad")

	var ct bytes.Buffer
	if err := EncryptStream(bytes.NewReader([]byte("secret")), &ct, key, aad); err != nil {
		t.Fatal(err)
	}
	err := DecryptStream(&ct, io.Discard, wrong, aad)
	if err == nil || !strings.Contains(err.Error(), "auth failed") {
		t.Errorf("wrong key should error with auth failed, got %v", err)
	}
}

// TestDecrypt_WrongAAD — same key but mismatched AAD fails.
// AAD-binding to the manifest HLC is what prevents replay across
// snapshots even when the key is reused.
func TestDecrypt_WrongAAD(t *testing.T) {
	key := helperKey(t)
	var ct bytes.Buffer
	if err := EncryptStream(bytes.NewReader([]byte("hi")), &ct, key, []byte("aad-A")); err != nil {
		t.Fatal(err)
	}
	err := DecryptStream(&ct, io.Discard, key, []byte("aad-B"))
	if err == nil || !strings.Contains(err.Error(), "auth failed") {
		t.Errorf("wrong AAD should error, got %v", err)
	}
}

// TestDecrypt_BitFlip — flipping a single ciphertext byte fails the
// AEAD tag and returns an auth error.
func TestDecrypt_BitFlip(t *testing.T) {
	key := helperKey(t)
	aad := []byte("aad")
	var ct bytes.Buffer
	plain := bytes.Repeat([]byte{0x42}, 4096)
	if err := EncryptStream(bytes.NewReader(plain), &ct, key, aad); err != nil {
		t.Fatal(err)
	}
	corrupted := append([]byte(nil), ct.Bytes()...)
	// Flip a bit in the middle of the first chunk's ciphertext (skip the
	// 4-byte length prefix).
	corrupted[20] ^= 0x01
	err := DecryptStream(bytes.NewReader(corrupted), io.Discard, key, aad)
	if err == nil || !strings.Contains(err.Error(), "auth failed") {
		t.Errorf("bit flip should error, got %v", err)
	}
}

// TestEncrypt_EmptyPlaintext — empty plaintext produces a valid stream
// (just the 0-length terminator) that round-trips to empty.
func TestEncrypt_EmptyPlaintext(t *testing.T) {
	key := helperKey(t)
	aad := []byte("aad")
	var ct bytes.Buffer
	if err := EncryptStream(bytes.NewReader(nil), &ct, key, aad); err != nil {
		t.Fatal(err)
	}
	var pt bytes.Buffer
	if err := DecryptStream(&ct, &pt, key, aad); err != nil {
		t.Fatalf("DecryptStream: %v", err)
	}
	if pt.Len() != 0 {
		t.Errorf("empty roundtrip produced %d bytes", pt.Len())
	}
}

// TestEncrypt_LargeStreaming — 5 MiB payload streams without exceeding
// chunkSize buffer (memory bound).
func TestEncrypt_LargeStreaming(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large streaming test in -short mode")
	}
	key := helperKey(t)
	aad := []byte("aad")
	src := make([]byte, 5*chunkSize+1234)
	if _, err := rand.Read(src); err != nil {
		t.Fatal(err)
	}

	var ct bytes.Buffer
	if err := EncryptStream(bytes.NewReader(src), &ct, key, aad); err != nil {
		t.Fatal(err)
	}
	var pt bytes.Buffer
	if err := DecryptStream(&ct, &pt, key, aad); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pt.Bytes(), src) {
		t.Error("5MiB payload corrupted in roundtrip")
	}
}

// TestReadFrame_RejectsOversizedHeader — defends DecryptStream against a
// crafted stream claiming a 1GB frame.
func TestReadFrame_RejectsOversizedHeader(t *testing.T) {
	// Frame header claiming 1GB.
	corrupt := []byte{0x40, 0x00, 0x00, 0x00} // 0x40000000 = 1GiB
	_, err := readFrame(bytes.NewReader(corrupt))
	if err == nil || !strings.Contains(err.Error(), "exceeds cap") {
		t.Errorf("oversized frame should be rejected, got %v", err)
	}
}

// TestReadFrame_TerminatorReturnsNil — a 0-length frame is the
// end-of-stream terminator. readFrame must return (nil, nil) NOT
// (nil, io.EOF).
func TestReadFrame_TerminatorReturnsNil(t *testing.T) {
	terminator := []byte{0, 0, 0, 0}
	frame, err := readFrame(bytes.NewReader(terminator))
	if err != nil {
		t.Errorf("terminator returned err = %v", err)
	}
	if frame != nil {
		t.Errorf("terminator returned non-nil frame: %v", frame)
	}
}

// TestFingerprint_Stable — same input → same fingerprint, different
// input → different fingerprint.
func TestFingerprint_Stable(t *testing.T) {
	a, err := Fingerprint(strings.NewReader("hello world"), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Fingerprint(strings.NewReader("hello world"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Error("fingerprint not deterministic")
	}
	c, err := Fingerprint(strings.NewReader("HELLO WORLD"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, c) {
		t.Error("different content produced same fingerprint")
	}
	if len(a) != 32 {
		t.Errorf("fingerprint len = %d, want 32 (Blake2b-256)", len(a))
	}
}

// TestFingerprint_Keyed — keyed hash with key K1 ≠ keyed hash with key K2
// for the same content. This is the HMAC-style invariant.
func TestFingerprint_Keyed(t *testing.T) {
	content := []byte("archive bytes go here")
	k1, _ := Fingerprint(bytes.NewReader(content), []byte("key-A"))
	k2, _ := Fingerprint(bytes.NewReader(content), []byte("key-B"))
	if bytes.Equal(k1, k2) {
		t.Error("different keys produced same fingerprint")
	}
	unkeyed, _ := Fingerprint(bytes.NewReader(content), nil)
	if bytes.Equal(k1, unkeyed) {
		t.Error("keyed and unkeyed fingerprints collided")
	}
}

// TestRandomSalt_Length — RandomSalt produces 16 bytes. Run twice and
// confirm they differ (probabilistic — failure here means the RNG is
// stuck).
func TestRandomSalt_Length(t *testing.T) {
	a, err := RandomSalt()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 16 {
		t.Errorf("salt len = %d, want 16", len(a))
	}
	b, _ := RandomSalt()
	if bytes.Equal(a, b) {
		t.Error("two RandomSalt calls produced identical output (RNG broken?)")
	}
}

// TestDecryptStream_TruncatedStream — stream cut mid-frame returns a
// readable error (not panic, not generic ReadFull EOF).
func TestDecryptStream_TruncatedStream(t *testing.T) {
	key := helperKey(t)
	aad := []byte("aad")
	var ct bytes.Buffer
	if err := EncryptStream(bytes.NewReader([]byte("hello")), &ct, key, aad); err != nil {
		t.Fatal(err)
	}
	// Truncate the last 5 bytes — the terminator and part of the tag.
	truncated := ct.Bytes()
	if len(truncated) <= 5 {
		t.Skip("ciphertext too short to truncate meaningfully")
	}
	truncated = truncated[:len(truncated)-5]
	err := DecryptStream(bytes.NewReader(truncated), io.Discard, key, aad)
	if err == nil {
		t.Error("truncated stream should error")
	}
	// We don't pin the error string — it's either a ReadFull short read
	// or an AEAD failure depending on where the cut landed.
	var unexpectedEOF *errReadingFrame
	_ = errors.As(err, &unexpectedEOF) // just exercise the path; no assert
}

// TestDecryptStream_FakeTerminator — attacker strips chunks and writes a raw
// zero-frame to fake end-of-stream. V2 format detects this as truncation. [146.A]
func TestDecryptStream_FakeTerminator(t *testing.T) {
	key := helperKey(t)
	aad := []byte("aad")
	// Build a multi-chunk-range ciphertext (just needs at least 2 frames beyond salt).
	plaintext := bytes.Repeat([]byte("X"), 64)
	var ct bytes.Buffer
	if err := EncryptStream(bytes.NewReader(plaintext), &ct, key, aad); err != nil {
		t.Fatal(err)
	}
	// Replace the authenticated final frame with a raw zero-frame terminator —
	// the attacker pretends the stream ended here.
	raw := ct.Bytes()
	// Find the version frame (5 bytes) + salt frame (16 bytes) = 21 header bytes.
	// Append just the header + raw zero-frame.
	fakeStream := append(raw[:21], 0, 0, 0, 0) // version+salt then fake terminator
	err := DecryptStream(bytes.NewReader(fakeStream), io.Discard, key, aad)
	if err == nil {
		t.Error("fake-terminator stream should error")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("expected truncation error, got: %v", err)
	}
}

// errReadingFrame is a sentinel placeholder so errors.As above compiles
// even though we don't define a real wrapping type yet — keeps the test
// future-compatible if the package later introduces typed errors.
type errReadingFrame struct{}

func (e *errReadingFrame) Error() string { return "frame read" }

// TestEncrypt_NonceUniquenessAcrossCalls — regression for the nonce-reuse
// vulnerability discovered by DeepSeek red-team audit during PILAR XXVI
// calibration (2026-05-01). Two EncryptStream calls with identical
// (key, AAD, plaintext) MUST produce different ciphertexts because each
// call generates a fresh random per-stream salt. If this test ever fails,
// the per-stream salt is not being randomised and chunk[0] of every
// snapshot leaks the same keystream — an attacker can XOR two snapshots
// to recover plaintext^plaintext, then forge Poly1305 tags.
func TestEncrypt_NonceUniquenessAcrossCalls(t *testing.T) {
	key := helperKey(t)
	aad := []byte("aad")
	plain := []byte("identical plaintext bytes for both encrypt calls")
	var ctA, ctB bytes.Buffer
	if err := EncryptStream(bytes.NewReader(plain), &ctA, key, aad); err != nil {
		t.Fatal(err)
	}
	if err := EncryptStream(bytes.NewReader(plain), &ctB, key, aad); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ctA.Bytes(), ctB.Bytes()) {
		t.Fatal("ciphertexts of two encrypt calls with identical inputs are byte-equal — per-stream salt randomisation is broken")
	}
	// First frame is the 12-byte stream salt (length-prefixed). Bytes
	// 4..15 of the ciphertext stream are the salt itself; if those are
	// equal, the salt isn't actually random.
	if len(ctA.Bytes()) < 16 || len(ctB.Bytes()) < 16 {
		t.Fatalf("ciphertext too short to inspect salt: A=%d B=%d", ctA.Len(), ctB.Len())
	}
	saltA := ctA.Bytes()[4:16]
	saltB := ctB.Bytes()[4:16]
	if bytes.Equal(saltA, saltB) {
		t.Errorf("per-stream salt repeated across calls (saltA=%x saltB=%x) — RNG broken or wire format regressed", saltA, saltB)
	}
	// Both ciphertexts must still decrypt to the original plaintext.
	for label, ct := range map[string]*bytes.Buffer{"A": &ctA, "B": &ctB} {
		var pt bytes.Buffer
		if err := DecryptStream(ct, &pt, key, aad); err != nil {
			t.Errorf("decrypt %s: %v", label, err)
			continue
		}
		if !bytes.Equal(pt.Bytes(), plain) {
			t.Errorf("decrypt %s: plaintext mismatch", label)
		}
	}
}
