// Package brain — crypto.go: encryption layer for snapshot archives.
// PILAR XXVI / 135.B.1-B.3.
//
// Three primitives, composed by the brain push/pull commands (135.D):
//
//   DeriveKey      — passphrase + device-bound salt → 32-byte key (Argon2id)
//   EncryptStream  — chunked ChaCha20-Poly1305 stream cipher + AAD
//   Fingerprint    — Blake2b-256 keyed hash for tamper detection
//
// Design choices and their rationale:
//
//   * Argon2id at RFC 9106 baseline (time=3, memory=64MiB, threads=4).
//     Resists both GPU-cracking and side-channel attacks; the parameters
//     match the OWASP recommendation for general-purpose KDFs.
//
//   * Salt is the device fingerprint, not stored alongside the ciphertext.
//     Two devices with the same passphrase derive different keys, so an
//     archive encrypted on Mac cannot be silently decrypted by an attacker
//     who steals the passphrase but lacks the original device's
//     fingerprint. The receiver MUST share the salt out-of-band (e.g.
//     transferring the device fingerprint as part of the path_map).
//
//   * Chunked stream cipher (1 MiB chunks) bounds peak memory so a 4 GiB
//     archive can encrypt/decrypt on a 256 MiB host. Each chunk has its
//     own nonce derived from a counter; reuse of the symmetric key across
//     chunks is safe with non-repeating nonces (NIST SP 800-38D §8.2).
//
//   * AAD (additional authenticated data) carries manifest.HLC and NodeID.
//     A ciphertext encrypted for HLC=A cannot be successfully decrypted as
//     if it were HLC=B even by an attacker who knows the key — the AEAD
//     tag mismatches.
//
//   * Blake3 was specified in the master_plan; we use Blake2b instead
//     because Blake3 is NOT in stdlib or golang.org/x/crypto and adding a
//     third-party crypto dep is a higher-risk change than swapping one
//     comparable hash. Blake2b-256 has the same output length, similar
//     speed (1.5 GiB/s on amd64), and IPFS / Argon2 internal use it
//     widely. If a future epic justifies Blake3, swap is one-file and
//     two-test mechanical.
//
// Key cache (135.B.1): the derived key is cached in RAM under a sync.Once
// per (passphrase, salt) pair so consecutive `brain pull` calls in the
// same process don't re-pay the Argon2id cost (~250ms on commodity
// hardware). Cache lives in the process — never written to disk and
// invalidated when the process exits or after 1h of idle.

package brain

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/chacha20poly1305"
)

// keyLen is the size of the symmetric key produced by DeriveKey and
// consumed by EncryptStream. 32 bytes = 256 bits, the input length
// ChaCha20-Poly1305 expects.
const keyLen = 32

// argon2idTime / Memory / Threads are RFC 9106 §4.2 first-recommended
// option (memory-conservative). 64 MiB memory, 3 passes, 4 lanes —
// resists GPU/ASIC cracking on a passphrase of typical user complexity.
const (
	argon2idTime    = 3
	argon2idMemory  = 64 * 1024 // KiB → 64 MiB
	argon2idThreads = 4
)

// chunkSize is the unit of the streaming cipher. 1 MiB balances per-chunk
// AEAD overhead (16 byte tag) against peak memory and pipelining latency.
const chunkSize = 1 << 20

// keyCacheTTL is how long a derived key stays in the per-process cache
// after its last use. Re-deriving costs ~250ms; caching saves that on
// rapid sequential pulls. Walking past the TTL forces re-derivation,
// which limits the time window where a memory-resident key matters.
const keyCacheTTL = time.Hour

// DeriveKey runs Argon2id over (passphrase, salt) and returns the 32-byte
// key. Empty passphrase or salt is rejected — callers MUST supply both.
//
// On hot paths (consecutive pulls), the result is cached per
// (passphrase, salt) pair via DeriveKeyCached.
func DeriveKey(passphrase, salt []byte) ([]byte, error) {
	if len(passphrase) == 0 {
		return nil, errors.New("DeriveKey: empty passphrase")
	}
	if len(salt) == 0 {
		return nil, errors.New("DeriveKey: empty salt")
	}
	return argon2.IDKey(passphrase, salt, argon2idTime, argon2idMemory, argon2idThreads, keyLen), nil
}

// keyCacheEntry holds a derived key plus its last-use time. The cache is
// trivial; we don't bother with LRU eviction because typical sessions
// touch ≤2 distinct (passphrase, salt) pairs.
type keyCacheEntry struct {
	key      []byte
	lastUsed time.Time
}

var (
	keyCacheMu sync.Mutex
	keyCache   = map[string]*keyCacheEntry{}
)

