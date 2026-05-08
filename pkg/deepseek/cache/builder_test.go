package cache

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSameFilesSameKey(t *testing.T) {
	dir := t.TempDir()
	f1 := writeFile(t, dir, "a.go", "package a")
	f2 := writeFile(t, dir, "b.go", "package b")

	tr := NewTracker()
	k1 := tr.Snapshot([]string{f1, f2})
	k2 := tr.Snapshot([]string{f2, f1}) // reversed order
	if k1 != k2 {
		t.Errorf("key mismatch: %s vs %s", k1, k2)
	}
}

func TestFileChangeNewKey(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "x.go", "v1")

	tr := NewTracker()
	k1 := tr.Snapshot([]string{f})

	writeFile(t, dir, "x.go", "v2")
	k2 := tr.Snapshot([]string{f})

	if k1 == k2 {
		t.Error("key must change after file mutation")
	}
}

func TestBlock1Truncation(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "big.go", strings.Repeat("x", 5000))

	b := NewBuilder("SYS", "", 100, time.Minute)
	block1, _, _ := b.BuildBlock1([]string{f})
	if len(block1) > 100 {
		t.Errorf("block1 len=%d, want ≤100", len(block1))
	}
}

func TestAssemblePromptFormat(t *testing.T) {
	b := NewBuilder("", "", 80000, time.Minute)
	result := b.AssemblePrompt("BLOCK1", "do the thing")
	want := "BLOCK1\n\n---TASK---\n\ndo the thing"
	if result != want {
		t.Errorf("got %q, want %q", result, want)
	}
}

func TestConcurrentSnapshot(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for i := range 10 {
		paths = append(paths, writeFile(t, dir, "f"+string(rune('a'+i))+".go", "pkg"))
	}
	tr := NewTracker()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			tr.Snapshot(paths)
		})
	}
	wg.Wait()
	// No race or panic — test passes if it completes.
}

func TestCacheTTLExpiry(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "z.go", "pkg z")

	b := NewBuilder("SYS", "", 80000, 50*time.Millisecond)
	_, _, hit1 := b.BuildBlock1([]string{f})
	if hit1 {
		t.Error("first call must not be a cache hit")
	}
	_, _, hit2 := b.BuildBlock1([]string{f})
	if !hit2 {
		t.Error("second call with same key must be a cache hit")
	}
	time.Sleep(80 * time.Millisecond) // wait for TTL to expire
	_, _, hit3 := b.BuildBlock1([]string{f})
	if hit3 {
		t.Error("call after TTL expiry must rebuild (not a hit)")
	}
}
