// Package rag — Merkle Search Trees for BoltDB bucket indexing. [SRE-37.1]
//
// Each BoltDB bucket is hashed into a Merkle tree so two NeoAnvil nodes can
// compare roots in O(1) and exchange only the differing subtrees (delta-sync).
// The tree is a complete binary tree stored in a flat slice (heap-indexed).
package rag

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"

	"go.etcd.io/bbolt"
)

// MerkleNode represents a single node in the Merkle tree.
type MerkleNode struct {
	Hash     [32]byte // SHA-256 of children hashes (internal) or key||value (leaf)
	KeyStart []byte   // first key in this subtree (for range identification)
	KeyEnd   []byte   // last key in this subtree
	IsLeaf   bool
}

// MerkleTree is a flat-array Merkle tree over a BoltDB bucket's key-value pairs.
type MerkleTree struct {
	Nodes      []MerkleNode
	LeafCount  int
	BucketName string
}

// MerkleDigest is a compact wire-format for comparing trees across peers. [SRE-37.2]
type MerkleDigest struct {
	BucketName string `json:"bucket"`
	RootHash   string `json:"root_hash"`
	LeafCount  int    `json:"leaf_count"`
	Depth      int    `json:"depth"`
}

// MerkleDiff describes a range of keys that differ between two peers. [SRE-37.2]
type MerkleDiff struct {
	BucketName string `json:"bucket"`
	NodeIndex  int    `json:"node_index"`
	KeyStart   string `json:"key_start"`
	KeyEnd     string `json:"key_end"`
	LocalHash  string `json:"local_hash"`
	RemoteHash string `json:"remote_hash"`
}

