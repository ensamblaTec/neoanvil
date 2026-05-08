package cpg

// bridge.go — Cross-boundary BLAST_RADIUS: links Go HTTP handlers to TypeScript
// frontend callers via HTTP contract nodes. PILAR XXX, épicas 254-257, 289.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ContractNode represents an HTTP route contract linking a backend handler to its
// frontend callers. Source indicates how the contract was discovered.
type ContractNode struct {
	Method          string      // GET, POST, PUT, DELETE, PATCH
	Path            string      // normalized path, e.g. /api/users/{id}
	BackendFn       string      // Go function name
	BackendFile     string      // workspace-relative path
	BackendLine     int         // 1-based source line
	Source          string      // "openapi" | "parsed" | "inferred"
	FrontendCallers []CallerRef // TS/JS files that call this route
}

// CallerRef is a source location for a frontend caller.
type CallerRef struct {
	File string // workspace-relative path
	Line int
}

// openAPI3 is a minimal subset of OpenAPI 3.x / Swagger 2.x sufficient for
// extracting paths and HTTP methods. Handles both formats via the same struct.
type openAPI3 struct {
	Paths map[string]map[string]any `yaml:"paths"`
}

// openAPIHotCache caches ParseOpenAPIContracts results keyed by workspace path.
// Entries expire after openAPIHotCacheTTL or when InvalidateOpenAPICache is called. [334.B]
var (
	openAPIMu       sync.Mutex
	openAPIHotCache = map[string]openAPICacheEntry{}
)

const openAPIHotCacheTTL = 5 * time.Minute

type openAPICacheEntry struct {
	contracts []ContractNode
	expiresAt time.Time
}

// InvalidateOpenAPICache removes the cached spec for the given workspace, forcing
// the next ParseOpenAPIContracts call to re-read from disk. [334.B]
func InvalidateOpenAPICache(workspace string) {
	openAPIMu.Lock()
	delete(openAPIHotCache, workspace)
	openAPIMu.Unlock()
	log.Printf("[334.B] openapi spec cache invalidated for %s", filepath.Base(workspace))
}

// findOpenAPISpec walks workspace up to depth 4 looking for swagger/openapi files. [289.A]
// Returns (data, relPath, error). Stops at the first match. Max 200 entries scanned.
func findOpenAPISpec(workspace string) ([]byte, string, error) {
	const maxScan = 200
	const maxDepth = 4
	var found []byte
	var foundRel string
	scanned := 0

	apiNames := map[string]bool{
		"openapi.yaml": true, "openapi.json": true,
		"swagger.yaml": true, "swagger.json": true,
	}
	skipDirs := map[string]bool{
		"vendor": true, "node_modules": true, ".neo": true,
		"testdata": true, "__pycache__": true, "venv": true, ".venv": true,
	}

	if werr := filepath.WalkDir(workspace, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != nil {
			return nil
		}
		rel, _ := filepath.Rel(workspace, path)
		depth := strings.Count(rel, string(filepath.Separator))
		if d.IsDir() {
			if skipDirs[d.Name()] || depth >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if scanned >= maxScan {
			return filepath.SkipAll
		}
		scanned++
		if apiNames[strings.ToLower(d.Name())] {
			data, rerr := os.ReadFile(path) //nolint:gosec // G304-DIR-WALK: scoped to workspace path
			if rerr == nil {
				found = data
				foundRel = filepath.ToSlash(rel)
			}
		}
		return nil
	}); werr != nil {
		log.Printf("[CPG-WARN] findOpenAPISpec: walk %s failed: %v", workspace, werr)
	}
	return found, foundRel, nil
}

// FindOpenAPISpecData is the exported wrapper around findOpenAPISpec.
// Returns (specBytes, relPath, error). Used by 334.A for hash-change detection. [334.A]
func FindOpenAPISpecData(workspace string) ([]byte, string, error) {
	return findOpenAPISpec(workspace)
}

