package incidents

import (
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/rag"
)

// — splitForEmbedding —

func TestSplitForEmbedding_Empty(t *testing.T) {
	if got := splitForEmbedding("", 100); len(got) != 0 {
		t.Errorf("empty text: want 0 chunks, got %d", len(got))
	}
}

func TestSplitForEmbedding_FitsInBudget(t *testing.T) {
	chunks := splitForEmbedding("hello world", 100)
	if len(chunks) != 1 || chunks[0] != "hello world" {
		t.Errorf("short text: want [\"hello world\"], got %v", chunks)
	}
}

func TestSplitForEmbedding_SplitsOnNewline(t *testing.T) {
	// "line1\nline2" (11 bytes), maxBytes=8 → LastIndex("\n") in "line1\nli" = 5 ≥ maxBytes/2=4
	chunks := splitForEmbedding("line1\nline2", 8)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "line1" {
		t.Errorf("chunk[0]: want \"line1\", got %q", chunks[0])
	}
	if chunks[1] != "line2" {
		t.Errorf("chunk[1]: want \"line2\", got %q", chunks[1])
	}
}

func TestSplitForEmbedding_HardSlice_NoNewline(t *testing.T) {
	// No newline → hard-slice at maxBytes; all input chars preserved across chunks.
	text := "abcdefghij"
	chunks := splitForEmbedding(text, 4)
	if len(chunks) < 2 {
		t.Fatalf("want multiple chunks, got %d: %v", len(chunks), chunks)
	}
	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	if total != len(text) {
		t.Errorf("total chars across chunks %d ≠ input len %d", total, len(text))
	}
}

// — IndexIncidentsBM25Only —

func TestIndexIncidentsBM25Only_NilIndex(t *testing.T) {
	IndexIncidentsBM25Only(t.TempDir(), nil) // must not panic
}

func TestIndexIncidentsBM25Only_MissingDir(t *testing.T) {
	lex := rag.NewLexicalIndex()
	IndexIncidentsBM25Only(t.TempDir(), lex) // .neo/incidents absent → no panic, no documents
}

func TestIndexIncidentsBM25Only_WithFiles(t *testing.T) {
	ws := t.TempDir()
	dir := ws + "/.neo/incidents"
	mustMkdir(t, dir)
	mustWrite(t, dir+"/INC-20260417-083232.md", fixtureINC)
	mustWrite(t, dir+"/INC-20260418-090000.md", "# TICKET SRE: INC-20260418-090000\nsome other content about memory")
	mustWrite(t, dir+"/not-inc.md", "this file should be ignored by the indexer")

	lex := rag.NewLexicalIndex()
	IndexIncidentsBM25Only(ws, lex)

	// fixtureINC contains "latency" → BM25 should return a result
	ranked := lex.Search("latency", 5)
	if len(ranked) == 0 {
		t.Error("expected at least one BM25 result for 'latency' after indexing")
	}
}

// — SearchIncidentsBM25 —

func TestSearchIncidentsBM25_NilIndex(t *testing.T) {
	if got := SearchIncidentsBM25("query", nil, t.TempDir(), 5); got != nil {
		t.Errorf("nil index: want nil, got %v", got)
	}
}

func TestSearchIncidentsBM25_EmptyQuery(t *testing.T) {
	lex := rag.NewLexicalIndex()
	if got := SearchIncidentsBM25("", lex, t.TempDir(), 5); got != nil {
		t.Errorf("empty query: want nil, got %v", got)
	}
}

func TestSearchIncidentsBM25_ZeroLimit(t *testing.T) {
	lex := rag.NewLexicalIndex()
	if got := SearchIncidentsBM25("query", lex, t.TempDir(), 0); got != nil {
		t.Errorf("zero limit: want nil, got %v", got)
	}
}

func TestSearchIncidentsBM25_WithMatches(t *testing.T) {
	ws := t.TempDir()
	dir := ws + "/.neo/incidents"
	mustMkdir(t, dir)
	mustWrite(t, dir+"/INC-20260417-083232.md", fixtureINC)

	lex := rag.NewLexicalIndex()
	IndexIncidentsBM25Only(ws, lex)

	results := SearchIncidentsBM25("latency", lex, ws, 5)
	if len(results) == 0 {
		t.Fatal("expected at least one result for 'latency' query")
	}
	if results[0].ID != "INC-20260417-083232" {
		t.Errorf("want INC-20260417-083232, got %q", results[0].ID)
	}
}

func TestSearchIncidentsBM25_NoMatch(t *testing.T) {
	ws := t.TempDir()
	dir := ws + "/.neo/incidents"
	mustMkdir(t, dir)
	mustWrite(t, dir+"/INC-20260417-083232.md", "# INC\nnothing special here\n")

	lex := rag.NewLexicalIndex()
	IndexIncidentsBM25Only(ws, lex)

	// A token with zero IDF hits returns nil or empty — both acceptable.
	_ = SearchIncidentsBM25("xyzzy_nonexistent_token_99", lex, ws, 5)
}
