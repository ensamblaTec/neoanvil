package mctx

import (
	"fmt"
)

// ToGraphData empaqueta el arbol actual en un JSON seguro para SRE HUD
func (tree *Tree) ToGraphData() map[string]any {
	nodes := []map[string]any{}
	edges := []map[string]any{}
	maxBound := tree.nextNodeIdx.Load()

	if maxBound == 0 {
		return map[string]any{"nodes": nodes, "edges": edges}
	}

	// 🛡️ [SRE PATTERN] Back-Tracking Causal para mitigar GC Spikes
	const maxLeaves = 150
	startIdx := uint32(1)
	if maxBound > maxLeaves {
		startIdx = maxBound - maxLeaves
	}

	included := make(map[uint32]struct{}, maxLeaves*2)
	included[0] = struct{}{}

	for i := startIdx; i < maxBound; i++ {
		curr := i
		for {
			if _, exists := included[curr]; exists {
				break
			}
			included[curr] = struct{}{}
			if curr == 0 {
				break
			}
			curr = tree.Nodes[curr].Parent
		}
	}

	for i := uint32(0); i < maxBound; i++ {
		if _, ok := included[i]; !ok {
			continue
		}

		node := &tree.Nodes[i]
		visits, score := unpackState(node.state.Load())

		if visits > 0 || i == 0 {
			nodes = append(nodes, map[string]any{
				"id":   fmt.Sprintf("mcts_%d", i),
				"name": fmt.Sprintf("MCTS-%d", i),
				"type": "leaf",
				"val":  float64(visits)/10.0 + float64(score)*5.0 + 1.0,
			})
			if i > 0 {
				edges = append(edges, map[string]any{
					"source": fmt.Sprintf("mcts_%d", node.Parent),
					"target": fmt.Sprintf("mcts_%d", i),
				})
			}
		}
	}
	return map[string]any{"nodes": nodes, "edges": edges}
}
