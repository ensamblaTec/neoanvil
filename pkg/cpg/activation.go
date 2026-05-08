package cpg

import "math"

// Activate computes spreading activation energy over the call graph starting
// from seeds, decaying by alpha per hop (energy[n] = energy[parent] × alpha^depth).
// Returns a map of NodeID → cumulative energy. Nodes not reached have energy 0.
// If seeds is empty or g is nil, returns nil (caller falls back to HNSW-only).
func Activate(g *Graph, seeds []NodeID, alpha float64, maxDepth int) map[NodeID]float64 {
	if g == nil || len(seeds) == 0 || maxDepth <= 0 {
		return nil
	}

	energy := make(map[NodeID]float64, len(g.Nodes))
	for _, s := range seeds {
		energy[s] = 1.0
	}

	frontier := make(map[NodeID]float64, len(seeds))
	for _, s := range seeds {
		frontier[s] = 1.0
	}

	for depth := range maxDepth {
		decay := math.Pow(alpha, float64(depth+1))
		next := make(map[NodeID]float64)
		for _, e := range g.Edges {
			if e.Kind != EdgeCall {
				continue
			}
			if parentEnergy, ok := frontier[e.From]; ok {
				propagated := parentEnergy * decay
				if propagated > next[e.To] {
					next[e.To] = propagated
				}
			}
		}
		for id, en := range next {
			if en > energy[id] {
				energy[id] = en
			}
		}
		frontier = next
		if len(frontier) == 0 {
			break
		}
	}
	return energy
}

// NormalizeEnergy scales all energy values to [0,1] by dividing by the max.
// Returns nil if input is nil or empty.
func NormalizeEnergy(energy map[NodeID]float64) map[NodeID]float64 {
	if len(energy) == 0 {
		return nil
	}
	var maxE float64
	for _, v := range energy {
		if v > maxE {
			maxE = v
		}
	}
	if maxE == 0 {
		return energy
	}
	normalized := make(map[NodeID]float64, len(energy))
	for k, v := range energy {
		normalized[k] = v / maxE
	}
	return normalized
}