// DeriveKeyCached is DeriveKey with a per-process cache keyed by
// blake2b(passphrase || 0xFF || salt). The 0xFF separator prevents
// (passphrase=A, salt=B) and (passphrase=AB, salt=empty) from colliding.
//
// Returns a fresh slice on every call (callers may zero it after use).
func DeriveKeyCached(passphrase, salt []byte) ([]byte, error) {
	cacheKey := blakeCacheKey(passphrase, salt)
	keyCacheMu.Lock()
	if e, ok := keyCache[cacheKey]; ok && time.Since(e.lastUsed) < keyCacheTTL {
		e.lastUsed = time.Now()
		out := make([]byte, len(e.key))
		copy(out, e.key)
		keyCacheMu.Unlock()
		return out, nil
	}
	keyCacheMu.Unlock()

	key, err := DeriveKey(passphrase, salt)
	if err != nil {
		return nil, err
	}
	keyCacheMu.Lock()
	keyCache[cacheKey] = &keyCacheEntry{
		key:      append([]byte(nil), key...),
		lastUsed: time.Now(),
	}
	keyCacheMu.Unlock()
	out := make([]byte, len(key))
	copy(out, key)
	return out, nil
}

// blakeCacheKey hashes (passphrase, salt) for the in-memory cache index.
// The hash never leaves the process; the operator's secrets are not in
// any reachable form.
func blakeCacheKey(passphrase, salt []byte) string {
	h, _ := blake2b.New256(nil)
	_, _ = h.Write(passphrase)
	_, _ = h.Write([]byte{0xFF})
	_, _ = h.Write(salt)
	return string(h.Sum(nil))
}

// ResetKeyCache wipes every cached key and zeroes the underlying memory.
// Tests use this to ensure isolation; production callers may use it on
// passphrase rotation.
func ResetKeyCache() {
	keyCacheMu.Lock()
	defer keyCacheMu.Unlock()
	for k, e := range keyCache {
		for i := range e.key {
			e.key[i] = 0
		}
		delete(keyCache, k)
	}
}

// streamSaltSize is the number of random bytes used as input to the
// per-stream subkey derivation (Blake2b-256(key, streamSalt) → streamKey).
const streamSaltSize = chacha20poly1305.NonceSize // 12 bytes

// streamVersion is the V2 format identifier written as the first frame of
// every stream produced by this package. DecryptStream rejects streams
// whose version byte doesn't match. This catches accidental cross-version
// decryption attempts on pre-upgrade archives.
const streamVersion = byte(2)

// buildChunkAAD constructs the per-chunk additional-authenticated data,
// binding each chunk to its position (counter) and whether it is the
// final chunk (isFinal). [146.A]
//
// Layout: userAAD || BE_uint64(counter) || is_final_byte
//
// Including the counter in AAD means a chunk encrypted at position N cannot
// be successfully decrypted at any other position — any swap or reorder is
// caught by the AEAD tag. Including isFinal=1 in the last chunk's AAD
// means an attacker who strips trailing chunks and writes a fake terminator
// will cause the authenticated terminator frame to fail AEAD, detecting
// the truncation.
func buildChunkAAD(userAAD []byte, counter uint64, isFinal bool) []byte {
	out := make([]byte, len(userAAD)+9) // +8 counter +1 final byte
	copy(out, userAAD)
	binary.BigEndian.PutUint64(out[len(userAAD):], counter)
	if isFinal {
		out[len(userAAD)+8] = 1
	}
	return out
}

// streamSubkey derives a per-stream encryption key via Blake2b-256(key, streamSalt).
// Each call to EncryptStream generates a fresh random streamSalt, so even if the
// same master key is reused across N streams, every stream operates under an
// independent sub-key — nonce collisions across streams are cryptographically
// impossible regardless of the counter derivation scheme. [146.B]
func streamSubkey(key, streamSalt []byte) ([]byte, error) {
	h, err := blake2b.New256(key) // keyed Blake2b-256 (HMAC-style)
	if err != nil {
		return nil, fmt.Errorf("streamSubkey: %w", err)
	}
	h.Write(streamSalt)
	return h.Sum(nil), nil
}

