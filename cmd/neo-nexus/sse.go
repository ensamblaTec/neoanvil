// cmd/neo-nexus/sse.go — MCP SSE transport layer for Nexus dispatcher. [SRE-92]
//
// Nexus is the SSE entry point for Claude Code. Children (neo-mcp) are headless
// RPC workers — they only expose POST /mcp/message. This file implements the
// server-side SSE protocol (MCP 2025-03-26 spec) at the dispatcher level:
//
//   GET  /mcp/sse           → opens SSE stream, sends "endpoint" event
//   POST /mcp/message?sessionId=X → forwarded to child, response sent as SSE "message"
//
// Each SSE session maps to exactly one child (resolved at connect time).

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/nexus"
	"github.com/ensamblatec/neoanvil/pkg/sre"
	"github.com/ensamblatec/neoanvil/pkg/workspace"
)

// maxMessageBody limits POST body size to prevent memory exhaustion (10 MB).
const maxMessageBody = 10 * 1024 * 1024

// sseSession tracks one open SSE client connection.
type sseSession struct {
	id           string
	childPort    int           // resolved child port for this session
	workspaceID  string        // workspace ID from X-Neo-Workspace header at connect time [MCPI-46 bugfix]
	eventCh      chan sseEvent // buffered channel for outgoing events
	done         chan struct{}  // closed when the client disconnects
	createdAt    time.Time
	remoteIP     string    // client IP for per-IP throttle [145.B]
	lastActivity time.Time // updated on each event dispatch; used for idle timeout [145.B]
}

type sseEvent struct {
	Event string // "endpoint", "message"
	Data  string
}

// sseSessionStore manages active SSE sessions. Thread-safe. [145.B]
type sseSessionStore struct {
	mu          sync.RWMutex
	sessions    map[string]*sseSession
	perIP       map[string]int // active session count by remote IP
	maxTotal    int            // max concurrent sessions (0 = unlimited)
	maxPerIP    int            // max sessions per remote IP (0 = unlimited)
	idleTimeout time.Duration  // close sessions idle longer than this (0 = disabled)
}

func newSSESessionStore(maxTotal, maxPerIP int, idleTimeout time.Duration) *sseSessionStore {
	return &sseSessionStore{
		sessions:    make(map[string]*sseSession),
		perIP:       make(map[string]int),
		maxTotal:    maxTotal,
		maxPerIP:    maxPerIP,
		idleTimeout: idleTimeout,
	}
}

// Add attempts to register a new session. Returns an error if a capacity limit
// would be exceeded, leaving the store unchanged in that case.
func (s *sseSessionStore) Add(sess *sseSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maxTotal > 0 && len(s.sessions) >= s.maxTotal {
		return fmt.Errorf("SSE session limit reached (%d)", s.maxTotal)
	}
	if s.maxPerIP > 0 && s.perIP[sess.remoteIP] >= s.maxPerIP {
		return fmt.Errorf("SSE per-IP limit reached for %s (%d)", sess.remoteIP, s.maxPerIP)
	}
	s.sessions[sess.id] = sess
	s.perIP[sess.remoteIP]++
	return nil
}

func (s *sseSessionStore) Get(id string) (*sseSession, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[id]
	s.mu.RUnlock()
	return sess, ok
}

func (s *sseSessionStore) Remove(id string) {
	s.mu.Lock()
	if sess, ok := s.sessions[id]; ok {
		delete(s.sessions, id)
		if s.perIP[sess.remoteIP] > 1 {
			s.perIP[sess.remoteIP]--
		} else {
			delete(s.perIP, sess.remoteIP)
		}
	}
	s.mu.Unlock()
}

// isChildPortAlive checks if a child on the given port is still running in the pool.
func isChildPortAlive(pool *nexus.ProcessPool, port int) bool {
	for _, p := range pool.List() {
		if p.Port == port && p.Status == nexus.StatusRunning {
			return true
		}
	}
	return false
}

