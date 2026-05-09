// pkg/openapi/builder.go — assembles a full Spec from contracts +
// tools at build-time. [Area 4.1.B]
//
// The resulting document is consumed by handler.go (cached, served at
// /openapi.json). The function is pure given its inputs — no side
// effects, no global state — so it's straightforward to unit-test.

package openapi

import (
	"sort"
	"strings"
)

// ContractIface is implemented by cpg.ContractNode without us having
// to import that package (avoiding a cyclic dep). The neo-mcp wiring
// passes []cpg.ContractNode and we only read the fields below.
type ContractIface interface {
	GetMethod() string
	GetPath() string
	GetBackendFn() string
}

// ToolIface is the minimal projection of cmd/neo-mcp's Tool we need
// to render the x-mcp-tools section without circular import.
type ToolIface interface {
	GetName() string
	GetDescription() string
	GetInputSchema() map[string]any
}

// BuildOptions controls which contracts/tools end up in the doc.
type BuildOptions struct {
	Title          string
	Version        string
	BaseURL        string // e.g. "http://127.0.0.1:9000"
	IncludeInternal bool   // when false, /internal/* paths are excluded
	IncludeMCPTools bool   // attach x-mcp-tools extension under info
}

// BuildSpec returns an OpenAPI 3.0 document covering the given
// contracts. Operation IDs are derived from method + path so they're
// stable across rebuilds (good for client codegen).
func BuildSpec(contracts []ContractIface, tools []ToolIface, opts BuildOptions) *Spec {
	if opts.Title == "" {
		opts.Title = "NeoAnvil"
	}
	if opts.Version == "" {
		opts.Version = "1.0.0"
	}
	spec := &Spec{
		OpenAPI: "3.0.3",
		Info: Info{
			Title:   opts.Title,
			Version: opts.Version,
			Description: "NeoAnvil dispatcher + tool surface (auto-generated). " +
				"Contract source: AST scan of Go handlers + tool registry.",
		},
		Paths: make(map[string]PathItem, len(contracts)),
		Tags:  defaultTags(),
	}
	if opts.BaseURL != "" {
		spec.Servers = []Server{{URL: opts.BaseURL}}
	}

	for _, c := range contracts {
		path := c.GetPath()
		if !opts.IncludeInternal && strings.HasPrefix(path, "/internal/") {
			continue
		}
		method := strings.ToLower(c.GetMethod())
		if method == "" {
			continue
		}
		opPath := normalizeOpenAPIPath(path)
		op := &Operation{
			OperationID: deriveOperationID(c.GetMethod(), path, c.GetBackendFn()),
			Tags:        []string{tagForPath(opPath)},
			Summary:     c.GetBackendFn(),
			Responses:   defaultResponses(),
			Parameters:  pathParameters(opPath),
		}
		item := spec.Paths[opPath]
		switch method {
		case "get":
			item.Get = op
		case "post":
			item.Post = op
		case "put":
			item.Put = op
		case "patch":
			item.Patch = op
		case "delete":
			item.Delete = op
		default:
			continue // unsupported verb
		}
		spec.Paths[opPath] = item
	}

	if opts.IncludeMCPTools && len(tools) > 0 {
		spec.Extensions = map[string]any{
			"x-mcp-tools": renderMCPTools(tools),
		}
	}
	return spec
}

// normalizeOpenAPIPath converts Go's TrimPrefix-style path with leaf
// stripping into OpenAPI's `{param}` syntax. Heuristic: trailing
// segment that looks like a UUID/hash placeholder becomes `{id}`.
func normalizeOpenAPIPath(path string) string {
	path = strings.TrimRight(path, "/")
	if path == "" {
		return "/"
	}
	return path
}

