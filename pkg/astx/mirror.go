package astx

import (
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/css"
	"github.com/smacker/go-tree-sitter/html"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
)

func ValidateSyntax(ctx context.Context, src []byte, filename string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ext := filepath.Ext(filename)
	if filename == "" {
		ext = ".go"
	}
	switch ext {
	case ".go":
		return validateGoSyntax(src)
	case ".json":
		if !json.Valid(src) {
			return fmt.Errorf("invalid json syntax")
		}
		return nil
	case ".js", ".jsx", ".ts", ".tsx", ".py", ".rs", ".html", ".css":
		return validateSitterSyntax(ctx, ext, src)
	default:
		return nil
	}
}

func validateGoSyntax(src []byte) error {
	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("Go syntax error: %w", err)
	}
	return nil
}

func validateSitterSyntax(ctx context.Context, ext string, src []byte) error {
	p := sitter.NewParser()
	switch ext {
	case ".js", ".jsx":
		p.SetLanguage(javascript.GetLanguage())
	case ".ts", ".tsx":
		p.SetLanguage(tsx.GetLanguage())
	case ".py":
		p.SetLanguage(python.GetLanguage())
	case ".rs":
		p.SetLanguage(rust.GetLanguage())
	case ".html":
		p.SetLanguage(html.GetLanguage())
	case ".css":
		p.SetLanguage(css.GetLanguage())
	}
	tree, err := p.ParseCtx(ctx, nil, src)
	if err != nil {
		return fmt.Errorf("failed to parse %s: %w", ext, err)
	}
	if tree.RootNode().HasError() {
		return fmt.Errorf("syntax error in %s via tree-sitter", ext)
	}
	return nil
}