// ParseOpenAPIContracts searches workspace for an OpenAPI/Swagger spec using
// recursive WalkDir (up to depth 4, max 200 files) and returns one ContractNode
// per (method, path) pair. Returns nil,nil when no spec is found. [Épica 254.A / 289.A]
func ParseOpenAPIContracts(workspace string) ([]ContractNode, error) {
	// [334.B] HotCache: skip expensive WalkDir when spec hasn't changed.
	openAPIMu.Lock()
	if entry, ok := openAPIHotCache[workspace]; ok && time.Now().Before(entry.expiresAt) {
		openAPIMu.Unlock()
		return entry.contracts, nil
	}
	openAPIMu.Unlock()

	specData, specRel, _ := findOpenAPISpec(workspace)
	if specData == nil {
		return nil, nil
	}
	log.Printf("[289.A] OpenAPI spec found: %s", specRel)
	var spec openAPI3
	if err := yaml.Unmarshal(specData, &spec); err != nil {
		return nil, nil // unparseable spec — treat as soft miss
	}

	httpMethods := map[string]bool{
		"get": true, "post": true, "put": true, "delete": true,
		"patch": true, "head": true, "options": true,
	}
	var contracts []ContractNode
	for rawPath, ops := range spec.Paths {
		normPath := normalizeRoutePath(rawPath)
		for method := range ops {
			if !httpMethods[strings.ToLower(method)] {
				continue
			}
			contracts = append(contracts, ContractNode{
				Method: strings.ToUpper(method),
				Path:   normPath,
				Source: "openapi",
			})
		}
	}
	// [334.B] Store in HotCache with TTL.
	openAPIMu.Lock()
	openAPIHotCache[workspace] = openAPICacheEntry{
		contracts: contracts,
		expiresAt: time.Now().Add(openAPIHotCacheTTL),
	}
	openAPIMu.Unlock()
	return contracts, nil
}

var goRouteSkipDirs = map[string]bool{
	"vendor": true, "node_modules": true, ".neo": true,
	"testdata": true,
}

// ExtractGoRoutes performs an AST scan of Go source files under workspace looking
// for HTTP route registrations (net/http, gin, echo, gorilla/mux). Returns
// ContractNodes with Source:"parsed". Max 200 files. [Épica 254.B]
func ExtractGoRoutes(workspace string) ([]ContractNode, error) {
	const maxFiles = 200
	fset := token.NewFileSet()
	var contracts []ContractNode
	scanned := 0

	if werr := filepath.WalkDir(workspace, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if goRouteSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if scanned >= maxFiles {
			return filepath.SkipAll
		}
		scanned++

		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(workspace, path)
		rel = filepath.ToSlash(rel)

		// [330.G] First pass: collect `var := parent.Group("literal")` chains so we can
		// resolve the accumulated prefix of each RouterGroup identifier. Supports
		// arbitrarily nested gin/echo/chi-style sub-groups.
		prefixes := collectRouteGroupPrefixes(f)

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			method, routePath, handlerName := extractRouteCall(fset, call, prefixes)
			if method == "" || routePath == "" {
				return true
			}
			pos := fset.Position(call.Pos())
			contracts = append(contracts, ContractNode{
				Method:      method,
				Path:        normalizeRoutePath(routePath),
				BackendFn:   handlerName,
				BackendFile: rel,
				BackendLine: pos.Line,
				Source:      "parsed",
			})
			return true
		})
		return nil
	}); werr != nil {
		log.Printf("[CPG-WARN] ExtractGoRoutes: walk %s failed: %v", workspace, werr)
	}
	return contracts, nil
}