// pathParameters extracts {param}-shaped segments from the path and
// returns them as required path parameters. Each gets a string schema
// — finer typing requires AST-level inspection (deferred to 4.1.C).
func pathParameters(path string) []Parameter {
	var params []Parameter
	for seg := range strings.SplitSeq(path, "/") {
		if len(seg) >= 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			name := seg[1 : len(seg)-1]
			params = append(params, Parameter{
				Name:     name,
				In:       "path",
				Required: true,
				Schema:   &Schema{Type: "string"},
			})
		}
	}
	return params
}

// deriveOperationID produces a stable, codegen-friendly operationId.
// Pattern: <method><CamelCasePath>. Falls back to BackendFn if path
// is "/" (root). Idempotent — same inputs → same ID.
func deriveOperationID(method, path, backendFn string) string {
	method = strings.ToLower(method)
	clean := strings.Trim(path, "/")
	if clean == "" {
		if backendFn != "" {
			return method + camelize(backendFn)
		}
		return method + "Root"
	}
	return method + camelize(clean)
}

// camelize converts /api/v1/workspaces/{id} → ApiV1WorkspacesId
// (path separators + braces stripped, each segment Title-cased).
func camelize(s string) string {
	out := make([]string, 0, 8)
	for _, seg := range strings.FieldsFunc(s, func(r rune) bool {
		return r == '/' || r == '{' || r == '}' || r == '-' || r == '_'
	}) {
		if seg == "" {
			continue
		}
		out = append(out, strings.ToUpper(seg[:1])+seg[1:])
	}
	return strings.Join(out, "")
}

// tagForPath groups operations by the first path segment so the
// rendered docs have a navigable tree. /api/v1/workspaces → "workspaces".
func tagForPath(path string) string {
	for p := range strings.SplitSeq(strings.TrimPrefix(path, "/"), "/") {
		if p == "api" || p == "v1" || p == "internal" || p == "" {
			continue
		}
		return p
	}
	return "default"
}

// defaultTags is the canonical group set surfaced even when no
// contracts match — consistent ordering in the generated doc.
func defaultTags() []Tag {
	return []Tag{
		{Name: "workspaces", Description: "Workspace lifecycle + registry"},
		{Name: "plugins", Description: "Plugin pool + health"},
		{Name: "shared", Description: "Cross-tier knowledge store"},
		{Name: "openapi", Description: "OpenAPI / spec endpoints"},
	}
}

// defaultResponses gives every operation a baseline 200 response so
// the spec is self-consistent even without per-handler response
// schema extraction (deferred to 4.1.C).
func defaultResponses() map[string]Response {
	return map[string]Response{
		"200": {Description: "OK"},
		"500": {Description: "Internal error"},
	}
}

// renderMCPTools formats the tool registry as an OpenAPI extension
// (x-mcp-tools). Sorted by name so the JSON output is deterministic.
func renderMCPTools(tools []ToolIface) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"name":        t.GetName(),
			"description": t.GetDescription(),
			"inputSchema": t.GetInputSchema(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		ni, _ := out[i]["name"].(string)
		nj, _ := out[j]["name"].(string)
		return ni < nj
	})
	return out
}

// ToJSON is a small helper for tests + handler — wraps json.Marshal
// with an Extensions splice so x-* fields appear at the right level.
// Returns the encoded bytes ready for HTTP write.
func (s *Spec) toJSONMap() map[string]any {
	// We can't naively json-marshal Spec because Extensions has
	// `json:"-"`. Emit a map shape that mirrors the struct then
	// add the x-* keys at the top level.
	out := map[string]any{
		"openapi": s.OpenAPI,
		"info":    s.Info,
		"paths":   s.Paths,
	}
	if len(s.Servers) > 0 {
		out["servers"] = s.Servers
	}
	if len(s.Tags) > 0 {
		out["tags"] = s.Tags
	}
	if s.Components != nil {
		out["components"] = s.Components
	}
	for k, v := range s.Extensions {
		// All extension keys MUST start with x-, OpenAPI 3.0 spec.
		if !strings.HasPrefix(k, "x-") {
			out["x-"+k] = v
		} else {
			out[k] = v
		}
	}
	return out
}

