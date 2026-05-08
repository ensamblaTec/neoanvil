package graph

import "sync"

// DAG represents a Directed Acyclic Graph.
type DAG struct {
	mu    sync.RWMutex
	Edges map[string][]string // Adjacency list: from -> list of dependencies (to)
}

func NewDAG() *DAG {
	return &DAG{
		Edges: make(map[string][]string),
	}
}

func (d *DAG) AddEdge(from, to string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Edges[from] = append(d.Edges[from], to)
}

type tarjanState struct {
	edges       map[string][]string
	nodeList    []string
	nodeIndex   map[string]int
	index       []int
	lowlink     []int
	onStack     []bool
	stack       []int
	timeCounter int
	sccs        [][]string
}

func newTarjanState(edges map[string][]string) *tarjanState {
	nodeIndex := make(map[string]int, len(edges)*2)
	nodeList := make([]string, 0, len(edges))

	for from, toList := range edges {
		if _, ok := nodeIndex[from]; !ok {
			nodeIndex[from] = len(nodeList)
			nodeList = append(nodeList, from)
		}
		for _, to := range toList {
			if _, ok := nodeIndex[to]; !ok {
				nodeIndex[to] = len(nodeList)
				nodeList = append(nodeList, to)
			}
		}
	}

	n := len(nodeList)
	idx := make([]int, n)
	for i := range n {
		idx[i] = -1
	}

	return &tarjanState{
		edges:     edges,
		nodeList:  nodeList,
		nodeIndex: nodeIndex,
		index:     idx,
		lowlink:   make([]int, n),
		onStack:   make([]bool, n),
		stack:     make([]int, 0, n),
	}
}

func (ts *tarjanState) processComponent(v int) {
	if ts.lowlink[v] == ts.index[v] {
		var scc []string
		for {
			w := ts.stack[len(ts.stack)-1]
			ts.stack = ts.stack[:len(ts.stack)-1]
			ts.onStack[w] = false
			scc = append(scc, ts.nodeList[w])
			if w == v {
				break
			}
		}
		ts.sccs = append(ts.sccs, scc)
	}
}

func (ts *tarjanState) strongConnect(v int) {
	ts.index[v] = ts.timeCounter
	ts.lowlink[v] = ts.timeCounter
	ts.timeCounter++
	ts.stack = append(ts.stack, v)
	ts.onStack[v] = true

	vStr := ts.nodeList[v]
	for _, wStr := range ts.edges[vStr] {
		w := ts.nodeIndex[wStr]
		if ts.index[w] == -1 {
			ts.strongConnect(w)
			if ts.lowlink[w] < ts.lowlink[v] {
				ts.lowlink[v] = ts.lowlink[w]
			}
		} else if ts.onStack[w] {
			if ts.index[w] < ts.lowlink[v] {
				ts.lowlink[v] = ts.index[w]
			}
		}
	}

	ts.processComponent(v)
}

// TarjanSCC implementation in Go with zero-allocation optimizations for state.
func (d *DAG) TarjanSCC() [][]string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	ts := newTarjanState(d.Edges)
	if len(ts.nodeList) == 0 {
		return [][]string{}
	}

	for i := 0; i < len(ts.nodeList); i++ {
		if ts.index[i] == -1 {
			ts.strongConnect(i)
		}
	}
	return ts.sccs
}
