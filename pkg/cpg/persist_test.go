package cpg

import (
	"encoding/gob"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCPGPersistRoundTrip verifies SaveCPG + LoadCPG preserves node/edge count. [Épica 262.A/B]
func TestCPGPersistRoundTrip(t *testing.T) {
	g := newGraph()
	g.addNode(Node{Kind: NodeFunc, Package: "pkg/a", Name: "Foo", Line: 10})
	g.addNode(Node{Kind: NodeFunc, Package: "pkg/b", Name: "Bar", Line: 20})
	g.addEdge(NodeID(0), NodeID(1), EdgeCall)

	dir := t.TempDir()
	path := filepath.Join(dir, "cpg.bin")

	if err := SaveCPG(g, path); err != nil {
		t.Fatalf("SaveCPG: %v", err)
	}
	// Verify file exists with restricted perms.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if info.Mode()&0o777 != 0o600 {
		t.Errorf("file perms = %o, want 0600", info.Mode()&0o777)
	}

	loaded, hdr, err := LoadCPG(path)
	if err != nil {
		t.Fatalf("LoadCPG: %v", err)
	}
	if len(loaded.Nodes) != len(g.Nodes) {
		t.Errorf("nodes: got %d want %d", len(loaded.Nodes), len(g.Nodes))
	}
	if len(loaded.Edges) != len(g.Edges) {
		t.Errorf("edges: got %d want %d", len(loaded.Edges), len(g.Edges))
	}
	if hdr.Version != cpgSchemaVersion {
		t.Errorf("header version %d != %d", hdr.Version, cpgSchemaVersion)
	}
	// Index must be rebuilt so NodeByName works after load.
	if _, ok := loaded.NodeByName("pkg/a", "Foo"); !ok {
		t.Error("index not rebuilt: pkg/a.Foo not found after LoadCPG")
	}
}

// TestCPGSchemaMismatch verifies ErrSchemaMismatch is returned for a stale schema. [Épica 262.B]
func TestCPGSchemaMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cpg.bin")

	// Write a header with a future (invalid) version number.
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := gob.NewEncoder(f)
	if encErr := enc.Encode(cpgHeader{Version: 9999, BuildAtUnix: time.Now().Unix()}); encErr != nil {
		f.Close()
		t.Fatal(encErr)
	}
	f.Close()

	_, _, loadErr := LoadCPG(path)
	if loadErr != ErrSchemaMismatch {
		t.Errorf("expected ErrSchemaMismatch, got %v", loadErr)
	}
}

// TestIsCPGStale verifies staleness detection based on file mtime vs BuildAtUnix. [Épica 262.C]
func TestIsCPGStale(t *testing.T) {
	dir := t.TempDir()
	goFile := filepath.Join(dir, "main.go")
	if err := os.WriteFile(goFile, []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Header built an hour ago — file is newer → stale.
	oldHdr := cpgHeader{Version: cpgSchemaVersion, BuildAtUnix: time.Now().Unix() - 3600}
	if !IsCPGStale(dir, oldHdr) {
		t.Error("expected stale=true for old BuildAtUnix")
	}

	// Header built in the future — file is older → not stale.
	freshHdr := cpgHeader{Version: cpgSchemaVersion, BuildAtUnix: time.Now().Unix() + 3600}
	if IsCPGStale(dir, freshHdr) {
		t.Error("expected stale=false for fresh BuildAtUnix")
	}

	// Zero BuildAtUnix → always stale.
	zeroHdr := cpgHeader{}
	if !IsCPGStale(dir, zeroHdr) {
		t.Error("expected stale=true for zero BuildAtUnix")
	}
}
