// cmd/neo-nexus/presence.go — In-memory agent presence table for multi-agent
// coordination. Each neo-mcp child heartbeats every 30s; Nexus keeps the last
// entry per workspace and exposes it via GET /api/v1/presence. [337.A]
package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// presenceEntry tracks an active agent session for a workspace. [337.A]
type presenceEntry struct {
	WorkspaceID      string   `json:"workspace_id"`
	SessionAgentID   string   `json:"session_agent_id"`
	LastActivityUnix int64    `json:"last_activity_unix"`
	ActiveTools      []string `json:"active_tools"`
}

// presenceTable stores the latest heartbeat per workspace ID. [337.A]
var presenceTable sync.Map // map[string]presenceEntry

// handlePresenceHeartbeat accepts POST /internal/presence from neo-mcp children
// and upserts the workspace entry in the in-memory table.
func handlePresenceHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var entry presenceEntry
	if err := json.NewDecoder(r.Body).Decode(&entry); err != nil || entry.WorkspaceID == "" {
		http.Error(w, "bad request: workspace_id required", http.StatusBadRequest)
		return
	}
	entry.LastActivityUnix = time.Now().Unix()
	presenceTable.Store(entry.WorkspaceID, entry)
	RecordFederationActivity(entry.WorkspaceID, "presence_heartbeat", entry.SessionAgentID) // [341]
	w.WriteHeader(http.StatusNoContent)
}

// handlePresenceList handles GET /api/v1/presence — returns all sessions whose
// last heartbeat is within the staleness threshold (120s). [337.A]
func handlePresenceList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	threshold := time.Now().Unix() - 120
	entries := make([]presenceEntry, 0)
	presenceTable.Range(func(_, v any) bool {
		e := v.(presenceEntry)
		if e.LastActivityUnix >= threshold {
			entries = append(entries, e)
		}
		return true
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}
