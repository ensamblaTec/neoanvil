package mes

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/mesh"
	"github.com/ensamblatec/neoanvil/pkg/rag"
	"github.com/ensamblatec/neoanvil/pkg/sre"
	"github.com/ensamblatec/neoanvil/pkg/telemetry"

	"github.com/gorilla/websocket"
	"tailscale.com/tsnet"
)

type FrontendError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Stack   string `json:"stack"`
	Time    int64  `json:"time"`
}

type ErrorRing struct {
	mu     sync.RWMutex
	errors [50]FrontendError
	head   int
	count  int
}

var globalErrorRing ErrorRing

// pool recicla las estructuras para aliviar la presión del Garbage Collector a 15k RPS.
var telemetryPool = sync.Pool{
	New: func() any {
		return &TelemetryEvent{}
	},
}

// drainPool recicla buffers para consumir sockets y preservar el Keep-Alive TLS sin alocar memoria.
var drainPool = sync.Pool{
	New: func() any {
		b := make([]byte, 4096)
		return &b
	},
}

// IngestionServer envuelve el servidor HTTP y el dispatcher.
type IngestionServer struct {
	httpSrv    *http.Server
	dispatcher *EventDispatcher
	hub        *Hub
	router     *mesh.Router // Inyección de dependencia SRE
	ragDbPath  string
	tsServer   *tsnet.Server
}

