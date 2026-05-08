package cpg

// NodeKind classifies what a node represents in the code property graph.
type NodeKind uint8

const (
	NodeFunc     NodeKind = iota // top-level function or method
	NodeBlock                    // basic block within a function
	NodeType                     // type declaration
	NodeGlobal                   // package-level variable or constant
	NodeContract                 // HTTP contract node linking backend handler to frontend callers
)

// EdgeKind classifies the relationship between two nodes.
type EdgeKind uint8

const (
	EdgeCall     EdgeKind = iota // caller → callee (call graph)
	EdgeCFG                      // basic block → successor (control flow)
	EdgeContain                  // function → its basic blocks
	EdgeContract                 // HTTP contract: backend handler → ContractNode → frontend caller
)

// NodeID is a stable numeric identifier for a graph node.
type NodeID uint32

// Node is a vertex in the code property graph.
type Node struct {
	ID       NodeID
	Kind     NodeKind
	Package  string
	File     string
	Name     string // symbol name (function, type, …)
	Line     int    // 1-based source line
	SSAValue string // SSA instruction string for blocks, empty for funcs
}

// Edge is a directed arc between two nodes.
type Edge struct {
	From NodeID
	To   NodeID
	Kind EdgeKind
}

// Graph is an in-memory directed CPG built from one or more packages.
type Graph struct {
	Nodes    []Node
	Edges    []Edge
	index    map[string]NodeID  // package.name → NodeID for node dedup
	edgeSet  map[uint64]struct{} // packed key → exists, for edge dedup
}

func newGraph() *Graph {
	return &Graph{
		index:   make(map[string]NodeID),
		edgeSet: make(map[uint64]struct{}),
	}
}

func (g *Graph) addNode(n Node) NodeID {
	key := n.Package + "." + n.Name
	if id, ok := g.index[key]; ok {
		return id
	}
	n.ID = NodeID(len(g.Nodes))
	g.Nodes = append(g.Nodes, n)
	g.index[key] = n.ID
	return n.ID
}

// edgeKey packs (from, to, kind) into a uint64 for O(1) dedup.
// Layout: from[63:32] | to[31:4] | kind[3:0]  — supports up to 2^28 nodes and 16 edge kinds.
func edgeKey(from, to NodeID, kind EdgeKind) uint64 {
	return uint64(from)<<32 | uint64(to)<<4 | uint64(kind)
}

func (g *Graph) addEdge(from, to NodeID, kind EdgeKind) {
	k := edgeKey(from, to, kind)
	if _, exists := g.edgeSet[k]; exists {
		return
	}
	g.edgeSet[k] = struct{}{}
	g.Edges = append(g.Edges, Edge{From: from, To: to, Kind: kind})
}

// NodeByName resolves a node by its package path and symbol name in O(1).
func (g *Graph) NodeByName(pkgPath, name string) (NodeID, bool) {
	id, ok := g.index[pkgPath+"."+name]
	return id, ok
}

// CallersOf returns all direct callers of id via EdgeCall (inverse BFS depth=1).
func (g *Graph) CallersOf(id NodeID) []NodeID {
	var callers []NodeID
	for _, e := range g.Edges {
		if e.Kind == EdgeCall && e.To == id {
			callers = append(callers, e.From)
		}
	}
	return callers
}

// ReachableFrom returns all nodes reachable from id via EdgeCall within maxDepth hops (BFS forward).
func (g *Graph) ReachableFrom(id NodeID, maxDepth int) []NodeID {
	if maxDepth <= 0 {
		return nil
	}
	visited := make(map[NodeID]struct{}, 64)
	visited[id] = struct{}{}
	frontier := []NodeID{id}
	var reachable []NodeID

	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		var next []NodeID
		for _, e := range g.Edges {
			if e.Kind != EdgeCall {
				continue
			}
			for _, f := range frontier {
				if e.From == f {
					if _, seen := visited[e.To]; !seen {
						visited[e.To] = struct{}{}
						next = append(next, e.To)
						reachable = append(reachable, e.To)
					}
				}
			}
		}
		frontier = next
	}
	return reachable
}
