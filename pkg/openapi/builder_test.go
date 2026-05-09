package openapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// fakeContract implements ContractIface for tests.
type fakeContract struct{ method, path, fn string }

func (f fakeContract) GetMethod() string    { return f.method }
func (f fakeContract) GetPath() string      { return f.path }
func (f fakeContract) GetBackendFn() string { return f.fn }

// fakeTool implements ToolIface for tests.
type fakeTool struct {
	name, desc string
	input      map[string]any
}

func (f fakeTool) GetName() string             { return f.name }
func (f fakeTool) GetDescription() string      { return f.desc }
func (f fakeTool) GetInputSchema() map[string]any { return f.input }

func TestBuildSpec_RendersPathsAndOperations(t *testing.T) {
	contracts := []ContractIface{
		fakeContract{"GET", "/api/v1/workspaces", "ListWorkspaces"},
		fakeContract{"POST", "/api/v1/workspaces", "CreateWorkspace"},
		fakeContract{"GET", "/api/v1/workspaces/{id}", "GetWorkspace"},
		fakeContract{"DELETE", "/api/v1/workspaces/{id}", "DeleteWorkspace"},
	}
	spec := BuildSpec(contracts, nil, BuildOptions{
		Title:   "test",
		Version: "1.0.0",
		BaseURL: "http://127.0.0.1:9000",
	})
	if spec.Info.Title != "test" {
		t.Errorf("Title = %q, want test", spec.Info.Title)
	}
	if got := len(spec.Paths); got != 2 {
		t.Errorf("paths = %d, want 2 (workspaces + workspaces/{id})", got)
	}
	listPath, ok := spec.Paths["/api/v1/workspaces"]
	if !ok {
		t.Fatalf("missing /api/v1/workspaces")
	}
	if listPath.Get == nil || listPath.Post == nil {
		t.Errorf("expected GET + POST on /workspaces, got %+v", listPath)
	}
	idPath := spec.Paths["/api/v1/workspaces/{id}"]
	if idPath.Get == nil || idPath.Delete == nil {
		t.Errorf("expected GET + DELETE on /workspaces/{id}")
	}
	// Path parameters should have been extracted.
	if len(idPath.Get.Parameters) != 1 || idPath.Get.Parameters[0].Name != "id" {
		t.Errorf("expected {id} param on GET /workspaces/{id}, got %+v", idPath.Get.Parameters)
	}
}

func TestBuildSpec_ExcludesInternalByDefault(t *testing.T) {
	contracts := []ContractIface{
		fakeContract{"GET", "/api/v1/status", "Status"},
		fakeContract{"GET", "/internal/debug", "Debug"},
	}
	spec := BuildSpec(contracts, nil, BuildOptions{IncludeInternal: false})
	if _, ok := spec.Paths["/internal/debug"]; ok {
		t.Errorf("internal path should be excluded by default")
	}
	specInc := BuildSpec(contracts, nil, BuildOptions{IncludeInternal: true})
	if _, ok := specInc.Paths["/internal/debug"]; !ok {
		t.Errorf("internal path should be included with flag")
	}
}

func TestBuildSpec_MCPToolsExtension(t *testing.T) {
	tools := []ToolIface{
		fakeTool{name: "neo_radar", desc: "radar", input: map[string]any{"type": "object"}},
		fakeTool{name: "aaa_first_alphabetical", desc: "x", input: map[string]any{}},
	}
	spec := BuildSpec(nil, tools, BuildOptions{IncludeMCPTools: true})
	jsonMap := spec.toJSONMap()
	xtools, ok := jsonMap["x-mcp-tools"].([]map[string]any)
	if !ok {
		t.Fatalf("x-mcp-tools missing or wrong type: %T", jsonMap["x-mcp-tools"])
	}
	if len(xtools) != 2 {
		t.Fatalf("got %d tools, want 2", len(xtools))
	}
	if xtools[0]["name"] != "aaa_first_alphabetical" {
		t.Errorf("expected sorted alpha; got %v first", xtools[0]["name"])
	}
}

func TestCache_BuildsLazilyAndMemoizes(t *testing.T) {
	calls := 0
	build := func(internal bool) *Spec {
		calls++
		return &Spec{OpenAPI: "3.0.3", Info: Info{Title: "t"}, Paths: map[string]PathItem{}}
	}
	c := NewCache(build)
	body1, err := c.bytes(false)
	if err != nil {
		t.Fatal(err)
	}
	body2, err := c.bytes(false)
	if err != nil {
		t.Fatal(err)
	}
	if string(body1) != string(body2) {
		t.Errorf("cache returned different bytes")
	}
	if calls != 1 {
		t.Errorf("expected 1 build call, got %d", calls)
	}
	// Different bucket builds again.
	if _, err := c.bytes(true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("expected 2 build calls after include_internal=true, got %d", calls)
	}
	// Invalidate forces rebuild.
	c.InvalidateCache()
	if _, err := c.bytes(false); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("expected 3 build calls after Invalidate, got %d", calls)
	}
}

func TestCache_HandlerReturnsValidJSON(t *testing.T) {
	build := func(internal bool) *Spec {
		return BuildSpec(
			[]ContractIface{fakeContract{"GET", "/api/v1/workspaces", "ListWS"}},
			nil,
			BuildOptions{Title: "T", Version: "1.0"},
		)
	}
	c := NewCache(build)
	body, _ := c.bytes(false)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, body)
	}
	if got := parsed["openapi"]; got != "3.0.3" {
		t.Errorf("openapi version = %v, want 3.0.3", got)
	}
	paths, ok := parsed["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		t.Errorf("paths missing or empty")
	}
}

func TestCamelize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"api/v1/workspaces", "ApiV1Workspaces"},
		{"api/v1/workspaces/{id}", "ApiV1WorkspacesId"},
		{"foo-bar_baz", "FooBarBaz"},
		{"", ""},
	}
	for _, c := range cases {
		if got := camelize(c.in); got != c.want {
			t.Errorf("camelize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDeriveOperationID_StableAcrossCalls(t *testing.T) {
	a := deriveOperationID("GET", "/api/v1/workspaces/{id}", "")
	b := deriveOperationID("get", "/api/v1/workspaces/{id}/", "")
	if a != b {
		t.Errorf("operationId not stable across method case + trailing slash: %q vs %q", a, b)
	}
	// Substring check decoupled from camel rules
	for _, want := range []string{"get", "Workspaces", "Id"} {
		if !strings.Contains(a, want) {
			t.Errorf("op id %q missing %q", a, want)
		}
	}
}
