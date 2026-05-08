package cpg

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

var bucketCPG = []byte("cpg_cache")

// GraphCache persists and retrieves Graph objects from a BoltDB bucket.
// Cache key = "cpg:<pkgPath>:<maxMtime>" — invalidated automatically when any
// .go file in the package directory has a newer mtime than the stored key.
type GraphCache struct {
	db *bolt.DB
}

// NewGraphCache wraps an existing BoltDB handle. The caller owns the DB lifetime.
func NewGraphCache(db *bolt.DB) (*GraphCache, error) {
	err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketCPG)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("cpg cache: create bucket: %w", err)
	}
	return &GraphCache{db: db}, nil
}

// cacheKey builds the BoltDB key for a given package path and mtime.
func cacheKey(pkgPath string, mtime int64) []byte {
	return fmt.Appendf(nil, "cpg:%s:%d", pkgPath, mtime)
}

// Get retrieves a cached Graph. Returns (nil, false) on miss or stale entry.
func (c *GraphCache) Get(pkgPath string, mtime int64) (*Graph, bool) {
	var g Graph
	key := cacheKey(pkgPath, mtime)

	err := c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketCPG)
		if b == nil {
			return fmt.Errorf("bucket missing")
		}
		data := b.Get(key)
		if data == nil {
			return fmt.Errorf("miss")
		}
		return gob.NewDecoder(bytes.NewReader(data)).Decode(&g)
	})
	if err != nil {
		return nil, false
	}
	// Rebuild the runtime index from persisted Nodes.
	g.edgeSet = make(map[uint64]struct{}, len(g.Edges))
	for _, e := range g.Edges {
		g.edgeSet[edgeKey(e.From, e.To, e.Kind)] = struct{}{}
	}
	g.index = make(map[string]NodeID, len(g.Nodes))
	for _, n := range g.Nodes {
		g.index[n.Package+"."+n.Name] = n.ID
	}
	return &g, true
}

// Put stores a Graph under the given package path and mtime key.
func (c *GraphCache) Put(pkgPath string, mtime int64, g *Graph) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(g); err != nil {
		return fmt.Errorf("cpg cache: encode: %w", err)
	}
	key := cacheKey(pkgPath, mtime)
	return c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketCPG)
		if b == nil {
			return fmt.Errorf("bucket missing")
		}
		return b.Put(key, buf.Bytes())
	})
}

// MaxMtime walks pkgDir and returns the maximum mtime of any *.go file.
// Returns 0 if the directory is empty or unreadable.
// [AUDIT-2026-04-23] Outer walk error is now logged — a silent permission failure
// on pkgDir used to return 0 and flag every build as "cache miss due to unchanged
// mtime", defeating the fast-boot cache with no operator visibility.
func MaxMtime(pkgDir string) int64 {
	var maxT int64
	if werr := filepath.WalkDir(pkgDir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(d.Name()) != ".go" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if t := info.ModTime().UnixNano(); t > maxT {
			maxT = t
		}
		return nil
	}); werr != nil {
		log.Printf("[CPG-WARN] MaxMtime: walk %s failed: %v — cache behaves as if dir is empty", pkgDir, werr)
	}
	return maxT
}

// BuildCached builds the CPG for pkgPattern, returning a cached Graph if the
// package directory has not changed since the last build. A miss builds fresh
// and persists the result. pkgDir must be the absolute directory containing
// the package's *.go files (used for mtime scanning).
func (b *CPGBuilder) BuildCached(pkgPattern, pkgDir string, cache *GraphCache) (*Graph, time.Duration, error) {
	mtime := MaxMtime(pkgDir)
	if g, ok := cache.Get(pkgPattern, mtime); ok {
		return g, 0, nil
	}

	start := time.Now()
	g, err := b.Build(context.Background(), pkgPattern)
	elapsed := time.Since(start)
	if err != nil {
		return nil, elapsed, err
	}

	_ = cache.Put(pkgPattern, mtime, g) // best-effort; ignore persist errors
	return g, elapsed, nil
}
