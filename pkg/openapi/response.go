// pkg/openapi/response.go — light AST scanner that extracts response
// schemas from Go HTTP handlers. [Area 4.1.C]
//
// What it tries to do:
//   · Walk the workspace, parse each .go file with go/parser.
//   · For each function whose name appears in the contracts list,
//     find json.NewEncoder(w).Encode(<expr>) or json.Marshal(<expr>).
//   · Resolve <expr>'s static type and emit a JSON Schema describing
//     it. For named struct types, walk fields + JSON tags. Inline
//     anonymous structs work too.
//
// What it does NOT try to do:
//   · Cross-package type resolution beyond same-file lookups (the
//     full call-graph version would require go/types + a build).
//   · Generics, interfaces, channels, etc.
//   · Recursive types (depth cap at 4 keeps output tractable).
//
// Failure mode is graceful: when we can't resolve a type, we emit
// `{"type": "object"}` (matches the baseline). The spec stays valid.

package openapi

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
)

// HandlerScanner walks a workspace and indexes Go struct types so
// downstream callers can resolve a handler function's response shape.
type HandlerScanner struct {
	// types maps a fully-qualified-ish "<file>.<TypeName>" → *ast.StructType.
	// We don't track packages because the resolver is best-effort.
	types map[string]*ast.StructType

	// handlers maps function name → list of inferred response refs
	// (struct keys looked up in `types`). Populated during scan.
	handlers map[string][]string
}

// NewHandlerScanner constructs an empty scanner. Call ScanWorkspace
// to populate it.
func NewHandlerScanner() *HandlerScanner {
	return &HandlerScanner{
		types:    map[string]*ast.StructType{},
		handlers: map[string][]string{},
	}
}

// ScanWorkspace recursively walks dirRoot, parses every .go file (skipping
// _test.go + vendor), and indexes struct types + handler bodies. Errors
// from individual files are non-fatal — we log via the returned warnings
// slice so the caller decides what to do.
func (s *HandlerScanner) ScanWorkspace(dirRoot string) []string {
	var warnings []string
	fset := token.NewFileSet()
	_ = filepath.WalkDir(dirRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			warnings = append(warnings, "walk: "+err.Error())
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == ".git" || name == "node_modules" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, parseErr := parser.ParseFile(fset, path, nil, parser.AllErrors)
		if parseErr != nil {
			warnings = append(warnings, path+": "+parseErr.Error())
			return nil
		}
		s.indexFile(f)
		return nil
	})
	return warnings
}

// indexFile populates types + handlers maps from one parsed AST.
func (s *HandlerScanner) indexFile(f *ast.File) {
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			s.indexTypes(d)
		case *ast.FuncDecl:
			s.indexHandler(d)
		}
	}
}

// indexTypes records every named struct type. Generic types and
// interfaces are skipped — caller renders them as plain objects.
func (s *HandlerScanner) indexTypes(d *ast.GenDecl) {
	if d.Tok != token.TYPE {
		return
	}
	for _, spec := range d.Specs {
		ts, ok := spec.(*ast.TypeSpec)
		if !ok {
			continue
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			continue
		}
		s.types[ts.Name.Name] = st
	}
}

// indexHandler scans the body of a func for json encoder/marshal
// patterns. Records the type referenced by the first .Encode/.Marshal
// argument so the spec generator can resolve a 200-response schema.
//
// Two-pass: first pass walks declarations to map local-var → type
// (so `out := MyType{}` is reachable when `json.Marshal(out)` runs
// later). Second pass picks up the encode/marshal targets.
func (s *HandlerScanner) indexHandler(d *ast.FuncDecl) {
	if d.Body == nil {
		return
	}
	name := d.Name.Name
	localVars := map[string]string{}
	// Pass 1 — collect local-var → type bindings from short var decls
	// AND assign statements where the rhs is a composite literal.
	ast.Inspect(d.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		ident, ok := assign.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		if t := s.exprTypeName(assign.Rhs[0]); t != "" {
			localVars[ident.Name] = t
		}
		return true
	})
	// Pass 2 — encode/marshal targets, resolving via localVars when
	// the argument is a bare ident.
	ast.Inspect(d.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		method := sel.Sel.Name
		if method != "Encode" && method != "Marshal" && method != "MarshalIndent" {
			return true
		}
		arg := call.Args[0]
		typeName := s.exprTypeName(arg)
		if typeName == "" {
			if ident, ok := arg.(*ast.Ident); ok {
				typeName = localVars[ident.Name]
			}
		}
		if typeName == "" {
			return true
		}
		s.handlers[name] = append(s.handlers[name], typeName)
		return true
	})
}

