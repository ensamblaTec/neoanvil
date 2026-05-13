package rag

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestHotFilesCache_MissThenHit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	writeFile(t, path, "hello")

	c := NewHotFilesCache(1024)
	// First Get: miss (cache empty).
	if _, ok := c.Get(path); ok {
		t.Fatal("expected miss on empty cache")
	}
	// Read + Put.
	data, _ := os.ReadFile(path)
	c.Put(path, data)
	// Second Get: hit.
	got, ok := c.Get(path)
	if !ok {
		t.Fatal("expected hit after Put")
	}
	if string(got) != "hello" {
		t.Errorf("content mismatch: got %q want hello", got)
	}
	st := c.Stats()
	if st.Hits != 1 || st.Misses != 1 {
		t.Errorf("stats hits=%d misses=%d want 1/1", st.Hits, st.Misses)
	}
	if st.EntryCount != 1 || st.TotalBytes != 5 {
		t.Errorf("stats entryCount=%d totalBytes=%d want 1/5", st.EntryCount, st.TotalBytes)
	}
}

func TestHotFilesCache_StaleInvalidatesOnMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "b.txt")
	writeFile(t, path, "v1")

	c := NewHotFilesCache(1024)
	data, _ := os.ReadFile(path)
	c.Put(path, data)
	// Sanity hit.
	if _, ok := c.Get(path); !ok {
		t.Fatal("expected hit")
	}
	// Mutate file → mtime changes.
	time.Sleep(15 * time.Millisecond) // ensure mtime tick on filesystems with low resolution
	writeFile(t, path, "v2 longer")
	if _, ok := c.Get(path); ok {
		t.Error("stale entry must be invalidated when mtime changes")
	}
	if c.Stats().Stale != 1 {
		t.Errorf("expected 1 stale invalidation, got %d", c.Stats().Stale)
	}
}

func TestHotFilesCache_StaleInvalidatesOnSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.txt")
	writeFile(t, path, "abc")
	c := NewHotFilesCache(1024)
	c.Put(path, []byte("abc"))
	// Force a same-mtime write with different size — write+os.Chtimes.
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("abcd"), 0600); err != nil {
		t.Fatal(err)
	}
	// Restore the original mtime so only size differs.
	if err := os.Chtimes(path, time.Now(), original.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get(path); ok {
		t.Error("size mismatch should invalidate even with same mtime")
	}
}

func TestHotFilesCache_DeletedFileEvicts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ephemeral.txt")
	writeFile(t, path, "data")
	c := NewHotFilesCache(1024)
	c.Put(path, []byte("data"))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get(path); ok {
		t.Error("Get after file deletion should miss")
	}
	if c.Stats().EntryCount != 0 {
		t.Errorf("entry should have been evicted, got count=%d", c.Stats().EntryCount)
	}
}

func TestHotFilesCache_LRUEviction(t *testing.T) {
	dir := t.TempDir()
	// Cache capacity small: 25 bytes. Entries of 10 bytes each.
	c := NewHotFilesCache(25)
	paths := []string{
		filepath.Join(dir, "1.txt"),
		filepath.Join(dir, "2.txt"),
		filepath.Join(dir, "3.txt"),
	}
	for i, p := range paths {
		writeFile(t, p, "0123456789") // 10 bytes
		c.Put(p, []byte("0123456789"))
		// Slight delay so mtimes differ if needed.
		_ = i
	}
	// Cap=25, three entries of 10 bytes each = 30 total. One must have been
	// evicted (the oldest, paths[0]).
	if c.Stats().TotalBytes > 25 {
		t.Errorf("totalBytes %d exceeds cap 25", c.Stats().TotalBytes)
	}
	if _, ok := c.Get(paths[0]); ok {
		t.Error("oldest entry should have been LRU-evicted")
	}
	if _, ok := c.Get(paths[2]); !ok {
		t.Error("newest entry should still be cached")
	}
	if c.Stats().Evictions < 1 {
		t.Errorf("expected at least 1 eviction, got %d", c.Stats().Evictions)
	}
}

func TestHotFilesCache_OversizedSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.txt")
	huge := make([]byte, 200)
	writeFile(t, path, string(huge))
	c := NewHotFilesCache(100) // cap < entry size
	c.Put(path, huge)
	if c.Stats().EntryCount != 0 {
		t.Errorf("oversized entry must be skipped, got count=%d", c.Stats().EntryCount)
	}
}

func TestHotFilesCache_OverwriteSameKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d.txt")
	writeFile(t, path, "v1")
	c := NewHotFilesCache(1024)
	c.Put(path, []byte("v1"))
	// Simulate re-read after the file was touched (mtime same in test, but
	// caller may overwrite — semantics are "latest Put wins").
	time.Sleep(15 * time.Millisecond)
	writeFile(t, path, "v2-longer-content")
	c.Put(path, []byte("v2-longer-content"))
	if c.Stats().EntryCount != 1 {
		t.Errorf("expected 1 entry after overwrite, got %d", c.Stats().EntryCount)
	}
	got, ok := c.Get(path)
	if !ok || string(got) != "v2-longer-content" {
		t.Errorf("overwrite Get failed: ok=%v got=%q", ok, got)
	}
}

func TestHotFilesCache_Invalidate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "e.txt")
	writeFile(t, path, "x")
	c := NewHotFilesCache(1024)
	c.Put(path, []byte("x"))
	c.Invalidate(path)
	if c.Stats().EntryCount != 0 {
		t.Error("Invalidate should remove the entry")
	}
	if _, ok := c.Get(path); ok {
		t.Error("Get after Invalidate must miss")
	}
}

func TestHotFilesCache_DisabledCacheSafe(t *testing.T) {
	c := NewHotFilesCache(0) // cap=0 → disabled but safe
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	writeFile(t, path, "y")
	c.Put(path, []byte("y"))
	if c.Stats().EntryCount != 0 {
		t.Error("cap=0 cache must not store entries")
	}
	if _, ok := c.Get(path); ok {
		t.Error("cap=0 cache must miss")
	}
}

func TestHotFilesCache_ConcurrentSafety(t *testing.T) {
	dir := t.TempDir()
	c := NewHotFilesCache(10_000)
	paths := make([]string, 20)
	for i := range paths {
		p := filepath.Join(dir, "f"+itoa(i)+".txt")
		writeFile(t, p, "content"+itoa(i))
		paths[i] = p
	}
	// 8 goroutines doing 1000 random Get/Put each.
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				p := paths[(g+i)%len(paths)]
				if i%3 == 0 {
					data, _ := os.ReadFile(p)
					c.Put(p, data)
				} else {
					c.Get(p)
				}
			}
		}(g)
	}
	wg.Wait()
	// No assertions on counts — race-free completion is the assertion.
	if c.Stats().EntryCount > len(paths) {
		t.Errorf("entry count %d exceeds path universe %d", c.Stats().EntryCount, len(paths))
	}
}
