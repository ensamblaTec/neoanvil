package auth

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func openLog(t *testing.T) (*AuditLog, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	log, err := OpenAuditLog(path)
	if err != nil {
		t.Fatalf("OpenAuditLog: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log, path
}

func TestAuditLog_AppendAndVerify(t *testing.T) {
	log, _ := openLog(t)

	for i := range 3 {
		_, err := log.Append(Event{
			Kind:     "credential_use",
			Actor:    "test",
			Provider: "jira",
			Tool:     "jira/transition",
			Details:  map[string]any{"ticket": fmt.Sprintf("PROJ-%d", i+1)},
		})
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := log.Verify(); err != nil {
		t.Errorf("Verify clean: %v", err)
	}
}

func TestAuditLog_GenesisFirstEntry(t *testing.T) {
	log, _ := openLog(t)

	entry, err := log.Append(Event{Kind: "first"})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if entry.Seq != 1 {
		t.Errorf("Seq=%d want 1", entry.Seq)
	}
	if entry.PrevHash != genesisPrevHash {
		t.Errorf("PrevHash=%q want %q", entry.PrevHash, genesisPrevHash)
	}
	if entry.Hash == "" {
		t.Error("Hash should be set")
	}
}

func TestAuditLog_HashChainsCorrectly(t *testing.T) {
	log, _ := openLog(t)

	first, _ := log.Append(Event{Kind: "a"})
	second, _ := log.Append(Event{Kind: "b"})

	if second.PrevHash != first.Hash {
		t.Errorf("second.PrevHash=%q want %q", second.PrevHash, first.Hash)
	}
	if second.Seq != 2 {
		t.Errorf("second.Seq=%d want 2", second.Seq)
	}
}

func TestAuditLog_PersistsAcrossOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")

	log1, err := OpenAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := log1.Append(Event{Kind: "first"})
	_ = log1.Close()

	log2, err := OpenAuditLog(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer log2.Close()

	second, err := log2.Append(Event{Kind: "second"})
	if err != nil {
		t.Fatalf("Append after reopen: %v", err)
	}
	if second.Seq != 2 {
		t.Errorf("Seq=%d after reopen, want 2", second.Seq)
	}
	if second.PrevHash != first.Hash {
		t.Errorf("PrevHash=%q want %q (chain broken across reopen)", second.PrevHash, first.Hash)
	}
	if err := log2.Verify(); err != nil {
		t.Errorf("Verify after reopen: %v", err)
	}
}

func TestAuditLog_DetectsTamperedField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	log, _ := OpenAuditLog(path)
	_, _ = log.Append(Event{Kind: "a", Actor: "actor1"})
	_, _ = log.Append(Event{Kind: "a", Actor: "actor2"})
	_, _ = log.Append(Event{Kind: "a", Actor: "actor3"})
	_ = log.Close()

	// Tamper the middle entry's Actor without recomputing Hash.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(raw, []byte(`"actor":"actor2"`), []byte(`"actor":"EVIL"`), 1)
	if bytes.Equal(raw, tampered) {
		t.Fatal("test setup failed: substitution did not occur")
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}

	log2, err := OpenAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log2.Close()
	err = log2.Verify()
	if err == nil {
		t.Fatal("expected Verify to detect tampering")
	}
	if !strings.Contains(err.Error(), "seq=2") {
		t.Errorf("expected error to name seq=2, got %v", err)
	}
}

func TestAuditLog_DetectsSequenceGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	log, _ := OpenAuditLog(path)
	_, _ = log.Append(Event{Kind: "a"})
	_, _ = log.Append(Event{Kind: "b"})
	_ = log.Close()

	// Rewrite the second entry with seq=5 instead of 2.
	raw, _ := os.ReadFile(path)
	tampered := bytes.Replace(raw, []byte(`"seq":2`), []byte(`"seq":5`), 1)
	_ = os.WriteFile(path, tampered, 0o600)

	log2, err := OpenAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log2.Close()
	if err := log2.Verify(); err == nil {
		t.Error("expected Verify to detect sequence gap")
	}
}

func TestAuditLog_VerifyEmptyFile(t *testing.T) {
	log, _ := openLog(t)
	if err := log.Verify(); err != nil {
		t.Errorf("Verify on empty log: %v", err)
	}
}

func TestAuditLog_AppendAfterCloseFails(t *testing.T) {
	log, _ := openLog(t)
	_ = log.Close()
	if _, err := log.Append(Event{Kind: "x"}); err == nil {
		t.Error("Append after Close should fail")
	}
}

func TestAuditLog_ConcurrentAppendsSerialize(t *testing.T) {
	log, _ := openLog(t)

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, _ = log.Append(Event{Kind: "concurrent", Details: map[string]any{"n": n}})
		}(i)
	}
	wg.Wait()

	if err := log.Verify(); err != nil {
		t.Errorf("Verify after concurrent appends: %v", err)
	}
}
