package main

// [SRE-85.A] Dashboard data types and internal API registration.
//
// The SPA and standalone HTTP server have been moved to cmd/neo-nexus (Épica 85).
// neo-mcp is now headless — it exposes dashboard data endpoints on its main
// sseMux (same port as /health and /mcp/*). Nexus proxies them to the operator.
//
// Kept here:
//   - SystemState, SnapshotBuffer — vitals ring buffer used by homeostasis loop
//   - EmitHeartbeat — publishes heartbeat events to the bus
//   - DashboardOpts — closures for internal data access
//   - RegisterDashboardAPI — registers data endpoints on a given mux

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/observability"
	"github.com/ensamblatec/neoanvil/pkg/pubsub"
	"github.com/ensamblatec/neoanvil/pkg/rag"
	"github.com/ensamblatec/neoanvil/pkg/sre"
	"github.com/ensamblatec/neoanvil/pkg/telemetry"
)

// SystemState is a point-in-time snapshot of NeoAnvil vitals. [SRE-32.3.1]
type SystemState struct {
	At          time.Time `json:"at"`
	Watts       float64   `json:"watts"`
	Goroutines  int       `json:"goroutines"`
	Mode        string    `json:"mode"`
	Stabilizing bool      `json:"stabilizing"`
}

// SnapshotBuffer is a thread-safe ring buffer of the last N SystemStates.
type SnapshotBuffer struct {
	mu    sync.Mutex
	items []SystemState
	max   int
}

// NewSnapshotBuffer allocates a ring buffer with capacity cap.
func NewSnapshotBuffer(cap int) *SnapshotBuffer {
	return &SnapshotBuffer{max: cap, items: make([]SystemState, 0, cap)}
}

// Push appends a state, evicting the oldest entry when full.
func (r *SnapshotBuffer) Push(s SystemState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, s)
	if len(r.items) > r.max {
		r.items = r.items[len(r.items)-r.max:]
	}
}

// All returns a copy of all stored snapshots in chronological order.
func (r *SnapshotBuffer) All() []SystemState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SystemState, len(r.items))
	copy(out, r.items)
	return out
}

// DashboardOpts groups all dependencies for the internal dashboard API endpoints.
type DashboardOpts struct {
	Bus       *pubsub.Bus
	MCTSFn    func() map[string]any
	ChaosFn   func(ctx context.Context) string
	Snapshots *SnapshotBuffer
	Mode      string
	Version   string

	AuditFn        func(path string) (string, error)
	RecallFn       func(ctx context.Context, q string) []string
	RemFn          func(ctx context.Context) error
	PendingTasksFn func() int
	DiagnoseFn     func(ctx context.Context, path string) (map[string]any, error)
	HealFn         func(ctx context.Context, mode string) (string, error)
	RAGMetricsFn   func() map[string]any
	HyperGraphFn   func() *rag.HyperGraph
	MerkleFn       func() string
	Workspace      string
	OracleFn       func() map[string]any

	// [PILAR-XXVII/244.A] Workspace identity + boot timestamp threaded
	// through for /api/v1/metrics. WorkspaceName is the human label
	// (neo.yaml → server.workspace or last-segment of path); BootUnix is
	// set once at boot so uptime = now - BootUnix.
	WorkspaceName string
	BootUnix      int64

	// [PILAR-XXXIV/268] Graph pointer for IndexCoverage + DominantLang from neo.yaml.
	Graph        *rag.Graph
	DominantLang string
}