// remoteIP extracts the remote IP from the request (no port).
func remoteIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	// Unwrap IPv6 bracket notation.
	return strings.Trim(host, "[]")
}

// handleSSEConnect handles GET /mcp/sse — opens an SSE stream and sends the
// endpoint event so the MCP client knows where to POST messages. [SRE-92.A]
// Broadcast sends an SSE event to all sessions belonging to the given
// workspace. Non-blocking: skips sessions whose eventCh is full. [376.E]
func (s *sseSessionStore) Broadcast(workspaceID string, event sseEvent) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sent := 0
	for _, sess := range s.sessions {
		if sess.workspaceID == workspaceID {
			select {
			case sess.eventCh <- event:
				sent++
			default:
			}
		}
	}
	return sent
}

func handleSSEConnect(store *sseSessionStore, registry *workspace.Registry, pool *nexus.ProcessPool, baseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}

		// [147.A] DNS-rebinding guard: reject requests whose Host header does not
		// resolve to loopback. An attacker's page cannot forge a same-origin request
		// to 127.0.0.1 if the browser enforces Host, but some clients omit it entirely
		// (curl without -H) — those are accepted for CLI/SDK use.
		if host := r.Host; host != "" {
			hostOnly := host
			if i := strings.LastIndex(host, ":"); i >= 0 {
				hostOnly = host[:i]
			}
			if hostOnly != "127.0.0.1" && hostOnly != "localhost" && hostOnly != "[::1]" {
				http.Error(w, "forbidden: only loopback access allowed", http.StatusForbidden)
				return
			}
		}

		// Resolve which child this session should route to.
		childPort := resolveChildPort(r, registry, pool)
		if childPort == 0 {
			http.Error(w, "no active workspace", http.StatusBadGateway)
			return
		}

		// Ensure we can stream.
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		now := time.Now()
		sessionID := newSessionID()
		sess := &sseSession{
			id:           sessionID,
			childPort:    childPort,
			workspaceID:  r.Header.Get("X-Neo-Workspace"), // [MCPI-46 bugfix] propagate for plugin ACL checks
			eventCh:      make(chan sseEvent, 64),
			done:         make(chan struct{}),
			createdAt:    now,
			remoteIP:     remoteIP(r),
			lastActivity: now,
		}
		// [145.B] Enforce session capacity limits before committing the session.
		if err := store.Add(sess); err != nil {
			log.Printf("[NEXUS-SSE] rejected new session from %s: %v", sess.remoteIP, err)
			http.Error(w, "too many connections", http.StatusTooManyRequests)
			return
		}

		log.Printf("[NEXUS-SSE] session %s opened (child port %d)", sessionID, childPort)

		// Set SSE headers.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "http://127.0.0.1:9000") // [147.A] restrict to loopback — prevent DNS-rebinding from foreign origins
		w.WriteHeader(http.StatusOK)

		// Send the endpoint event — tells the client where to POST messages.
		endpointURL := fmt.Sprintf("%s/mcp/message?sessionId=%s", baseURL, sessionID)
		if _, err := fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", endpointURL); err != nil {
			return
		}
		flusher.Flush()

		// Keep the stream open, writing events as they arrive.
		// The client disconnecting will close r.Context().
		ctx := r.Context()
		keepAlive := time.NewTicker(15 * time.Second)
		defer keepAlive.Stop()
		defer func() {
			close(sess.done)
			store.Remove(sessionID)
			log.Printf("[NEXUS-SSE] session %s closed (uptime %s)", sessionID,
				time.Since(sess.createdAt).Truncate(time.Second))
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-sess.eventCh:
				// SSE spec: newlines in data must be split into separate data: lines.
				escaped := strings.ReplaceAll(ev.Data, "\n", "\ndata: ")
				if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Event, escaped); err != nil {
					return
				}
				flusher.Flush()
				// [145.B] Update activity timestamp on every dispatched event so the
				// idle-timeout check in keepAlive.C sees fresh state.
				sess.lastActivity = time.Now()
			case <-keepAlive.C:
				// [145.B] Idle-timeout eviction: close sessions that received no events
				// for longer than idleTimeout (default 300s).
				if store.idleTimeout > 0 && time.Since(sess.lastActivity) > store.idleTimeout {
					log.Printf("[NEXUS-SSE] session %s idle >%s — closing", sessionID, store.idleTimeout)
					_, _ = fmt.Fprintf(w, "event: error\ndata: {\"error\":\"idle_timeout\"}\n\n")
					flusher.Flush()
					return
				}
				// Check child is still alive — close session if it died or was restarted.
				if !isChildPortAlive(pool, sess.childPort) {
					_, _ = fmt.Fprintf(w, "event: error\ndata: {\"error\":\"child_died\"}\n\n")
					flusher.Flush()
					return
				}
				// SSE keep-alive comment to prevent proxy/LB timeouts.
				if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}
}

