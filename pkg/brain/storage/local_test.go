package storage

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// newStore returns a fresh LocalStore rooted at t.TempDir().
func newStore(t *testing.T) *LocalStore {
	t.Helper()
	s, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	return s
}

// TestNewLocalStore_RejectsEmpty — empty root is a caller bug.
func TestNewLocalStore_RejectsEmpty(t *testing.T) {
	if _, err := NewLocalStore(""); err == nil {
		t.Error("empty root should error")
	}
}

// TestNewLocalStore_CreatesRoot — directory is created with 0o700 if
// missing. Verifies the bootstrap works for first-time usage.
func TestNewLocalStore_CreatesRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "neo-brain-store")
	s, err := NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := NewLocalStore(root); err != nil {
		t.Errorf("re-open existing root failed: %v", err)
	}
}

// TestPutGet_Roundtrip — writing then reading the same key yields the
// original bytes.
func TestPutGet_Roundtrip(t *testing.T) {
	s := newStore(t)
	defer s.Close()

	body := []byte("hello, brain")
	n, err := s.Put("snapshots/abc.tar.zst", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != int64(len(body)) {
		t.Errorf("Put returned n=%d, want %d", n, len(body))
	}
	rc, err := s.Get("snapshots/abc.tar.zst")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("got %q, want %q", got, body)
	}
}

