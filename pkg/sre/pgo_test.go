package sre

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writePGOFile(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, []byte("fake profile"), 0o600); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
	if age > 0 {
		past := time.Now().Add(-age)
		if err := os.Chtimes(full, past, past); err != nil {
			t.Fatalf("chtimes %s: %v", full, err)
		}
	}
	return full
}

// TestPrunePGOProfiles_RemovesOld verifies files older than maxAge are removed,
// recent ones kept, and non-.pgo files ignored. [364.C]
func TestPrunePGOProfiles_RemovesOld(t *testing.T) {
	dir := t.TempDir()
	recent := writePGOFile(t, dir, "profile-1000.pgo", 0)                 // just created
	oldish := writePGOFile(t, dir, "profile-1001.pgo", 2*time.Hour)       // 2h old — kept at 24h cutoff
	stale := writePGOFile(t, dir, "profile-1002.pgo", 30*time.Hour)       // stale
	other := writePGOFile(t, dir, "README.md", 0)                         // non-pgo — ignored

	removed, err := PrunePGOProfiles(dir, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	// Stale gone.
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale file not pruned: %s", stale)
	}
	// Others survive.
	for _, keep := range []string{recent, oldish, other} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("unexpected removal of %s: %v", keep, err)
		}
	}
}

// TestPrunePGOProfiles_MissingDir returns (0, nil) when dir absent. [364.C]
func TestPrunePGOProfiles_MissingDir(t *testing.T) {
	removed, err := PrunePGOProfiles(filepath.Join(t.TempDir(), "does-not-exist"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}

// TestLatestPGOProfile returns newest by mtime and "" when none. [364.C]
func TestLatestPGOProfile(t *testing.T) {
	dir := t.TempDir()
	// Empty dir.
	got, err := LatestPGOProfile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("expected empty on empty dir, got %q", got)
	}

	writePGOFile(t, dir, "profile-1000.pgo", 10*time.Hour)
	writePGOFile(t, dir, "profile-1001.pgo", 5*time.Hour)
	newest := writePGOFile(t, dir, "profile-1002.pgo", 0) // newest
	writePGOFile(t, dir, "README.md", 0)                  // ignored

	got, err = LatestPGOProfile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != newest {
		t.Errorf("LatestPGOProfile = %q, want %q", got, newest)
	}
}

// TestContinuousPGOCapture_DisabledReturnsImmediately ensures an interval <= 0
// short-circuits without goroutine leak. [364.C]
func TestContinuousPGOCapture_DisabledReturnsImmediately(t *testing.T) {
	done := make(chan struct{})
	go func() {
		ContinuousPGOCapture(context.Background(), t.TempDir(), 0)
		close(done)
	}()
	select {
	case <-done:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Error("ContinuousPGOCapture did not return on intervalMin=0")
	}
}

// TestContinuousPGOCapture_CreatesDirEvenIfNeverTicks verifies the side-effect
// of MkdirAll is applied upon entry. [364.C]
func TestContinuousPGOCapture_CreatesDirEvenIfNeverTicks(t *testing.T) {
	ws := t.TempDir()
	// 99999 min interval → ticker never fires in test timeframe, but MkdirAll ran.
	ctx, cancel := context.WithCancel(context.Background())
	go ContinuousPGOCapture(ctx, ws, 99999)
	time.Sleep(50 * time.Millisecond)
	cancel()
	if _, err := os.Stat(filepath.Join(ws, ".neo", "pgo")); err != nil {
		t.Errorf(".neo/pgo dir not created: %v", err)
	}
}
