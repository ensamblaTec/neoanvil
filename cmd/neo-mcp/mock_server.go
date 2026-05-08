// cmd/neo-mcp/mock_server.go — Ephemeral HTTP mock server. [PILAR-XXXVIII/291.A]
//
// MockServer serves deterministic fake-but-type-correct JSON responses derived
// from CONTRACT_QUERY contracts. Used by the frontend agent to run tests without
// needing the real backend.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
)

// MockServer is an ephemeral HTTP server that mounts fake handlers for each contract. [291.A]
type MockServer struct {
	mu        sync.Mutex
	port      int
	contracts []cpg.ContractNode
	stopCh    chan struct{}
	srv       *http.Server
	schema    map[string][]cpg.SchemaNode // path → schema fields for richer responses
}

// NewMockServer creates a MockServer for the given endpoint paths (filtered from contracts). [291.A]
func NewMockServer(contracts []cpg.ContractNode, endpoints []string) *MockServer {
	// Filter to requested endpoints; if endpoints empty use all.
	filtered := contracts
	if len(endpoints) > 0 {
		set := make(map[string]struct{}, len(endpoints))
		for _, e := range endpoints {
			set[e] = struct{}{}
		}
		filtered = filtered[:0]
		for _, c := range contracts {
			if _, ok := set[c.Path]; ok {
				filtered = append(filtered, c)
			}
		}
	}
	return &MockServer{
		contracts: filtered,
		stopCh:    make(chan struct{}),
		schema:    make(map[string][]cpg.SchemaNode),
	}
}

// AddSchema registers request schema fields for a given path (used for richer fake responses). [291.A]
func (m *MockServer) AddSchema(path string, fields []cpg.SchemaNode) {
	m.mu.Lock()
	m.schema[path] = fields
	m.mu.Unlock()
}

// Start binds to port (0 = OS-assigned) and begins serving. Returns the bound port. [291.A]
func (m *MockServer) Start(port int) (int, error) {
	mux := http.NewServeMux()

	// Register a handler per contract.
	for _, c := range m.contracts {
		path := c.Path
		method := c.Method
		fields := m.schema[path]
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if method != "" && r.Method != method && r.Method != "OPTIONS" {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Mock-Server", "neo-mock/1")
			body := buildFakeResponse(path, fields)
			_ = json.NewEncoder(w).Encode(body)
		})
	}

	// Health check.
	mux.HandleFunc("/__mock/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","routes":%d}`, len(m.contracts))
	})

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return 0, fmt.Errorf("mock_server: listen: %w", err)
	}
	m.mu.Lock()
	m.port = ln.Addr().(*net.TCPAddr).Port
	m.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	m.mu.Unlock()

	go func() {
		_ = m.srv.Serve(ln) //nolint:gosec // G114: intentional ephemeral test server on loopback
	}()
	return m.port, nil
}

// Port returns the bound port (valid after Start). [291.A]
func (m *MockServer) Port() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.port
}

// Stop shuts down the mock server gracefully. [291.A/E]
func (m *MockServer) Stop() {
	m.mu.Lock()
	srv := m.srv
	m.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
}

// buildFakeResponse constructs a deterministic JSON-serializable map for the endpoint. [291.A]
func buildFakeResponse(path string, fields []cpg.SchemaNode) map[string]any {
	resp := make(map[string]any)
	if len(fields) > 0 {
		for _, f := range fields {
			resp[f.Field] = fakeValue(f.Field, f.Type)
		}
		return resp
	}
	// Default when no schema is available: derive minimal fields from path segments.
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) > 0 {
		last := parts[len(parts)-1]
		if last == "" || strings.HasPrefix(last, ":") || strings.HasPrefix(last, "{") {
			resp["id"] = 42
			resp["status"] = "ok"
		} else {
			resp["id"] = 42
			resp[last] = "sample_" + last
		}
	}
	resp["_mock"] = true
	return resp
}

// fakeCompositeValue matches a field by name semantics. [302.D]
// HasSuffix(name,"id") subsumes HasSuffix(name,"_id") — no redundant clause needed.
func fakeCompositeValue(name string) (any, bool) {
	switch {
	case name == "email" || strings.HasSuffix(name, "email"):
		return "test@example.com", true
	case name == "id" || strings.HasSuffix(name, "id"):
		return 42, true
	case name == "name" || strings.HasSuffix(name, "name"):
		return "sample_name", true
	case name == "url" || strings.HasSuffix(name, "url"):
		return "https://example.com", true
	case name == "token" || strings.HasSuffix(name, "token"):
		return "tok_sample", true
	case name == "count" || name == "total" || name == "limit" || name == "offset":
		return 10, true
	}
	return nil, false
}

// fakeScalarValue maps a primitive Go type to a representative value. [302.D]
// Returns nil for unknown types so fakeValue can fall back to a string default.
func fakeScalarValue(goType string) any {
	switch {
	case goType == "bool":
		return true
	case strings.HasPrefix(goType, "int") || strings.HasPrefix(goType, "uint"):
		return 42
	case strings.HasPrefix(goType, "float"):
		return 3.14
	default:
		return nil
	}
}

// fakeContainerValue builds a single-element slice whose item is synthesised
// recursively, unwrapping one layer of the Go slice type. [302.D]
func fakeContainerValue(fieldName, goType string) []any {
	return []any{fakeValue(fieldName+"_item", strings.TrimPrefix(goType, "[]"))}
}

// fakeValue returns a deterministic fake value for a struct field. [291.A]
// Rules are intentionally naive — just enough for frontend test assertions.
func fakeValue(fieldName, goType string) any {
	name := strings.ToLower(fieldName)
	if v, ok := fakeCompositeValue(name); ok {
		return v
	}
	if strings.HasPrefix(goType, "[]") {
		return fakeContainerValue(fieldName, goType)
	}
	if v := fakeScalarValue(goType); v != nil {
		return v
	}
	return "sample_" + name
}
