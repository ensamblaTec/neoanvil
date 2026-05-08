// Package rag — Hypergraph Brain: multidimensional relationships. [SRE-42]
//
// Extends the HNSW vector graph with typed hyper-edges that connect code nodes,
// error events, and hardware states in a single unified structure. This enables
// "butterfly effect" analysis: predict the ripple impact of a code change by
// tracing hyper-edges across dimensions.
//
// Dimensions:
//   - CODE:     source files, functions, modules
//   - ERROR:    error signatures, stack traces, panics
//   - HARDWARE: heap states, GC events, RAPL readings
//   - CAUSAL:   intent chains, decision paths
package rag

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// HyperEdge connects N nodes across M dimensions. [SRE-42.1]
type HyperEdge struct {
	ID        string     `json:"id"`
	Nodes     []HyperRef `json:"nodes"`     // the connected nodes
	EdgeType  string     `json:"edge_type"` // "code_error", "error_hardware", "code_causal", etc.
	Weight    float64    `json:"weight"`    // strength of relationship (0-1)
	CreatedAt int64      `json:"created_at"`
	Metadata  string     `json:"metadata"`  // JSON blob for extra context
}

// HyperRef is a reference to a node in a specific dimension. [SRE-42.1]
type HyperRef struct {
	Dimension string `json:"dimension"` // "code", "error", "hardware", "causal"
	NodeID    string `json:"node_id"`   // file path, error signature, metric name, etc.
	Label     string `json:"label"`     // human-readable label
}

// HyperGraph is the multidimensional relationship graph. [SRE-42.1]
type HyperGraph struct {
	mu            sync.RWMutex
	edges         map[string]*HyperEdge  // edge ID → edge
	index         map[string][]string    // node key ("dim:id") → edge IDs
	maxDepth      int                    // from HyperGraphConfig.MaxImpactDepth
	riskDecay     float64                // from HyperGraphConfig.RiskDecayFactor
	minRiskThresh float64                // from HyperGraphConfig.MinRiskThreshold
}

// ImpactPrediction is the result of a butterfly effect analysis. [SRE-42.2]
type ImpactPrediction struct {
	SourceNode   HyperRef           `json:"source_node"`
	ImpactedNodes []ImpactedNode    `json:"impacted_nodes"`
	TotalRisk    float64            `json:"total_risk"`    // 0-1 composite risk score
	Depth        int                `json:"depth"`         // how many hops the analysis went
	EdgeCount    int                `json:"edge_count"`    // total edges traversed
}

// ImpactedNode describes a node that could be affected by a change. [SRE-42.2]
type ImpactedNode struct {
	Node       HyperRef `json:"node"`
	Risk       float64  `json:"risk"`        // propagated risk score (0-1)
	Hops       int      `json:"hops"`        // distance from source
	PathEdges  []string `json:"path_edges"`  // edge IDs in the impact path
	Dimension  string   `json:"dimension"`   // which dimension this impact is in
}

// NewHyperGraph creates an empty hyper-graph. [SRE-42.1]
// cfg fields: MaxImpactDepth, RiskDecayFactor, MinRiskThreshold.
func NewHyperGraph(maxDepth int, riskDecay, minRisk float64) *HyperGraph {
	if maxDepth <= 0 {
		maxDepth = 5
	}
	if riskDecay <= 0 {
		riskDecay = 0.7
	}
	if minRisk <= 0 {
		minRisk = 0.01
	}
	return &HyperGraph{
		edges:        make(map[string]*HyperEdge),
		index:        make(map[string][]string),
		maxDepth:     maxDepth,
		riskDecay:    riskDecay,
		minRiskThresh: minRisk,
	}
}

// AddEdge inserts a hyper-edge connecting nodes across dimensions. [SRE-42.1]
func (hg *HyperGraph) AddEdge(edge HyperEdge) {
	hg.mu.Lock()
	defer hg.mu.Unlock()

	if edge.ID == "" {
		edge.ID = fmt.Sprintf("he_%d", time.Now().UnixNano())
	}
	if edge.CreatedAt == 0 {
		edge.CreatedAt = time.Now().Unix()
	}

	hg.edges[edge.ID] = &edge

	// Index each node
	for _, ref := range edge.Nodes {
		key := nodeKey(ref)
		hg.index[key] = append(hg.index[key], edge.ID)
	}
}

// RecordCodeError creates a hyper-edge between a code file and an error. [SRE-42.1]
func (hg *HyperGraph) RecordCodeError(filePath, errorSig string, weight float64) {
	hg.AddEdge(HyperEdge{
		Nodes: []HyperRef{
			{Dimension: "code", NodeID: filePath, Label: filePath},
			{Dimension: "error", NodeID: errorSig, Label: errorSig},
		},
		EdgeType: "code_error",
		Weight:   weight,
	})
}

