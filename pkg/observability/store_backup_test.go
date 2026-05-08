// pkg/observability/store_backup_test.go — backup + rotation tests.
// [PILAR-XXVII/242.K]

package observability

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStore_CreateBackup_RoundTrip — backup is a valid bbolt DB with the
// same records as the live DB.
func TestStore_CreateBackup_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Seed data.
	s.RecordCall("backed_up", "x", 500*time.Nanosecond, "ok", "", 5, 10)
	if err := s.flushNow(); err != nil {
		t.Fatalf("flushNow: %v", err)
	}

	backupPath := filepath.Join(dir, ".neo", "db", "observability-20260418.db.bak")
	if err := s.CreateBackup(backupPath); err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}
	if s.LastBackupAt().IsZero() {
		t.Error("LastBackupAt not updated after successful backup")
	}
	_ = s.Close()

	// Re-open the BACKUP as if it were the live DB by copying it over
	// the canonical location, then Open a fresh Store. This verifies the
	// snapshot is self-sufficient.
	livePath := filepath.Join(dir, ".neo", "db", "observability.db")
	if err := os.Remove(livePath); err != nil {
		t.Fatalf("remove live: %v", err)
	}
	if err := copyFile(backupPath, livePath); err != nil {
		t.Fatalf("copy backup → live: %v", err)
	}

	restored, err := Open(dir)
	if err != nil {
		t.Fatalf("re-Open from backup: %v", err)
	}
	defer restored.Close()

	aggs := restored.ToolAggregates()
	if agg, ok := aggs["backed_up"]; !ok || agg.Calls != 1 {
		t.Errorf("backup did not preserve aggregates: %+v", aggs)
	}
}

// TestStore_RotateBackups_Rolling — once there are more than keepN backup
// files, the oldest get pruned lexicographically.
func TestStore_RotateBackups_Rolling(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Create 10 backups with dated filenames so sort.Strings orders them.
	dbDir := filepath.Dir(s.Path())
	days := []string{
		"20260401", "20260402", "20260403", "20260404", "20260405",
		"20260406", "20260407", "20260408", "20260409", "20260410",
	}
	for _, d := range days {
		p := filepath.Join(dbDir, "observability-"+d+".db.bak")
		if err := s.CreateBackup(p); err != nil {
			t.Fatalf("CreateBackup %s: %v", d, err)
		}
	}

	removed, err := s.RotateBackups(7)
	if err != nil {
		t.Fatalf("RotateBackups: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed = %d, want 3", removed)
	}

	// The 7 newest must survive; the 3 oldest must be gone.
	for i, d := range days {
		p := filepath.Join(dbDir, "observability-"+d+".db.bak")
		_, err := os.Stat(p)
		exists := err == nil
		shouldExist := i >= 3 // last 7
		if exists != shouldExist {
			t.Errorf("day %s: exists=%v, shouldExist=%v", d, exists, shouldExist)
		}
	}
}

// TestStore_RotateBackups_KeepAll — when fewer backups than keepN exist,
// nothing is removed.
func TestStore_RotateBackups_KeepAll(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	dbDir := filepath.Dir(s.Path())
	for _, d := range []string{"20260401", "20260402"} {
		p := filepath.Join(dbDir, "observability-"+d+".db.bak")
		if err := s.CreateBackup(p); err != nil {
			t.Fatalf("CreateBackup: %v", err)
		}
	}
	removed, err := s.RotateBackups(7)
	if err != nil {
		t.Fatalf("RotateBackups: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}

// TestStore_CreateBackup_NilStore — nil store returns error, no panic.
func TestStore_CreateBackup_NilStore(t *testing.T) {
	var s *Store
	if err := s.CreateBackup("/tmp/shouldnotexist.bak"); err == nil {
		t.Error("expected error from nil store, got nil")
	}
}

// BenchmarkBackup_SmallDB — seeds ~100 calls, measures snapshot time.
func BenchmarkBackup_SmallDB(b *testing.B) {
	dir := b.TempDir()
	s, err := Open(dir)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer s.Close()
	for i := 0; i < 100; i++ {
		s.RecordCall("seed", "x", 100*time.Nanosecond, "ok", "", 1, 1)
	}
	if err := s.flushNow(); err != nil {
		b.Fatalf("flushNow: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst := filepath.Join(dir, "bench.bak")
		if err := s.CreateBackup(dst); err != nil {
			b.Fatalf("CreateBackup: %v", err)
		}
		_ = os.Remove(dst)
	}
}

// copyFile is a minimal testing helper.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src) //nolint:gosec // test helper, paths from t.TempDir
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}
