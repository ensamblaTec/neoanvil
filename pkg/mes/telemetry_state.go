package mes

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/ensamblatec/neoanvil/pkg/telemetry"
)

var globalMCTSData map[string]any
var globalMCTSMu sync.RWMutex

func (s *IngestionServer) handleMCTSSync(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	globalMCTSMu.Lock()
	globalMCTSData = payload
	globalMCTSMu.Unlock()

	w.WriteHeader(http.StatusOK)
}

func (s *IngestionServer) handleToolsSync(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var payload []telemetry.ToolStat
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	telemetry.OverrideTools(payload)
	w.WriteHeader(http.StatusOK)
}
