// Package merge — orchestrate.go: high-level merge operations.
// PILAR XXVI / 136.C + 136.D.2/D.3.
//
// Orchestrate composes the diff + detect + resolve pipeline behind a
// strategy chooser:
//
//   StrategyAutoOnly   — fail when any non-trivial conflict remains
//                         after AutoResolveBatch (CI/scripted use)
//   StrategyTakeLocal  — every conflict resolves to LocalValue
//                         (operator override; manual fallback)
//   StrategyTakeRemote — every conflict resolves to RemoteValue
//   StrategyInteractive — caller-supplied resolver function decides
//                         per-conflict (powers `neo brain merge` CLI)
//
// The package itself does NO terminal I/O; the CLI in cmd/neo/brain.go
// supplies the InteractiveResolver. Keeps the tests pure.

package merge

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/ensamblatec/neoanvil/pkg/brain/diff"
)

// Strategy is how Orchestrate handles non-auto conflicts.
type Strategy string

const (
	StrategyAutoOnly    Strategy = "auto-only"
	StrategyTakeLocal   Strategy = "local"
	StrategyTakeRemote  Strategy = "remote"
	StrategyInteractive Strategy = "interactive"
)

// InteractiveResolver returns the operator's choice for one conflict.
// Implementations may return a fresh ResolvedValue (editor-merged) or
// pick LocalValue / RemoteValue by reference. Returning an error
// aborts the merge.
type InteractiveResolver func(c Conflict) (ResolvedValue []byte, err error)

// MergeStats captures the resolution counts the operator gets at the
// end of `neo brain merge`. 136.D.3.
type MergeStats struct {
	AutoResolved      int // resolved by AutoResolve
	InteractivePicked int // resolved by InteractiveResolver
	StrategyForced    int // local/remote/etc. blanket choice
	Aborted           int // operator returned a sentinel mid-flow
}

// MergedKey is one key after the strategy chose. Both Bucket and Key
// are required so callers can route the result back to the right
// destination at write time.
type MergedKey struct {
	Bucket string
	Key    string
	Value  []byte
}

// OrchestrateInput captures one bucket's worth of conflicts plus the
// kind hint AutoResolve needs.
// [146.O] Callers may optionally supply LocalHLC and RemoteHLC so that
// Orchestrate can compute a strictly-monotonic MergedHLC for the result.
// Zero values are safe — MergedHLC will be zero when inputs are not supplied.
type OrchestrateInput struct {
	Bucket    string
	Kind      BucketKind
	Conflicts []Conflict
	LocalHLC  MergeHLC // optional: HLC of the local snapshot
	RemoteHLC MergeHLC // optional: HLC of the remote snapshot
}

// MergeHLC is a simplified Hybrid Logical Clock value for merge operations.
// Mirrors brain.HLC without creating an import dependency between subpackages.
// [146.O]
type MergeHLC struct {
	WallMS         int64
	LogicalCounter int64
}

// OrchestrateResult is the merged output for one bucket.
type OrchestrateResult struct {
	Bucket     string
	Merged     []MergedKey
	Stats      MergeStats
	MergedHLC  MergeHLC // [146.O] max(local, remote)+1; zero when inputs not supplied
	Aborted    bool
}