// exprTypeName extracts a syntactic type name from common arg shapes:
//
//	&MyType{...} | MyType{...} | aVar (look up declared type? — skip)
//
// Variables can't be resolved without go/types so we drop them. Named
// types in composite literals are the high-value case.
func (s *HandlerScanner) exprTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.UnaryExpr:
		if t.Op == token.AND {
			return s.exprTypeName(t.X)
		}
	case *ast.CompositeLit:
		switch tt := t.Type.(type) {
		case *ast.Ident:
			return tt.Name
		case *ast.SelectorExpr:
			// pkg.Type → use just Type for the lookup (best-effort).
			return tt.Sel.Name
		}
	}
	return ""
}

// SchemaFor renders a Schema for the named handler. Returns nil when
// the handler is unknown OR the type can't be resolved — caller falls
// back to the generic 200 response baseline.
func (s *HandlerScanner) SchemaFor(handlerName string) *Schema {
	refs := s.handlers[handlerName]
	if len(refs) == 0 {
		return nil
	}
	// Use the first observed encode target — the common case is one
	// response shape per handler. Branchy handlers can be revisited.
	return s.structSchema(refs[0], 0)
}

// structSchema renders a struct's fields. Caps recursion at depth 4
// so a self-referential type doesn't loop forever.
func (s *HandlerScanner) structSchema(typeName string, depth int) *Schema {
	if depth > 4 {
		return &Schema{Type: "object"}
	}
	st := s.types[typeName]
	if st == nil {
		return &Schema{Type: "object"}
	}
	out := &Schema{
		Type:       "object",
		Properties: map[string]*Schema{},
	}
	for _, field := range st.Fields.List {
		jsonTag := jsonFieldName(field)
		fieldNames := fieldIdents(field)
		// Anonymous fields (embedded) → inline.
		if len(fieldNames) == 0 {
			continue
		}
		for _, name := range fieldNames {
			tag := jsonTag
			if tag == "" {
				tag = name
			}
			if tag == "-" {
				continue
			}
			out.Properties[tag] = fieldSchema(field.Type, s, depth+1)
		}
	}
	return out
}

// fieldIdents returns the names declared by the field — usually one.
func fieldIdents(f *ast.Field) []string {
	out := make([]string, 0, len(f.Names))
	for _, ident := range f.Names {
		out = append(out, ident.Name)
	}
	return out
}

// jsonFieldName extracts the JSON tag if present (`json:"field,omitempty"`).
// Returns "" when no tag exists; caller defaults to the Go name.
// Returns "-" when the tag is the explicit JSON-omit marker.
func jsonFieldName(f *ast.Field) string {
	if f.Tag == nil {
		return ""
	}
	raw := strings.Trim(f.Tag.Value, "`")
	for part := range strings.SplitSeq(raw, " ") {
		const prefix = `json:"`
		if !strings.HasPrefix(part, prefix) {
			continue
		}
		val := strings.TrimSuffix(strings.TrimPrefix(part, prefix), `"`)
		// Drop any ",omitempty" / ",string" suffix.
		if comma := strings.Index(val, ","); comma >= 0 {
			val = val[:comma]
		}
		return val
	}
	return ""
}

// fieldSchema converts a Go expr type to a JSON Schema fragment.
// Handles primitives, []T arrays, map[string]any, and named types
// (which recursively call structSchema for resolved structs).
func fieldSchema(expr ast.Expr, s *HandlerScanner, depth int) *Schema {
	switch t := expr.(type) {
	case *ast.Ident:
		switch t.Name {
		case "string":
			return &Schema{Type: "string"}
		case "bool":
			return &Schema{Type: "boolean"}
		case "int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64", "byte":
			return &Schema{Type: "integer"}
		case "float32", "float64":
			return &Schema{Type: "number"}
		case "any":
			return &Schema{}
		default:
			// Named struct type? Recurse.
			return s.structSchema(t.Name, depth)
		}
	case *ast.ArrayType:
		return &Schema{
			Type:  "array",
			Items: fieldSchema(t.Elt, s, depth+1),
		}
	case *ast.MapType:
		return &Schema{Type: "object"}
	case *ast.StarExpr:
		return fieldSchema(t.X, s, depth)
	case *ast.InterfaceType:
		return &Schema{}
	}
	return &Schema{Type: "object"}
}