// BuildMerkleTree constructs a Merkle tree from a BoltDB bucket. [SRE-37.1]
// Reads all key-value pairs, hashes each as a leaf, then builds the tree bottom-up.
// The bucket is read in a single View transaction (no lock contention).
func BuildMerkleTree(db *bbolt.DB, bucketName []byte) (*MerkleTree, error) {
	var leaves []MerkleNode

	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		if b == nil {
			return fmt.Errorf("bucket %q not found", bucketName)
		}
		return b.ForEach(func(k, v []byte) error {
			h := sha256.New()
			h.Write(k)
			h.Write(v)
			var hash [32]byte
			copy(hash[:], h.Sum(nil))

			keyCopy := make([]byte, len(k))
			copy(keyCopy, k)

			leaves = append(leaves, MerkleNode{
				Hash:     hash,
				KeyStart: keyCopy,
				KeyEnd:   keyCopy,
				IsLeaf:   true,
			})
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	if len(leaves) == 0 {
		return &MerkleTree{
			BucketName: string(bucketName),
		}, nil
	}

	// Pad to next power of 2 for complete binary tree
	n := nextPow2(len(leaves))
	for len(leaves) < n {
		leaves = append(leaves, MerkleNode{IsLeaf: true}) // empty leaves hash to zero
	}

	// Build tree bottom-up. Tree has 2*n - 1 nodes.
	// Leaves are at indices [n-1, 2*n-2]. Internal nodes at [0, n-2].
	tree := make([]MerkleNode, 2*n-1)
	copy(tree[n-1:], leaves)

	for i := n - 2; i >= 0; i-- {
		left := tree[2*i+1]
		right := tree[2*i+2]

		h := sha256.New()
		h.Write(left.Hash[:])
		h.Write(right.Hash[:])
		var hash [32]byte
		copy(hash[:], h.Sum(nil))

		node := MerkleNode{
			Hash:     hash,
			KeyStart: left.KeyStart,
			KeyEnd:   right.KeyEnd,
			IsLeaf:   false,
		}
		// If right subtree is empty padding, inherit left's KeyEnd
		if len(right.KeyEnd) == 0 {
			node.KeyEnd = left.KeyEnd
		}
		tree[i] = node
	}

	return &MerkleTree{
		Nodes:      tree,
		LeafCount:  len(leaves),
		BucketName: string(bucketName),
	}, nil
}

// Digest returns the compact root hash for wire comparison. [SRE-37.2]
func (mt *MerkleTree) Digest() MerkleDigest {
	d := MerkleDigest{
		BucketName: mt.BucketName,
		LeafCount:  mt.LeafCount,
	}
	if len(mt.Nodes) > 0 {
		d.RootHash = hex.EncodeToString(mt.Nodes[0].Hash[:])
		d.Depth = treeDepth(len(mt.Nodes))
	}
	return d
}

// DiffAgainst compares this tree against a remote tree's node hashes.
// Returns a list of differing subtree ranges. The remote tree is represented
// as a flat slice of hex-encoded hashes (same heap-indexed layout). [SRE-37.2]
func (mt *MerkleTree) DiffAgainst(remoteHashes []string) []MerkleDiff {
	if len(mt.Nodes) == 0 || len(remoteHashes) == 0 {
		return nil
	}

	var diffs []MerkleDiff
	mt.diffWalk(0, remoteHashes, &diffs)
	return diffs
}

func (mt *MerkleTree) diffWalk(idx int, remoteHashes []string, diffs *[]MerkleDiff) {
	if idx >= len(mt.Nodes) || idx >= len(remoteHashes) {
		return
	}

	localHex := hex.EncodeToString(mt.Nodes[idx].Hash[:])
	if localHex == remoteHashes[idx] {
		return // subtree matches, skip
	}

	// If leaf or no children to recurse into, record the diff
	if mt.Nodes[idx].IsLeaf || 2*idx+2 >= len(mt.Nodes) {
		*diffs = append(*diffs, MerkleDiff{
			BucketName: mt.BucketName,
			NodeIndex:  idx,
			KeyStart:   hex.EncodeToString(mt.Nodes[idx].KeyStart),
			KeyEnd:     hex.EncodeToString(mt.Nodes[idx].KeyEnd),
			LocalHash:  localHex,
			RemoteHash: remoteHashes[idx],
		})
		return
	}

	// Recurse into children
	mt.diffWalk(2*idx+1, remoteHashes, diffs)
	mt.diffWalk(2*idx+2, remoteHashes, diffs)
}

// ExportHashes exports all node hashes as hex strings for wire transfer. [SRE-37.2]
func (mt *MerkleTree) ExportHashes() []string {
	hashes := make([]string, len(mt.Nodes))
	for i, n := range mt.Nodes {
		hashes[i] = hex.EncodeToString(n.Hash[:])
	}
	return hashes
}

// BuildAllBucketTrees builds Merkle trees for the critical HNSW buckets. [SRE-37.1]
func BuildAllBucketTrees(db *bbolt.DB) (map[string]*MerkleTree, error) {
	buckets := [][]byte{bucketDocs, bucketNodes, bucketEdges, bucketVectors, bucketDirectives}
	trees := make(map[string]*MerkleTree, len(buckets))

	for _, b := range buckets {
		tree, err := BuildMerkleTree(db, b)
		if err != nil {
			log.Printf("[SRE-MERKLE] Skipping bucket %s: %v", b, err)
			continue
		}
		trees[string(b)] = tree
	}
	return trees, nil
}

// WALMerkleDigests returns compact digests for all critical buckets. [SRE-37.2]
// Used by the gossip protocol to quickly compare state between peers.
func (wal *WAL) MerkleDigests() ([]MerkleDigest, error) {
	trees, err := BuildAllBucketTrees(wal.db)
	if err != nil {
		return nil, err
	}
	digests := make([]MerkleDigest, 0, len(trees))
	for _, t := range trees {
		digests = append(digests, t.Digest())
	}
	return digests, nil
}

// MerkleTreeForBucket builds and returns the Merkle tree for a specific bucket.
func (wal *WAL) MerkleTreeForBucket(bucketName string) (*MerkleTree, error) {
	return BuildMerkleTree(wal.db, []byte(bucketName))
}

// ExportBucketData exports all key-value pairs from a bucket within a key range.
// Used by delta-sync to transfer only the differing data. [SRE-37.2]
func (wal *WAL) ExportBucketData(bucketName string, keyStart, keyEnd []byte) (map[string][]byte, error) {
	result := make(map[string][]byte)
	err := wal.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		if b == nil {
			return fmt.Errorf("bucket %q not found", bucketName)
		}
		c := b.Cursor()
		for k, v := c.Seek(keyStart); k != nil; k, v = c.Next() {
			if len(keyEnd) > 0 && string(k) > string(keyEnd) {
				break
			}
			keyCopy := make([]byte, len(k))
			copy(keyCopy, k)
			valCopy := make([]byte, len(v))
			copy(valCopy, v)
			result[hex.EncodeToString(keyCopy)] = valCopy
		}
		return nil
	})
	return result, err
}

// ImportBucketData writes key-value pairs into a bucket. [SRE-37.2]
// Used by delta-sync to apply received data from a remote peer.
func (wal *WAL) ImportBucketData(bucketName string, data map[string][]byte) error {
	if len(data) == 0 {
		return nil
	}
	return wal.db.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		if b == nil {
			return fmt.Errorf("bucket %q not found", bucketName)
		}
		for hexKey, val := range data {
			key, err := hex.DecodeString(hexKey)
			if err != nil {
				continue
			}
			if err := b.Put(key, val); err != nil {
				return err
			}
		}
		return nil
	})
}

func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

func treeDepth(totalNodes int) int {
	d := 0
	n := totalNodes
	for n > 1 {
		n /= 2
		d++
	}
	return d
}
