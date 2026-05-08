package mctx

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
)

const VirtualLoss float32 = 1.0

type Tree struct {
	Nodes       []Node
	nextNodeIdx atomic.Uint32
	freeList    [1024]uint32
	freeLen     int
	freeMu      sync.Mutex
}

func NewTree(arenaMemory []Node) *Tree {
	tree := &Tree{
		Nodes:   arenaMemory,
		freeLen: 0,
	}

	tree.nextNodeIdx.Store(1)
	return tree
}

func (tree *Tree) AddChild(parentIdx uint32) (uint32, error) {
	tree.freeMu.Lock()
	if tree.freeLen > 0 {
		idx := tree.freeList[tree.freeLen-1]
		tree.freeLen--
		tree.freeMu.Unlock()

		tree.Nodes[idx].Parent = parentIdx
		tree.Nodes[idx].state.Store(0)
		return idx, nil
	}
	tree.freeMu.Unlock()

	idx := tree.nextNodeIdx.Add(1) - 1
	if int(idx) >= len(tree.Nodes) {
		return 0, fmt.Errorf("MTCS OOM: maximum capacity from sand (%d nodes) exceeded", len(tree.Nodes))
	}
	tree.Nodes[idx].Parent = parentIdx
	return idx, nil
}

func (tree *Tree) PruneNode(nodeIdx uint32) {
	tree.freeMu.Lock()
	if tree.freeLen < 1024 {
		tree.freeList[tree.freeLen] = nodeIdx
		tree.freeLen++
	}
	tree.freeMu.Unlock()
}

func (tree *Tree) ApplyVirtualLoss(nodeIdx uint32) {
	for {
		oldState := tree.Nodes[nodeIdx].state.Load()
		oldVisits, oldScore := unpackState(oldState)

		newState := packState(oldVisits+1, oldScore-VirtualLoss)

		if tree.Nodes[nodeIdx].state.CompareAndSwap(oldState, newState) {
			break
		}
		runtime.Gosched()
	}
}

func (tree *Tree) Backpropagate(ctx context.Context, nodeIdx uint32, reward float32) error {
	curr := nodeIdx

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		for {
			oldState := tree.Nodes[curr].state.Load()
			oldVisits, oldScore := unpackState(oldState)

			visitsForMath := oldVisits
			if visitsForMath == 0 {
				visitsForMath = 1
			}

			newScore := oldScore + VirtualLoss + ((reward - oldScore) / float32(visitsForMath))
			newState := packState(oldVisits, newScore)

			if tree.Nodes[curr].state.CompareAndSwap(oldState, newState) {
				break
			}
			runtime.Gosched()
		}

		if curr == 0 {
			break
		}

		curr = tree.Nodes[curr].Parent
	}

	return nil
}

// ResetArena purges the MCTS arena to prevent OOM. Resets nextNodeIdx and freeList.
// [SRE-13.2.1] Called by homeostasis cronjob after wal.Vacuum().
func (t *Tree) ResetArena() {
	t.freeMu.Lock()
	t.freeLen = 0
	t.freeMu.Unlock()
	t.nextNodeIdx.Store(1)
}

func (t *Tree) ExportTopKGraph(limit int) map[string]any {
	nodes := make([]map[string]any, 0, limit)
	edges := make([]map[string]any, 0, limit)
	count := 0

	for i := uint32(0); i < t.nextNodeIdx.Load() && count < limit; i++ {
		node := &t.Nodes[i]
		visits, score := unpackState(node.state.Load())

		// Ignorar nodos vacíos a menos que sean el pivot central
		if visits == 0 && i != 0 {
			continue
		}

		valStr := fmt.Sprintf("%.2f", score)
		idStr := "mcts_root"
		if i != 0 {
			idStr = fmt.Sprintf("mcts_%d", i)
		}

		nodes = append(nodes, map[string]any{
			"id":   idStr,
			"name": fmt.Sprintf("MCTS %d (V:%d S:%s)", i, visits, valStr),
			"val":  float64(visits)/10.0 + 1.0,
			"type": "state", // Could be action or root
		})

		// Re-anclar las aristas hacia el pivot si existieran
		if i != 0 {
			source := "mcts_root"
			if node.Parent != 0 {
				source = fmt.Sprintf("mcts_%d", node.Parent)
			}
			edges = append(edges, map[string]any{
				"source": source,
				"target": idStr,
			})
		}
		count++
	}

	if len(nodes) == 0 {
		nodes = append(nodes, map[string]any{
			"id": "mcts_root", "name": "MCTS Root", "type": "root", "val": 1.0,
		})
	}

	return map[string]any{
		"nodes": nodes,
		"edges": edges,
	}
}
