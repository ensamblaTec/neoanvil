package nexus

// topology.go — Project Federation runtime index. [Épica 283]
//
// TopologyIndex maps projectID→[]workspaceID and workspaceID→projectID so that
// RecordToolCall, watchdog, scatter, and knowledge broadcast can reason about
// project membership without re-reading the registry on every hot-path call.

import (
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/workspace"
)

// ProjectActivityCounters holds aggregated MCP activity for a project. [283.C]
type ProjectActivityCounters struct {
	LastToolCallUnix int64
	ToolCallCount    int64
}

// TopologyIndex is the runtime project→workspace index. [283.A]
type TopologyIndex struct {
	mu          sync.RWMutex
	byProject   map[string][]string // projectID → []workspaceID
	byWorkspace map[string]string   // workspaceID → projectID
}

// BuildTopology constructs a TopologyIndex from the workspace registry. [283.A]
// For each entry with Type=="project", loads .neo-project/neo.yaml and resolves
// member_workspaces paths to IDs via reverse lookup in the registry.
func BuildTopology(reg *workspace.Registry) *TopologyIndex {
	t := &TopologyIndex{
		byProject:   make(map[string][]string),
		byWorkspace: make(map[string]string),
	}

	// Build path→ID reverse index from all entries.
	pathToID := make(map[string]string, len(reg.Workspaces))
	for _, e := range reg.Workspaces {
		pathToID[filepath.Clean(e.Path)] = e.ID
	}

	for _, e := range reg.Workspaces {
		if e.Type != "project" {
			continue
		}
		pc, err := config.LoadProjectConfig(e.Path)
		if err != nil || pc == nil {
			continue
		}
		for _, memberPath := range pc.MemberWorkspaces {
			clean := filepath.Clean(memberPath)
			memberID, ok := pathToID[clean]
			if !ok {
				log.Printf("[NEXUS-TOPOLOGY] member path %q not in registry — skipping", memberPath)
				continue
			}
			t.byProject[e.ID] = append(t.byProject[e.ID], memberID)
			t.byWorkspace[memberID] = e.ID
		}
		log.Printf("[NEXUS-TOPOLOGY] project %s has %d members", e.ID, len(t.byProject[e.ID]))
	}
	return t
}

// ProjectForWorkspace returns the project ID that owns wsID, or "" if standalone.
func (t *TopologyIndex) ProjectForWorkspace(wsID string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.byWorkspace[wsID]
}

// SiblingsOf returns all workspace IDs in the same project as wsID, excluding wsID itself.
func (t *TopologyIndex) SiblingsOf(wsID string) []string {
	t.mu.RLock()
	projID := t.byWorkspace[wsID]
	members := t.byProject[projID]
	t.mu.RUnlock()
	if projID == "" || len(members) == 0 {
		return nil
	}
	out := make([]string, 0, len(members)-1)
	for _, m := range members {
		if m != wsID {
			out = append(out, m)
		}
	}
	return out
}

// ActiveSiblings returns siblings whose idle_seconds < idleThresholdSec. [283.A]
func (t *TopologyIndex) ActiveSiblings(wsID string, pool *ProcessPool, idleThresholdSec int64) []string {
	siblings := t.SiblingsOf(wsID)
	now := time.Now().Unix()
	out := make([]string, 0, len(siblings))
	for _, sid := range siblings {
		proc, ok := pool.GetProcess(sid)
		if !ok || proc.Status != StatusRunning {
			continue
		}
		if proc.LastToolCallUnix == 0 {
			continue
		}
		idle := now - proc.LastToolCallUnix
		if idle < idleThresholdSec {
			out = append(out, sid)
		}
	}
	return out
}

// Rebuild atomically replaces the index from a new registry snapshot. [283.F]
func (t *TopologyIndex) Rebuild(reg *workspace.Registry) {
	fresh := BuildTopology(reg)
	t.mu.Lock()
	t.byProject = fresh.byProject
	t.byWorkspace = fresh.byWorkspace
	t.mu.Unlock()
	log.Printf("[NEXUS-TOPOLOGY] index rebuilt: %d projects", len(fresh.byProject))
}
