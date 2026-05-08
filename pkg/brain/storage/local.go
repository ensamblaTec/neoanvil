// Package storage — local.go: filesystem-backed BrainStore. PILAR XXVI /
// 135.C.2.
//
// Stores every object as a regular file under <root>/<key>, where <key>
// is sanitized to remove path-traversal characters and converted to
// forward slashes. Locks live in <root>/.locks/<name>.json with the
// holder + expiration encoded inline.
//
// Goal: a drop-in target for `neo brain push --remote=local:///path` so
// operators can back up snapshots to a NAS, USB drive, or the tsnet-
// mounted directory from 137.E without involving R2. No reliance on
// fcntl or external lock services — the lock file itself is the lease.

package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// defaultMaxObjectBytes caps a single Put — prevents unbounded writes from
// exhausting disk or being used for denial-of-service. [146.E]
const defaultMaxObjectBytes = 10 << 30 // 10 GiB

// LocalStore is the filesystem driver. Root is the directory under which
// objects are written. Created on first use if missing.
type LocalStore struct {
	root           string
	maxObjectBytes int64      // [146.E] per-object write cap; 0 disables the cap
	mu             sync.Mutex // guards Lock/Unlock against intra-process races; FS ensures inter-process
}

// NewLocalStore returns a LocalStore rooted at the given absolute path.
// The directory is created (with 0o700) if it doesn't exist.
func NewLocalStore(root string) (*LocalStore, error) {
	if root == "" {
		return nil, errors.New("LocalStore: root required")
	}
	if !filepath.IsAbs(root) {
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("LocalStore: abs path: %w", err)
		}
		root = abs
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("LocalStore: mkdir root: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".locks"), 0o700); err != nil {
		return nil, fmt.Errorf("LocalStore: mkdir locks: %w", err)
	}
	return &LocalStore{root: root, maxObjectBytes: defaultMaxObjectBytes}, nil
}

// SetMaxObjectBytes overrides the per-object write cap. Pass 0 to disable.
// Typical use: pass cfg.Brain.MaxArchiveBytes so the limit is operator-tunable. [146.E]
func (s *LocalStore) SetMaxObjectBytes(n int64) { s.maxObjectBytes = n }

// Put writes r's bytes to <root>/<key> via a temp-file-and-rename so a
// concurrent reader never sees a partial write.
func (s *LocalStore) Put(key string, r io.Reader) (int64, error) {
	dest, err := s.resolve(key)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return 0, fmt.Errorf("Put: mkdir parent: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".put-*.tmp")
	if err != nil {
		return 0, fmt.Errorf("Put: tmp file: %w", err)
	}
	tmpName := tmp.Name()
	// [146.E] Cap the source to prevent unbounded disk writes.
	src := io.Reader(r)
	if s.maxObjectBytes > 0 {
		src = io.LimitReader(r, s.maxObjectBytes+1) // +1 so we can detect exceeding the limit below
	}
	n, copyErr := io.Copy(tmp, src)
	if s.maxObjectBytes > 0 && n > s.maxObjectBytes {
		_ = os.Remove(tmpName)
		return n, fmt.Errorf("Put: object %q exceeds size cap %d bytes (got %d)", key, s.maxObjectBytes, n)
	}
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if copyErr != nil {
		_ = os.Remove(tmpName)
		return n, fmt.Errorf("Put: copy: %w", copyErr)
	}
	if syncErr != nil {
		_ = os.Remove(tmpName)
		return n, fmt.Errorf("Put: sync tmp: %w", syncErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmpName)
		return n, fmt.Errorf("Put: close tmp: %w", closeErr)
	}
	// [146.D] Use renameat(2) with a held parent-dir fd instead of os.Rename
	// (which resolves the path anew). Holding the fd makes directory-symlink
	// substitution attacks ineffective: even if an attacker swaps
	// filepath.Dir(dest) with a symlink between CreateTemp and here, the
	// kernel uses the already-opened fd — it does not re-walk the path.
	parentDir := filepath.Dir(dest)
	dfd, derr := os.Open(parentDir)
	if derr != nil {
		_ = os.Remove(tmpName)
		return n, fmt.Errorf("Put: open parent dir: %w", derr)
	}
	renErr := unix.Renameat(int(dfd.Fd()), filepath.Base(tmpName), int(dfd.Fd()), filepath.Base(dest))
	if renErr != nil {
		_ = dfd.Close()
		_ = os.Remove(tmpName)
		return n, fmt.Errorf("Put: renameat: %w", renErr)
	}
	_ = dfd.Sync()
	_ = dfd.Close()
	return n, nil
}

// Get opens the object at key for reading. Returns ErrNotFound if the
// key does not exist.
func (s *LocalStore) Get(key string) (io.ReadCloser, error) {
	dest, err := s.resolve(key)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(dest, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // G304-WORKSPACE-CANON: dest = filepath.Join(root, sanitized-key); resolve rejects "..". O_NOFOLLOW prevents symlink traversal.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("Get: %w", err)
	}
	return f, nil
}

// List returns ChunkRefs for every regular file whose path under root
// starts with prefix. Lock files (under .locks/) are filtered.
func (s *LocalStore) List(prefix string) ([]ChunkRef, error) {
	var out []ChunkRef
	walkErr := filepath.WalkDir(s.root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if filepath.Base(path) == ".locks" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(s.root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, ChunkRef{
			Key:       key,
			Size:      info.Size(),
			UpdatedAt: info.ModTime(),
		})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("List: %w", walkErr)
	}
	return out, nil
}

// Delete removes the object at key. Idempotent — missing key returns nil.
func (s *LocalStore) Delete(key string) error {
	dest, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("Delete: %w", err)
	}
	return nil
}

