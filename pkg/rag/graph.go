package rag

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.etcd.io/bbolt"
)

type GraphEdge struct {
	SourceNode string `json:"source"`
	TargetNode string `json:"target"`
	Relation   string `json:"relation"`
}

func InitGraphRAG(wal *WAL) error {
	return wal.db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("GRAPH_EDGES"))
		return err
	})
}

func GetImpactedNodes(wal *WAL, target string) ([]string, error) {
	var impacted []string
	err := wal.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("GRAPH_EDGES"))
		if b == nil {
			return fmt.Errorf("GRAPH_EDGES bucket not found")
		}

		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var edge GraphEdge
			if err := json.Unmarshal(v, &edge); err == nil {
				if edge.TargetNode == target {
					impacted = append(impacted, edge.SourceNode)
				}
			}
		}
		return nil
	})
	return impacted, err
}

func SaveGraphEdges(wal *WAL, edges []GraphEdge) error {
	return wal.db.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("GRAPH_EDGES"))
		if b == nil {
			return fmt.Errorf("GRAPH_EDGES bucket not found")
		}
		for _, edge := range edges {
			data, err := json.Marshal(edge)
			if err != nil {
				continue
			}

			key := []byte(edge.SourceNode + "->" + edge.TargetNode)
			if err := b.Put(key, data); err != nil {
				return err
			}
		}
		return nil
	})
}

// IndexCoverage returns the fraction of non-test .go files in workspace that have
// a corresponding node in the HNSW graph. Returns 0.0 on empty workspace or nil graph.
// O(files) — only called on BLAST_RADIUS fallback, not on hot paths.
func IndexCoverage(g *Graph, workspace string) float64 {
	if g == nil || workspace == "" {
		return 0.0
	}
	indexed := len(g.Nodes)

	var total int
	_ = filepath.Walk(workspace, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(workspace, path)
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(path, ".go") &&
			!strings.HasSuffix(path, "_test.go") &&
			!strings.Contains(rel, "/vendor/") &&
			!strings.HasPrefix(rel, ".neo/") {
			total++
		}
		return nil
	})

	if total == 0 {
		return 0.0
	}
	if indexed > total {
		indexed = total // cap at 100%
	}
	return float64(indexed) / float64(total)
}

// GetAllGraphEdges extrae la topología completa desde BoltDB a la RAM.
func GetAllGraphEdges(wal *WAL) (map[string][]string, error) {
	edges := make(map[string][]string)
	err := wal.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("GRAPH_EDGES"))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var edge GraphEdge
			if err := json.Unmarshal(v, &edge); err == nil {
				edges[edge.SourceNode] = append(edges[edge.SourceNode], edge.TargetNode)
			}
		}
		return nil
	})
	return edges, err
}
