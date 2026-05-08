// Package chunker splits source files and logs into token-sized chunks
// for DeepSeek prompt assembly (PILAR XXIV / 131.F).
//
// Two strategies:
//   - ASTChunker: Go files → FuncDecl + TypeSpec boundaries (semantic)
//   - LineChunker: any text → fixed-size windows with 10% overlap (structural)
//
// Token estimation: 1 token ≈ 4 characters (OpenAI/DeepSeek rough average).
package chunker

// Chunk is a contiguous slice of source content with optional metadata.
type Chunk struct {
	Name       string // function/type name (AST) or "" (line)
	Body       string // full text of the chunk
	StartLine  int    // 1-based
	EndLine    int    // 1-based, inclusive
	DocComment string // leading doc comment (AST only)
}
