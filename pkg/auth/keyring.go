package auth

// keyring.go — Encrypted credential backend via github.com/99designs/keyring.
// PILAR XXIII / Épica 124.1.
//
// The default credential store remains the file-based ~/.neo/credentials.json
// (0600). KeyringBackend is an opt-in alternative that delegates to the OS
// native secret store: Keychain on macOS, libsecret/GNOME Keyring on Linux,
// Credential Manager on Windows. When no native backend is available, the
// 99designs/keyring library falls back to an encrypted file (passphrase
// protected) under FileDir.
//
// Both backends implement Backend so callers (cmd/neo login, plugin pool)
// can swap implementations transparently.

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/99designs/keyring"
)

const (
	defaultKeyringServiceName = "neoanvil"
	keyringItemKey            = "credentials"
)

// Backend abstracts credential storage so callers can switch between the
// on-disk JSON file and an OS-native keyring without changing call sites.
type Backend interface {
	Save(*Credentials) error
	Load() (*Credentials, error)
}

// FileBackend wraps the existing Save/Load functions for symmetry with
// KeyringBackend. The package-level functions remain available for callers
// that prefer the procedural style.
type FileBackend struct {
	Path string
}

// NewFileBackend returns a FileBackend at path. Empty path -> DefaultCredentialsPath.
func NewFileBackend(path string) *FileBackend {
	if path == "" {
		path = DefaultCredentialsPath()
	}
	return &FileBackend{Path: path}
}

// Save delegates to the package-level Save function.
func (b *FileBackend) Save(c *Credentials) error { return Save(c, b.Path) }

// Load delegates to the package-level Load function.
func (b *FileBackend) Load() (*Credentials, error) { return Load(b.Path) }

// KeyringBackend stores credentials in the OS-native secret store. Opened
// once at startup and reused for the process lifetime.
type KeyringBackend struct {
	ring keyring.Keyring
}

// KeyringConfig overrides the defaults for OpenKeyring. Zero value selects
// production defaults (auto-detect best backend, "neoanvil" service name).
//
// Tests should set AllowedBackends to []keyring.BackendType{keyring.FileBackend}
// with a deterministic FileDir + PasswordFunc — this avoids touching the
// developer's real Keychain/libsecret during go test.
type KeyringConfig struct {
	ServiceName     string
	AllowedBackends []keyring.BackendType
	FileDir         string
	PasswordFunc    func(prompt string) (string, error)
}

// OpenKeyring opens the OS keyring with the given configuration. Returns an
// error if no usable backend is available.
func OpenKeyring(cfg KeyringConfig) (*KeyringBackend, error) {
	if cfg.ServiceName == "" {
		cfg.ServiceName = defaultKeyringServiceName
	}
	ring, err := keyring.Open(keyring.Config{
		ServiceName:      cfg.ServiceName,
		AllowedBackends:  cfg.AllowedBackends,
		FileDir:          cfg.FileDir,
		FilePasswordFunc: cfg.PasswordFunc,
	})
	if err != nil {
		return nil, fmt.Errorf("open keyring (%s): %w", cfg.ServiceName, err)
	}
	return &KeyringBackend{ring: ring}, nil
}

// Save serializes credentials as JSON and stores them under keyringItemKey.
func (b *KeyringBackend) Save(creds *Credentials) error {
	if creds == nil {
		return errors.New("nil credentials")
	}
	data, err := json.Marshal(creds)
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}
	return b.ring.Set(keyring.Item{
		Key:   keyringItemKey,
		Data:  data,
		Label: "NeoAnvil credentials",
	})
}

// Load returns credentials from the keyring. Returns an empty Credentials
// (Version: 1) when no entry exists — same semantics as Load() on a missing
// credentials.json file.
func (b *KeyringBackend) Load() (*Credentials, error) {
	item, err := b.ring.Get(keyringItemKey)
	if err != nil {
		if errors.Is(err, keyring.ErrKeyNotFound) {
			return &Credentials{Version: 1}, nil
		}
		return nil, fmt.Errorf("keyring get: %w", err)
	}
	var creds Credentials
	if err := json.Unmarshal(item.Data, &creds); err != nil {
		return nil, fmt.Errorf("unmarshal credentials: %w", err)
	}
	return &creds, nil
}