// Orchestrate runs the full pipeline for one bucket. The interactive
// strategy requires resolver != nil; the other strategies don't read it.
//
// Order of operations:
//
//   1. AutoResolveBatch over the input conflicts.
//   2. For remaining (non-auto) conflicts, dispatch by strategy:
//        StrategyAutoOnly   → return error
//        StrategyTakeLocal  → emit LocalValue
//        StrategyTakeRemote → emit RemoteValue
//        StrategyInteractive → call resolver per conflict
//   3. Combine into Merged slice + Stats.
func Orchestrate(in OrchestrateInput, strategy Strategy, resolver InteractiveResolver) (OrchestrateResult, error) {
	out := OrchestrateResult{Bucket: in.Bucket}
	// [146.O] Compute strictly-monotonic merged HLC from the two input sides.
	// When inputs are zero (callers that don't set LocalHLC/RemoteHLC), the
	// result stays zero — backward compatible with existing callers.
	out.MergedHLC = mergedHLC(in.LocalHLC, in.RemoteHLC)

	autoResolved, remaining := AutoResolveBatch(in.Conflicts, in.Kind)
	out.Stats.AutoResolved = len(autoResolved)
	for _, ar := range autoResolved {
		out.Merged = append(out.Merged, MergedKey{
			Bucket: in.Bucket,
			Key:    ar.Conflict.Key,
			Value:  ar.ResolvedValue,
		})
	}

	if len(remaining) == 0 {
		return out, nil
	}

	switch strategy {
	case StrategyAutoOnly:
		return out, fmt.Errorf("orchestrate: %d non-trivial conflict(s) in bucket %q (use --strategy=interactive|local|remote to resolve)", len(remaining), in.Bucket)
	case StrategyTakeLocal:
		for _, c := range remaining {
			out.Merged = append(out.Merged, MergedKey{Bucket: c.Bucket, Key: c.Key, Value: bytes.Clone(c.LocalValue)})
			out.Stats.StrategyForced++
		}
	case StrategyTakeRemote:
		for _, c := range remaining {
			out.Merged = append(out.Merged, MergedKey{Bucket: c.Bucket, Key: c.Key, Value: bytes.Clone(c.RemoteValue)})
			out.Stats.StrategyForced++
		}
	case StrategyInteractive:
		if resolver == nil {
			return out, errors.New("orchestrate: interactive strategy requires a non-nil resolver")
		}
		for _, c := range remaining {
			val, err := resolver(c)
			if err != nil {
				out.Aborted = true
				out.Stats.Aborted++
				return out, fmt.Errorf("interactive resolver for %s/%s: %w", c.Bucket, c.Key, err)
			}
			out.Merged = append(out.Merged, MergedKey{Bucket: c.Bucket, Key: c.Key, Value: bytes.Clone(val)})
			out.Stats.InteractivePicked++
		}
	default:
		return out, fmt.Errorf("orchestrate: unknown strategy %q", strategy)
	}
	return out, nil
}

// mergedHLC computes a strictly-monotonic HLC from two input sides.
// When walls differ, the winner is the side with the higher wall time;
// when equal, the logical counter is max+1 to advance past both sides.
// [146.O]
func mergedHLC(local, remote MergeHLC) MergeHLC {
	if local.WallMS == 0 && local.LogicalCounter == 0 && remote.WallMS == 0 && remote.LogicalCounter == 0 {
		return MergeHLC{} // no HLC supplied — stay zero
	}
	if local.WallMS > remote.WallMS {
		return MergeHLC{WallMS: local.WallMS, LogicalCounter: local.LogicalCounter + 1}
	}
	if remote.WallMS > local.WallMS {
		return MergeHLC{WallMS: remote.WallMS, LogicalCounter: remote.LogicalCounter + 1}
	}
	// Walls equal — advance logical counter past both.
	return MergeHLC{WallMS: local.WallMS, LogicalCounter: max(local.LogicalCounter, remote.LogicalCounter) + 1}
}

// FormatStats renders MergeStats in one human-readable line. 136.D.3.
func FormatStats(s MergeStats) string {
	return fmt.Sprintf("merged %d auto, %d interactive, %d strategy-forced, %d aborted",
		s.AutoResolved, s.InteractivePicked, s.StrategyForced, s.Aborted)
}

// AddStats sums b into a in place. Useful when orchestrating across
// multiple buckets to produce one final report.
func AddStats(a *MergeStats, b MergeStats) {
	a.AutoResolved += b.AutoResolved
	a.InteractivePicked += b.InteractivePicked
	a.StrategyForced += b.StrategyForced
	a.Aborted += b.Aborted
}

// CommitMerge writes the merged keys from all results via a caller-supplied
// commit function. The caller MUST wrap the function in a single database
// transaction so that either all keys land or none do (atomicity). [146.P]
//
// Typical usage with bbolt:
//
//	db.Update(func(tx *bbolt.Tx) error {
//	    return merge.CommitMerge(results, func(bucket, key string, value []byte) error {
//	        b, err := tx.CreateBucketIfNotExists([]byte(bucket))
//	        if err != nil { return err }
//	        return b.Put([]byte(key), value)
//	    })
//	})
func CommitMerge(results []OrchestrateResult, commit func(bucket, key string, value []byte) error) error {
	for _, r := range results {
		if r.Aborted {
			return fmt.Errorf("commit aborted: bucket %q had aborted resolution", r.Bucket)
		}
		for _, mk := range r.Merged {
			if err := commit(mk.Bucket, mk.Key, mk.Value); err != nil {
				return fmt.Errorf("commit %s/%s: %w", mk.Bucket, mk.Key, err)
			}
		}
	}
	return nil
}

// _ = diff is here to anchor the package's import block while the
// only diff usage is inside detect.go's classifyPair signature.
var _ = diff.ChangeUnchanged
