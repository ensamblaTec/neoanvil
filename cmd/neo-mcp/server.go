package main

// [SRE-85.C] Headless RPC server — no stdio, no SSE broadcast.
//
// neo-mcp is a pure HTTP worker. It exposes POST /mcp/message as the sole
// JSON-RPC entry point. Nexus (cmd/neo-nexus) is the only MCP transport
// endpoint — it handles stdio/SSE client connections and proxies to us.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// [SRE-28.3.1] Activity sensor — updated on every tool call, drives REM sleep detection.
var LastActivityTimestamp atomic.Int64

// [SRE-31.3.2] ThermicStabilizing — set to 1 when RAPL > 60W. DaemonTool checks this
// before executing tasks to avoid scheduling CPU load during thermal events.
var ThermicStabilizing atomic.Int32

type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type RPCResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// [SRE-22.3.1] Global atomic counters for token budget awareness
var (
	bytesReceived atomic.Int64
	bytesSent     atomic.Int64
)

// MCPServer handles JSON-RPC requests via HTTP POST. [SRE-85.C]
type MCPServer struct {
	cbMutex           sync.Mutex
	consecutiveErrors int
	cbResetAt         time.Time
}

func NewMCPServer() *MCPServer {
	return &MCPServer{}
}

// HandleMessage returns an HTTP handler for POST /mcp/message (JSON-RPC).
// Each request is decoded, dispatched to the handler, and the response is
// written synchronously. No SSE broadcast, no fan-out. [SRE-85.C.3]
func (mcp *MCPServer) HandleMessage(handler func(ctx context.Context, req RPCRequest) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req RPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON-RPC", http.StatusBadRequest)
			return
		}

		bytesReceived.Add(r.ContentLength)

		// Circuit breaker check.
		mcp.cbMutex.Lock()
		if mcp.consecutiveErrors >= 5 && !mcp.cbResetAt.IsZero() && time.Now().After(mcp.cbResetAt) {
			log.Printf("[SRE-CB-RECOVERY] Circuit Breaker auto-reset after cooldown.")
			mcp.consecutiveErrors = 0
			mcp.cbResetAt = time.Time{}
		}
		if mcp.consecutiveErrors >= 5 {
			if mcp.cbResetAt.IsZero() {
				mcp.cbResetAt = time.Now().Add(30 * time.Second)
			}
			mcp.cbMutex.Unlock()
			w.Header().Set("Content-Type", "application/json")
			resp := RPCResponse{
				ID:      req.ID,
				JSONRPC: "2.0",
				Error:   &RPCError{Code: -32001, Message: "[SRE-VETO] Circuit Breaker triggered. Cool down required."},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		mcp.cbMutex.Unlock()

		// Execute with panic recovery and timeout.
		var result any
		var execErr error
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					stack := debug.Stack()
					log.Printf("[SRE-ERROR] Panic in RPC handler for ID %v: %v\nStack: %s", req.ID, rec, string(stack))
					execErr = fmt.Errorf("internal panic: %v", rec)
				}
			}()
			const maxToolDuration = 5 * time.Minute
			toolCtx, cancel := context.WithTimeout(r.Context(), maxToolDuration)
			defer cancel()
			result, execErr = handler(toolCtx, req)
		}()

		if execErr != nil {
			mcp.cbMutex.Lock()
			mcp.consecutiveErrors++
			mcp.cbMutex.Unlock()
		} else {
			mcp.cbMutex.Lock()
			mcp.consecutiveErrors = 0
			mcp.cbMutex.Unlock()
		}

		// JSON-RPC notifications have no id — must not send a response.
		if req.ID == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		resp := RPCResponse{ID: req.ID, JSONRPC: "2.0"}
		if execErr != nil {
			resp.Error = &RPCError{Code: -32000, Message: execErr.Error()}
		} else {
			resp.Result = result
		}

		data, marshalErr := json.Marshal(resp)
		if marshalErr != nil {
			http.Error(w, "marshal error", http.StatusInternalServerError)
			return
		}
		bytesSent.Add(int64(len(data)))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

// GetIOStats returns bytes received/sent for token budget awareness.
func GetIOStats() (int64, int64) {
	return bytesReceived.Load(), bytesSent.Load()
}
