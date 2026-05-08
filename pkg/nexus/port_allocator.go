// Package nexus implements the multi-workspace router for NeoAnvil. [SRE-68]
//
// The Nexus is a lightweight dispatcher that maintains a pool of neo-mcp child
// processes (one per workspace) and exposes a single MCP endpoint that routes
// each tool call to the correct child process.
package nexus

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
)

const (
	defaultBasePort  = 9100
	defaultPortRange = 200
)

// PortAllocation maps a workspace path to a deterministic port. [SRE-68.1.1]
type PortAllocation struct {
	WorkspaceID   string `json:"workspace_id"`
	WorkspacePath string `json:"workspace_path"`
	Port          int    `json:"port"`
}

// PortAllocator assigns deterministic ports per workspace. [SRE-68.1.1]
// Ports are computed as: basePort + (hash(workspacePath) % portRange).
// Collisions are resolved by incrementing. Allocations persist to disk.
type PortAllocator struct {
	mu          sync.Mutex
	basePort    int
	portRange   int
	allocations map[string]PortAllocation // workspacePath → allocation
	filePath    string
}

// NewPortAllocator creates an allocator backed by a JSON file. [SRE-68.1.1]
func NewPortAllocator(basePort, portRange int, filePath string) *PortAllocator {
	if basePort == 0 {
		basePort = defaultBasePort
	}
	if portRange == 0 {
		portRange = defaultPortRange
	}
	pa := &PortAllocator{
		basePort:    basePort,
		portRange:   portRange,
		allocations: make(map[string]PortAllocation),
		filePath:    filePath,
	}
	pa.load()
	return pa
}

// Allocate returns a port for the given workspace. Idempotent — returns
// the same port on repeated calls for the same path. [SRE-68.1.1]
func (pa *PortAllocator) Allocate(workspaceID, workspacePath string) (int, error) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	// Return existing allocation
	if alloc, ok := pa.allocations[workspacePath]; ok {
		return alloc.Port, nil
	}

	// Compute deterministic port
	port := pa.hashPort(workspacePath)

	// Resolve collisions
	usedPorts := make(map[int]bool, len(pa.allocations))
	for _, a := range pa.allocations {
		usedPorts[a.Port] = true
	}

	attempts := 0
	for usedPorts[port] || !isPortFree(port) {
		port++
		if port >= pa.basePort+pa.portRange {
			port = pa.basePort
		}
		attempts++
		if attempts >= pa.portRange {
			return 0, fmt.Errorf("no free ports in range %d-%d", pa.basePort, pa.basePort+pa.portRange)
		}
	}

	pa.allocations[workspacePath] = PortAllocation{
		WorkspaceID:   workspaceID,
		WorkspacePath: workspacePath,
		Port:          port,
	}
	_ = pa.save()
	return port, nil
}

// Release frees a port allocation. [SRE-68.1.1]
func (pa *PortAllocator) Release(workspacePath string) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	delete(pa.allocations, workspacePath)
	_ = pa.save()
}

// GetPort returns the allocated port for a workspace, or 0 if not allocated.
func (pa *PortAllocator) GetPort(workspacePath string) int {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	if alloc, ok := pa.allocations[workspacePath]; ok {
		return alloc.Port
	}
	return 0
}

// All returns all current allocations.
func (pa *PortAllocator) All() []PortAllocation {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	result := make([]PortAllocation, 0, len(pa.allocations))
	for _, a := range pa.allocations {
		result = append(result, a)
	}
	return result
}

func (pa *PortAllocator) hashPort(path string) int {
	h := sha256.Sum256([]byte(path))
	n := binary.BigEndian.Uint32(h[:4])
	return pa.basePort + int(n)%pa.portRange
}

func (pa *PortAllocator) load() {
	data, err := os.ReadFile(pa.filePath)
	if err != nil {
		return
	}
	var allocs []PortAllocation
	if err := json.Unmarshal(data, &allocs); err != nil {
		return
	}
	for _, a := range allocs {
		pa.allocations[a.WorkspacePath] = a
	}
}

func (pa *PortAllocator) save() error {
	allocs := make([]PortAllocation, 0, len(pa.allocations))
	for _, a := range pa.allocations {
		allocs = append(allocs, a)
	}
	data, err := json.MarshalIndent(allocs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(pa.filePath), 0700); err != nil {
		return err
	}
	return os.WriteFile(pa.filePath, data, 0600)
}

// AllocateMockPort finds a free port in the given range [base, base+size). [291.B]
// Used by neo_command(action:"mock_start") to assign ephemeral mock server ports.
// Returns 0 if no port is free (caller should fall back to port 0 / OS-assigned).
func AllocateMockPort(base, size int) int {
	if size <= 0 {
		size = 100
	}
	if base <= 0 {
		base = 34800
	}
	for i := 0; i < size; i++ {
		p := base + i
		if isPortFree(p) {
			return p
		}
	}
	return 0
}

func isPortFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}
