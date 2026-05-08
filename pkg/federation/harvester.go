// pkg/federation/harvester.go — Knowledge Harvester for federated dream synthesis. [SRE-94.A]
//
// Exports daily memex vectors from the local HNSW graph and collects vectors
// from remote fleet nodes via HTTP. Used by the Nexus dream synthesis pipeline.
package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// MemexVector represents a single lesson-learned vector for cross-node exchange. [SRE-94.A.1]
type MemexVector struct {
	ID        string    `json:"id"`
	Embedding []float32 `json:"embedding"`
	Topic     string    `json:"topic"`
	Scope     string    `json:"scope"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
	NodeID    string    `json:"node_id"`
}

// MemexExporter is the interface for extracting vectors from the local HNSW graph.
// Implemented by the RAG package to avoid circular imports.
type MemexExporter interface {
	// ExportSince returns all vectors inserted after the given timestamp.
	ExportSince(since time.Time) []MemexVector
}

// ExportDailyMemex extracts vectors inserted in the last 24h from the local
// HNSW graph. Returns them tagged with the local node ID. [SRE-94.A.1]
func ExportDailyMemex(exporter MemexExporter, nodeID string) []MemexVector {
	since := time.Now().Add(-24 * time.Hour)
	vectors := exporter.ExportSince(since)

	// Tag each vector with this node's ID.
	for i := range vectors {
		vectors[i].NodeID = nodeID
	}

	return vectors
}

// FleetNode represents a remote node for memex collection.
type FleetNode struct {
	ID      string
	Host    string
	Port    int
	UseTLS  bool
	Healthy bool
}

// CollectFleetMemex iterates all healthy fleet nodes and fetches their daily
// memex vectors via GET /api/v1/memex/daily. [SRE-94.A.3]
// [Épica 232.B] timeoutSec is now applied per-request via a context deadline
// so a single slow node can't block the whole harvest loop. A zero value
// defaults to 300 s — historical behaviour for backward compat.
func CollectFleetMemex(nodes []FleetNode, client *http.Client, timeoutSec int) []MemexVector {
	if timeoutSec <= 0 {
		timeoutSec = 300
	}
	perRequestTimeout := time.Duration(timeoutSec) * time.Second

	var allVectors []MemexVector
	collected := 0

	for _, node := range nodes {
		if !node.Healthy {
			continue
		}

		scheme := "http"
		if node.UseTLS {
			scheme = "https"
		}
		url := fmt.Sprintf("%s://%s:%d/api/v1/memex/daily", scheme, node.Host, node.Port)

		ctx, cancel := context.WithTimeout(context.Background(), perRequestTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			log.Printf("[FEDERATION] failed to create request for node %s: %v", node.ID, err)
			continue
		}

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			log.Printf("[FEDERATION] failed to collect from node %s: %v", node.ID, err)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			cancel()
			log.Printf("[FEDERATION] node %s returned status %d", node.ID, resp.StatusCode)
			continue
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 50*1024*1024)) // 50MB max per node
		resp.Body.Close()
		cancel()
		if err != nil {
			log.Printf("[FEDERATION] failed to read body from node %s: %v", node.ID, err)
			continue
		}

		var vectors []MemexVector
		if err := json.Unmarshal(body, &vectors); err != nil {
			log.Printf("[FEDERATION] failed to decode vectors from node %s: %v", node.ID, err)
			continue
		}

		allVectors = append(allVectors, vectors...)
		collected++
		log.Printf("[FEDERATION] collected %d vectors from node %s", len(vectors), node.ID)
	}

	log.Printf("[NEXUS-EVENT] memex_collected nodes=%d vectors=%d", collected, len(allVectors))
	return allVectors
}