// extractRouteCall inspects a *ast.CallExpr and, if it looks like an HTTP route
// registration, returns (method, path, handlerName). Returns empty strings on miss.
// When the receiver is a known RouterGroup identifier (see collectRouteGroupPrefixes),
// the accumulated prefix is prepended to the extracted path. [330.G]
func extractRouteCall(fset *token.FileSet, call *ast.CallExpr, prefixes map[string]string) (method, routePath, handlerName string) {
	_ = fset // kept for future position-aware filtering
	// Identify the selector or ident being called.
	var fnName string
	var recvIdent string
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		fnName = fn.Sel.Name
		if id, ok := fn.X.(*ast.Ident); ok {
			recvIdent = id.Name
		}
	case *ast.Ident:
		fnName = fn.Name
	default:
		return
	}

	// net/http: r.HandleFunc("/path", handlerFn) or mux.HandleFunc("/path", fn)
	if fnName == "HandleFunc" || fnName == "Handle" {
		if len(call.Args) >= 2 {
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				routePath = applyGroupPrefix(prefixes, recvIdent, strings.Trim(lit.Value, `"'`))
				method = "ANY"
				handlerName = exprName(call.Args[1])
			}
		}
		return
	}

	// gin/echo/gorilla/fiber: r.GET, e.POST, app.Get, app.Post, etc. [289.C]
	httpVerbs := map[string]string{
		"GET": "GET", "Get": "GET",
		"Post": "POST", "POST": "POST",
		"Put": "PUT", "PUT": "PUT",
		"Delete": "DELETE", "DELETE": "DELETE",
		"Patch": "PATCH", "PATCH": "PATCH",
		"Head": "HEAD", "HEAD": "HEAD",
		"Options": "OPTIONS", "OPTIONS": "OPTIONS",
	}
	if verb, ok := httpVerbs[fnName]; ok {
		if len(call.Args) >= 2 {
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				routePath = applyGroupPrefix(prefixes, recvIdent, strings.Trim(lit.Value, `"'`))
				method = verb
				handlerName = exprName(call.Args[len(call.Args)-1])
			}
		}
	}
	return
}

// applyGroupPrefix returns path with the RouterGroup accumulated prefix prepended
// when `recvIdent` is a known group var; otherwise returns path unchanged. [330.G]
func applyGroupPrefix(prefixes map[string]string, recvIdent, path string) string {
	if recvIdent == "" {
		return path
	}
	prefix, ok := prefixes[recvIdent]
	if !ok || prefix == "" {
		return path
	}
	return joinRoutePath(prefix, path)
}

// collectRouteGroupPrefixes performs a pre-order AST scan of the file to build a
// map from local identifier → accumulated HTTP path prefix, by following
// `var := <parent>.Group("literal")` chains. Root engines (identifiers not
// previously seen as a parent) resolve to empty prefix. Non-literal paths and
// chained method calls (`engine.Group("a").Group("b")`) without an intermediate
// variable are silently skipped. [330.G]
func collectRouteGroupPrefixes(f *ast.File) map[string]string {
	prefixes := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || assign.Tok != token.DEFINE {
			return true
		}
		if len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		lhsIdent, ok := assign.Lhs[0].(*ast.Ident)
		if !ok || lhsIdent.Name == "_" {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Group" {
			return true
		}
		if len(call.Args) < 1 {
			return true
		}
		pathLit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || pathLit.Kind != token.STRING {
			return true
		}
		path := strings.Trim(pathLit.Value, `"'`)
		var parentPrefix string
		if parentIdent, ok := sel.X.(*ast.Ident); ok {
			parentPrefix = prefixes[parentIdent.Name]
		}
		prefixes[lhsIdent.Name] = joinRoutePath(parentPrefix, path)
		return true
	})
	return prefixes
}

// joinRoutePath concatenates two URL path fragments with exactly one separating
// slash. Preserves the leading slash of `left` (or lack thereof). Strips any
// trailing slash from `left` and any leading slash from `right` before joining.
// Empty inputs pass through cleanly. [330.G]
func joinRoutePath(left, right string) string {
	if left == "" {
		return right
	}
	if right == "" {
		return left
	}
	l := strings.TrimRight(left, "/")
	r := strings.TrimLeft(right, "/")
	if r == "" {
		return left
	}
	return l + "/" + r
}