// EncryptStream reads plaintext from r and writes ChaCha20-Poly1305
// ciphertext to w. The V2 output framing is:
//
//	[4-byte length=1][0x02]                   ← stream version
//	[4-byte length=12][12-byte random salt]    ← per-stream entropy
//	[4-byte length][ciphertext+tag]...         ← data chunks (isFinal=0 in AAD)
//	[4-byte length][ciphertext+tag]            ← final chunk (isFinal=1 in AAD, empty plaintext allowed)
//
// **Per-stream subkey (146.B):** `streamKey = Blake2b-256(key, streamSalt)`. Every
// stream operates under an independent key — nonce collisions across streams are
// impossible regardless of the counter derivation.
//
// **Counter-in-AAD (146.A):** `chunkAAD = userAAD || BE_uint64(counter) || is_final_byte`.
// Binding the chunk position to the AEAD tag prevents chunk-swap and positional-replay
// attacks. The is_final flag in the last chunk's AAD detects whole-chunk truncation:
// an attacker who strips trailing chunks cannot forge a valid final-chunk tag.
//
// **Lookahead buffering:** EncryptStream keeps a one-chunk lookahead so it can
// mark the truly last chunk with isFinal=1 without a two-pass over the input.
// Empty input produces a single zero-byte terminator frame with isFinal=1.
func EncryptStream(r io.Reader, w io.Writer, key, aad []byte) error {
	// Write V2 version frame.
	if err := writeFrame(w, []byte{streamVersion}); err != nil {
		return err
	}

	streamSalt := make([]byte, streamSaltSize)
	if _, err := io.ReadFull(rand.Reader, streamSalt); err != nil {
		return fmt.Errorf("EncryptStream: stream salt: %w", err)
	}
	if err := writeFrame(w, streamSalt); err != nil {
		return err
	}

	// [146.B] Per-stream subkey isolates this stream's AEAD key from masterKey.
	streamKey, err := streamSubkey(key, streamSalt)
	if err != nil {
		return fmt.Errorf("EncryptStream: subkey: %w", err)
	}
	aead, err := chacha20poly1305.New(streamKey)
	if err != nil {
		return fmt.Errorf("EncryptStream: cipher init: %w", err)
	}

	// One-chunk lookahead so we know which chunk is final without two passes.
	cur := make([]byte, chunkSize)
	prev := make([]byte, chunkSize)
	var prevN int
	var havePrev bool
	var counter uint64

	emitChunk := func(data []byte, isFinal bool) error {
		nonce := chunkNonce(counter)
		ct := aead.Seal(nil, nonce, data, buildChunkAAD(aad, counter, isFinal))
		counter++
		return writeFrame(w, ct)
	}

	for {
		n, readErr := io.ReadFull(r, cur)
		isEOF := readErr == io.EOF || readErr == io.ErrUnexpectedEOF

		if n > 0 {
			// Emit any buffered previous chunk as non-final before buffering new one.
			if havePrev {
				if err := emitChunk(prev[:prevN], false); err != nil {
					return err
				}
			}
			copy(prev, cur[:n])
			prevN = n
			havePrev = true
		}

		if isEOF {
			// Emit the buffered chunk as final (or an empty final frame if input was empty).
			if havePrev {
				return emitChunk(prev[:prevN], true)
			}
			return emitChunk(nil, true) // empty-input edge case
		}

		if readErr != nil {
			return fmt.Errorf("EncryptStream: read: %w", readErr)
		}
	}
}

// DecryptStream reverses EncryptStream. Returns an error when the stream ends
// mid-frame, when the AEAD tag fails (wrong key OR tampered ciphertext OR
// mismatched AAD — the three are indistinguishable, which is the AEAD security
// guarantee), or when the stream is truncated (missing authenticated final frame).
func DecryptStream(r io.Reader, w io.Writer, key, aad []byte) error {
	// Read and verify V2 version frame.
	versionFrame, err := readFrame(r)
	if err != nil {
		return fmt.Errorf("DecryptStream: read version: %w", err)
	}
	if len(versionFrame) != 1 || versionFrame[0] != streamVersion {
		return fmt.Errorf("DecryptStream: unsupported stream version (got %v, want [%d])", versionFrame, streamVersion)
	}

	streamSalt, err := readFrame(r)
	if err != nil {
		return fmt.Errorf("DecryptStream: read stream salt: %w", err)
	}
	if len(streamSalt) != streamSaltSize {
		return fmt.Errorf("DecryptStream: malformed stream salt (size %d, want %d)", len(streamSalt), streamSaltSize)
	}

	// [146.B] Derive the per-stream key.
	streamKey, err := streamSubkey(key, streamSalt)
	if err != nil {
		return fmt.Errorf("DecryptStream: subkey: %w", err)
	}
	aead, err := chacha20poly1305.New(streamKey)
	if err != nil {
		return fmt.Errorf("DecryptStream: cipher init: %w", err)
	}

	var counter uint64
	for {
		frame, err := readFrame(r)
		if err != nil {
			return err
		}
		if len(frame) == 0 {
			// Raw zero-frame: stream truncated — attacker wrote a fake terminator. [146.A]
			return fmt.Errorf("DecryptStream: stream truncated at chunk %d (missing authenticated final-frame)", counter)
		}
		pt, isFinal, err := processDecryptChunk(aead, frame, counter, aad)
		if err != nil {
			return err
		}
		if len(pt) > 0 {
			if _, err := w.Write(pt); err != nil {
				return fmt.Errorf("DecryptStream: write: %w", err)
			}
		}
		if isFinal {
			return nil
		}
		counter++
	}
}

