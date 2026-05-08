// scripts/yaml-schema-check.go — verifies neo.yaml.example covers every
// field with a yaml tag in pkg/config NeoConfig. Detects schema drift
// when new config fields are added without updating the example.
//
// Usage:
//   go run scripts/yaml-schema-check.go
//
// Exits 0 if example is complete, 1 if missing fields, 2 on error.
// [SRE-114.A]

//go:build script

package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
}

func run() error {
	tags, err := collectYamlTags("pkg/config/config.go")
	if err != nil {
		return fmt.Errorf("parse config struct: %w", err)
	}

	exampleData, err := os.ReadFile("neo.yaml.example")
	if err != nil {
		return fmt.Errorf("read neo.yaml.example: %w", err)
	}
	exampleText := string(exampleData)

	missing := []string{}
	for _, tag := range tags {
		// Naive presence check: yaml tag appearing as a key (`tag:`) anywhere
		// in the example. Sufficient for catching truly absent fields; doesn't
		// validate nesting paths but the false-negative rate is low.
		if !strings.Contains(exampleText, tag+":") {
			missing = append(missing, tag)
		}
	}
	sort.Strings(missing)

	if len(missing) == 0 {
		fmt.Printf("[yaml-schema-check] ✓ neo.yaml.example covers all %d yaml tags\n", len(tags))
		return nil
	}

	fmt.Printf("[yaml-schema-check] ✗ %d yaml tags missing from neo.yaml.example:\n", len(missing))
	for _, m := range missing {
		fmt.Printf("  - %s\n", m)
	}
	os.Exit(1)
	return nil
}

// collectYamlTags parses the file and returns every yaml tag used in struct fields.
func collectYamlTags(path string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}
	ast.Inspect(f, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		for _, field := range st.Fields.List {
			if field.Tag == nil {
				continue
			}
			raw := strings.Trim(field.Tag.Value, "`")
			tag := reflect.StructTag(raw).Get("yaml")
			if tag == "" || tag == "-" {
				continue
			}
			// Strip options like ",omitempty".
			if i := strings.Index(tag, ","); i >= 0 {
				tag = tag[:i]
			}
			if tag != "" {
				seen[tag] = struct{}{}
			}
		}
		return true
	})

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}
