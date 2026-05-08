package cpg

import (
	"os"
	"path/filepath"
	"testing"
)

// TestContractNodeMerge verifies MergeContracts deduplicates by (method, path)
// and prefers openapi source. [Épica 254.D]
func TestContractNodeMerge(t *testing.T) {
	openapi := []ContractNode{
		{Method: "GET", Path: "/api/users", Source: "openapi"},
		{Method: "POST", Path: "/api/users", Source: "openapi"},
		{Method: "GET", Path: "/api/posts", Source: "openapi"},
	}
	parsed := []ContractNode{
		{Method: "GET", Path: "/api/users", BackendFn: "listUsers", Source: "parsed"},
		{Method: "DELETE", Path: "/api/posts/{id}", Source: "parsed"},
	}
	merged := MergeContracts(openapi, parsed)

	if len(merged) != 4 {
		t.Fatalf("expected 4 contracts, got %d: %v", len(merged), merged)
	}
	// Overlapping (GET /api/users) must keep openapi source.
	for _, c := range merged {
		if c.Method == "GET" && c.Path == "/api/users" {
			if c.Source != "openapi" {
				t.Errorf("expected source=openapi for GET /api/users, got %s", c.Source)
			}
		}
	}
}

// TestExtractGoRoutes verifies AST scan picks up gin-style route registrations. [Épica 254.B]
func TestExtractGoRoutes(t *testing.T) {
	workspace := t.TempDir()
	mustMkdirBridge(t, filepath.Join(workspace, "cmd", "api"))
	mustWriteBridge(t, filepath.Join(workspace, "cmd", "api", "routes.go"), `package api

import "github.com/gin-gonic/gin"

func RegisterRoutes(r *gin.Engine) {
	r.GET("/api/users", listUsers)
	r.POST("/api/users", createUser)
	r.DELETE("/api/users/:id", deleteUser)
}

func listUsers(c *gin.Context)  {}
func createUser(c *gin.Context) {}
func deleteUser(c *gin.Context) {}
`)

	contracts, err := ExtractGoRoutes(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(contracts) == 0 {
		t.Fatal("expected routes, got none")
	}
	pathsSeen := make(map[string]bool)
	for _, c := range contracts {
		pathsSeen[c.Method+":"+c.Path] = true
	}
	if !pathsSeen["GET:/api/users"] {
		t.Errorf("GET /api/users not found in %v", contracts)
	}
	if !pathsSeen["POST:/api/users"] {
		t.Errorf("POST /api/users not found in %v", contracts)
	}
	// :id → {id} normalization
	if !pathsSeen["DELETE:/api/users/{id}"] {
		t.Errorf("DELETE /api/users/{id} not found (normalization failed): %v", contracts)
	}
}

// TestTSLinker verifies LinkTSCallers matches fetch calls to contract paths. [Épica 255.D]
func TestTSLinker(t *testing.T) {
	workspace := t.TempDir()
	mustMkdirBridge(t, filepath.Join(workspace, "src", "hooks"))
	mustWriteBridge(t, filepath.Join(workspace, "src", "hooks", "useUsers.ts"), `
import { useQuery } from 'react-query'

export function useUsers() {
  return useQuery('users', () => fetch('/api/users').then(r => r.json()))
}
`)
	mustWriteBridge(t, filepath.Join(workspace, "src", "hooks", "useAuth.ts"), `
export function useAuth() {
  return fetch('/api/auth/me')
}
`)

	contracts := []ContractNode{
		{Method: "GET", Path: "/api/users", Source: "openapi"},
		{Method: "GET", Path: "/api/auth/me", Source: "openapi"},
		{Method: "GET", Path: "/api/posts", Source: "openapi"},
	}
	linked := LinkTSCallers(workspace, contracts)

	usersIdx, authIdx, postsIdx := -1, -1, -1
	for i, c := range linked {
		switch c.Path {
		case "/api/users":
			usersIdx = i
		case "/api/auth/me":
			authIdx = i
		case "/api/posts":
			postsIdx = i
		}
	}
	if usersIdx < 0 || len(linked[usersIdx].FrontendCallers) == 0 {
		t.Errorf("GET /api/users has no frontend callers")
	}
	if authIdx < 0 || len(linked[authIdx].FrontendCallers) == 0 {
		t.Errorf("GET /api/auth/me has no frontend callers")
	}
	if postsIdx >= 0 && len(linked[postsIdx].FrontendCallers) > 0 {
		t.Errorf("GET /api/posts should have no callers, got %v", linked[postsIdx].FrontendCallers)
	}
}

// TestCrossBoundaryEndToEnd verifies the full pipeline: ExtractGoRoutes → LinkTSCallers → InsertContractNodes. [Épica 257.B]
func TestCrossBoundaryEndToEnd(t *testing.T) {
	workspace := t.TempDir()

	// Backend: gin handler
	mustMkdirBridge(t, filepath.Join(workspace, "cmd", "api"))
	mustWriteBridge(t, filepath.Join(workspace, "cmd", "api", "routes.go"), `package api

import "github.com/gin-gonic/gin"

func Register(r *gin.Engine) {
	r.GET("/api/users", getUsers)
}
func getUsers(c *gin.Context) {}
`)

	// Frontend: fetch call
	mustMkdirBridge(t, filepath.Join(workspace, "src", "hooks"))
	mustWriteBridge(t, filepath.Join(workspace, "src", "hooks", "useUsers.ts"),
		`export const fetchUsers = () => fetch('/api/users').then(r => r.json())`)

	// tsconfig with @/ alias
	mustWriteBridge(t, filepath.Join(workspace, "tsconfig.json"),
		`{"compilerOptions":{"paths":{"@/*":["src/*"]}}}`)

	parsed, _ := ExtractGoRoutes(workspace)
	if len(parsed) == 0 {
		t.Fatal("ExtractGoRoutes returned no contracts")
	}
	linked := LinkTSCallers(workspace, parsed)

	var usersContract *ContractNode
	for i := range linked {
		if linked[i].Path == "/api/users" {
			usersContract = &linked[i]
			break
		}
	}
	if usersContract == nil {
		t.Fatal("/api/users contract not found")
	}
	if len(usersContract.FrontendCallers) == 0 {
		t.Errorf("expected frontend callers for /api/users, got none")
	}

	// Verify InsertContractNodes doesn't panic on a fresh graph.
	g := newGraph()
	InsertContractNodes(g, workspace, linked)
	contractNodes := 0
	for _, n := range g.Nodes {
		if n.Kind == NodeContract {
			contractNodes++
		}
	}
	if contractNodes == 0 {
		t.Error("expected NodeContract entries in graph after InsertContractNodes")
	}
}

func mustMkdirBridge(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWriteBridge(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