// loopbackHostGuard wraps a handler with a Host-header check that only
// accepts requests whose Host field resolves to a loopback address
// (127.0.0.1, localhost, ::1, with optional :port suffix). Returns 403
// on any other Host. Used to gate the tactical aux port (plain HTTP)
// against DNS-rebinding attacks from a browser visiting an attacker
// domain that resolves to 127.0.0.1 — the rebound request still carries
// the attacker's hostname in Host, which this guard rejects.
//
// PILAR XXVIII 143.B audit (2026-05-02). Standalone helper rather than
// inline in Start so the test suite can exercise it without spawning a
// real server.
func loopbackHostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		// Accept only the canonical loopback hostnames. Reject anything
		// else — including IP variants like 127.1, 0.0.0.0, or attacker
		// hostnames that DNS-rebound to 127.0.0.1 (Host header still
		// carries the original FQDN).
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			http.Error(w, "host not permitted on tactical aux port", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// NewIngestionServer SRE Refactorizado para mTLS
func NewIngestionServer(addr string, dispatcher *EventDispatcher, hub *Hub, tlsConfig *tls.Config, router *mesh.Router, ragPath string) *IngestionServer {
	mux := http.NewServeMux()

	srv := &IngestionServer{
		dispatcher: dispatcher,
		hub:        hub,
		router:     router,
		ragDbPath:  ragPath,
	}

	mux.HandleFunc("/api/v1/telemetry", srv.handleTelemetry)
	if hub != nil {
		mux.HandleFunc("/api/v1/hmi/ws", hub.ServeWS)
	}
	mux.HandleFunc("/api/v1/sre/ws", srv.handleSREWebSocket)
	mux.HandleFunc("/api/v1/sre/topology", srv.handleGetTopology)
	mux.HandleFunc("/api/v1/sre/mcts_sync", srv.handleMCTSSync)

	httpSrv := &http.Server{
		Addr:           addr,
		Handler:        mux,
		ReadTimeout:    2 * time.Second,  // Tiempo máximo para leer el request completo
		WriteTimeout:   2 * time.Second,  // Tiempo máximo para escribir la respuesta
		IdleTimeout:    30 * time.Second, // Mantener conexiones vivas (Keep-Alive) eficientemente
		MaxHeaderBytes: 1 << 20,          // [147.F] 1 MiB cap — prevent oversized-header DoS
		TLSConfig:      tlsConfig,        // Inyección de la directiva Zero-Trust
	}

	srv.httpSrv = httpSrv
	return srv
}

// Start lanza el servidor mTLS Zero-Trust.
// [LEY-ZERO-HARDCODING] tacticalAddr viene de cfg.Server.TacticalPort (via neo.yaml).
func (s *IngestionServer) Start(certPath, keyPath string, useTailscale bool, tacticalAddr string) {
	// [SRE-HUD] Receptor Táctico Auxiliar Exclusivo (Zero-Trust Local).
	//
	// PILAR XXVIII 143.B audit (2026-05-02): the tactical aux port is plain
	// HTTP (no mTLS) by design — it carries SRE telemetry that the main
	// mTLS port also accepts, but addressed for local same-process flows.
	// However, the original implementation:
	//   (1) bound to 127.0.0.1:tacticalPort (good)
	//   (2) had no Host header validation — a malicious page in the
	//       operator's browser doing DNS rebinding (attacker.com → 127.0.0.1)
	//       could POST to /api/v1/log_frontend (which has CORS *) and
	//       inject forged frontend errors into the operator's evidence ring,
	//       OR read /api/v1/sre/frontend_errors via cross-origin GET.
	//   (3) silently dropped the http.ListenAndServe error → if the bind
	//       fails (port taken by another process) the operator never knew.
	//
	// Mitigations applied: (a) loopbackHostGuard middleware rejects requests
	// whose Host header doesn't resolve to 127.0.0.1/localhost/::1 — this
	// closes the DNS-rebinding vector even with permissive CORS. (b) Capture
	// + log the ListenAndServe error so a port collision surfaces.
	go func() {
		tacticalMux := http.NewServeMux()
		tacticalMux.HandleFunc("/api/v1/sre/mcts_sync", s.handleMCTSSync)
		tacticalMux.HandleFunc("/api/v1/sre/tools_sync", s.handleToolsSync)
		tacticalMux.HandleFunc("/api/v1/log_frontend", s.handleFrontendLog)
		tacticalMux.HandleFunc("/api/v1/sre/frontend_errors", s.handleGetFrontendErrors)
		log.Printf("[SRE-TACTICAL] 📡 Puerto auxiliar activo en anillo local: http://%s", tacticalAddr)
		if err := http.ListenAndServe(tacticalAddr, loopbackHostGuard(tacticalMux)); err != nil { //nolint:gosec // G114-LOCAL-LOOPBACK: tactical aux is plain HTTP by design (NOT internet-facing); host guard above defends against DNS rebinding from operator's browser
			log.Printf("[SRE-TACTICAL] aux server stopped: %v", err)
		}
	}()
	if useTailscale {
		tsAuthKey := os.Getenv("TS_AUTHKEY")
		if tsAuthKey == "" {
			log.Fatalf("[SRE-FATAL] neo.yaml exige Tailscale, pero TS_AUTHKEY no está definida en el entorno.")
		}

		log.Printf("[SRE-SEC] 🌐 TS_AUTHKEY detectado. Iniciando Servidor en Tailscale Mesh (WireGuard P2P)...")

		s.tsServer = &tsnet.Server{
			Hostname:  "neo-node-sandbox",
			AuthKey:   tsAuthKey,
			Ephemeral: true,
			Logf:      func(format string, args ...any) {},
		}

		ln, err := s.tsServer.Listen("tcp", ":8081")
		if err != nil {
			log.Fatalf("[SRE-FATAL] Fallo al abrir socket en Tailscale Mesh: %v", err)
		}

		go func() {
			if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
				log.Fatalf("[FATAL] Caída del Mesh Tailscale: %v", err)
			}
		}()
	} else {
		log.Printf("[SRE-SEC] Iniciando Servidor de Ingesta mTLS Zero-Trust en %s (Local Network)", s.httpSrv.Addr)
		go func() {
			if err := s.httpSrv.ListenAndServeTLS(certPath, keyPath); err != nil && err != http.ErrServerClosed {
				log.Fatalf("[FATAL] Caída de barrera mTLS: %v", err)
			}
		}()
	}
}

// Shutdown realiza un apagado elegante del socket HTTP.
func (s *IngestionServer) Shutdown(ctx context.Context) error {
	if s.tsServer != nil {
		s.tsServer.Close()
	}
	return s.httpSrv.Shutdown(ctx)
}

// RegisterHandler permite registrar rutas middleware en el multiplexor interno.
func (s *IngestionServer) RegisterHandler(pattern string, handler http.Handler) {
	if mux, ok := s.httpSrv.Handler.(*http.ServeMux); ok {
		mux.Handle(pattern, handler)
	}
}

