package chunker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// ASTChunker splits Go source files by declaration boundaries (FuncDecl + TypeSpec).
// Falls back to LineChunker when the file cannot be parsed.
type ASTChunker struct {
	fallback *LineChunker
}

// NewASTChunker creates an ASTChunker.
// chunkSizeTokens is forwarded to the fallback LineChunker (0 → default 2000).
func NewASTChunker(chunkSizeTokens int) *ASTChunker {
	return &ASTChunker{fallback: NewLineChunker(chunkSizeTokens)}
}

// Chunk parses src as Go source and returns one Chunk per top-level declaration.
// Returns LineChunker output when the file cannot be parsed.
func (c *ASTChunker) Chunk(src string) []Chunk {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return c.fallback.Chunk(src)
	}

	lines := strings.Split(src, "\n")
	var chunks []Chunk

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			start := fset.Position(d.Pos()).Line
			end := fset.Position(d.End()).Line
			chunks = append(chunks, Chunk{
				Name:       d.Name.Name,
				Body:       joinLines(lines, start, end),
				StartLine:  start,
				EndLine:    end,
				DocComment: docString(d.Doc),
			})
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				start := fset.Position(d.Pos()).Line
				end := fset.Position(d.End()).Line
				chunks = append(chunks, Chunk{
					Name:       ts.Name.Name,
					Body:       joinLines(lines, start, end),
					StartLine:  start,
					EndLine:    end,
					DocComment: docString(d.Doc),
				})
			}
		}
	}

	if len(chunks) == 0 {
		return c.fallback.Chunk(src)
	}
	return chunks
}

func joinLines(lines []string, start, end int) string {
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start-1:end], "\n")
}

func docString(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range cg.List {
		b.WriteString(c.Text)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
