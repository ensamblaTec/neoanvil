// export_test.go exposes package-private helpers for black-box tests in cpg_test.
// This file is only compiled during `go test`.
package cpg

// ExportedNewGraph wraps newGraph for tests.
func ExportedNewGraph() *Graph { return newGraph() }

// ExportedAddNode wraps Graph.addNode for tests.
func (g *Graph) ExportedAddNode(n Node) NodeID { return g.addNode(n) }

// ExportedAddEdge wraps Graph.addEdge for tests.
func (g *Graph) ExportedAddEdge(from, to NodeID, kind EdgeKind) { g.addEdge(from, to, kind) }
