// identity_key.go — Ed25519 device identity for Brain Portable. [146.J]
//
// Every neoanvil installation generates a unique Ed25519 keypair stored in
// ~/.neo/identity.key (PEM PKCS8, 0600). The public-key fingerprint
// ("dev:<24 hex>") serves as the cryptographic root of trust for the
// workspace canonical_id fallback, replacing the path-derived hash that
// was fragile across renames and cross-machine syncs.
//
// The key is lazily generated on first access, never committed to any
// repository, and never leaves the operator's home directory.

package brain

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	// IdentityKeyFile is the path inside ~/.neo/ where the Ed25519 private
	// key lives. The companion public key is derived on load — we never
	// store the public key separately to avoid sync issues.
	IdentityKeyFile = ".neo/identity.key"

	// SourceDeviceKey is the CanonicalSource value used when ResolveCanonicalID
	// falls back to the Ed25519 device fingerprint. Replaces SourcePathHash.
	SourceDeviceKey CanonicalSource = "device_key"
)

var (
	deviceKeyOnce        sync.Once
	cachedDeviceKey      ed25519.PrivateKey
	cachedDeviceKeyError error
)

// IdentityKeyPath returns the absolute path to the Ed25519 private key file.
// Returns "" when the home directory cannot be resolved.
func IdentityKeyPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, IdentityKeyFile)
}

// LoadOrCreateDeviceKey returns the Ed25519 private key for this installation,
// generating and persisting a new one if none exists yet.
//
// The key file is written with mode 0600 under ~/.neo/identity.key. The
// function is goroutine-safe and memoizes the result for the process lifetime.
func LoadOrCreateDeviceKey() (ed25519.PrivateKey, error) {
	deviceKeyOnce.Do(func() {
		path := IdentityKeyPath()
		if path == "" {
			cachedDeviceKeyError = fmt.Errorf("cannot resolve home dir for identity key")
			return
		}
		key, err := loadKeyFile(path)
		if os.IsNotExist(err) {
			key, err = generateAndSaveKey(path)
		}
		cachedDeviceKey = key
		cachedDeviceKeyError = err
	})
	return cachedDeviceKey, cachedDeviceKeyError
}

// DeviceKeyFingerprint returns a stable identifier derived from an Ed25519
// public key. Format: "dev:<24 hex chars of sha256(pubkey)[:12]>".
func DeviceKeyFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "dev:" + hex.EncodeToString(sum[:12])
}

func loadKeyFile(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304-CLI-CONSENT: ~/.neo/identity.key is operator's own key file
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("identity key %s: no PEM block found", path)
	}
	raw, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("identity key %s: parse PKCS8: %w", path, err)
	}
	key, ok := raw.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("identity key %s: expected Ed25519, got %T", path, raw)
	}
	return key, nil
}

func generateAndSaveKey(path string) (ed25519.PrivateKey, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create .neo dir: %w", err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate identity key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal identity key: %w", err)
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	// Write atomically to prevent partial files.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".identity-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create identity key tmp: %w", err)
	}
	tmpName := tmp.Name()
	if err := pem.Encode(tmp, block); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("write identity key: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("chmod identity key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("close identity key tmp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("install identity key: %w", err)
	}
	return priv, nil
}
