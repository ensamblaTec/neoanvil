package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("Uso: go run main.go <directorios...>")
	}

	todosFile, err := os.OpenFile("todos_report.txt", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err == nil {
		defer func() { _ = todosFile.Close() }()
	}

	for _, dir := range os.Args[1:] {
		cleanDir := filepath.Clean(dir)
		walkErr := filepath.Walk(cleanDir, func(path string, info os.FileInfo, walkFnErr error) error {
			if walkFnErr != nil {
				return walkFnErr
			}
			if !info.IsDir() && strings.HasSuffix(path, ".go") && !strings.Contains(path, "vendor") {
				if procErr := processFile(path, todosFile); procErr != nil {
					log.Printf("Error processing %s: %v", path, procErr)
				}
			}
			return nil
		})
		if walkErr != nil {
			log.Fatalf("Error walking directory: %v", walkErr)
		}
	}
}

func processFile(path string, todosFile *os.File) error {
	fset := token.NewFileSet()
	fileWithComments, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return err
	}

	if todosFile != nil {
		for _, cg := range fileWithComments.Comments {
			for _, comment := range cg.List {
				upText := strings.ToUpper(comment.Text)
				if strings.Contains(upText, "TODO") || strings.Contains(upText, "FIXME") {
					line := fset.Position(comment.Pos()).Line
					_, _ = fmt.Fprintf(todosFile, "Archivo: %s (Línea %d) -> %s\n", path, line, comment.Text)
				}
			}
		}
	}

	cleanFile, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return err
	}

	ast.Inspect(cleanFile, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.File:
			node.Doc = nil
		case *ast.GenDecl:
			node.Doc = nil
		case *ast.FuncDecl:
			node.Doc = nil
		case *ast.TypeSpec:
			node.Doc = nil
		case *ast.Field:
			node.Doc = nil
		}
		return true
	})

	var buf bytes.Buffer
	err = printer.Fprint(&buf, fset, cleanFile)
	if err != nil {
		return err
	}

	return os.WriteFile(path, buf.Bytes(), 0600)
}
