package knowledge

// Internal test package (not _test): we reach into handleFSEvent and lockForKey
// to validate the 344.A fix without constructing a live fsnotify watcher.

import (
	"fmt"
	"sync"
	"testing"

	"github.com/fsnotify/fsnotify"
)

func newTestStoreHC(t *testing.T) (*KnowledgeStore, *HotCache) {
	t.Helper()
	ks, err := Open(t.TempDir() + "/knowledge.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { ks.Close() })
	return ks, NewHotCache()
}

// TestLockForKey verifies lockForKey returns the same mutex for the same key
// and distinct mutexes for different keys. [344.A]
func TestLockForKey(t *testing.T) {
	m1 := lockForKey("ns", "key-a")
	m2 := lockForKey("ns", "key-a")
	if m1 != m2 {
		t.Error("same key should return same mutex")
	}
	m3 := lockForKey("ns", "key-b")
	if m1 == m3 {
		t.Error("different keys should return different mutexes")
	}
	m4 := lockForKey("other-ns", "key-a")
	if m1 == m4 {
		t.Error("different namespace with same key should return different mutex")
	}
}

// TestHandleFSEvent_StalenessCheck verifies a watcher Put with an older
// UpdatedAt does NOT overwrite a fresher existing entry. [344.A]
func TestHandleFSEvent_StalenessCheck(t *testing.T) {
	ks, hc := newTestStoreHC(t)
	dir := t.TempDir()

	// Seed store with a FRESHER entry (UpdatedAt=100).
	fresh := KnowledgeEntry{
		Key:       "concurrent-key",
		Namespace: "contracts",
		Content:   "fresh version",
		UpdatedAt: 100,
	}
	if err := ks.Put(fresh.Namespace, fresh.Key, fresh); err != nil {
		t.Fatal(err)
	}

	// Write a STALE .md to disk (UpdatedAt=50).
	stale := KnowledgeEntry{
		Key:       "concurrent-key",
		Namespace: "contracts",
		Content:   "stale version",
		UpdatedAt: 50,
	}
	if err := ExportEntry(dir, stale); err != nil {
		t.Fatal(err)
	}

	// Simulate fsnotify Write event on the stale file.
	path := dir + "/contracts/concurrent-key.md"
	ev := fsnotify.Event{Name: path, Op: fsnotify.Write}
	handleFSEvent(ev, nil, ks, hc)

	// Store should still contain the fresh version.
	got, err := ks.Get("contracts", "concurrent-key")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Content != "fresh version" {
		t.Errorf("stale write clobbered fresh entry: %+v", got)
	}
}

// TestHandleFSEvent_FresherWins verifies the inverse: a fresher fsnotify
// write correctly updates the store. Uses far-future Unix timestamps so the
// fresher value beats the Put-assigned current-time UpdatedAt. [344.A]
func TestHandleFSEvent_FresherWins(t *testing.T) {
	ks, hc := newTestStoreHC(t)
	dir := t.TempDir()

	// Seed store with an entry — Put will set UpdatedAt = time.Now().Unix().
	if err := ks.Put("contracts", "k2", KnowledgeEntry{
		Key: "k2", Namespace: "contracts", Content: "old",
	}); err != nil {
		t.Fatal(err)
	}

	// Write .md with UpdatedAt far in the future so it beats the Put timestamp.
	fresh := KnowledgeEntry{
		Key:       "k2",
		Namespace: "contracts",
		Content:   "fresher content",
		UpdatedAt: 4_000_000_000, // ~2096-10
	}
	if err := ExportEntry(dir, fresh); err != nil {
		t.Fatal(err)
	}
	handleFSEvent(fsnotify.Event{Name: dir + "/contracts/k2.md", Op: fsnotify.Write}, nil, ks, hc)

	got, err := ks.Get("contracts", "k2")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Content != "fresher content" {
		t.Errorf("fresh write did not apply: %+v", got)
	}
}

// TestConcurrentFSEvents_SameKeyDontCorrupt launches many goroutines all
// firing watcher events on the same key. Verifies the per-key mutex
// serializes Put calls — after the storm settles there is exactly one
// stable entry whose Content matches one of the written versions (no
// partial state, no crash under -race). [344.A]
func TestConcurrentFSEvents_SameKeyDontCorrupt(t *testing.T) {
	ks, hc := newTestStoreHC(t)
	dir := t.TempDir()

	const N = 20
	var wg sync.WaitGroup
	for i := 1; i <= N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := KnowledgeEntry{
				Key:       "race-key",
				Namespace: "contracts",
				Content:   fmt.Sprintf("version-%d", i),
				UpdatedAt: int64(1_800_000_000 + i), // future Unix ts so the staleness guard always lets through
			}
			_ = ExportEntry(dir, e)
			handleFSEvent(fsnotify.Event{Name: dir + "/contracts/race-key.md", Op: fsnotify.Write}, nil, ks, hc)
		}(i)
	}
	wg.Wait()

	got, err := ks.Get("contracts", "race-key")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("nil after race")
	}
	// Content must be one of the written versions — no partial/torn state.
	valid := false
	for i := 1; i <= N; i++ {
		if got.Content == fmt.Sprintf("version-%d", i) {
			valid = true
			break
		}
	}
	if !valid {
		t.Errorf("post-race content is not a valid version string: %q", got.Content)
	}
}
