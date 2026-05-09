package openapi

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// TestHandlerScanner_ExtractsSimpleStruct verifies the scanner picks
// up a typed struct response.
func TestHandlerScanner_ExtractsSimpleStruct(t *testing.T) {
	src := `package x

import "encoding/json"

var _ = json.Marshal


type StatusReply struct {
	OK   bool   ` + "`json:\"ok\"`" + `
	Note string ` + "`json:\"note,omitempty\"`" + `
}

func GetStatus(w int) {
	out := StatusReply{OK: true}
	_ = json.Marshal(out)
}
`
	tmp := t.TempDir()
	path := filepath.Join(tmp, "x.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewHandlerScanner()
	warns := s.ScanWorkspace(tmp)
	if len(warns) > 0 {
		t.Logf("scanner warnings: %v", warns)
	}
	schema := s.SchemaFor("GetStatus")
	if schema == nil {
		t.Fatalf("SchemaFor(GetStatus) = nil; expected typed schema")
	}
	if schema.Type != "object" {
		t.Errorf("Type = %q, want object", schema.Type)
	}
	if okField, has := schema.Properties["ok"]; !has || okField.Type != "boolean" {
		t.Errorf("ok field missing or wrong type: %+v", schema.Properties)
	}
	if noteField, has := schema.Properties["note"]; !has || noteField.Type != "string" {
		t.Errorf("note field missing or wrong type: %+v", schema.Properties)
	}
}

// TestHandlerScanner_ResolvesArrayAndPointer verifies array/pointer types.
func TestHandlerScanner_ResolvesArrayAndPointer(t *testing.T) {
	src := `package x

import "encoding/json"

var _ = json.Marshal


type Item struct {
	Name string ` + "`json:\"name\"`" + `
}

type ListReply struct {
	Items []*Item ` + "`json:\"items\"`" + `
	Total int     ` + "`json:\"total\"`" + `
}

func ListThings() {
	out := ListReply{}
	_ = json.NewEncoder(nil).Encode(out)
}
`
	tmp := t.TempDir()
	path := filepath.Join(tmp, "list.go")
	_ = os.WriteFile(path, []byte(src), 0o644)
	s := NewHandlerScanner()
	s.ScanWorkspace(tmp)
	schema := s.SchemaFor("ListThings")
	if schema == nil {
		t.Fatalf("nil schema")
	}
	items := schema.Properties["items"]
	if items == nil || items.Type != "array" {
		t.Fatalf("items type = %+v, want array", items)
	}
	if items.Items == nil || items.Items.Type != "object" {
		t.Errorf("items.Items = %+v, want object", items.Items)
	}
	if items.Items.Properties["name"] == nil {
		t.Errorf("nested struct's name field missing: %+v", items.Items.Properties)
	}
	if total := schema.Properties["total"]; total == nil || total.Type != "integer" {
		t.Errorf("total = %+v, want integer", total)
	}
}

// TestHandlerScanner_FallsBackOnUnresolved verifies the scanner
// returns nil (so caller uses baseline) when it can't resolve.
func TestHandlerScanner_FallsBackOnUnresolved(t *testing.T) {
	src := `package x

import "encoding/json"

var _ = json.Marshal


func WeirdHandler() {
	out := someOpaqueVar // not a CompositeLit, scanner can't resolve
	_ = json.Marshal(out)
}
`
	tmp := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmp, "x.go"), []byte(src), 0o644)
	s := NewHandlerScanner()
	s.ScanWorkspace(tmp)
	if got := s.SchemaFor("WeirdHandler"); got != nil {
		t.Errorf("expected nil for unresolved, got %+v", got)
	}
}

// TestHandlerScanner_RespectsJSONOmitTag verifies "-" tag drops field.
func TestHandlerScanner_RespectsJSONOmitTag(t *testing.T) {
	src := `package x

import "encoding/json"

var _ = json.Marshal


type Cfg struct {
	Public  string ` + "`json:\"public\"`" + `
	Secret  string ` + "`json:\"-\"`" + `
}

func GetCfg() {
	_ = json.Marshal(Cfg{})
}
`
	tmp := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmp, "cfg.go"), []byte(src), 0o644)
	s := NewHandlerScanner()
	s.ScanWorkspace(tmp)
	schema := s.SchemaFor("GetCfg")
	if schema == nil {
		t.Fatal("nil schema")
	}
	if _, has := schema.Properties["public"]; !has {
		t.Error("public field missing")
	}
	if _, has := schema.Properties["secret"]; has {
		t.Error("secret should be omitted via json:\"-\" tag")
	}
}

// TestHandlerScanner_StopsAtRecursionCap prevents infinite loops on
// self-referential types.
func TestHandlerScanner_StopsAtRecursionCap(t *testing.T) {
	src := `package x

import "encoding/json"

var _ = json.Marshal


type Tree struct {
	Children []*Tree ` + "`json:\"children\"`" + `
}

func GetTree() {
	_ = json.Marshal(Tree{})
}
`
	tmp := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmp, "tree.go"), []byte(src), 0o644)
	s := NewHandlerScanner()
	s.ScanWorkspace(tmp)
	schema := s.SchemaFor("GetTree")
	if schema == nil {
		t.Fatal("nil schema")
	}
	// Walk down the children chain — should bottom out at the
	// recursion cap (depth 4) by returning {"type": "object"}.
	cur := schema
	for range 6 {
		children := cur.Properties["children"]
		if children == nil {
			break
		}
		cur = children.Items
		if cur == nil {
			break
		}
	}
	// Just verify we didn't infinite-loop (test would hang).
}

// TestExprTypeName_StarExpr verifies pointer + composite literal
// shapes.
func TestExprTypeName_StarExpr(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", `package x
func F() {
	_ = json.Marshal(&Foo{})
	_ = json.Marshal(Foo{})
}
type Foo struct { A int }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	s := NewHandlerScanner()
	s.indexFile(f)
	if len(s.handlers["F"]) != 2 {
		t.Errorf("expected 2 encode targets in F, got %v", s.handlers["F"])
	}
	for _, target := range s.handlers["F"] {
		if target != "Foo" {
			t.Errorf("expected target Foo, got %q", target)
		}
	}
}