// RecordErrorHardware creates a hyper-edge between an error and hardware state. [SRE-42.1]
func (hg *HyperGraph) RecordErrorHardware(errorSig string, heapMB float64, gcRuns uint32) {
	hwID := fmt.Sprintf("heap_%.0f_gc_%d", heapMB, gcRuns)
	hg.AddEdge(HyperEdge{
		Nodes: []HyperRef{
			{Dimension: "error", NodeID: errorSig, Label: errorSig},
			{Dimension: "hardware", NodeID: hwID, Label: fmt.Sprintf("Heap:%.0fMB GC:%d", heapMB, gcRuns)},
		},
		EdgeType: "error_hardware",
		Weight:   0.7,
	})
}

// RecordCodeCausal creates a hyper-edge between code and a causal chain entry. [SRE-42.1]
func (hg *HyperGraph) RecordCodeCausal(filePath, memexID, reason string) {
	hg.AddEdge(HyperEdge{
		Nodes: []HyperRef{
			{Dimension: "code", NodeID: filePath, Label: filePath},
			{Dimension: "causal", NodeID: memexID, Label: reason},
		},
		EdgeType: "code_causal",
		Weight:   0.8,
	})
}

// PredictImpact performs butterfly effect analysis from a source node. [SRE-42.2]
// Traverses hyper-edges up to maxDepth hops, propagating risk scores.
// Risk decays by edgeWeight * 0.7 per hop (geometric attenuation).
func (hg *HyperGraph) PredictImpact(source HyperRef, maxDepth int) ImpactPrediction {
	if maxDepth <= 0 {
		maxDepth = hg.maxDepth
	}

	hg.mu.RLock()
	defer hg.mu.RUnlock()

	prediction := ImpactPrediction{
		SourceNode: source,
	}

	visited := make(map[string]bool)
	sourceKey := nodeKey(source)
	visited[sourceKey] = true

	type frontier struct {
		ref   HyperRef
		risk  float64
		hops  int
		path  []string
	}

	queue := []frontier{{ref: source, risk: 1.0, hops: 0}}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if current.hops >= maxDepth {
			continue
		}

		key := nodeKey(current.ref)
		edgeIDs := hg.index[key]

		for _, eid := range edgeIDs {
			edge := hg.edges[eid]
			if edge == nil {
				continue
			}
			prediction.EdgeCount++

			for _, neighbor := range edge.Nodes {
				nkey := nodeKey(neighbor)
				if visited[nkey] {
					continue
				}
				visited[nkey] = true

				propagatedRisk := current.risk * edge.Weight * hg.riskDecay
				if propagatedRisk < hg.minRiskThresh {
					continue // risk too small to propagate
				}

				newPath := make([]string, len(current.path)+1)
				copy(newPath, current.path)
				newPath[len(current.path)] = eid

				impacted := ImpactedNode{
					Node:      neighbor,
					Risk:      propagatedRisk,
					Hops:      current.hops + 1,
					PathEdges: newPath,
					Dimension: neighbor.Dimension,
				}
				prediction.ImpactedNodes = append(prediction.ImpactedNodes, impacted)

				queue = append(queue, frontier{
					ref:  neighbor,
					risk: propagatedRisk,
					hops: current.hops + 1,
					path: newPath,
				})
			}
		}
	}

	// Sort by risk descending
	sort.Slice(prediction.ImpactedNodes, func(i, j int) bool {
		return prediction.ImpactedNodes[i].Risk > prediction.ImpactedNodes[j].Risk
	})

	// Compute total risk (capped at 1.0)
	for _, n := range prediction.ImpactedNodes {
		prediction.TotalRisk += n.Risk
	}
	prediction.TotalRisk = math.Min(1.0, prediction.TotalRisk)
	prediction.Depth = maxDepth

	return prediction
}

// GetEdgesForNode returns all hyper-edges connected to a specific node. [SRE-42.1]
func (hg *HyperGraph) GetEdgesForNode(ref HyperRef) []*HyperEdge {
	hg.mu.RLock()
	defer hg.mu.RUnlock()

	key := nodeKey(ref)
	edgeIDs := hg.index[key]
	edges := make([]*HyperEdge, 0, len(edgeIDs))
	for _, eid := range edgeIDs {
		if e := hg.edges[eid]; e != nil {
			edges = append(edges, e)
		}
	}
	return edges
}

// Stats returns summary statistics of the hyper-graph. [SRE-42.1]
type HyperGraphStats struct {
	TotalEdges     int            `json:"total_edges"`
	TotalNodes     int            `json:"total_nodes"`
	ByDimension    map[string]int `json:"by_dimension"`
	ByEdgeType     map[string]int `json:"by_edge_type"`
	AvgEdgesPerNode float64      `json:"avg_edges_per_node"`
}