// exprName extracts a printable name from an expression used as a handler argument.
func exprName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		if id, ok := v.X.(*ast.Ident); ok {
			return id.Name + "." + v.Sel.Name
		}
		return v.Sel.Name
	}
	return ""
}

// MergeContracts deduplicates contracts by (Method, normalized Path), preferring
// Source:"openapi" over "parsed". [Épica 254.C]
// sourcePriority returns a numeric priority for deduplication. Lower = higher priority. [289.E]
func sourcePriority(src string) int {
	switch src {
	case "openapi":
		return 0
	case "parsed":
		return 1
	case "python_parsed":
		return 2
	default:
		return 3 // "inferred" and unknown
	}
}

// MergeContracts deduplicates contracts by (Method, normalized Path), preferring
// by source priority: openapi > parsed > python_parsed > inferred. [Épica 254.C / 289.E]
func MergeContracts(openapi, parsed []ContractNode) []ContractNode {
	type key struct{ method, path string }
	seen := make(map[key]int)
	result := make([]ContractNode, 0, len(openapi)+len(parsed))

	add := func(c ContractNode) {
		k := key{c.Method, c.Path}
		if idx, ok := seen[k]; ok {
			if sourcePriority(c.Source) < sourcePriority(result[idx].Source) {
				result[idx] = c
			}
			return
		}
		seen[k] = len(result)
		result = append(result, c)
	}
	for _, c := range openapi {
		add(c)
	}
	for _, c := range parsed {
		add(c)
	}
	return result
}

var pyRouteSkipDirs = map[string]bool{
	"venv": true, ".venv": true, "__pycache__": true,
	"node_modules": true, ".neo": true, "testdata": true,
}

// pyRoutePatterns matches FastAPI/Flask/Django route decorators and urlpatterns. [289.B]
var pyRoutePatterns = []*regexp.Regexp{
	// FastAPI / APIRouter: @app.get("/path"), @router.post("/path"), @bp.delete("/path")
	regexp.MustCompile(`@(?:\w+)\.(?i)(get|post|put|delete|patch|head|options)\s*\(\s*["']([^"']+)["']`),
	// Flask: @app.route("/path", methods=["POST"]), @bp.route("/path")
	regexp.MustCompile(`@(?:\w+)\.route\s*\(\s*["']([^"']+)["'](?:[^)]*methods\s*=\s*\[([^\]]*)\])?`),
	// Django: path("users/", view) — heuristic
	regexp.MustCompile(`\bpath\s*\(\s*["']([^"']+)["']\s*,`),
}

