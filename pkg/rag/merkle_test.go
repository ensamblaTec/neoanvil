package rag

import (
	"os"
	"path/filepath"
	"testing"

	"go.etcd.io/bbolt"
)

func TestMerkleTreeBuildAndDiff(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test_merkle.db")

	db, err := bbolt.Open(dbPath, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	defer os.Remove(dbPath)

	bucket := []byte("test_bucket")

	// Create bucket with some data
	err = db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucket)
		if err != nil {
			return err
		}
		b.Put([]byte("key1"), []byte("value1"))
		b.Put([]byte("key2"), []byte("value2"))
		b.Put([]byte("key3"), []byte("value3"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Build tree
	tree, err := BuildMerkleTree(db, bucket)
	if err != nil {
		t.Fatal(err)
	}

	if tree.LeafCount < 3 {
		t.Errorf("expected at least 3 leaves, got %d", tree.LeafCount)
	}

	// Digest should have a root hash
	digest := tree.Digest()
	if digest.RootHash == "" {
		t.Error("expected non-empty root hash")
	}
	if digest.BucketName != "test_bucket" {
		t.Errorf("expected bucket name test_bucket, got %s", digest.BucketName)
	}

	// Export hashes
	hashes := tree.ExportHashes()
	if len(hashes) != len(tree.Nodes) {
		t.Errorf("hash count mismatch: %d vs %d", len(hashes), len(tree.Nodes))
	}

	// Diff against self should produce no diffs
	diffs := tree.DiffAgainst(hashes)
	if len(diffs) != 0 {
		t.Errorf("expected 0 diffs against self, got %d", len(diffs))
	}

	// Modify one entry and rebuild
	err = db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucket)
		return b.Put([]byte("key2"), []byte("MODIFIED"))
	})
	if err != nil {
		t.Fatal(err)
	}

	tree2, err := BuildMerkleTree(db, bucket)
	if err != nil {
		t.Fatal(err)
	}

	// Root hashes should differ
	digest2 := tree2.Digest()
	if digest.RootHash == digest2.RootHash {
		t.Error("expected different root hashes after modification")
	}

	// Diff should find the change
	diffs = tree2.DiffAgainst(hashes)
	if len(diffs) == 0 {
		t.Error("expected diffs after modification, got 0")
	}
}

func TestMerkleTreeEmpty(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test_empty.db")

	db, err := bbolt.Open(dbPath, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	defer os.Remove(dbPath)

	bucket := []byte("empty_bucket")
	db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucket)
		return err
	})

	tree, err := BuildMerkleTree(db, bucket)
	if err != nil {
		t.Fatal(err)
	}

	if tree.LeafCount != 0 {
		t.Errorf("expected 0 leaves for empty bucket, got %d", tree.LeafCount)
	}

	digest := tree.Digest()
	if digest.RootHash != "" {
		t.Errorf("expected empty root hash for empty tree, got %s", digest.RootHash)
	}
}