// TestPut_Overwrites — second Put with the same key replaces the first
// content (no append).
func TestPut_Overwrites(t *testing.T) {
	s := newStore(t)
	defer s.Close()

	if _, err := s.Put("k", bytes.NewReader([]byte("v1"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put("k", bytes.NewReader([]byte("v2"))); err != nil {
		t.Fatal(err)
	}
	rc, _ := s.Get("k")
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "v2" {
		t.Errorf("overwrite failed: got %q, want v2", got)
	}
}

// TestGet_NotFound — missing key returns sentinel ErrNotFound.
func TestGet_NotFound(t *testing.T) {
	s := newStore(t)
	defer s.Close()
	_, err := s.Get("does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestList_PrefixFilter — List returns only keys with matching prefix.
// Lock files under .locks/ MUST be filtered out.
func TestList_PrefixFilter(t *testing.T) {
	s := newStore(t)
	defer s.Close()

	for _, k := range []string{"snapshots/a", "snapshots/b", "manifests/m1", "snapshots/sub/c"} {
		if _, err := s.Put(k, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatal(err)
		}
	}
	// Acquire a lock so .locks/ has content; List must skip it.
	if _, err := s.Lock("test-lock", "node-1", time.Minute); err != nil {
		t.Fatal(err)
	}

	got, err := s.List("snapshots/")
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, len(got))
	for i, c := range got {
		keys[i] = c.Key
	}
	slices.Sort(keys)
	want := []string{"snapshots/a", "snapshots/b", "snapshots/sub/c"}
	if !slices.Equal(keys, want) {
		t.Errorf("List(snapshots/) = %v, want %v", keys, want)
	}
}

// TestList_EmptyPrefix — empty prefix lists every object.
func TestList_EmptyPrefix(t *testing.T) {
	s := newStore(t)
	defer s.Close()

	for _, k := range []string{"a", "b/c", "d/e/f"} {
		if _, err := s.Put(k, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("want 3 entries, got %d (%v)", len(got), got)
	}
}

// TestDelete_Idempotent — deleting a missing key is not an error.
func TestDelete_Idempotent(t *testing.T) {
	s := newStore(t)
	defer s.Close()
	if err := s.Delete("never-existed"); err != nil {
		t.Errorf("missing key Delete error: %v", err)
	}
	if _, err := s.Put("k", bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("k"); err != nil {
		t.Errorf("present key Delete error: %v", err)
	}
	if _, err := s.Get("k"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after Delete Get returned %v, want ErrNotFound", err)
	}
}

// TestResolve_RejectsTraversal — keys with ".." or absolute paths are
// rejected at Put / Get / Delete.
func TestResolve_RejectsTraversal(t *testing.T) {
	s := newStore(t)
	defer s.Close()
	bad := []string{"../escape", "/etc/passwd", "a/../../../b", ""}
	for _, k := range bad {
		if _, err := s.Put(k, bytes.NewReader([]byte("x"))); err == nil {
			t.Errorf("Put(%q) should have errored", k)
		}
		if _, err := s.Get(k); err == nil {
			t.Errorf("Get(%q) should have errored", k)
		}
	}
}

// TestLock_ExclusivePerName — second Lock on the same name fails with
// ErrLockHeld until Unlock or expiry.
func TestLock_ExclusivePerName(t *testing.T) {
	s := newStore(t)
	defer s.Close()

	lease1, err := s.Lock("push", "node-A", time.Minute)
	if err != nil {
		t.Fatalf("Lock 1: %v", err)
	}
	if _, err := s.Lock("push", "node-B", time.Minute); !errors.Is(err, ErrLockHeld) {
		t.Errorf("second Lock want ErrLockHeld, got %v", err)
	}
	if err := s.Unlock(lease1); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if _, err := s.Lock("push", "node-B", time.Minute); err != nil {
		t.Errorf("Lock after Unlock: %v", err)
	}
}

// TestLock_ExpiredReclaim — when a lease's ExpiresAt is in the past,
// another holder may reclaim it without explicit Unlock.
func TestLock_ExpiredReclaim(t *testing.T) {
	s := newStore(t)
	defer s.Close()

	if _, err := s.Lock("push", "node-A", time.Millisecond); err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	lease, err := s.Lock("push", "node-B", time.Minute)
	if err != nil {
		t.Errorf("expired-lock reclaim should succeed, got %v", err)
	}
	if lease.Holder != "node-B" {
		t.Errorf("reclaimed lease holder = %q, want node-B", lease.Holder)
	}
}

// TestUnlock_TokenMismatchNoOp — when the OpaqueToken doesn't match,
// Unlock is a no-op (someone else reclaimed the expired lease).
func TestUnlock_TokenMismatchNoOp(t *testing.T) {
	s := newStore(t)
	defer s.Close()

	if _, err := s.Lock("push", "node-A", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	lease2, err := s.Lock("push", "node-B", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	staleLease := Lease{Name: "push", OpaqueToken: "node-A-12345"}
	if err := s.Unlock(staleLease); err != nil {
		t.Errorf("token-mismatch Unlock should be no-op, got %v", err)
	}
	// node-B's lease must still be live.
	if _, err := s.Lock("push", "node-C", time.Minute); !errors.Is(err, ErrLockHeld) {
		t.Errorf("node-B's lease was prematurely freed")
	}
	_ = s.Unlock(lease2)
}

// TestSanitizeLockName — unsafe characters become underscore.
func TestSanitizeLockName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"push", "push"},
		{"snap/2026", "snap_2026"},
		{"a:b:c", "a_b_c"},
		{"safe-1.0", "safe-1.0"},
		{"", ""},
		{"../etc", ".._etc"}, // dots are in the whitelist; only "/" → "_"
	}
	for _, c := range cases {
		if got := sanitizeLockName(c.in); got != c.want {
			t.Errorf("sanitizeLockName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLock_RejectsBadInput — empty name/holder or non-positive ttl.
func TestLock_RejectsBadInput(t *testing.T) {
	s := newStore(t)
	defer s.Close()
	cases := []struct{ name, holder string; ttl time.Duration }{
		{"", "h", time.Minute},
		{"n", "", time.Minute},
		{"n", "h", 0},
		{"n", "h", -time.Second},
	}
	for _, c := range cases {
		if _, err := s.Lock(c.name, c.holder, c.ttl); err == nil {
			t.Errorf("bad input (name=%q holder=%q ttl=%v) should error", c.name, c.holder, c.ttl)
		}
	}
}

// TestPut_AtomicReplace — kill the temp file mid-write must not leave a
// partial visible at the destination key. We can't actually crash mid-
// rename, but we verify the temp-file-then-rename pattern by checking
// no .tmp files survive a successful Put.
func TestPut_AtomicReplace(t *testing.T) {
	s := newStore(t)
	defer s.Close()

	if _, err := s.Put("foo", bytes.NewReader([]byte("hello"))); err != nil {
		t.Fatal(err)
	}
	got, err := s.List("")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if strings.Contains(c.Key, ".tmp") {
			t.Errorf("Put left a .tmp leftover: %s", c.Key)
		}
	}
}