// ExtractPythonRoutes scans .py files for FastAPI/Flask/Django route registrations. [289.B]
// Returns ContractNodes with Source:"python_parsed". Max 200 files. Skips venv/.
func ExtractPythonRoutes(workspace string) ([]ContractNode, error) {
	const maxFiles = 200
	var contracts []ContractNode
	scanned := 0

	walkErr := filepath.WalkDir(workspace, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if pyRouteSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".py") {
			return nil
		}
		if scanned >= maxFiles {
			return filepath.SkipAll
		}
		scanned++

		data, rerr := os.ReadFile(path) //nolint:gosec // G304-DIR-WALK: scoped to workspace
		if rerr != nil {
			return nil
		}
		rel, _ := filepath.Rel(workspace, path)
		rel = filepath.ToSlash(rel)
		lines := strings.Split(string(data), "\n")

		for lineNum, line := range lines {
			// FastAPI/APIRouter pattern
			if m := pyRoutePatterns[0].FindStringSubmatch(line); len(m) == 3 {
				contracts = append(contracts, ContractNode{
					Method:      strings.ToUpper(m[1]),
					Path:        normalizeRoutePath(m[2]),
					BackendFile: rel,
					BackendLine: lineNum + 1,
					Source:      "python_parsed",
				})
				continue
			}
			// Flask route pattern
			if m := pyRoutePatterns[1].FindStringSubmatch(line); len(m) >= 2 {
				path_ := normalizeRoutePath(m[1])
				methods := []string{"GET"}
				if len(m) == 3 && m[2] != "" {
					methods = nil
					for me := range strings.SplitSeq(m[2], ",") {
						me = strings.Trim(strings.TrimSpace(me), `"'`)
						if me != "" {
							methods = append(methods, strings.ToUpper(me))
						}
					}
				}
				for _, meth := range methods {
					contracts = append(contracts, ContractNode{
						Method:      meth,
						Path:        path_,
						BackendFile: rel,
						BackendLine: lineNum + 1,
						Source:      "python_parsed",
					})
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		log.Printf("[CPG-WARN] ExtractPythonRoutes: walk %s failed: %v", workspace, walkErr)
	}
	return contracts, nil
}

// SchemaNode represents a struct field in a request payload schema. [289.D]
type SchemaNode struct {
	Field    string // JSON field name (from json tag or lowercase name)
	Type     string // Go type as string
	Required bool   // true if binding:"required" or validate:"required"
	Tags     string // raw struct tag string
}

// ExtractRequestSchema finds the request struct bound in handlerFn and returns its fields. [289.D]
// Traces: Decode/ShouldBind/BindJSON/Unmarshal → infers struct type → field list.
// Partial results returned with Partial:true comment when cross-package (documented limitation).
func ExtractRequestSchema(workspace, handlerFn string) ([]SchemaNode, bool, error) {
	fset := token.NewFileSet()
	bindTypeName, handlerPkg, handlerFile := findHandlerBindType(workspace, handlerFn, fset)
	if bindTypeName == "" {
		return nil, false, nil
	}
	fields, partial := extractStructFields(filepath.Dir(handlerFile), handlerPkg, bindTypeName, fset)
	return fields, partial, nil
}

// findHandlerBindType walks workspace Go files to find the function handlerFn and returns
// the name of the struct type it binds/decodes into, the package name, and the file path.
func findHandlerBindType(workspace, handlerFn string, fset *token.FileSet) (bindTypeName, handlerPkg, handlerFile string) {
	const maxFiles = 200
	scanned := 0
	if werr := filepath.WalkDir(workspace, func(path string, d fs.DirEntry, err error) error {
		if err != nil || bindTypeName != "" {
			return nil
		}
		if d.IsDir() {
			if goRouteSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if scanned >= maxFiles {
			return filepath.SkipAll
		}
		scanned++
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != handlerFn || fn.Body == nil {
				continue
			}
			bindTypeName = resolveBindType(fn)
			if bindTypeName != "" {
				handlerPkg = f.Name.Name
				handlerFile = path
			}
		}
		return nil
	}); werr != nil {
		log.Printf("[CPG-WARN] findHandlerBindType: walk %s failed: %v", workspace, werr)
	}
	return
}

// resolveBindType inspects a handler function body for decode/bind calls and returns
// the type name of the variable being decoded into.
func resolveBindType(fn *ast.FuncDecl) string {
	var found string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fnName := ""
		if sel, ok2 := call.Fun.(*ast.SelectorExpr); ok2 {
			fnName = sel.Sel.Name
		}
		if fnName != "Decode" && fnName != "ShouldBind" && fnName != "BindJSON" && fnName != "Unmarshal" {
			return true
		}
		if len(call.Args) == 0 {
			return true
		}
		unary, ok := call.Args[len(call.Args)-1].(*ast.UnaryExpr)
		if !ok {
			return true
		}
		id, ok := unary.X.(*ast.Ident)
		if !ok {
			return true
		}
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			vs, ok := inner.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range vs.Names {
				if name.Name != id.Name || i >= len(vs.Names) {
					continue
				}
				if ident, ok2 := vs.Type.(*ast.Ident); ok2 {
					found = ident.Name
				} else if sel, ok2 := vs.Type.(*ast.SelectorExpr); ok2 {
					found = sel.Sel.Name
				}
			}
			return found == ""
		})
		return true
	})
	return found
}

