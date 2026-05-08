// Package diff — boltdb.go: key-level diff between two BoltDB snapshots.
// PILAR XXVI / 136.A.2.
//
// DiffBuckets compares the contents of one bbolt bucket between two
// snapshots and classifies every key as added / modified / deleted /
// unchanged. The merge sync layer (136.B) intersects two such diffs
// (local-vs-ancestor, remote-vs-ancestor) to detect conflicts.
//
// We don't open the actual .db files here — that would require
// extracting the encrypted archive on disk and opening bbolt, which
// the caller may not want. Instead, the caller materializes each
// bucket's contents into a map[string][]byte (typically by walking
// the bucket once at extract time) and we operate on the maps. This
// keeps the diff engine pure and trivially testable.

package diff

import "bytes"

// BucketChangeKind enumerates the four states of a key.
type BucketChangeKind string

const (
	ChangeAdded     BucketChangeKind = "added"
	ChangeModified  BucketChangeKind = "modified"
	ChangeDeleted   BucketChangeKind = "deleted"
	ChangeUnchanged BucketChangeKind = "unchanged"
)

// BucketChange records one key's transition between ancestor and side.
// AncestorValue is empty when Kind=Added; SideValue is empty when
// Kind=Deleted. For ChangeModified both are populated so the merge
// resolver can render a three-way view.
type BucketChange struct {
	Key           string
	Kind          BucketChangeKind
	AncestorValue []byte
	SideValue     []byte
}

// BucketDiff is the full result for one bucket. Keys are sorted only
// when Sorted=true so callers that want stable ordering can rely on
// it; default is map iteration order (faster, no sort overhead).
type BucketDiff struct {
	Added     []BucketChange
	Modified  []BucketChange
	Deleted   []BucketChange
	Unchanged int // count only — stored as int because most tools don't need the keys
}

// DiffBuckets compares ancestor and side maps and returns the
// classification. Keys present in both with identical bytes go in
// Unchanged (only the count). Different bytes → Modified. Only-in-side
// → Added. Only-in-ancestor → Deleted.
//
// The implementation is O(|ancestor| + |side|) — single pass over each.
// Callers comparing huge buckets (millions of keys) should chunk the
// input rather than loading everything into one map.
func DiffBuckets(ancestor, side map[string][]byte) BucketDiff {
	var d BucketDiff
	// First pass: every key in ancestor.
	for k, av := range ancestor {
		sv, ok := side[k]
		switch {
		case !ok:
			d.Deleted = append(d.Deleted, BucketChange{Key: k, Kind: ChangeDeleted, AncestorValue: copyBytes(av)})
		case bytes.Equal(av, sv):
			d.Unchanged++
		default:
			d.Modified = append(d.Modified, BucketChange{Key: k, Kind: ChangeModified, AncestorValue: copyBytes(av), SideValue: copyBytes(sv)})
		}
	}
	// Second pass: keys in side but not in ancestor.
	for k, sv := range side {
		if _, ok := ancestor[k]; ok {
			continue
		}
		d.Added = append(d.Added, BucketChange{Key: k, Kind: ChangeAdded, SideValue: copyBytes(sv)})
	}
	return d
}

// IsEmpty returns true when no add/modify/delete changes were detected.
// Unchanged count is irrelevant.
func (d BucketDiff) IsEmpty() bool {
	return len(d.Added) == 0 && len(d.Modified) == 0 && len(d.Deleted) == 0
}

// Total returns the count of changed keys (excluding Unchanged).
func (d BucketDiff) Total() int {
	return len(d.Added) + len(d.Modified) + len(d.Deleted)
}

// copyBytes defensive-copies a byte slice so the diff result doesn't
// alias caller-owned memory. Callers may safely mutate their inputs
// after DiffBuckets returns.
func copyBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
