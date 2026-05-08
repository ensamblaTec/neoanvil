package cpg

import "sort"

// RankedNode pairs a Node with its PageRank score.
type RankedNode struct {
	Node
	Score float64
}

// ComputePageRank runs the classic PageRank algorithm over EdgeCall arcs only.
// Returns a map of NodeID → score. Scores sum to ≈1.0 after convergence.
// damping is typically 0.85; iters is typically 50.
func ComputePageRank(g *Graph, damping float64, iters int) map[NodeID]float64 {
	n := len(g.Nodes)
	if n == 0 {
		return nil
	}

	// Build call-only adjacency: out[from] = list of callees.
	out := make([][]NodeID, n)
	inDegree := make([]int, n) // number of callers pointing to each node

	for _, e := range g.Edges {
		if e.Kind != EdgeCall {
			continue
		}
		from, to := int(e.From), int(e.To)
		if from >= n || to >= n {
			continue
		}
		out[from] = append(out[from], e.To)
		inDegree[to]++
	}

	// Initialise ranks uniformly.
	rank := make([]float64, n)
	next := make([]float64, n)
	init := 1.0 / float64(n)
	for i := range rank {
		rank[i] = init
	}

	teleport := (1.0 - damping) / float64(n)

	for range iters {
		// Collect dangling mass (nodes with no outgoing call edges).
		var dangling float64
		for ri := range rank {
			if len(out[ri]) == 0 {
				dangling += rank[ri]
			}
		}
		danglingShare := damping * dangling / float64(n)

		for ni := range next {
			next[ni] = teleport + danglingShare
		}
		for from, callees := range out {
			if len(callees) == 0 {
				continue
			}
			share := damping * rank[from] / float64(len(callees))
			for _, to := range callees {
				next[int(to)] += share
			}
		}
		rank, next = next, rank
	}

	result := make(map[NodeID]float64, n)
	for i, r := range rank {
		result[NodeID(i)] = r
	}
	return result
}

// TopN returns the n highest-ranked NodeFunc nodes, sorted descending.
// pkgPrefix filters to nodes whose Package starts with pkgPrefix; pass "" for all.
// If ranks is nil or the graph is empty, returns nil.
func (g *Graph) TopN(n int, ranks map[NodeID]float64, pkgPrefix string) []RankedNode {
	if ranks == nil || len(g.Nodes) == 0 {
		return nil
	}
	nodes := make([]RankedNode, 0, len(g.Nodes))
	for _, node := range g.Nodes {
		if node.Kind != NodeFunc {
			continue
		}
		if pkgPrefix != "" && !hasPrefix(node.Package, pkgPrefix) {
			continue
		}
		nodes = append(nodes, RankedNode{Node: node, Score: ranks[node.ID]})
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].Score > nodes[j].Score
	})
	if n > 0 && n < len(nodes) {
		nodes = nodes[:n]
	}
	return nodes
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
