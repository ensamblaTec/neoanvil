package cpg

import (
	"path/filepath"
	"testing"
)

// TestExtractGoRoutes_NestedGroupChain replicates the strategos
// router pattern that triggered Nexus debt T004 (CONTRACT_QUERY 10%
// coverage). The router uses inline-chained Group() calls without
// intermediate variable bindings:
//
//	api.Group("/v1").Group("/auth").POST("/login", loginHandler)
//
// Pre-fix this returned `found:false` because collectRouteGroupPrefixes
// only catches `var := parent.Group(...)` assignments. walkRouterChain
// now unwinds inline chains and accumulates the path segments.
//
// [T004 nexus / 2026-05-10]
func TestExtractGoRoutes_NestedGroupChain(t *testing.T) {
	workspace := t.TempDir()
	mustMkdirBridge(t, filepath.Join(workspace, "router"))
	mustWriteBridge(t, filepath.Join(workspace, "router", "routes.go"), `package router

import "github.com/gin-gonic/gin"

func RegisterRoutes(r *gin.Engine) {
	// Pattern A: bound variable chain (already worked pre-fix).
	api := r.Group("/api")
	auth := api.Group("/auth")
	auth.POST("/login", loginHandler)

	// Pattern B: INLINE chain (the bug — these were dropped pre-fix).
	r.Group("/api").Group("/v2").GET("/users", listUsersV2)
	r.Group("/admin").Group("/system").Group("/health").GET("/ping", adminPing)

	// Pattern C: mix of bound + inline.
	api.Group("/orders").DELETE("/:id", deleteOrder)
}

func loginHandler(c *gin.Context)  {}
func listUsersV2(c *gin.Context)   {}
func adminPing(c *gin.Context)     {}
func deleteOrder(c *gin.Context)   {}
`)

	contracts, err := ExtractGoRoutes(workspace)
	if err != nil {
		t.Fatal(err)
	}

	seen := make(map[string]bool)
	for _, c := range contracts {
		seen[c.Method+":"+c.Path] = true
	}

	wants := []string{
		"POST:/api/auth/login",
		"GET:/api/v2/users",
		"GET:/admin/system/health/ping",
		"DELETE:/api/orders/{id}",
	}
	for _, w := range wants {
		if !seen[w] {
			t.Errorf("missing route %q\nall routes seen:\n  %v", w, contractKeys(seen))
		}
	}
}

// TestExtractGoRoutes_DynamicGroupSegmentSkipped verifies dynamic segments
// (variable instead of string literal) don't crash the extractor — they're
// silently dropped because we can't resolve the path at parse time.
func TestExtractGoRoutes_DynamicGroupSegmentSkipped(t *testing.T) {
	workspace := t.TempDir()
	mustMkdirBridge(t, filepath.Join(workspace, "router"))
	mustWriteBridge(t, filepath.Join(workspace, "router", "dynamic.go"), `package router

import "github.com/gin-gonic/gin"

func RegisterRoutes(r *gin.Engine) {
	prefix := "/api/v1"
	r.Group(prefix).GET("/dynamic", dynHandler)
	r.Group("/static").GET("/ok", okHandler)
}

func dynHandler(c *gin.Context) {}
func okHandler(c *gin.Context)  {}
`)
	contracts, err := ExtractGoRoutes(workspace)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, c := range contracts {
		seen[c.Method+":"+c.Path] = true
	}
	if !seen["GET:/static/ok"] {
		t.Errorf("GET /static/ok missing: %v", contractKeys(seen))
	}
}

func contractKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
