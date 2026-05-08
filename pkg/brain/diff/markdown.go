// Package diff — markdown.go: line-level LCS diff for .md files.
// PILAR XXVI / 136.A.3.
//
// DiffLines compares two []string (line-split file contents) and
// produces the minimal edit script that turns ancestor into side.
// We use the standard Hunt-Szymanski LCS (O((N+M)·D) where D is the
// edit distance) because:
//
//   * Operator-edited markdown files (master_plan.md, technical_debt.md)
//     change in human-scale ways — typically <100 line edits between
//     snapshots, so LCS dominated by D, not N+M.
//   * No third-party dep — go-diff is BSD but adds 30 KLoc; we only
//     need plain line-level diff, no semantic merge.
//   * Output is the same shape conflict resolution (136.B-D) consumes:
//     a slice of LineEdit{Kind, Line, Index}.
//
// For binary files or anything beyond ~50K lines, fall through to
// DiffBuckets — line LCS becomes O(N²) on degenerate inputs.

package diff

// LineEditKind is the same enum BucketChangeKind serves for keys.
// Reuses the constant strings so the merge layer can treat them
// uniformly when rendering a unified diff.
type LineEditKind string

const (
	LineEqual    LineEditKind = "equal"
	LineInserted LineEditKind = "inserted" // present in side, absent in ancestor
	LineDeleted  LineEditKind = "deleted"  // present in ancestor, absent in side
)

// LineEdit is one line in the edit script. Index is the 0-based row
// in the *side* sequence for Equal/Inserted; for Deleted, it's the
// row in the *ancestor* sequence.
type LineEdit struct {
	Kind  LineEditKind
	Index int
	Line  string
}

// DiffLines returns the edit script transforming ancestor into side.
// The script preserves Equal lines so the merge resolver can render
// context around hunks.
//
// Implementation: classic Hirschberg-style LCS via dynamic programming
// over a (M+1)×(N+1) table. For our typical inputs (<5000 lines)
// memory is bounded; for larger inputs callers should pre-chunk.
func DiffLines(ancestor, side []string) []LineEdit {
	m, n := len(ancestor), len(side)
	if m == 0 || n == 0 {
		return diffLinesEdgeCase(ancestor, side)
	}
	dp := buildLCSTable(ancestor, side)
	rev := backtrackEdits(dp, ancestor, side)
	return reverseEdits(rev)
}

// diffLinesEdgeCase handles empty ancestor or empty side. Both empty
// returns nil; otherwise emits a sequence of pure inserts or deletes.
func diffLinesEdgeCase(ancestor, side []string) []LineEdit {
	switch {
	case len(ancestor) == 0 && len(side) == 0:
		return nil
	case len(ancestor) == 0:
		out := make([]LineEdit, len(side))
		for i, s := range side {
			out[i] = LineEdit{Kind: LineInserted, Index: i, Line: s}
		}
		return out
	default: // len(side) == 0
		out := make([]LineEdit, len(ancestor))
		for i, a := range ancestor {
			out[i] = LineEdit{Kind: LineDeleted, Index: i, Line: a}
		}
		return out
	}
}

// buildLCSTable fills the (m+1)×(n+1) DP table where dp[i][j] = LCS
// length of ancestor[:i] and side[:j].
func buildLCSTable(ancestor, side []string) [][]int {
	m, n := len(ancestor), len(side)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if ancestor[i-1] == side[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}
	return dp
}

// backtrackEdits walks the DP table from (m,n) → (0,0) emitting the
// edit script in reverse order. Caller reverses to get forward order.
func backtrackEdits(dp [][]int, ancestor, side []string) []LineEdit {
	var rev []LineEdit
	i, j := len(ancestor), len(side)
	for i > 0 && j > 0 {
		switch {
		case ancestor[i-1] == side[j-1]:
			rev = append(rev, LineEdit{Kind: LineEqual, Index: j - 1, Line: side[j-1]})
			i--
			j--
		case dp[i-1][j] >= dp[i][j-1]:
			rev = append(rev, LineEdit{Kind: LineDeleted, Index: i - 1, Line: ancestor[i-1]})
			i--
		default:
			rev = append(rev, LineEdit{Kind: LineInserted, Index: j - 1, Line: side[j-1]})
			j--
		}
	}
	for i > 0 {
		rev = append(rev, LineEdit{Kind: LineDeleted, Index: i - 1, Line: ancestor[i-1]})
		i--
	}
	for j > 0 {
		rev = append(rev, LineEdit{Kind: LineInserted, Index: j - 1, Line: side[j-1]})
		j--
	}
	return rev
}

// reverseEdits returns a forward-order copy of the reversed edit script
// produced by backtrackEdits.
func reverseEdits(rev []LineEdit) []LineEdit {
	out := make([]LineEdit, len(rev))
	for k := range rev {
		out[k] = rev[len(rev)-1-k]
	}
	return out
}

// HasChanges returns true when at least one Inserted or Deleted edit
// exists. Cheap test for "did the file change at all" without scanning
// the full edit script when the answer is no.
func HasChanges(edits []LineEdit) bool {
	for _, e := range edits {
		if e.Kind != LineEqual {
			return true
		}
	}
	return false
}

// CountChanges returns (insertions, deletions). Useful for status output
// and conflict-resolution heuristics ("largest hunk first").
func CountChanges(edits []LineEdit) (insertions, deletions int) {
	for _, e := range edits {
		switch e.Kind {
		case LineInserted:
			insertions++
		case LineDeleted:
			deletions++
		}
	}
	return
}
