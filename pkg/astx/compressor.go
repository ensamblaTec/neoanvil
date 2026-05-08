package astx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/css"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/html"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
)

var astCache sync.Map

type cacheEntry struct {
	modTime    int64
	compressed []byte
}

type CollapseRange struct {
	Start int
	End   int
}

func SemanticCompress(ctx context.Context, filename string, src []byte, targetSymbol string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stat, statErr := os.Stat(filename)
	if cached, ok := loadCompressCache(filename, targetSymbol, stat, statErr); ok {
		return cached, nil
	}
	ext := filepath.Ext(filename)
	p := sitter.NewParser()
	if !selectSitterLanguage(ext, p) {
		return src, nil
	}
	tree, err := p.ParseCtx(ctx, nil, src)
	if err != nil {
		return nil, fmt.Errorf("error parsing AST: %s: %w", filename, err)
	}
	ranges := buildCollapseRanges(ctx, tree.RootNode(), src, targetSymbol)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	compressed := applyCollapseRanges(src, ranges)
	if statErr == nil {
		key := filename + ":" + targetSymbol
		astCache.Store(key, cacheEntry{modTime: stat.ModTime().UnixNano(), compressed: compressed})
	}
	return compressed, nil
}

func loadCompressCache(filename, targetSymbol string, stat os.FileInfo, statErr error) ([]byte, bool) {
	if statErr != nil {
		return nil, false
	}
	key := filename + ":" + targetSymbol
	if cached, ok := astCache.Load(key); ok {
		entry := cached.(cacheEntry)
		if entry.modTime == stat.ModTime().UnixNano() {
			return entry.compressed, true
		}
	}
	return nil, false
}

func selectSitterLanguage(ext string, p *sitter.Parser) bool {
	switch ext {
	case ".go":
		p.SetLanguage(golang.GetLanguage())
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
	default:
		return false
	}
	return true
}

func buildCollapseRanges(ctx context.Context, root *sitter.Node, src []byte, targetSymbol string) []CollapseRange {
	var ranges []CollapseRange
	var walk func(*sitter.Node)
	walk = func(node *sitter.Node) {
		if ctx.Err() != nil {
			return
		}
		t := node.Type()
		if strings.Contains(t, "function") || strings.Contains(t, "method") || strings.Contains(t, "class") {
			nameMatch := false
			for i := 0; i < int(node.ChildCount()); i++ {
				child := node.Child(i)
				if child.Type() == "identifier" || child.Type() == "name" {
					if child.Content(src) == targetSymbol {
						nameMatch = true
						break
					}
				}
			}
			if !nameMatch {
				for i := 0; i < int(node.ChildCount()); i++ {
					child := node.Child(i)
					ct := child.Type()
					if ct == "block" || ct == "statement_block" {
						start := int(child.StartByte()) + 1
						end := int(child.EndByte()) - 1
						if start < end {
							ranges = append(ranges, CollapseRange{Start: start, End: end})
						}
					}
				}
			}
		}
		for i := 0; i < int(node.ChildCount()); i++ {
			walk(node.Child(i))
		}
	}
	walk(root)
	return ranges
}

func applyCollapseRanges(src []byte, ranges []CollapseRange) []byte {
	sort.Slice(ranges, func(i, j int) bool {
		return ranges[i].Start < ranges[j].Start
	})
	var buf bytes.Buffer
	buf.Grow(len(src))
	last := 0
	for _, r := range ranges {
		if r.Start < last {
			continue
		}
		buf.Write(src[last:r.Start])
		buf.WriteString("\n\t/* ... Code omitted for safety and brevity. MCP Compression ... */\n")
		last = r.End
	}
	buf.Write(src[last:])
	return buf.Bytes()
}
