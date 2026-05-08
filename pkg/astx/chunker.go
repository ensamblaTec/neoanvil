package astx

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/css"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/html"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
)

func SemanticChunk(ctx context.Context, src []byte, ext string) [][]byte {
	parser := sitter.NewParser()
	defer parser.Close()

	switch ext {
	case ".go":
		parser.SetLanguage(golang.GetLanguage())
	case ".js", ".jsx":
		parser.SetLanguage(javascript.GetLanguage())
	case ".ts", ".tsx":
		parser.SetLanguage(tsx.GetLanguage())
	case ".py":
		parser.SetLanguage(python.GetLanguage())
	case ".rs":
		parser.SetLanguage(rust.GetLanguage())
	case ".html":
		parser.SetLanguage(html.GetLanguage())
	case ".css":
		parser.SetLanguage(css.GetLanguage())
	default:
		return nil
	}

	tree, err := parser.ParseCtx(ctx, nil, src)
	if err != nil {
		return nil
	}
	defer tree.Close()

	var chunks [][]byte
	var walk func(*sitter.Node)
	walk = func(node *sitter.Node) {
		if ctx.Err() != nil {
			return
		}

		t := node.Type()
		if strings.Contains(t, "function") || strings.Contains(t, "method") || strings.Contains(t, "class") || strings.Contains(t, "struct") || strings.Contains(t, "type_declaration") || strings.Contains(t, "interface") {
			start := node.StartByte()
			end := node.EndByte()
			if start < end && int(end) <= len(src) {
				chunks = append(chunks, src[start:end])
				return
			}
		}

		for i := 0; i < int(node.ChildCount()); i++ {
			walk(node.Child(i))
		}
	}

	walk(tree.RootNode())

	return chunks
}

// ExtractTopLevelNodes parses a file and extracts the source code of top-level
// structs, interfaces, and functions to be used by the BM25 motor.
func ExtractTopLevelNodes(filepath string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath, nil, 0)
	if err != nil {
		return nil, err
	}

	var chunks []string
	for _, decl := range f.Decls {
		switch decl.(type) {
		case *ast.FuncDecl, *ast.GenDecl:
			var buf bytes.Buffer
			err := printer.Fprint(&buf, fset, decl)
			if err == nil {
				chunks = append(chunks, buf.String())
			}
		}
	}
	return chunks, nil
}
