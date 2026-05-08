package auth

// keystore.go — local API key storage in ~/.neo/credentials.json with 0600 perms.
// PILAR XXXIII, épicas 264.A-B.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// keystoreAudit is an optional hash-chained audit log for Load/Save operations.
// Set via SetKeystoreAuditLog. Nil means no audit trail (non-fatal). [145.F]
var (
	keystoreAuditMu  sync.Mutex
	keystoreAuditLog *AuditLog
)

// SetKeystoreAuditLog installs the audit log that Load and Save write to.
// Call once at program startup. Thread-safe. [145.F]
func SetKeystoreAuditLog(l *AuditLog) {
	keystoreAuditMu.Lock()
	keystoreAuditLog = l
	keystoreAuditMu.Unlock()
}

// DefaultKeystoreAuditPath returns ~/.neo/audit-keystore.log. [145.F]
func DefaultKeystoreAuditPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".neo", "audit-keystore.log")
}

// appendKeystoreEvent writes ev to the keystore audit log if one is installed.
// Errors are logged but never propagated — audit is best-effort. [145.F]
func appendKeystoreEvent(ev Event) {
	keystoreAuditMu.Lock()
	al := keystoreAuditLog
	keystoreAuditMu.Unlock()
	if al == nil {
		return
	}
	if _, err := al.Append(ev); err != nil {
		log.Printf("[KEYSTORE] audit log write failed: %v", err)
	}
}

// credHash returns the first 8 hex chars of sha256(token) — a safe proxy for
// "which credential" without exposing the token value. [145.F]
func credHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:4]) // 4 bytes = 8 hex chars
}

// Credentials is the top-level structure persisted to credentials.json.
type Credentials struct {
	Version int         `json:"version"`
	Entries []CredEntry `json:"entries"`
}

// Credential type constants. Type drives which Provider implementation
// (pkg/auth/provider.go — Épica 124.3) handles refresh and validation.
const (
	CredTypeAPIToken = "api_token" // simple bearer / Basic auth (Atlassian, GitHub PAT)
	CredTypeOAuth2   = "oauth2"    // OAuth 2.0 (3LO) with refresh token
)