// handleSSEMessage handles POST /mcp/message?sessionId=X — forwards the
// JSON-RPC body to the child, then pushes the response onto the SSE stream
// as a "message" event. Also returns 202 Accepted to the POST caller. [SRE-92.B]
//
// PILAR XXIII integration: when pluginRT != nil, two interception points:
//   - tools/call with a registered plugin namespace prefix bypasses the
//     child entirely and dispatches to the plugin's MCP client.
//   - tools/list responses from the child are augmented with plugin tools
//     before being pushed onto the SSE stream.
func handleSSEMessage(store *sseSessionStore, registry *workspace.Registry, pool *nexus.ProcessPool, pluginRT *pluginRuntime) http.HandlerFunc {
	// [SRE-103.A] Dedicated client for child requests — server-controlled
	// loopback target, so SafeInternalHTTPClient (loopback-only guard) not
	// SafeHTTPClient (external-capable). 300s covers long-running tool calls.
	childClient := sre.SafeInternalHTTPClient(300)

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}

		sessionID := r.URL.Query().Get("sessionId")
		log.Printf("[NEXUS-MSG] POST %s sessionId=%q origin=%s", r.URL.Path, sessionID, r.RemoteAddr)

		// Read the body (needed for scatter-gather inspection and forwarding).
		bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxMessageBody))
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		// Log method name for protocol tracing (first 120 bytes is enough).
		if len(bodyBytes) > 0 {
			preview := bodyBytes
			if len(preview) > 120 {
				preview = preview[:120]
			}
			log.Printf("[NEXUS-MSG] body=%s", preview)
		}

		// Determine child port: session-based → scatter-gather → header → active.
		var childPort int
		var sess *sseSession

		if sessionID != "" {
			sess, _ = store.Get(sessionID)
		}

		if sess != nil {
			childPort = sess.childPort
			// [SRE-92.TW] Honor target_workspace ONLY when the body carries an
			// EXPLICIT target_workspace matching this session's workspace. Without
			// an explicit target_workspace the session's pinned childPort is
			// authoritative — falling back to activeWorkspacePort would break
			// affinity when the active workspace changes after rebuild-restart.
			targetWS := workspaceIDFromBody(bodyBytes)
			if targetWS != "" {
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				if twPort := resolveChildPortFromMessage(r, registry, pool); twPort != 0 && twPort != sess.childPort {
					if targetWS == sess.workspaceID {
						childPort = twPort
						log.Printf("[NEXUS-SSE] target_workspace override: session child → port %d", twPort)
					} else {
						log.Printf("[NEXUS-SSE-ACL] cross-workspace override refused: session=%s target=%s", sess.workspaceID, targetWS)
					}
				}
			}
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes)) // re-wind for forwarding
		} else if sessionID != "" {
			// [PILAR-XXIII] Client has a session id but we no longer hold it —
			// the child was restarted, the keep-alive closed the stream, or the
			// process was recycled. Returning 404 forces the MCP SDK to tear
			// down the old SSE stream and open a fresh session, instead of
			// silently routing responses into a dead channel and leaving the
			// caller waiting forever. Fixed the 12-minute "ghost hang" bug.
			//
			// [130.4] Body now carries a JSON-RPC error envelope with a
			// `suggest_fallback_curl` flag and a copy-pasteable curl line
			// targeting /mcp/message directly (no session). Lets the agent
			// recover programmatically instead of relying on operator manual
			// intervention. The 404 status is preserved so SDK reconnect
			// logic still kicks in for SSE-aware clients.
			respondSessionLost(w, r, bodyBytes, sessionID)
			return
		} else {
			// No session — stateless POST (e.g. from curl or a non-SSE client).
			// Reconstruct body for resolveChildPortFromMessage.
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			childPort = resolveChildPortFromMessage(r, registry, pool)
		}

		if childPort == 0 {
			http.Error(w, "no active workspace", http.StatusBadGateway)
			return
		}

		// [PILAR-XXIII] Plugin tools/call routing — when name has a registered
		// plugin namespace prefix, dispatch to the plugin's MCP client instead
		// of forwarding to the child. The plugin response is wrapped as a
		// JSON-RPC envelope and pushed onto the SSE stream.
		if conn, localName := detectPluginToolCall(bodyBytes, pluginRT); conn != nil {
			// [PILAR-XXVIII 143.A] Use the workspace ID bound at SSE connect time as the
			// authoritative identity for plugin ACL enforcement. The X-Neo-Workspace header
			// and request body are client-controlled and must not be trusted for authorization
			// when a session exists — a spoofed header could bypass the allowed_workspaces
			// allowlist. Only fall through to header/body when there is no session.
			var wsID string
			if sess != nil {
				wsID = sess.workspaceID
			} else {
				wsID = r.Header.Get("X-Neo-Workspace")
				if wsID == "" {
					wsID = workspaceIDFromBody(bodyBytes)
				}
			}
			pluginResp, perr := callPluginTool(r.Context(), bodyBytes, conn, localName, pluginRT, wsID)
			if perr != nil {
				log.Printf("[NEXUS-PLUGIN] %s: %v", conn.Name, perr)
				http.Error(w, "plugin call failed", http.StatusInternalServerError)
				return
			}
			if sess != nil {
				select {
				case sess.eventCh <- sseEvent{Event: "message", Data: string(pluginResp)}:
				case <-sess.done:
					http.Error(w, "session closed", http.StatusGone)
					return
				}
				w.WriteHeader(http.StatusAccepted)
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(pluginResp)
			}
			return
		}

		// [SRE-87.A.1] Scatter-gather interceptor for cross-workspace BLAST_RADIUS.
		if isBlastRadiusCall(bodyBytes) && len(pool.List()) > 1 {
			log.Printf("[NEXUS-SCATTER] BLAST_RADIUS intercepted — fan-out to %d children", len(pool.List()))
			fused := scatterBlastRadius(bodyBytes, pool)
			if fused != nil {
				if sess != nil {
					select {
					case sess.eventCh <- sseEvent{Event: "message", Data: string(fused)}:
					case <-sess.done:
						http.Error(w, "session closed", http.StatusGone)
						return
					}
					w.WriteHeader(http.StatusAccepted)
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(fused)
				}
				return
			}
			// Fallthrough to single-child if scatter fails.
		}

		// Forward to child via POST /mcp/message.
		childURL := fmt.Sprintf("http://127.0.0.1:%d/mcp/message", childPort)
		childReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, childURL, bytes.NewReader(bodyBytes))
		if err != nil {
			http.Error(w, "failed to create child request", http.StatusInternalServerError)
			return
		}
		childReq.Header.Set("Content-Type", "application/json")

		resp, err := childClient.Do(childReq)
		if err != nil {
			log.Printf("[NEXUS-SSE] child request failed (port %d): %v", childPort, err)
			http.Error(w, "child unreachable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)

		// [PILAR-XXIII] tools/list augmentation — if the request was tools/list
		// and the child returned successfully, merge plugin tools into the
		// response so Claude sees core + plugin tools as one unified list.
		if isToolsListRequest(bodyBytes) {
			respBody = interceptPluginTools(respBody, pluginRT)
		}

		if sess != nil {
			// Push response onto the SSE stream (guarded against disconnected client).
			// Skip empty bodies — JSON-RPC notifications return no result.
			if len(respBody) > 0 {
				select {
				case sess.eventCh <- sseEvent{Event: "message", Data: string(respBody)}:
				case <-sess.done:
					http.Error(w, "session closed", http.StatusGone)
					return
				}
			}
			w.WriteHeader(http.StatusAccepted)
		} else {
			// No session — return response directly (stateless fallback).
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(respBody)
		}
	}
}

