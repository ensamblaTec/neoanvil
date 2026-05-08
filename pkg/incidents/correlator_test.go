package incidents

import (
	"strings"
	"testing"
)

func TestFormatCPGSection_Empty(t *testing.T) {
	if got := FormatCPGSection(nil); got != "" {
		t.Errorf("nil input: want empty, got %q", got)
	}
	if got := FormatCPGSection([]CPGCorrelation{}); got != "" {
		t.Errorf("empty slice: want empty, got %q", got)
	}
}

func TestFormatCPGSection_NonEmpty(t *testing.T) {
	corrs := []CPGCorrelation{
		{File: "pkg/rag/hnsw.go", FuncName: "Search", CodeRank: 0.42, CallerCount: 7},
	}
	out := FormatCPGSection(corrs)
	for _, want := range []string{"Search", "hnsw.go", "0.420000", "7", "CPG Blast Radius"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestFormatCPGSection_MultipleRows(t *testing.T) {
	corrs := []CPGCorrelation{
		{File: "a.go", FuncName: "Foo", CodeRank: 0.1, CallerCount: 1},
		{File: "b.go", FuncName: "Bar", CodeRank: 0.2, CallerCount: 3},
	}
	out := FormatCPGSection(corrs)
	if !strings.Contains(out, "Foo") || !strings.Contains(out, "Bar") {
		t.Errorf("expected both functions in output: %s", out)
	}
}

func TestCorrelateWithCPG_NilManager(t *testing.T) {
	result := CorrelateWithCPG([]byte("crash in pkg/rag/hnsw.go:42"), nil, 5)
	if result != nil {
		t.Errorf("nil manager: want nil, got %v", result)
	}
}
