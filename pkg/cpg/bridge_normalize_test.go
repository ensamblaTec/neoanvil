package cpg

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNormalizeRoutePath covers framework-path-param normalization and trailing-slash rules. [Épica 270.A]
func TestNormalizeRoutePath(t *testing.T) {
	cases := []struct{ input, want string }{
		{"/users/:id", "/users/{id}"},
		{"/items/<slug>", "/items/{slug}"},
		{"/api/v1/posts/:postID/comments/:commentID", "/api/v1/posts/{postID}/comments/{commentID}"},
		{"/api/users/", "/api/users"},
		{"/", "/"},
		{"/plain/path", "/plain/path"},
		{"/:a/:b/:c", "/{a}/{b}/{c}"},
		{"/mixed/:id/static", "/mixed/{id}/static"},
	}
	for _, tc := range cases {
		got := normalizeRoutePath(tc.input)
		if got != tc.want {
			t.Errorf("normalizeRoutePath(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestExtractRouteCallGoFile verifies extractRouteCall picks up GET/POST/DELETE/HandleFunc
// via a real temp-file AST walk using ExtractGoRoutes. [Épica 270.B]
func TestExtractRouteCallGoFile(t *testing.T) {
	workspace := t.TempDir()
	mustMkdirBridge(t, filepath.Join(workspace, "cmd"))
	mustWriteBridge(t, filepath.Join(workspace, "cmd", "routes.go"), `package cmd

func Register() {
	r.GET("/health", healthHandler)
	r.POST("/api/users", createUser)
	r.DELETE("/api/users/:id", deleteUser)
	mux.HandleFunc("/api/status", statusHandler)
}
`)
	contracts, err := ExtractGoRoutes(workspace)
	if err != nil {
		t.Fatalf("ExtractGoRoutes: %v", err)
	}
	got := map[string]string{} // "METHOD:path" → handlerName
	for _, c := range contracts {
		got[c.Method+":"+c.Path] = c.BackendFn
	}
	type want struct{ key, handler string }
	expected := []want{
		{"GET:/health", "healthHandler"},
		{"POST:/api/users", "createUser"},
		{"DELETE:/api/users/{id}", "deleteUser"},
		{"ANY:/api/status", "statusHandler"},
	}
	for _, w := range expected {
		h, ok := got[w.key]
		if !ok {
			t.Errorf("missing contract %s (got keys: %v)", w.key, keys(got))
			continue
		}
		if h != w.handler {
			t.Errorf("contract %s: handler = %q, want %q", w.key, h, w.handler)
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestExtractRouteCall_NestedGroups verifies that routes declared on nested
// RouterGroup identifiers resolve to their full accumulated path. Mirrors the
// gin/echo pattern used in strategos/backend router.go. [330.G]
func TestExtractRouteCall_NestedGroups(t *testing.T) {
	workspace := t.TempDir()
	mustMkdirBridge(t, filepath.Join(workspace, "cmd"))
	mustWriteBridge(t, filepath.Join(workspace, "cmd", "router.go"), `package cmd

func Register(c Engine) {
	c.GET("/health", healthHandler)
	v1 := c.Group("/api/v1/")
	authGroup := v1.Group("auth")
	authGroup.POST("/login", authLogin)
	protected := v1.Group("")
	adminOnly := protected.Group("")
	users := adminOnly.Group("users")
	users.GET("", userList)
	users.GET("/:id", userGet)
	users.DELETE("/:id", userDelete)
	users.POST("", userCreate)
	users.PUT("/:id", userUpdate)
	stations := adminOnly.Group("stations")
	stations.POST("", stationCreate)
	stations.GET("", stationList)
	adminOnly.POST("auth/register", authRegister)
}
`)
	contracts, err := ExtractGoRoutes(workspace)
	if err != nil {
		t.Fatalf("ExtractGoRoutes: %v", err)
	}
	got := map[string]string{}
	for _, c := range contracts {
		got[c.Method+":"+c.Path] = c.BackendFn
	}
	type want struct{ key, handler string }
	expected := []want{
		{"GET:/health", "healthHandler"},
		{"POST:/api/v1/auth/login", "authLogin"},
		{"GET:/api/v1/users", "userList"},
		{"GET:/api/v1/users/{id}", "userGet"},
		{"DELETE:/api/v1/users/{id}", "userDelete"},
		{"POST:/api/v1/users", "userCreate"},
		{"PUT:/api/v1/users/{id}", "userUpdate"},
		{"POST:/api/v1/stations", "stationCreate"},
		{"GET:/api/v1/stations", "stationList"},
		{"POST:/api/v1/auth/register", "authRegister"},
	}
	for _, w := range expected {
		h, ok := got[w.key]
		if !ok {
			t.Errorf("missing contract %s (got keys: %v)", w.key, keys(got))
			continue
		}
		if h != w.handler {
			t.Errorf("contract %s: handler = %q, want %q", w.key, h, w.handler)
		}
	}
	if len(contracts) < len(expected) {
		t.Errorf("expected at least %d contracts, got %d", len(expected), len(contracts))
	}
}

// TestJoinRoutePath covers edge cases of path joining: trailing/leading slashes,
// empty inputs, and the classic `/api/v1/` + `auth` pattern. [330.G]
func TestJoinRoutePath(t *testing.T) {
	cases := []struct{ left, right, want string }{
		{"/api/v1/", "auth", "/api/v1/auth"},
		{"/api/v1", "/users", "/api/v1/users"},
		{"/api/v1/users", "/:id", "/api/v1/users/:id"},
		{"", "/health", "/health"},
		{"/api", "", "/api"},
		{"", "", ""},
		{"/api/v1/", "", "/api/v1/"},
		{"/", "/foo", "/foo"},
	}
	for _, tc := range cases {
		got := joinRoutePath(tc.left, tc.right)
		if got != tc.want {
			t.Errorf("joinRoutePath(%q, %q) = %q, want %q", tc.left, tc.right, got, tc.want)
		}
	}
}

// TestParseOpenAPIContractsFixture verifies ParseOpenAPIContracts parses a real spec. [Épica 270.C]
func TestParseOpenAPIContractsFixture(t *testing.T) {
	workspace := t.TempDir()
	spec := `
openapi: "3.0.0"
info:
  title: Test API
paths:
  /api/users:
    get:
      summary: List users
    post:
      summary: Create user
  /api/users/{id}:
    get:
      summary: Get user
    delete:
      summary: Delete user
  /api/health:
    get:
      summary: Health check
`
	if err := os.WriteFile(filepath.Join(workspace, "openapi.yaml"), []byte(spec), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	contracts, err := ParseOpenAPIContracts(workspace)
	if err != nil {
		t.Fatalf("ParseOpenAPIContracts: %v", err)
	}
	if len(contracts) < 5 {
		t.Errorf("expected ≥5 contracts, got %d: %v", len(contracts), contracts)
	}
	for _, c := range contracts {
		if c.Source != "openapi" {
			t.Errorf("contract %s %s has Source=%q, want openapi", c.Method, c.Path, c.Source)
		}
	}
	found := false
	for _, c := range contracts {
		if c.Path == "/api/users/{id}" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("/api/users/{id} not found in contracts: %v", contracts)
	}
}

// TestMatchesContractPathDirect tests matchesContractPath on exact, partial, and alias cases. [Épica 270.E]
func TestMatchesContractPathDirect(t *testing.T) {
	aliases := map[string]string{"@": "src"}

	cases := []struct {
		line    string
		path    string
		wantHit bool
		desc    string
	}{
		{`fetch("/api/users")`, "/api/users", true, "exact double-quote match"},
		{`fetch('/api/users')`, "/api/users", true, "exact single-quote match"},
		{"fetch(`/api/users`)", "/api/users", true, "exact backtick match"},
		{`axios.get("/api/other")`, "/api/users", false, "different path no match"},
		{`const url = "/api/users"`, "/api/users", true, "string in variable assignment"},
		// partial segment — last segment ≥4 chars, no exact match in line
		{`fetch("/v1/users")`, "/api/users", true, "partial segment 'users' (≥4 chars)"},
		// short segment (<4 chars) — no partial match, no exact match either
		{`fetch("/v1")`, "/api/v1", false, "segment 'v1' length 2 < 4 → no partial match"},
		// param segment should not partial match
		{`fetch("/items/123")`, "/api/{id}", false, "brace-prefixed segment skipped"},
		// alias resolution: "@" maps to "src", contractPath "src/api/users" → aliasedPath "@/api/users"
		{`fetch("@/api/users")`, "src/api/users", true, "alias @/ → src/ match"},
	}

	for _, tc := range cases {
		got := matchesContractPath(tc.line, tc.path, aliases)
		if got != tc.wantHit {
			t.Errorf("[%s] matchesContractPath(%q, %q) = %v, want %v",
				tc.desc, tc.line, tc.path, got, tc.wantHit)
		}
	}
}