// extractStructFields scans pkgDir for the struct definition of bindTypeName and returns
// its fields as SchemaNodes. partial=true when a field type requires cross-package resolution.
func extractStructFields(pkgDir, handlerPkg, bindTypeName string, fset *token.FileSet) ([]SchemaNode, bool) {
	var fields []SchemaNode
	partial := false
	entries, _ := os.ReadDir(pkgDir)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(pkgDir, entry.Name()), nil, 0)
		if perr != nil || f.Name.Name != handlerPkg {
			continue
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != bindTypeName {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, fld := range st.Fields.List {
					if node, isPartial := schemaNodeFromField(fld); node != nil {
						fields = append(fields, *node)
						if isPartial {
							partial = true
						}
					}
				}
			}
		}
	}
	return fields, partial
}

// schemaNodeFromField converts a single ast.Field into a SchemaNode.
// Returns (nil, false) for embedded/anonymous fields. partial=true for cross-package types.
func schemaNodeFromField(fld *ast.Field) (*SchemaNode, bool) {
	if len(fld.Names) == 0 {
		return nil, false
	}
	name := fld.Names[0].Name
	typStr, partial := fieldTypeName(fld.Type)
	rawTag := ""
	required := false
	jsonName := strings.ToLower(name)
	if fld.Tag != nil {
		rawTag = fld.Tag.Value
		if jt := extractTagValue(rawTag, "json"); jt != "" {
			if parts := strings.Split(jt, ","); parts[0] != "" && parts[0] != "-" {
				jsonName = parts[0]
			}
		}
		bv := extractTagValue(rawTag, "binding")
		vv := extractTagValue(rawTag, "validate")
		required = strings.Contains(bv, "required") || strings.Contains(vv, "required")
	}
	return &SchemaNode{Field: jsonName, Type: typStr, Required: required, Tags: strings.Trim(rawTag, "`")}, partial
}

// fieldTypeName returns the string representation of a field type and whether it is cross-package.
func fieldTypeName(expr ast.Expr) (string, bool) {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name, false
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return "*" + id.Name, false
		}
	case *ast.ArrayType:
		return "[]...", false
	case *ast.SelectorExpr:
		return exprName(t), true
	}
	return "", false
}

// extractTagValue returns the value of a struct tag key (e.g. "json", "binding"). [289.D]
func extractTagValue(rawTag, key string) string {
	tag := strings.Trim(rawTag, "`")
	_, after, ok := strings.Cut(tag, key+`:"`)
	if !ok {
		return ""
	}
	val, _, _ := strings.Cut(after, `"`)
	return val
}

var tsFetchSkipDirs = map[string]bool{
	".neo": true, "node_modules": true, ".next": true,
	"dist": true, "out": true, "build": true, ".turbo": true,
}

var tsFetchExts = map[string]bool{
	".ts": true, ".tsx": true, ".js": true, ".jsx": true,
}

// tsFetchPatterns are call-site prefixes that indicate an HTTP fetch in TS/JS.
var tsFetchPatterns = []string{"fetch(", "axios.get(", "axios.post(", "axios.put(",
	"axios.delete(", "axios.patch(", "useQuery(", "useMutation(", "http.get(", "http.post("}

