// Package openapi generates an OpenAPI 3.0 specification from the
// running NeoAnvil contracts (HTTP routes) + tool registry, served at
// /openapi.json. Operators can pipe the result to swagger-ui, redoc,
// or any code-generator that speaks OpenAPI 3.x.
//
// Design constraints:
//   · Pure Go, no external deps. Encoded via encoding/json (struct
//     tags chosen so the JSON output is OpenAPI 3.0-compliant on the
//     wire — no manual map fiddling required).
//   · Lazy build + in-memory cache (see handler.go) so each /openapi.json
//     hit doesn't re-scan contracts.
//   · Cache invalidation hook tied to existing /internal/openapi/refresh.
//
// [Area 4.1.A]

package openapi

// Spec is the root OpenAPI 3.0 document. Optional fields use omitempty
// so the rendered JSON is the minimum-viable document — extra noise
// would only confuse downstream tooling.
type Spec struct {
	OpenAPI    string                 `json:"openapi"`
	Info       Info                   `json:"info"`
	Servers    []Server               `json:"servers,omitempty"`
	Paths      map[string]PathItem    `json:"paths"`
	Components *Components            `json:"components,omitempty"`
	Tags       []Tag                  `json:"tags,omitempty"`
	Extensions map[string]any `json:"-"` // x-* fields, custom-injected post-marshal
}

type Info struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

type Server struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

// PathItem holds the operations available on a path. OpenAPI 3
// allows up to 8 verbs per path; we support the 5 common ones.
type PathItem struct {
	Get    *Operation `json:"get,omitempty"`
	Post   *Operation `json:"post,omitempty"`
	Put    *Operation `json:"put,omitempty"`
	Patch  *Operation `json:"patch,omitempty"`
	Delete *Operation `json:"delete,omitempty"`
}

// Operation describes a single endpoint. Tags group related operations
// in the rendered docs (e.g., "workspaces", "plugins", "openapi").
type Operation struct {
	Tags        []string             `json:"tags,omitempty"`
	Summary     string               `json:"summary,omitempty"`
	Description string               `json:"description,omitempty"`
	OperationID string               `json:"operationId,omitempty"`
	Parameters  []Parameter          `json:"parameters,omitempty"`
	RequestBody *RequestBody         `json:"requestBody,omitempty"`
	Responses   map[string]Response  `json:"responses"`
}

type Parameter struct {
	Name        string  `json:"name"`
	In          string  `json:"in"` // "path" | "query" | "header"
	Description string  `json:"description,omitempty"`
	Required    bool    `json:"required,omitempty"`
	Schema      *Schema `json:"schema,omitempty"`
}

type RequestBody struct {
	Description string                  `json:"description,omitempty"`
	Required    bool                    `json:"required,omitempty"`
	Content     map[string]MediaType    `json:"content"`
}

type Response struct {
	Description string                  `json:"description"`
	Content     map[string]MediaType    `json:"content,omitempty"`
}

type MediaType struct {
	Schema *Schema `json:"schema,omitempty"`
}

// Schema is a minimal JSON Schema subset sufficient for OpenAPI 3.0
// payload descriptors. Untyped fields (e.g. struct map[string]any in
// the tool registry) render as `{"type": "object"}` with empty
// Properties — accurate, just not deeply-typed.
type Schema struct {
	Type        string             `json:"type,omitempty"`
	Format      string             `json:"format,omitempty"`
	Description string             `json:"description,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Required    []string           `json:"required,omitempty"`
	Items       *Schema            `json:"items,omitempty"`
	Enum        []any      `json:"enum,omitempty"`
	Ref         string             `json:"$ref,omitempty"`
}

type Components struct {
	Schemas map[string]*Schema `json:"schemas,omitempty"`
}

type Tag struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}
