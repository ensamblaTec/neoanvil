// cmd/neo-nexus/federation_api.go — /api/v1/federation/overview endpoint and
// cross-workspace activity log ring buffer for the HUD Federation Panel. [341]
package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/nexus"
)

// globalPool holds the process pool reference set by main() so federation
// handlers can read workspace statuses without passing pool through every call. [341]
var globalPool *nexus.ProcessPool

// activityEvent is one entry in the cross-workspace activity log. [341]
type activityEvent struct {
	UnixTS    int64  `json:"unix_ts"`
	Workspace string `json:"workspace"`
	EventType string `json:"event_type"` // presence_heartbeat, knowledge_broadcast, contract_drift, handoff_delegate, etc.
	Detail    string `json:"detail,omitempty"`
}

// activityRing is a fixed-capacity ring buffer safe for concurrent use.
type activityRing struct {
	mu  sync.RWMutex
	buf []activityEvent
	cap int
	pos int // next write position
}

func newActivityRing(capacity int) *activityRing {
	return &activityRing{buf: make([]activityEvent, 0, capacity), cap: capacity}
}

// Push appends an event, evicting the oldest when the ring is full.
func (r *activityRing) Push(e activityEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) < r.cap {
		r.buf = append(r.buf, e)
		return
	}
	r.buf[r.pos] = e
	r.pos = (r.pos + 1) % r.cap
}

// All returns a copy of events in insertion order (oldest first).
func (r *activityRing) All() []activityEvent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]activityEvent, len(r.buf))
	n := len(r.buf)
	if n < r.cap {
		copy(out, r.buf)
	} else {
		// Unwrap ring: pos is the oldest.
		copy(out, r.buf[r.pos:])
		copy(out[r.cap-r.pos:], r.buf[:r.pos])
	}
	return out
}

// federationLog holds the last 200 cross-workspace activity events. [341]
var federationLog = newActivityRing(200)

// RecordFederationActivity appends an event to the ring. Called from presence,
// knowledge-broadcast, and contract-drift handlers. [341]
func RecordFederationActivity(workspace, eventType, detail string) {
	federationLog.Push(activityEvent{
		UnixTS:    time.Now().Unix(),
		Workspace: workspace,
		EventType: eventType,
		Detail:    detail,
	})
}

// federationWorkspace is one row in the Project Overview table.
type federationWorkspace struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Status          string   `json:"status"`
	SessionAgentID  string   `json:"session_agent_id,omitempty"`
	LastActivityAgo int64    `json:"last_activity_ago_s,omitempty"`
	ActiveTools     []string `json:"active_tools,omitempty"`
	UptimeSeconds   float64  `json:"uptime_seconds,omitempty"`
}

// handleFederationOverview serves GET /api/v1/federation/overview.
// Merges workspace status list (from pool) with presence heartbeat data. [341]
func handleFederationOverview(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().Unix()

	// Collect workspace statuses from presenceTable.
	presenceByWS := make(map[string]presenceEntry)
	presenceTable.Range(func(k, v any) bool {
		if p, ok := v.(presenceEntry); ok {
			presenceByWS[p.WorkspaceID] = p
		}
		return true
	})

	// Build merged workspace rows from pool if accessible.
	var wsRows []federationWorkspace
	if globalPool != nil {
		for _, ps := range globalPool.List() {
			row := federationWorkspace{
				ID:            ps.Entry.ID,
				Name:          ps.Entry.Name,
				Status:        string(ps.Status),
				UptimeSeconds: time.Since(ps.StartedAt).Seconds(),
			}
			if p, ok := presenceByWS[ps.Entry.ID]; ok {
				row.SessionAgentID = p.SessionAgentID
				row.LastActivityAgo = now - p.LastActivityUnix
				row.ActiveTools = p.ActiveTools
			}
			wsRows = append(wsRows, row)
		}
	}
	// Fallback: include presence-only rows not in pool.
	knownIDs := make(map[string]bool, len(wsRows))
	for _, r := range wsRows {
		knownIDs[r.ID] = true
	}
	presenceTable.Range(func(k, v any) bool {
		p, ok := v.(presenceEntry)
		if !ok || knownIDs[p.WorkspaceID] {
			return true
		}
		if now-p.LastActivityUnix > 120 {
			return true // stale — skip
		}
		wsRows = append(wsRows, federationWorkspace{
			ID:             p.WorkspaceID,
			Status:         "running",
			SessionAgentID: p.SessionAgentID,
			LastActivityAgo: now - p.LastActivityUnix,
			ActiveTools:    p.ActiveTools,
		})
		return true
	})

	// Activity log: last 200 entries reversed (newest first for UI).
	all := federationLog.All()
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}

	out := map[string]any{
		"generated_at": time.Now().Format(time.RFC3339),
		"workspaces":   wsRows,
		"activity_log": all,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
