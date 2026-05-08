// Package merge implements three-way merge sync for brain snapshots.
// PILAR XXVI / 136.B + 136.D.
//
// DetectConflicts intersects two BucketDiffs (local-vs-ancestor and
// remote-vs-ancestor) and returns the keys both sides changed since
// the common ancestor — the conflict set the operator (or auto-rules)
// resolves before the merged snapshot is pushed.
//
// AutoResolve applies three rules that handle trivial conflicts
// without operator input. The rules are bucket-aware: caller passes a
// BucketKind hint so the resolver knows which strategy applies.

package merge

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/ensamblatec/neoanvil/pkg/brain/diff"
)

// Conflict describes one key that both sides changed since the
// common ancestor. The merge resolver displays AncestorValue as
// shared context, LocalValue and RemoteValue as the two divergent
// sides; the operator (or an auto-rule) picks one or composes a new
// value.
type Conflict struct {
	Bucket        string
	Key           string
	AncestorValue []byte // empty when neither side had it before (concurrent insert)
	LocalValue    []byte // empty when local deleted
	RemoteValue   []byte // empty when remote deleted
	Reason        string // human-readable trigger ("modified on both sides", "delete vs modify", ...)
}

// DetectConflicts finds keys that appear in both localDiff and
// remoteDiff with non-trivial overlap. Returns nothing when one side
// is purely additive over the other (those are merged without prompt).
//
// Conflict triggers:
//
//	modified-modified  — same key, both sides changed to different values
//	delete-modified    — local deleted, remote modified (or vice versa)
//	added-added        — both sides inserted the same key with different values
//
// Identical-modified (both sides changed to the same value) is NOT a
// conflict — it auto-merges.
func DetectConflicts(bucket string, localDiff, remoteDiff diff.BucketDiff) []Conflict {
	localChanges := indexChanges(localDiff)
	remoteChanges := indexChanges(remoteDiff)

	var out []Conflict
	for key, lc := range localChanges {
		rc, both := remoteChanges[key]
		if !both {
			continue
		}
		if c, isConflict := classifyPair(bucket, key, lc, rc); isConflict {
			out = append(out, c)
		}
	}
	return out
}

// indexChanges builds a map of key → BucketChange for fast intersection
// lookup. Unchanged keys aren't recorded — they don't need merging.
func indexChanges(d diff.BucketDiff) map[string]diff.BucketChange {
	m := make(map[string]diff.BucketChange, len(d.Added)+len(d.Modified)+len(d.Deleted))
	for _, c := range d.Added {
		m[c.Key] = c
	}
	for _, c := range d.Modified {
		m[c.Key] = c
	}
	for _, c := range d.Deleted {
		m[c.Key] = c
	}
	return m
}

// classifyPair decides whether (lc, rc) — same key, change on both sides —
// is a real conflict needing resolution, or merges trivially.
//
// Returns (Conflict, false) for trivial cases the caller can ignore.
func classifyPair(bucket, key string, lc, rc diff.BucketChange) (Conflict, bool) {
	// Modified vs modified: conflict only if values differ.
	if lc.Kind == diff.ChangeModified && rc.Kind == diff.ChangeModified {
		if bytes.Equal(lc.SideValue, rc.SideValue) {
			return Conflict{}, false
		}
		return Conflict{
			Bucket:        bucket,
			Key:           key,
			AncestorValue: bytes.Clone(lc.AncestorValue),
			LocalValue:    bytes.Clone(lc.SideValue),
			RemoteValue:   bytes.Clone(rc.SideValue),
			Reason:        "modified on both sides with different values",
		}, true
	}
	// Delete vs modify (either direction): always a conflict.
	if lc.Kind == diff.ChangeDeleted && rc.Kind == diff.ChangeModified {
		return Conflict{
			Bucket:        bucket,
			Key:           key,
			AncestorValue: bytes.Clone(lc.AncestorValue),
			LocalValue:    nil,
			RemoteValue:   bytes.Clone(rc.SideValue),
			Reason:        "local deleted, remote modified",
		}, true
	}
	if lc.Kind == diff.ChangeModified && rc.Kind == diff.ChangeDeleted {
		return Conflict{
			Bucket:        bucket,
			Key:           key,
			AncestorValue: bytes.Clone(lc.AncestorValue),
			LocalValue:    bytes.Clone(lc.SideValue),
			RemoteValue:   nil,
			Reason:        "local modified, remote deleted",
		}, true
	}
	// Added vs added with different values.
	if lc.Kind == diff.ChangeAdded && rc.Kind == diff.ChangeAdded {
		if bytes.Equal(lc.SideValue, rc.SideValue) {
			return Conflict{}, false
		}
		return Conflict{
			Bucket:      bucket,
			Key:         key,
			LocalValue:  bytes.Clone(lc.SideValue),
			RemoteValue: bytes.Clone(rc.SideValue),
			Reason:      "concurrent insert with different values",
		}, true
	}
	// Delete-delete: both sides already agree (key is gone); no conflict.
	if lc.Kind == diff.ChangeDeleted && rc.Kind == diff.ChangeDeleted {
		return Conflict{}, false
	}
	// [146.N] Catch-all for inconsistent kind pairs (e.g. Added+Modified,
	// Deleted+Added) that should not arise from a well-formed diff but could
	// appear in a crafted or corrupted manifest. Surface as an explicit
	// conflict rather than silently dropping the key — the operator (or
	// AutoResolve) decides how to handle it.
	return Conflict{
		Bucket:        bucket,
		Key:           key,
		AncestorValue: bytes.Clone(lc.AncestorValue),
		LocalValue:    bytes.Clone(lc.SideValue),
		RemoteValue:   bytes.Clone(rc.SideValue),
		Reason:        fmt.Sprintf("inconsistent change kinds: local=%s remote=%s", lc.Kind, rc.Kind),
	}, true
}