// RegisterDashboardAPI registers all dashboard data endpoints on the given mux.
// These endpoints are served on the child's main HTTP port (alongside /health,
// /mcp/sse, /mcp/message). Nexus proxies them to the operator HUD. [SRE-85.A.4]
func RegisterDashboardAPI(mux *http.ServeMux, opts DashboardOpts) {
	// GET /events — SSE stream from the event bus. [SRE-32.1.3]
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		ch, unsub := opts.Bus.Subscribe()
		defer unsub()

		enc := json.NewEncoder(w)
		for {
			select {
			case <-r.Context().Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				_, _ = fmt.Fprint(w, "data: ")
				_ = enc.Encode(ev)
				_, _ = fmt.Fprint(w, "\n")
				flusher.Flush()
			}
		}
	})

	// GET /mcts — live MCTS tree snapshot. [SRE-32.2.1]
	mux.HandleFunc("/mcts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var payload map[string]any
		if opts.MCTSFn != nil {
			payload = opts.MCTSFn()
		}
		if payload == nil {
			payload = map[string]any{"nodes": []any{}, "edges": []any{}}
		}
		_ = json.NewEncoder(w).Encode(payload)
	})

	// GET /snapshot — last 50 system states. [SRE-32.3.1]
	mux.HandleFunc("/snapshot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var states []SystemState
		if opts.Snapshots != nil {
			states = opts.Snapshots.All()
		}
		if states == nil {
			states = []SystemState{}
		}
		_ = json.NewEncoder(w).Encode(states)
	})

	// POST /chaos — Gameday chaos drill trigger. [SRE-32]
	mux.HandleFunc("/chaos", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var report string
		if opts.ChaosFn != nil {
			report = opts.ChaosFn(r.Context())
		} else {
			report = "Chaos drill not configured."
		}
		opts.Bus.Publish(pubsub.Event{Type: pubsub.EventChaos, Payload: report})
		_ = json.NewEncoder(w).Encode(map[string]string{"report": report})
	})

	// GET /api/status — machine-readable daemon vitals. [SRE-33.1.3]
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		pending := 0
		if opts.PendingTasksFn != nil {
			pending = opts.PendingTasksFn()
		}
		var watts float64
		if opts.Snapshots != nil {
			if all := opts.Snapshots.All(); len(all) > 0 {
				watts = all[len(all)-1].Watts
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":     opts.Version,
			"mode":        opts.Mode,
			"watts":       watts,
			"stabilizing": ThermicStabilizing.Load() == 1,
			"goroutines":  runtime.NumGoroutine(),
			"pending":     pending,
		})
	})

	// POST /api/audit — AST audit. [SRE-33.2.1]
	mux.HandleFunc("/api/audit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
			http.Error(w, `{"error":"path required"}`, http.StatusBadRequest)
			return
		}
		if opts.AuditFn == nil {
			_ = json.NewEncoder(w).Encode(map[string]string{"report": "Audit not configured."})
			return
		}
		report, auditErr := opts.AuditFn(req.Path)
		if auditErr != nil {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": auditErr.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"report": report})
	})

	// POST /api/recall — HNSW vector search. [SRE-33.2.2]
	mux.HandleFunc("/api/recall", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Query == "" {
			http.Error(w, `{"error":"query required"}`, http.StatusBadRequest)
			return
		}
		var results []string
		if opts.RecallFn != nil {
			results = opts.RecallFn(r.Context(), req.Query)
		}
		if results == nil {
			results = []string{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	})

	// POST /api/rem — force REM consolidation. [SRE-33.2.3]
	mux.HandleFunc("/api/rem", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if opts.RemFn != nil {
			if err := opts.RemFn(r.Context()); err != nil {
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "REM cycle triggered"})
	})

	// POST /api/diagnose — 4-level inference. [SRE-34.2.2]
	mux.HandleFunc("/api/diagnose", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
			http.Error(w, `{"error":"path required"}`, http.StatusBadRequest)
			return
		}
		if opts.DiagnoseFn == nil {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "inference not configured"})
			return
		}
		result, err := opts.DiagnoseFn(r.Context(), req.Path)
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		opts.Bus.Publish(pubsub.Event{Type: pubsub.EventInference, Payload: result})
		_ = json.NewEncoder(w).Encode(result)
	})

	// POST /api/heal — Watchdog heal cycle. [SRE-34.2.2]
	mux.HandleFunc("/api/heal", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			req.Mode = "manual"
		}
		if req.Mode == "" {
			req.Mode = "manual"
		}
		if opts.HealFn == nil {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "heal not configured"})
			return
		}
		plan, err := opts.HealFn(r.Context(), req.Mode)
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		resp := map[string]string{"plan": plan}
		if req.Mode == "auto" {
			resp["status"] = "[AUTO-APPROVED] heal cycle triggered"
			opts.Bus.Publish(pubsub.Event{
				Type:    pubsub.EventAutoApprove,
				Payload: map[string]any{"tag": "[AUTO-APPROVED]", "action": "heal", "mode": req.Mode},
			})
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// GET /api/v1/metrics/rag — RAG graph health. [SRE-35.3.1]
	mux.HandleFunc("/api/v1/metrics/rag", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if opts.RAGMetricsFn == nil {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "rag metrics not configured"})
			return
		}
		metrics := opts.RAGMetricsFn()
		if metrics == nil {
			metrics = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(metrics)
	})

	// GET /api/v1/hypergraph/topology — full topology. [SRE-45.1/45.2]
	mux.HandleFunc("/api/v1/hypergraph/topology", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if opts.HyperGraphFn == nil {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "hypergraph not configured"})
			return
		}
		snap := opts.HyperGraphFn().Topology(3.0)
		_ = json.NewEncoder(w).Encode(snap)
	})

	// GET /api/v1/hypergraph/node/{key}/flashback — edges for a node. [SRE-45.3]
	mux.HandleFunc("/api/v1/hypergraph/node/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if opts.HyperGraphFn == nil {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "hypergraph not configured"})
			return
		}
		path := r.URL.Path
		const prefix = "/api/v1/hypergraph/node/"
		const suffix = "/flashback"
		if len(path) <= len(prefix)+len(suffix) {
			http.Error(w, "invalid node key", http.StatusBadRequest)
			return
		}
		key := path[len(prefix) : len(path)-len(suffix)]
		edges := opts.HyperGraphFn().NodeEdges(key)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_key":   key,
			"edges":      edges,
			"edge_count": len(edges),
		})
	})

	// GET /api/v1/brain/merkle — Merkle root. [SRE-59.2]
	mux.HandleFunc("/api/v1/brain/merkle", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		root := ""
		if opts.MerkleFn != nil {
			root = opts.MerkleFn()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"merkle_root": root,
			"at":          time.Now().UTC().Format(time.RFC3339),
		})
	})

	// GET /api/v1/oracle/risk — current failure probability. [SRE-61.3]
	mux.HandleFunc("/api/v1/oracle/risk", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if opts.OracleFn == nil {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "oracle not available"})
			return
		}
		_ = json.NewEncoder(w).Encode(opts.OracleFn())
	})

	// POST /api/v1/hotreload — rebuild binary in background. [SRE-59.3]
	mux.HandleFunc("/api/v1/hotreload", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ws := opts.Workspace
		if ws == "" {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "workspace not configured"})
			return
		}
		outBin := filepath.Join(ws, "bin", "neo-mcp-rebuilt")
		cmd := exec.CommandContext(r.Context(), "go", "build", "-o", outBin, "./cmd/neo-mcp") //nolint:gosec // G204-LITERAL-BIN
		cmd.Dir = ws
		sre.HardenSubprocess(cmd, 0) // [T006-sweep] HUD rebuild may stall on cgo grandchildren
		out, buildErr := cmd.CombinedOutput()
		if buildErr != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "build_failed",
				"output": string(out),
				"error":  buildErr.Error(),
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ready",
			"binary":  outBin,
			"output":  string(out),
			"message": "New binary built. Restart the server to apply.",
		})
	})

	// GET /api/v1/sre/state — data source for HUD_STATE radar intent. [SRE-85]
	mux.HandleFunc("/api/v1/sre/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var mctsNodes int
		if opts.MCTSFn != nil {
			if snap := opts.MCTSFn(); snap != nil {
				if nodes, ok := snap["nodes"].([]any); ok {
					mctsNodes = len(nodes)
				}
			}
		}

		var watts float64
		if opts.Snapshots != nil {
			if all := opts.Snapshots.All(); len(all) > 0 {
				watts = all[len(all)-1].Watts
			}
		}
		color := "green"
		switch {
		case watts >= 60:
			color = "red"
		case watts >= 30:
			color = "yellow"
		}

		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		ramMB := float64(ms.Alloc) / (1024 * 1024)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"mcts_nodes": mctsNodes,
			"color":      color,
			"ram_mb":     ramMB,
		})
	})

	// POST /api/v1/sre/record_hotspot_bypass — records a bypass mutation. [Épica 159.A]
	mux.HandleFunc("/api/v1/sre/record_hotspot_bypass", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
			http.Error(w, `{"error":"path required"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := telemetry.RecordBypassMutation(req.Path); err != nil {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		// [PILAR-XXVII/243.G] Also mirror to the persistent observability
		// store with bypassed=true so the HUD can surface the ⚠️ pile.
		observability.GlobalStore.RecordMutation(req.Path, true)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "recorded", "path": req.Path})
	})

	// GET /api/v1/metrics — unified observability snapshot. [PILAR-XXVII/244.A]
	// Extended with index_coverage and dominant_lang for cross-workspace scatter. [PILAR-XXXIV/268]
	mux.HandleFunc("/api/v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=2")
		snap := observability.GlobalStore.Snapshot(opts.Workspace, opts.WorkspaceName, opts.BootUnix)
		ext := struct {
			observability.Snapshot
			IndexCoverage float64 `json:"index_coverage"`
			DominantLang  string  `json:"dominant_lang"`
		}{
			Snapshot:      snap,
			IndexCoverage: rag.IndexCoverage(opts.Graph, opts.Workspace),
			DominantLang:  opts.DominantLang,
		}
		_ = json.NewEncoder(w).Encode(ext)
	})

	log.Println("[SRE-85] Dashboard internal API registered on worker mux")
}

// EmitHeartbeat publishes a heartbeat event with current vitals to the bus.
// Called periodically by the HomeostasisLoop. [SRE-32.1.3]
func EmitHeartbeat(bus *pubsub.Bus, watts float64, mode string, snapshots *SnapshotBuffer) {
	goroutines := runtime.NumGoroutine()
	stabilizing := ThermicStabilizing.Load() == 1

	state := SystemState{
		At:          time.Now(),
		Watts:       watts,
		Goroutines:  goroutines,
		Mode:        mode,
		Stabilizing: stabilizing,
	}
	if snapshots != nil {
		snapshots.Push(state)
	}
	bus.Publish(pubsub.Event{
		Type: pubsub.EventHeartbeat,
		Payload: map[string]any{
			"watts":       watts,
			"goroutines":  goroutines,
			"mode":        mode,
			"stabilizing": stabilizing,
		},
	})
}