// LinkTSCallers scans TS/JS files for fetch/axios/useQuery calls matching each
// contract's path, populating ContractNode.FrontendCallers. Returns the updated
// slice. [Épica 255.A / 255.B]
func LinkTSCallers(workspace string, contracts []ContractNode) []ContractNode {
	if len(contracts) == 0 {
		return contracts
	}
	aliases := ReadTSConfigPaths(workspace)

	walkErr := filepath.WalkDir(workspace, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if tsFetchSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !tsFetchExts[filepath.Ext(path)] {
			return nil
		}
		data, rErr := os.ReadFile(path) //nolint:gosec // G304-DIR-WALK
		if rErr != nil {
			return nil
		}
		rel, _ := filepath.Rel(workspace, path)
		rel = filepath.ToSlash(rel)
		lines := strings.Split(string(data), "\n")

		for lineNum, line := range lines {
			trimmed := strings.TrimSpace(line)
			// Must be a fetch-like call, not a comment or arbitrary string.
			isFetch := false
			for _, pat := range tsFetchPatterns {
				if strings.Contains(trimmed, pat) {
					isFetch = true
					break
				}
			}
			if !isFetch {
				continue
			}
			for i := range contracts {
				if matchesContractPath(line, contracts[i].Path, aliases) {
					contracts[i].FrontendCallers = append(contracts[i].FrontendCallers, CallerRef{
						File: rel,
						Line: lineNum + 1,
					})
					break // one entry per line per contract
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		log.Printf("[CPG-WARN] LinkTSCallers: walk %s failed: %v", workspace, walkErr)
	}
	return contracts
}

// matchesContractPath returns true when line contains a string literal whose
// value matches the contract path (accounting for tsconfig aliases).
func matchesContractPath(line, contractPath string, aliases map[string]string) bool {
	// Direct path match: line contains the contract path as a string fragment.
	if strings.Contains(line, `"`+contractPath+`"`) ||
		strings.Contains(line, `'`+contractPath+`'`) ||
		strings.Contains(line, "`"+contractPath+"`") {
		return true
	}
	// Partial segment match: check if the last segment of the path appears.
	segments := strings.Split(strings.Trim(contractPath, "/"), "/")
	if len(segments) > 0 {
		last := segments[len(segments)-1]
		// Skip overly generic segments like "api" or "v1".
		if len(last) >= 4 && !strings.HasPrefix(last, "{") {
			if strings.Contains(line, "/"+last+`"`) ||
				strings.Contains(line, "/"+last+`'`) ||
				strings.Contains(line, "/"+last+"`") {
				return true
			}
		}
	}
	// Alias-resolved match: check @/api/users against src/api/users.
	for aliasPrefix, srcPrefix := range aliases {
		if suffix, ok := strings.CutPrefix(contractPath, "/"+srcPrefix); ok {
			aliasedPath := aliasPrefix + suffix
			if strings.Contains(line, `"`+aliasedPath+`"`) ||
				strings.Contains(line, `'`+aliasedPath+`'`) {
				return true
			}
		}
	}
	return false
}

// InsertContractNodes registers contract nodes and their edges into the CPG.
// Backend handler → ContractNode edge kind: EdgeContract.
// ContractNode → frontend caller listed as Name on a stub NodeContract. [Épica 255.C]
func InsertContractNodes(g *Graph, workspace string, contracts []ContractNode) {
	for i := range contracts {
		c := &contracts[i]
		contractName := c.Method + " " + c.Path
		contractID := g.addNode(Node{
			Kind:    NodeContract,
			Package: "contract",
			File:    "",
			Name:    contractName,
		})
		// Link backend handler → ContractNode.
		if c.BackendFn != "" {
			if backendID, ok := g.NodeByName("", c.BackendFn); ok {
				g.addEdge(backendID, contractID, EdgeContract)
			}
		}
		// Link ContractNode → each frontend caller stub.
		for _, caller := range c.FrontendCallers {
			callerName := "ts:" + caller.File + ":" + string(rune('0'+caller.Line%10))
			callerID := g.addNode(Node{
				Kind:    NodeContract,
				Package: "frontend",
				File:    caller.File,
				Name:    callerName,
				Line:    caller.Line,
			})
			g.addEdge(contractID, callerID, EdgeContract)
		}
	}
}

// normalizeRoutePath converts framework-specific path params to {param} notation.
// :id → {id}, <id> → {id}. Strips trailing slash except root.
func normalizeRoutePath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		if strings.HasPrefix(part, ":") {
			parts[i] = "{" + part[1:] + "}"
		} else if strings.HasPrefix(part, "<") && strings.HasSuffix(part, ">") {
			parts[i] = "{" + part[1:len(part)-1] + "}"
		}
	}
	result := strings.Join(parts, "/")
	if result != "/" {
		result = strings.TrimRight(result, "/")
	}
	return result
}
