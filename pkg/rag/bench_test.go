package rag

import (
	"testing"
)

func BenchmarkLexicalIndex_AddDocument(b *testing.B) {
	b.ReportAllocs()
	index := NewLexicalIndex()
	doc := "func performShadowCompilation(astNode *sitter.Node) error { return nil }"

	b.ResetTimer()
	for i := uint64(0); i < uint64(b.N); i++ {
		index.AddDocument(i, doc)
	}
}

func BenchmarkLexicalIndex_Search(b *testing.B) {
	b.ReportAllocs()
	index := NewLexicalIndex()
	doc := "func performShadowCompilation(astNode *sitter.Node) error { return nil }"
	index.AddDocument(1, doc)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		index.Search("ShadowCompilation", 5)
	}
}
