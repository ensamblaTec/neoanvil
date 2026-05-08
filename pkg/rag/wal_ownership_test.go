package rag

import (
	"path/filepath"
	"testing"
)

// TestSessionPathAnchoredInWorkspace covers the defense-in-depth guard inside
// pkg/rag/wal.go. If the certify-level guard in cmd/neo-mcp is ever bypassed
// by a caller, this guard prevents the WAL bucket from being polluted with
// cross-workspace paths. [Épica 330.L]
func TestSessionPathAnchoredInWorkspace(t *testing.T) {
	wsA := t.TempDir()
	wsB := t.TempDir()

	// Inside → true
	inside := filepath.Join(wsA, "file.go")
	if !sessionPathAnchoredInWorkspace(wsA, inside) {
		t.Errorf("inside path should anchor: ws=%s path=%s", wsA, inside)
	}

	// Outside → false
	outside := filepath.Join(wsB, "file.go")
	if sessionPathAnchoredInWorkspace(wsA, outside) {
		t.Errorf("outside path must NOT anchor: ws=%s path=%s", wsA, outside)
	}

	// .. escape → false
	escape := filepath.Join(wsA, "..", "other", "file.go")
	if sessionPathAnchoredInWorkspace(wsA, escape) {
		t.Errorf(".. escape must NOT anchor: ws=%s path=%s", wsA, escape)
	}

	// workspace itself → true (rel is ".")
	if !sessionPathAnchoredInWorkspace(wsA, wsA) {
		t.Error("workspace dir itself should anchor")
	}
}

// TestAppendSessionCertified_RejectsCrossWorkspacePath — E2E: write with a
// cross-workspace path, verify bucket is NOT polluted. [Épica 330.L]
func TestAppendSessionCertified_RejectsCrossWorkspacePath(t *testing.T) {
	tmp := t.TempDir()
	wal, err := OpenWAL(tmp + "/hnsw.db")
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	defer wal.Close()

	// Simulate vision-link's sessionID + a path from strategos (the leak bug).
	visionWS := "/path/to/vision-link"
	sessionID := visionWS + "|1776983500"
	foreignPath := "/path/to/strategos/backend/internal/domain/services/auth_service.go"

	// AppendSessionCertified must NOT fail — it silently rejects with a log.
	// The caller sees "success" (nil error) but the bucket stays clean.
	if err := wal.AppendSessionCertified(sessionID, foreignPath); err != nil {
		t.Fatalf("AppendSessionCertified should not error on reject, got %v", err)
	}

	// Verify the bucket is empty / doesn't contain the foreign path.
	paths, err := wal.GetSessionMutations(sessionID)
	if err != nil {
		t.Fatalf("GetSessionMutations: %v", err)
	}
	for _, p := range paths {
		if p == foreignPath {
			t.Errorf("foreign path leaked into session_state: %s", p)
		}
	}
	if len(paths) != 0 {
		t.Logf("bucket has %d paths but none should be the foreign one", len(paths))
	}
}

// TestAppendSessionCertified_AcceptsOwnedPath — positive control: path inside
// the sessionID's workspace IS accepted. [Épica 330.L]
func TestAppendSessionCertified_AcceptsOwnedPath(t *testing.T) {
	tmp := t.TempDir()
	wal, err := OpenWAL(tmp + "/hnsw.db")
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	defer wal.Close()

	sessionID := tmp + "|1776983500"
	ownedPath := filepath.Join(tmp, "backend", "handler.go")

	if err := wal.AppendSessionCertified(sessionID, ownedPath); err != nil {
		t.Fatalf("AppendSessionCertified owned path: %v", err)
	}

	paths, _ := wal.GetSessionMutations(sessionID)
	found := false
	for _, p := range paths {
		if p == ownedPath {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("owned path should be persisted, paths=%v", paths)
	}
}