// CredEntry holds one provider's credentials. The schema spans both
// simple API tokens and OAuth 2.0 entries — fields not relevant to a given
// Type are omitted from JSON via omitempty.
//
//	API token:  Provider, Type=api_token, Token, Email/Domain (provider-specific), ExpiresAt
//	OAuth 2.0:  Provider, Type=oauth2,    Token, RefreshToken, Scopes,             ExpiresAt
//
// CreatedAt is always stamped. ExpiresAt is RFC3339; empty = never expires
// (legacy entries from PILAR XXXIII).
type CredEntry struct {
	Provider     string   `json:"provider"`
	Type         string   `json:"type,omitempty"`
	Token        string   `json:"token"`
	RefreshToken string   `json:"refresh_token,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	Email        string   `json:"email,omitempty"`
	Domain       string   `json:"domain,omitempty"`
	TenantID     string   `json:"tenant_id,omitempty"`
	CreatedAt    string   `json:"created_at"`
	ExpiresAt    string   `json:"expires_at,omitempty"`
}

// IsExpired reports whether ExpiresAt is set and in the past relative to now.
// Empty ExpiresAt or a malformed timestamp returns false — treat as
// non-expiring rather than fail-closed; operator owns source of truth.
func (e *CredEntry) IsExpired(now time.Time) bool {
	if e == nil || e.ExpiresAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, e.ExpiresAt)
	if err != nil {
		return false
	}
	return now.After(t)
}

// ExpiresIn returns the duration until expiry. Zero when ExpiresAt is empty
// or unparseable; negative when the entry is already expired.
func (e *CredEntry) ExpiresIn(now time.Time) time.Duration {
	if e == nil || e.ExpiresAt == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, e.ExpiresAt)
	if err != nil {
		return 0
	}
	return t.Sub(now)
}

// DefaultCredentialsPath returns ~/.neo/credentials.json.
func DefaultCredentialsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".neo", "credentials.json")
}

// Load reads credentials from path. Returns an empty Credentials if the file
// does not exist (os.IsNotExist). Returns an error only for parse failures.
//
// Token-redaction discipline: json.Unmarshal errors include a quoted excerpt
// of the input bytes around the offending offset (e.g. "invalid character ']'
// looking for beginning of object key string"). When the input is a corrupted
// credentials.json, that excerpt may include plaintext tokens — and any caller
// that logs the error then leaks the token to disk / monitoring / terminal.
// We catch the unmarshal error and return a generic "format invalid" message
// that NEVER references the raw bytes. The position offset (if available) is
// preserved as a non-sensitive aid to forensics.
//
// Audit finding pkg/auth/keystore.go:78 (2026-05-01, SEV 8: token exposure
// via json.Unmarshal error messages).
func Load(path string) (*Credentials, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304-CLI-CONSENT: operator-managed credentials file
	if err != nil {
		if os.IsNotExist(err) {
			return &Credentials{Version: 1}, nil
		}
		return nil, err
	}
	// [145.D] Verify file permissions after read. Warn and tighten if group/other
	// bits are set — credentials must be readable only by the owning user (0600).
	if fi, statErr := os.Stat(path); statErr == nil && fi.Mode().Perm()&0o077 != 0 {
		log.Printf("[KEYSTORE] WARNING: credentials file %q has loose permissions (%s); tightening to 0600", path, fi.Mode().Perm())
		_ = os.Chmod(path, 0o600) //nolint:gosec // G302: intentional tighten of credentials file mode
	}
	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		// Strip the raw-byte excerpt from json error variants. We surface the
		// byte offset (if any) — that is metadata, not credential bytes — so
		// operators can still narrow down which line of the corrupted file
		// produced the error.
		if syntaxErr, ok := errors.AsType[*json.SyntaxError](err); ok {
			return nil, fmt.Errorf("credentials format invalid at offset %d (file contents redacted)", syntaxErr.Offset)
		}
		if unmarshalTypeErr, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
			return nil, fmt.Errorf("credentials format invalid at offset %d (file contents redacted)", unmarshalTypeErr.Offset)
		}
		return nil, errors.New("credentials format invalid (file contents redacted)")
	}
	// [145.F] Build per-entry fingerprints for audit trail without exposing token values.
	hashes := make([]string, len(creds.Entries))
	for i, e := range creds.Entries {
		hashes[i] = e.Provider + ":" + credHash(e.Token)
	}
	appendKeystoreEvent(Event{
		Kind:    "keystore_load",
		Actor:   "keystore",
		Details: map[string]any{"path": path, "entry_count": len(creds.Entries), "token_hashes": hashes},
	})
	return &creds, nil
}

// withKeystoreLock acquires an exclusive advisory POSIX lock on a companion
// lock file (<dir>/.credentials.lock) for the duration of fn. This serializes
// cross-process Save calls so two processes that both Load → modify → Save
// cannot interleave their writes even on non-POSIX-atomic filesystems.
// [145.E] The lock is released when fn returns.
func withKeystoreLock(credPath string, fn func() error) error {
	lockPath := filepath.Join(filepath.Dir(credPath), ".credentials.lock")
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304-CLI-CONSENT: lock file in operator-managed ~/.neo dir
	if err != nil {
		return fmt.Errorf("open keystore lock file: %w", err)
	}
	defer func() { _ = lf.Close() }()
	// Blocking exclusive lock — waits for any concurrent Save to finish.
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire keystore lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// Save writes credentials to path with 0600 permissions. Creates parent
// directories as needed.
//
// Symlink + concurrent-Save safety: previously Save used os.OpenFile with
// O_TRUNC, which follows symlinks at path. An adversary with write access
// to the parent directory could pre-place a symlink at credentials.json
// pointing to /etc/passwd or any world-readable location; the next Save
// would then write the full token set to the attacker's chosen target
// (audit finding pkg/auth/keystore.go:104, SEV 9). Two concurrent Saves
// could also interleave and leave the file empty/half-written.
//
// The fix uses write-tmp + fsync + atomic rename:
//
//   1. Create a fresh tmp file in the SAME directory via os.CreateTemp.
//      CreateTemp opens with O_CREATE|O_EXCL and a random suffix, so it
//      never follows an existing symlink at the tmp path. Mode is 0600
//      from the start (CreateTemp default).
//   2. Write + Sync + Close to the tmp file.
//   3. os.Rename(tmp, path) — atomic on POSIX. Rename does NOT follow
//      symlinks at the destination; if path is a symlink, the symlink
//      is atomically replaced by the regular file we just wrote, and
//      the symlink target is never touched.
//
// On any pre-Rename failure, the tmp file is removed so we don't leak
// .credentials.tmp.* litter.
func Save(creds *Credentials, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	// [145.E] Serialize concurrent cross-process Saves via POSIX advisory lock.
	if err := withKeystoreLock(path, func() error {
		return atomicWriteCredentials(path, data)
	}); err != nil {
		return err
	}
	appendKeystoreEvent(Event{ // [145.F] audit save
		Kind:    "keystore_save",
		Actor:   "keystore",
		Details: map[string]any{"path": path, "entry_count": len(creds.Entries)},
	})
	return nil
}

// atomicWriteCredentials encapsulates the symlink-safe + concurrent-safe
// write protocol for credentials.json. Split out from Save so the latter
// stays under the CC cap and so the protocol is unit-testable.
func atomicWriteCredentials(path string, data []byte) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, "."+base+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleaned := false
	defer func() {
		if !cleaned {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleaned = true
	return nil
}

// AddEntry adds or replaces the entry for provider with a simple API-token
// shape. Preserved for backward compatibility with PILAR XXXIII callers.
// New code targeting OAuth or expiry should use Add(CredEntry) directly.
func (c *Credentials) AddEntry(provider, token, tenantID string) {
	c.Add(CredEntry{
		Provider: provider,
		Type:     CredTypeAPIToken,
		Token:    token,
		TenantID: tenantID,
	})
}

// Add adds or replaces the entry keyed by Provider. CreatedAt is stamped to
// now if not already set, so callers that build CredEntry literals don't
// need to remember the timestamp. ExpiresAt, RefreshToken, Scopes and the
// rest are passed through verbatim.
//
// [145.G] Provider names are case-folded to lowercase on write so that
// lookups via GetByProvider are case-insensitive. Callers should not rely
// on a specific case — "Jira" and "jira" refer to the same entry.
func (c *Credentials) Add(entry CredEntry) {
	entry.Provider = strings.ToLower(entry.Provider)
	if entry.CreatedAt == "" {
		entry.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	for i, existing := range c.Entries {
		if existing.Provider == entry.Provider {
			c.Entries[i] = entry
			return
		}
	}
	c.Entries = append(c.Entries, entry)
}

// GetByProvider returns the first entry matching provider, or nil.
// [145.G] Lookup is case-insensitive — "Jira" matches a stored "jira" entry.
func (c *Credentials) GetByProvider(provider string) *CredEntry {
	key := strings.ToLower(provider)
	for i := range c.Entries {
		if c.Entries[i].Provider == key {
			return &c.Entries[i]
		}
	}
	return nil
}