// lockFile is the on-disk form of a Lease, persisted to <root>/.locks/<name>.json.
type lockFile struct {
	Name        string    `json:"name"`
	Holder      string    `json:"holder"`
	ExpiresAt   time.Time `json:"expires_at"`
	OpaqueToken string    `json:"opaque_token"`
}

// Lock acquires <root>/.locks/<name>.json via O_CREATE|O_EXCL. If the
// file exists but its expires_at is in the past, the lock is reclaimed.
// OpaqueToken is a fresh nanosecond-granular timestamp the holder must
// echo back in Unlock to detect ownership swaps.
//
// Cross-process safety: the previous implementation used os.WriteFile,
// which is non-atomic across PIDs (two processes that both observe the
// lock as missing-or-expired could both call WriteFile and produce the
// same Lease, defeating the mutual-exclusion guarantee). The intra-
// process s.mu only serialises within one *LocalStore instance, so it
// did not protect concurrent `brain push` invocations from different
// shells. We now use os.OpenFile with O_CREATE|O_EXCL as the authoritative
// claim primitive: at most one process can succeed in creating the file.
// The expired-lease reclaim path uses a Remove + retry-once-with-EXCL
// dance; if another process wins the reclaim race, our second EXCL fails
// cleanly with ErrLockHeld instead of silently overwriting their lease.
func (s *LocalStore) Lock(name, holder string, ttl time.Duration) (Lease, error) {
	if name == "" || holder == "" {
		return Lease{}, errors.New("Lock: name and holder required")
	}
	if ttl <= 0 {
		return Lease{}, errors.New("Lock: ttl must be > 0")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	path := filepath.Join(s.root, ".locks", sanitizeLockName(name)+".json")
	// Fast-path: surface "actively held" without competing for the atomic
	// create. This keeps the hot path read-only when the lock is held.
	if existing, ok := s.readLockFile(path); ok && time.Now().Before(existing.ExpiresAt) {
		return Lease{}, fmt.Errorf("%w: held by %s until %s", ErrLockHeld, existing.Holder, existing.ExpiresAt.Format(time.RFC3339))
	}
	lf := lockFile{
		Name:        name,
		Holder:      holder,
		ExpiresAt:   time.Now().Add(ttl),
		OpaqueToken: fmt.Sprintf("%s-%d", holder, time.Now().UnixNano()),
	}
	data, err := json.Marshal(lf)
	if err != nil {
		return Lease{}, fmt.Errorf("Lock: marshal: %w", err)
	}
	// Two attempts: first try a clean atomic create. If EEXIST, the file is
	// stale (we already verified above it's expired); remove it and retry.
	// If the second attempt also EEXISTs, another reclaimer beat us — fail
	// rather than overwrite their valid claim.
	for attempt := range 2 {
		lease, holdErr, retryable := s.tryClaimLock(path, data, lf)
		if !retryable {
			return lease, holdErr
		}
		if attempt == 1 {
			break // exit and report race
		}
		// Stale/unreadable: try to remove and retry exactly once.
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			return Lease{}, fmt.Errorf("Lock: reclaim remove: %w", rmErr)
		}
	}
	return Lease{}, fmt.Errorf("%w: race during reclaim retry", ErrLockHeld)
}