// handleTelemetry procesa el request de alta frecuencia.
func (s *IngestionServer) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// 🛡️ BARRERA DE ADMISIÓN O(1)
	if err := s.router.Acquire(); err != nil {
		sre.MetricsMESShedded.Add(1)

		// Zero-Allocation Socket Drain explícito para Keep-Alive mTLS Mpx
		bufPtr := drainPool.Get().(*[]byte)
		for {
			if _, err := r.Body.Read(*bufPtr); err != nil {
				break
			}
		}
		drainPool.Put(bufPtr)

		http.Error(w, "Service Unavailable: In-Flight Limit Reached", http.StatusServiceUnavailable)
		return
	}
	// Liberación garantizada
	defer s.router.Release()

	//nolint:gosec // trusted enum output
	// [SRE-Regla 1] log.Printf erradicado en hot-path para evitar C-State Panic
	// 1. Defensa de Memoria: Limitar payload a 4KB (Un evento OEE rara vez supera los 500 bytes)
	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	// 2. Reciclaje: Obtener struct pre-alocado
	ev := telemetryPool.Get().(*TelemetryEvent)

	// Limpieza manual rápida (evitar datos residuales del request anterior)
	ev.MachineID = ""
	ev.Timestamp = 0
	ev.State = 0
	ev.Payload = ev.Payload[:0]

	// 3. Decodificación directa del stream (sin leer a un []byte intermedio grande)
	if err := json.NewDecoder(r.Body).Decode(ev); err != nil {
		// Devolvemos el struct al pool si falló
		telemetryPool.Put(ev)
		http.Error(w, "Bad Request: Invalid JSON", http.StatusBadRequest)
		return
	}

	// 4. Despacho Non-Blocking
	if err := s.dispatcher.Publish(*ev); err != nil {
		// Backpressure detectado. Responder con 503 para que el PLC reintente.
		telemetryPool.Put(ev)
		http.Error(w, "Service Unavailable: Backpressure SRE", http.StatusServiceUnavailable)
		return
	}

	// 5. Retorno al Pool
	// Nota: El struct se pasa por valor en dispatcher.Publish, así que podemos reciclar el puntero original.
	telemetryPool.Put(ev)

	// 202 Accepted: Procesamiento asíncrono garantizado por el dispatcher
	w.WriteHeader(http.StatusAccepted)
}