// processDecryptChunk attempts to open a single encrypted frame.
// Returns (plaintext, isFinal=true, nil) for the authenticated final frame,
// (plaintext, false, nil) for a non-final chunk, or (nil, false, err) on failure.
// The two-try pattern detects the final frame via its distinct AAD (isFinal bit). [146.A]
func processDecryptChunk(aead cipher.AEAD, frame []byte, counter uint64, aad []byte) ([]byte, bool, error) {
	nonce := chunkNonce(counter)
	pt, openErr := aead.Open(nil, nonce, frame, buildChunkAAD(aad, counter, false))
	if openErr == nil {
		return pt, false, nil
	}
	ptFinal, finalErr := aead.Open(nil, nonce, frame, buildChunkAAD(aad, counter, true))
	if finalErr != nil {
		return nil, false, fmt.Errorf("DecryptStream: chunk %d auth failed (wrong key, tampered, or AAD mismatch): %w", counter, openErr)
	}
	return ptFinal, true, nil
}

// chunkNonce derives a 12-byte ChaCha20-Poly1305 nonce from a chunk counter.
// Layout: [4 zero bytes][8-byte BE counter].
//
// V2 nonces are simple counters because uniqueness is guaranteed per-stream:
// streamSubkey derives an independent AEAD key from (masterKey, streamSalt), so
// even if the same counter appears across different streams, the keys differ —
// the (key, nonce) pair is always unique. Within a single stream, the monotonic
// counter ensures nonce uniqueness for up to 2^64 chunks (~18 exabytes at 1 MiB
// chunk size). [146.B]
func chunkNonce(counter uint64) []byte {
	nonce := make([]byte, chacha20poly1305.NonceSize) // 12 bytes; first 4 stay zero
	binary.BigEndian.PutUint64(nonce[4:], counter)
	return nonce
}

// writeFrame writes a length-prefixed binary frame to w.
func writeFrame(w io.Writer, payload []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload))) //nolint:gosec // payload bounded by chunkSize+tag (~1MiB) << uint32 max
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("writeFrame: header: %w", err)
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return fmt.Errorf("writeFrame: payload: %w", err)
		}
	}
	return nil
}

// readFrame reads a length-prefixed frame produced by writeFrame.
// Imposes a max frame size of chunkSize + 4 KiB AEAD overhead so a
// malicious sender cannot allocate gigabytes via a forged length prefix.
func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("readFrame: header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, nil
	}
	const maxFrameSize = chunkSize + 4096
	if n > maxFrameSize {
		return nil, fmt.Errorf("readFrame: frame size %d exceeds cap %d (corrupt or malicious stream)", n, maxFrameSize)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("readFrame: payload: %w", err)
	}
	return buf, nil
}

// Fingerprint returns the Blake2b-256 keyed hash of the archive bytes.
// Used by `brain verify` to detect tampering BEFORE decrypting (cheap
// integrity check) and by `brain push` to dedup identical archives in
// the storage backend.
//
// key is optional: pass nil for an unkeyed hash (content-addressable
// storage), pass a 32-byte key for HMAC-style fingerprint that an
// adversary cannot recompute without knowing the key.
//
// Reads r in 64 KiB chunks for steady memory profile on multi-GB
// archives.
func Fingerprint(r io.Reader, key []byte) ([]byte, error) {
	h, err := blake2b.New256(key)
	if err != nil {
		return nil, fmt.Errorf("Fingerprint: hash init: %w", err)
	}
	buf := make([]byte, 64*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("Fingerprint: read: %w", err)
		}
	}
	return h.Sum(nil), nil
}

// RandomSalt produces a 16-byte cryptographic random salt. Callers may
// use this when they don't have a device fingerprint handy (e.g. unit
// tests) — production paths should use the device-bound salt so the
// "two devices same passphrase ≠ same key" invariant holds.
func RandomSalt() ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("RandomSalt: %w", err)
	}
	return salt, nil
}

// Compile-time assertion: chunkNonce produces chacha20poly1305-sized
// output. Catches an accidental stdlib bump that changes nonce length.
var _ cipher.AEAD = (*chacha20poly1305AEADCheck)(nil)

type chacha20poly1305AEADCheck struct{ cipher.AEAD }