// tryClaimLock attempts an O_CREATE|O_EXCL claim of path with the given
// payload. Returns (lease, nil, false) on success, (zero, ErrLockHeld, false)
// if another holder still owns it, (zero, err, false) on a hard error, and
// (zero, nil, true) when the file is stale/expired and the caller should
// remove + retry. Splitting this out keeps Lock's cyclomatic complexity
// under the audit cap.
func (s *LocalStore) tryClaimLock(path string, data []byte, lf lockFile) (Lease, error, bool) {
	f, openErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if openErr == nil {
		_, writeErr := f.Write(data)
		closeErr := f.Close()
		if writeErr != nil {
			_ = os.Remove(path) // best-effort cleanup of half-written file
			return Lease{}, fmt.Errorf("Lock: write: %w", writeErr), false
		}
		if closeErr != nil {
			_ = os.Remove(path)
			return Lease{}, fmt.Errorf("Lock: close: %w", closeErr), false
		}
		return Lease(lf), nil, false
	}
	if !os.IsExist(openErr) {
		return Lease{}, fmt.Errorf("Lock: open: %w", openErr), false
	}
	// EEXIST. Re-check expiry — file may have been recreated by a fresh
	// holder between our fast-path read and this EXCL attempt.
	if existing, ok := s.readLockFile(path); ok && time.Now().Before(existing.ExpiresAt) {
		return Lease{}, fmt.Errorf("%w: held by %s until %s", ErrLockHeld, existing.Holder, existing.ExpiresAt.Format(time.RFC3339)), false
	}
	// File is stale or unreadable — caller should remove and retry.
	return Lease{}, nil, true
}

// Unlock removes the lock file when the OpaqueToken matches. A token
// mismatch means another holder reclaimed the expired lease — Unlock
// becomes a no-op rather than removing someone else's lock.
func (s *LocalStore) Unlock(lease Lease) error {
	if lease.Name == "" {
		return errors.New("Unlock: empty lease")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.root, ".locks", sanitizeLockName(lease.Name)+".json")
	existing, ok := s.readLockFile(path)
	if !ok {
		return nil // already gone
	}
	if existing.OpaqueToken != lease.OpaqueToken {
		return nil // someone else owns it now
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("Unlock: %w", err)
	}
	return nil
}

// Close is a no-op for the FS driver — there are no long-lived handles.
func (s *LocalStore) Close() error { return nil }

// resolve maps key to an absolute path under s.root, rejecting traversal
// (".." segments) and absolute paths.
func (s *LocalStore) resolve(key string) (string, error) {
	if key == "" {
		return "", errors.New("storage: empty key")
	}
	clean := filepath.Clean(filepath.FromSlash(key))
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") || strings.Contains(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("storage: refusing key with traversal or absolute path: %q", key)
	}
	return filepath.Join(s.root, clean), nil
}

// readLockFile reads + parses a lock file. Returns ok=false on missing
// or unreadable/unparseable file (treated as "no lock").
func (s *LocalStore) readLockFile(path string) (lockFile, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // G304-WORKSPACE-CANON: path is filepath.Join(s.root, ".locks", sanitized-name); s.root is operator-controlled.
	if err != nil {
		return lockFile{}, false
	}
	var lf lockFile
	if err := json.Unmarshal(data, &lf); err != nil {
		return lockFile{}, false
	}
	return lf, true
}

// sanitizeLockName converts a free-form lock name into a path-safe basename.
// Same alphabet rule as sanitizeID in archive.go (alphanumerics + -.) —
// other characters become "_".
func sanitizeLockName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