func (hg *HyperGraph) Stats() HyperGraphStats {
	hg.mu.RLock()
	defer hg.mu.RUnlock()

	stats := HyperGraphStats{
		TotalEdges:  len(hg.edges),
		TotalNodes:  len(hg.index),
		ByDimension: make(map[string]int),
		ByEdgeType:  make(map[string]int),
	}

	for _, edge := range hg.edges {
		stats.ByEdgeType[edge.EdgeType]++
		for _, ref := range edge.Nodes {
			stats.ByDimension[ref.Dimension]++
		}
	}

	if stats.TotalNodes > 0 {
		totalEdgeRefs := 0
		for _, eids := range hg.index {
			totalEdgeRefs += len(eids)
		}
		stats.AvgEdgesPerNode = float64(totalEdgeRefs) / float64(stats.TotalNodes)
	}

	return stats
}

func nodeKey(ref HyperRef) string {
	return ref.Dimension + ":" + ref.NodeID
}

// TopologyNode is a serializable node in the topology snapshot. [SRE-45.1]
type TopologyNode struct {
	Key          string  `json:"key"`           // "dimension:nodeID"
	Dimension    string  `json:"dimension"`
	NodeID       string  `json:"node_id"`
	Label        string  `json:"label"`
	EdgeCount    int     `json:"edge_count"`
	EntropyScore float64 `json:"entropy_score"` // edge_count / log2(edge_count+2)
	IsHotspot    bool    `json:"is_hotspot"`    // entropy_score > hotspot_threshold
}

// TopologySnapshot is the full serializable topology. [SRE-45.1]
type TopologySnapshot struct {
	Nodes            []TopologyNode       `json:"nodes"`
	Edges            []*HyperEdge         `json:"edges"`
	HotspotThreshold float64              `json:"hotspot_threshold"`
	TotalNodes       int                  `json:"total_nodes"`
	TotalEdges       int                  `json:"total_edges"`
	GeneratedAt      int64                `json:"generated_at"`
}

// Topology exports the full graph as a serializable snapshot with entropy scores. [SRE-45.1/45.2]
// EntropyScore = degree / log2(degree+2). Nodes with score > hotspotThreshold are flagged.
func (hg *HyperGraph) Topology(hotspotThreshold float64) TopologySnapshot {
	if hotspotThreshold <= 0 {
		hotspotThreshold = 3.0
	}

	hg.mu.RLock()
	defer hg.mu.RUnlock()

	// Collect unique node labels from edges.
	labelMap := make(map[string]string, len(hg.index))
	for _, edge := range hg.edges {
		for _, ref := range edge.Nodes {
			k := nodeKey(ref)
			if _, exists := labelMap[k]; !exists {
				labelMap[k] = ref.Label
			}
		}
	}

	nodes := make([]TopologyNode, 0, len(hg.index))
	for key, edgeIDs := range hg.index {
		degree := len(edgeIDs)
		entropyScore := float64(degree) / math.Log2(float64(degree)+2)

		// Parse key back into dimension:nodeID
		dim, nid := "", key
		for _, prefix := range []string{"code:", "error:", "hardware:", "causal:"} {
			if len(key) > len(prefix) && key[:len(prefix)] == prefix {
				dim = prefix[:len(prefix)-1]
				nid = key[len(prefix):]
				break
			}
		}
		nodes = append(nodes, TopologyNode{
			Key:          key,
			Dimension:    dim,
			NodeID:       nid,
			Label:        labelMap[key],
			EdgeCount:    degree,
			EntropyScore: entropyScore,
			IsHotspot:    entropyScore > hotspotThreshold,
		})
	}

	// Sort by entropy descending for easy hotspot detection.
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].EntropyScore > nodes[j].EntropyScore
	})

	edges := make([]*HyperEdge, 0, len(hg.edges))
	for _, e := range hg.edges {
		edges = append(edges, e)
	}

	return TopologySnapshot{
		Nodes:            nodes,
		Edges:            edges,
		HotspotThreshold: hotspotThreshold,
		TotalNodes:       len(nodes),
		TotalEdges:       len(edges),
		GeneratedAt:      time.Now().Unix(),
	}
}

// NodeEdges returns all edges involving a node identified by its "dimension:id" key. [SRE-45.3]
func (hg *HyperGraph) NodeEdges(key string) []*HyperEdge {
	hg.mu.RLock()
	defer hg.mu.RUnlock()
	ids, ok := hg.index[key]
	if !ok {
		return nil
	}
	out := make([]*HyperEdge, 0, len(ids))
	for _, id := range ids {
		if e, ok := hg.edges[id]; ok {
			out = append(out, e)
		}
	}
	return out
}