// newSessionID generates a random hex session ID without external dependencies.
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		log.Fatalf("[NEXUS-SSE] crypto/rand failed: %v", err)
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// respondSessionLost emits a JSON-RPC error envelope with HTTP 404 when the
// SSE session referenced by the POST has expired. Body fields:
//
//	error.code    = -32001 (custom: SSE session lost — distinct from -32601 method-not-found)
//	error.message = human-readable explanation
//	error.data.suggest_fallback_curl = true  // [130.4.2] programmatic flag
//	error.data.fallback_curl         = "curl -X POST ..."  // [130.4.1] copy-paste recovery
//
// The 404 status is preserved so SSE-aware MCP SDKs still trigger reconnect.
// A second log line tagged [NEXUS-FALLBACK] surfaces the curl on Nexus
// stderr/log so an operator looking at journalctl/log files sees the same
// recovery path the agent receives.
func respondSessionLost(w http.ResponseWriter, r *http.Request, bodyBytes []byte, sessionID string) {
	// Best-effort id extraction — JSON-RPC `id` may be number, string, or null.
	var rpc struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(bodyBytes, &rpc)
	idJSON := rpc.ID
	if len(idJSON) == 0 {
		idJSON = json.RawMessage("null")
	}

	wsID := r.Header.Get("X-Neo-Workspace")
	if wsID == "" {
		wsID = workspaceIDFromBody(bodyBytes)
	}
	hdr := ""
	if wsID != "" {
		hdr = fmt.Sprintf(`-H "X-Neo-Workspace: %s" `, wsID)
	}
	// Bash-safe single-quoted body: replace any embedded single quote with
	// '\'' (close quote, escaped quote, reopen quote).
	bodyForCurl := strings.ReplaceAll(string(bodyBytes), "'", `'\''`)
	scheme := "http"
	host := r.Host
	if host == "" {
		host = "127.0.0.1:9000"
	}
	fallbackCurl := fmt.Sprintf(
		`curl -X POST %s://%s/mcp/message -H "Content-Type: application/json" %s-d '%s'`,
		scheme, host, hdr, bodyForCurl,
	)

	log.Printf("[NEXUS-SSE] POST with expired sessionId=%q — returning 404 so client reconnects", sessionID)
	log.Printf("[NEXUS-FALLBACK] %s", fallbackCurl)

	envelope := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				SuggestFallbackCurl bool   `json:"suggest_fallback_curl"`
				FallbackCurl        string `json:"fallback_curl"`
				SessionID           string `json:"session_id"`
			} `json:"data"`
		} `json:"error"`
	}{
		JSONRPC: "2.0",
		ID:      idJSON,
	}
	envelope.Error.Code = -32001
	envelope.Error.Message = "SSE session not found — likely after make rebuild-restart. Either reopen the SSE stream or POST to /mcp/message directly using the curl in error.data.fallback_curl."
	envelope.Error.Data.SuggestFallbackCurl = true
	envelope.Error.Data.FallbackCurl = fallbackCurl
	envelope.Error.Data.SessionID = sessionID

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	if err := json.NewEncoder(w).Encode(envelope); err != nil {
		// Encoding shouldn't fail with our static struct, but if it does
		// fall back to plain text so the client still sees something.
		_, _ = w.Write([]byte(`{"error":"session not found"}`))
		log.Printf("[NEXUS-SSE] respondSessionLost encode error: %v", err)
	}
}