func (s *IngestionServer) handleFrontendLog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var fe FrontendError
	if err := json.NewDecoder(r.Body).Decode(&fe); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	fe.Time = time.Now().UnixMilli()

	globalErrorRing.mu.Lock()
	globalErrorRing.errors[globalErrorRing.head] = fe
	globalErrorRing.head = (globalErrorRing.head + 1) % 50
	if globalErrorRing.count < 50 {
		globalErrorRing.count++
	}
	globalErrorRing.mu.Unlock()

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"captured"}`))
}

func (s *IngestionServer) handleGetFrontendErrors(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")

	globalErrorRing.mu.RLock()
	defer globalErrorRing.mu.RUnlock()

	result := make([]FrontendError, 0, globalErrorRing.count)
	idx := globalErrorRing.head - globalErrorRing.count
	if idx < 0 {
		idx += 50
	}

	for i := 0; i < globalErrorRing.count; i++ {
		result = append(result, globalErrorRing.errors[idx])
		idx = (idx + 1) % 50
	}

	json.NewEncoder(w).Encode(result)
}

var sreUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (s *IngestionServer) handleSREWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := sreUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[SRE] WS Upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	// Zero-Allocation pre-assignments
	heatmapNodes := make([]map[string]any, 0, 100)
	heatmapEdges := make([]map[string]any, 0, 100)
	toolsNodes := make([]map[string]any, 0, 100)
	toolsEdges := make([]map[string]any, 0, 100)

	for {
		<-ticker.C
		var memStats runtime.MemStats
		runtime.ReadMemStats(&memStats)

		globalMCTSMu.RLock()
		mctsState := globalMCTSData
		globalMCTSMu.RUnlock()

		if mctsState == nil {
			mctsState = map[string]any{
				"nodes": []map[string]any{
					{"id": "mcts_root", "name": "MCTS Inactivo", "type": "root", "val": 1.0},
				},
				"edges": []map[string]any{},
			}
		}

		hotspots, _ := telemetry.GetTopHotspots(50)
		heatmapNodes = heatmapNodes[:0]
		heatmapEdges = heatmapEdges[:0]
		heatmapNodes = append(heatmapNodes, map[string]any{"id": "root_heat", "name": "Technical Debt", "type": "root", "val": 10, "heat": 0})
		for _, hs := range hotspots {
			heatmapNodes = append(heatmapNodes, map[string]any{"id": hs.File, "name": hs.File, "type": "file", "heat": float64(hs.Mutations), "val": float64(hs.Mutations)})
			heatmapEdges = append(heatmapEdges, map[string]any{"source": "root_heat", "target": hs.File})
		}
		heatmapState := map[string]any{"nodes": heatmapNodes, "edges": heatmapEdges}

		activeTools := telemetry.GetActiveTools()
		toolsNodes = toolsNodes[:0]
		toolsEdges = toolsEdges[:0]
		toolsNodes = append(toolsNodes, map[string]any{"id": "agent_core", "name": "NeoAnvil Agent", "type": "root", "val": 20})
		for _, t := range activeTools {
			toolsNodes = append(toolsNodes, map[string]any{"id": t.Name, "name": t.Name, "type": "tool", "errorCount": t.Errors, "val": t.Requests, "duration": t.Duration})
			toolsEdges = append(toolsEdges, map[string]any{"source": "agent_core", "target": t.Name, "errorCount": t.Errors})
		}
		toolsState := map[string]any{"nodes": toolsNodes, "edges": toolsEdges}

		sysStats := telemetry.GetSystemStats()
		// [SRE-24.3.1] Token Budget and Chaos state in WS payload
		payload := map[string]any{
			"type": "SRE_STATE",
			"stats": map[string]any{
				"goroutines": runtime.NumGoroutine(),
				"alloc_mb":   memStats.Alloc / 1024 / 1024,
				"sys_mb":     memStats.Sys / 1024 / 1024,
				"gc_cycles":  memStats.NumGC,
			},
			"token_budget": map[string]any{
				"recv_kb":      float64(sysStats.IOBytesRecv) / 1024.0,
				"sent_kb":      float64(sysStats.IOBytesSent) / 1024.0,
				"total_kb":     float64(sysStats.IOBytesRecv+sysStats.IOBytesSent) / 1024.0,
				"chaos_active": sysStats.ChaosActive,
				"chaos_level":  sysStats.ChaosLevel,
			},
			"mcts":    mctsState,
			"heatmap": heatmapState,
			"tools":   toolsState,
			"log":     "[SRE] Telemetría Push Activa",
		}

		if err := conn.WriteJSON(payload); err != nil {
			log.Printf("[SRE] WS Error escribiendo JSON: %v", err)
			break
		}
	}
}

func (s *IngestionServer) handleGetTopology(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if s.ragDbPath == "" {
		http.Error(w, "WAL path not configured", http.StatusInternalServerError)
		return
	}

	// [SRE-SNAPSHOT] Zero-Friction Isolation: Copiamos el WAL temporalmente para evitar bloquear BoltDB
	tmpWalHash := fmt.Sprintf("/tmp/hnsw_snapshot_%d.db", time.Now().UnixNano())
	if err := copyFile(s.ragDbPath, tmpWalHash); err != nil {
		http.Error(w, "Failed to create snapshot", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmpWalHash)

	ephemeralWal, err := rag.OpenWAL(tmpWalHash)
	if err != nil {
		http.Error(w, "Failed to open snapshot", http.StatusInternalServerError)
		return
	}
	defer ephemeralWal.Close()

	edgesMap, err := rag.GetAllGraphEdges(ephemeralWal)
	if err != nil {
		http.Error(w, "Failed to get edges from snapshot", http.StatusInternalServerError)
		return
	}

	inbound := make(map[string]int)
	nodesMap := make(map[string]bool)

	for src, tgts := range edgesMap {
		nodesMap[src] = true
		for _, tgt := range tgts {
			nodesMap[tgt] = true
			inbound[tgt]++
		}
	}

	var nodes []map[string]any
	for n := range nodesMap {
		size := 1 + float64(inbound[n])*2.0
		nodes = append(nodes, map[string]any{
			"id":   n,
			"name": n,
			"type": "file",
			"val":  size,
		})
	}

	edgesList := make([]map[string]any, 0)
	for src, tgts := range edgesMap {
		for _, tgt := range tgts {
			edgesList = append(edgesList, map[string]any{
				"source": src,
				"target": tgt,
			})
		}
	}

	if len(nodes) == 0 {
		nodes = append(nodes, map[string]any{
			"id": "empty_hnsw", "name": "Grafo Vacío: Usa neo_index_file", "type": "root", "val": 10,
		})
	}

	payload := map[string]any{
		"nodes": nodes,
		"edges": edgesList,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(payload)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
