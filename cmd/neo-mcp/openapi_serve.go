// cmd/neo-mcp/openapi_serve.go — wires the openapi package's cached
// handler to the running neo-mcp's contract source + tool registry.
// [Area 4.2.A]

package main

import (
	"net/http"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
	"github.com/ensamblatec/neoanvil/pkg/openapi"
)

// openapiDocsHandler returns the Swagger UI handler. Wrapper here so
// main.go doesn't have to import pkg/openapi directly. [Area 4.2.C]
func openapiDocsHandler() http.Handler {
	return openapi.DocsHandler()
}

// openAPIServeCache is the per-process spec cache. The Handler is
// wired from main.go into the SSE mux under `/openapi.json`. Cache
// invalidation is triggered by the existing `/internal/openapi/refresh`
// endpoint (Nexus broadcasts on contract changes).
//
// Initialised via setupOpenAPIServeCache from the main function once
// the workspace path + registry are known.
var openAPIServeCache *openapi.Cache

// setupOpenAPIServeCache binds a fresh openapi.Cache to the current
// workspace path + tool registry. Called once during main() boot
// before the HTTP mux is wired.
func setupOpenAPIServeCache(workspace string, registry *ToolRegistry) {
	build := func(includeInternal bool) *openapi.Spec {
		// Discover contracts from OpenAPI spec files + AST scan.
		// Empty list is a valid result — the spec just has no paths.
		spec, _ := cpg.ParseOpenAPIContracts(workspace)
		parsed, _ := cpg.ExtractGoRoutes(workspace)
		merged := cpg.MergeContracts(spec, parsed)

		contracts := make([]openapi.ContractIface, 0, len(merged))
		for _, c := range merged {
			contracts = append(contracts, contractAdapter{c})
		}

		var tools []openapi.ToolIface
		if registry != nil {
			for _, m := range registry.List() {
				name, _ := m["name"].(string)
				desc, _ := m["description"].(string)
				input, _ := m["inputSchema"].(map[string]any)
				tools = append(tools, toolAdapter{name: name, desc: desc, input: input})
			}
		}

		return openapi.BuildSpec(contracts, tools, openapi.BuildOptions{
			Title:           "NeoAnvil",
			Version:         "1.0.0",
			BaseURL:         "http://127.0.0.1:9000",
			IncludeInternal: includeInternal,
			IncludeMCPTools: true,
		})
	}
	openAPIServeCache = openapi.NewCache(build)
}

// contractAdapter bridges cpg.ContractNode → openapi.ContractIface.
// We use an adapter (not type alias / direct embed) to avoid leaking
// the full cpg type into the openapi package's API surface.
type contractAdapter struct{ c cpg.ContractNode }

func (a contractAdapter) GetMethod() string    { return a.c.Method }
func (a contractAdapter) GetPath() string      { return a.c.Path }
func (a contractAdapter) GetBackendFn() string { return a.c.BackendFn }

// toolAdapter wraps the map[string]any returned by ToolRegistry.List
// so we don't have to depend on the local Tool interface in openapi.
type toolAdapter struct {
	name, desc string
	input      map[string]any
}

func (t toolAdapter) GetName() string                { return t.name }
func (t toolAdapter) GetDescription() string         { return t.desc }
func (t toolAdapter) GetInputSchema() map[string]any { return t.input }
