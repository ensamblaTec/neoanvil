package main

import (
	"strings"
	"testing"
)

// TestSemanticResultQuality covers the three retrieval-quality classes used
// by SEMANTIC_CODE to label its response footer. [ÉPICA 153]
func TestSemanticResultQuality(t *testing.T) {
	cases := []struct {
		denseCount, minResults int
		want                   string
	}{
		{0, 1, "empty"},      // dense found nothing
		{0, 3, "empty"},      // dense found nothing, higher bar
		{1, 3, "undershoot"}, // some dense hits but below threshold
		{2, 3, "undershoot"}, // closer to ok but still under
		{3, 3, "ok"},         // exact threshold
		{5, 3, "ok"},         // over threshold
		{1, 1, "ok"},         // single-hit threshold
	}
	for _, tc := range cases {
		got := semanticResultQuality(tc.denseCount, tc.minResults)
		if got != tc.want {
			t.Errorf("semanticResultQuality(dense=%d, min=%d) = %q; want %q",
				tc.denseCount, tc.minResults, got, tc.want)
		}
	}
}

// TestClassifySemanticFallback covers the four fallback_used tags emitted
// in the SEMANTIC_CODE response footer. [ÉPICA 153]
func TestClassifySemanticFallback(t *testing.T) {
	cases := []struct {
		quality              string
		bm25Count, grepCount int
		want                 string
	}{
		// quality=ok → no fallback regardless of bm25/grep counts.
		{"ok", 0, 0, "none"},
		{"ok", 5, 3, "none"},
		// grep hits dominate when present (most actionable signal).
		{"undershoot", 0, 1, "grep"},
		{"undershoot", 5, 3, "grep"},
		{"empty", 0, 2, "grep"},
		// bm25-only when undershoot has BM25 hits but grep returned 0.
		{"undershoot", 3, 0, "bm25_only"},
		{"empty", 5, 0, "bm25_only"},
		// grep_no_match when nothing found at all.
		{"undershoot", 0, 0, "grep_no_match"},
		{"empty", 0, 0, "grep_no_match"},
	}
	for _, tc := range cases {
		got := classifySemanticFallback(tc.quality, tc.bm25Count, tc.grepCount)
		if got != tc.want {
			t.Errorf("classifySemanticFallback(quality=%q, bm25=%d, grep=%d) = %q; want %q",
				tc.quality, tc.bm25Count, tc.grepCount, got, tc.want)
		}
	}
}

// TestBuildSemanticGrepFallback_DenseSatisfiedSkipsGrep verifies the fast
// path: when dense already cleared minResults, grep does not run and the
// section is empty. [ÉPICA 153]
func TestBuildSemanticGrepFallback_DenseSatisfiedSkipsGrep(t *testing.T) {
	tool := &RadarTool{workspace: t.TempDir()}
	var sb strings.Builder
	count := buildSemanticGrepFallback(tool, &sb, "anything", 5, 3)
	if count != 0 {
		t.Errorf("count = %d; want 0 (grep should not run when dense >= min)", count)
	}
	if sb.Len() != 0 {
		t.Errorf("section non-empty: %q; expected empty when dense satisfies min", sb.String())
	}
}

// TestSanitizeFenced covers the markdown-fence-injection guard from
// 153.H. Files containing literal ``` lines must not break the
// surrounding ``` block we wrap them in. The ZWJ-insertion preserves
// visual fidelity while neutralizing the parser-level terminator.
func TestSanitizeFenced(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no_fence_passthrough",
			in:   "func foo() { return 1 }",
			want: "func foo() { return 1 }",
		},
		{
			name: "single_fence_neutralized",
			in:   "```go\nfunc foo() {}\n```",
			want: "``‍`go\nfunc foo() {}\n``‍`",
		},
		{
			name: "multiple_fences_all_neutralized",
			in:   "first```block```done```end",
			want: "first``‍`block``‍`done``‍`end",
		},
		{
			name: "double_backtick_unchanged",
			in:   "use ``foo`` for inline",
			want: "use ``foo`` for inline",
		},
		{
			name: "single_backtick_unchanged",
			in:   "`code`",
			want: "`code`",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeFenced(tc.in); got != tc.want {
				t.Errorf("sanitizeFenced(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
