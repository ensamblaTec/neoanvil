package cpg

import "slices"

// TraversalQuery defines a BFS walk over the CPG. [PILAR-XX/148.A]
type TraversalQuery struct {
	StartSymbol string     // function/type name to start from (matched by Name)
	EdgeKinds   []EdgeKind // nil = all kinds
	MaxDepth    int        // 0 or negative = depth 1
}

// Walk performs a BFS from the node matching q.StartSymbol and returns all
// reachable nodes within q.MaxDepth hops filtered by q.EdgeKinds.
// If no node matches StartSymbol, returns nil.
func (g *Graph) Walk(q TraversalQuery) []Node {
	if g == nil || q.StartSymbol == "" {
		return nil
	}
	maxDepth := q.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 1
	}

	// Resolve start node — match by Name (first occurrence).
	startID := NodeID(^uint32(0)) // invalid sentinel
	for _, n := range g.Nodes {
		if n.Name == q.StartSymbol {
			startID = n.ID
			break
		}
	}
	if startID == NodeID(^uint32(0)) {
		return nil
	}

	// Build adjacency: edgeKind filter.
	wantKind := func(k EdgeKind) bool {
		if len(q.EdgeKinds) == 0 {
			return true
		}
		return slices.Contains(q.EdgeKinds, k)
	}

	visited := make(map[NodeID]struct{}, 64)
	visited[startID] = struct{}{}
	frontier := []NodeID{startID}
	var result []Node

	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		var next []NodeID
		for _, e := range g.Edges {
			if !wantKind(e.Kind) {
				continue
			}
			for _, f := range frontier {
				if e.From == f {
					if _, seen := visited[e.To]; !seen {
						visited[e.To] = struct{}{}
						next = append(next, e.To)
						if int(e.To) < len(g.Nodes) {
							result = append(result, g.Nodes[e.To])
						}
					}
				}
			}
		}
		frontier = next
	}
	return result
}