// =============================================================================
// 136.D — Auto-resolution rules
// =============================================================================

// BucketKind hints at the bucket's semantics so AutoResolve knows
// which strategy applies. Operator code maps bucket names to kinds at
// dispatch time.
type BucketKind int

const (
	// BucketGeneric — no special semantics; conflicts always need operator.
	BucketGeneric BucketKind = iota

	// BucketAppendOnly — entries only added, never modified
	// (memex_buffer, incidents/*). Concurrent inserts auto-merge as
	// union; never produces a real conflict.
	BucketAppendOnly

	// BucketMonotonicCounter — key holds a stringified integer that
	// only ever grows (token_budget). Conflict resolves to max(local, remote).
	BucketMonotonicCounter

	// BucketTombstones — keys map to either "live" or "tombstone"
	// (debt resolved markers). Tombstone wins to ensure deletes
	// propagate.
	BucketTombstones
)

// AutoResolution captures one auto-resolved conflict. Mirrors
// Conflict but adds the chosen value + reason.
type AutoResolution struct {
	Conflict      Conflict
	ResolvedValue []byte
	Strategy      string // "append_union" | "monotonic_max" | "tombstone_wins"
}

// AutoResolve applies the kind-specific rule to a conflict. Returns
// (resolution, true) when resolved automatically; (zero, false) when
// the conflict needs operator review.
func AutoResolve(c Conflict, kind BucketKind) (AutoResolution, bool) {
	switch kind {
	case BucketAppendOnly:
		// Append-only: both LocalValue and RemoteValue are kept; when a key
		// collision occurs use bytes.Compare as a deterministic tie-breaker so
		// any two nodes independently resolving the same conflict converge to
		// the same value regardless of which side is "local" vs "remote".
		winner := c.LocalValue
		if bytes.Compare(c.LocalValue, c.RemoteValue) < 0 {
			winner = c.RemoteValue
		}
		return AutoResolution{
			Conflict:      c,
			ResolvedValue: winner,
			Strategy:      "append_union",
		}, true
	case BucketMonotonicCounter:
		l, lok := parseInt(c.LocalValue)
		r, rok := parseInt(c.RemoteValue)
		if !lok || !rok {
			return AutoResolution{}, false
		}
		winner := max(l, r)
		return AutoResolution{
			Conflict:      c,
			ResolvedValue: fmt.Appendf(nil, "%d", winner),
			Strategy:      "monotonic_max",
		}, true
	case BucketTombstones:
		// Tombstone is empty value; live entries are non-empty. Empty
		// (delete) wins.
		if len(c.LocalValue) == 0 || len(c.RemoteValue) == 0 {
			return AutoResolution{
				Conflict:      c,
				ResolvedValue: nil,
				Strategy:      "tombstone_wins",
			}, true
		}
		return AutoResolution{}, false
	default:
		return AutoResolution{}, false
	}
}

// AutoResolveBatch runs AutoResolve over a slice of conflicts, splitting
// them into resolved and remaining. Order in both result slices mirrors
// input order — useful for deterministic reporting.
func AutoResolveBatch(conflicts []Conflict, kind BucketKind) (resolved []AutoResolution, remaining []Conflict) {
	for _, c := range conflicts {
		if r, ok := AutoResolve(c, kind); ok {
			resolved = append(resolved, r)
		} else {
			remaining = append(remaining, c)
		}
	}
	return resolved, remaining
}

// parseInt is a small helper for the monotonic-counter rule. Tolerates
// leading/trailing whitespace; refuses anything else.
func parseInt(b []byte) (int64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

