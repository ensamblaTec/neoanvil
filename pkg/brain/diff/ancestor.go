// Package diff implements three-way diffing for brain snapshots.
// PILAR XXVI / 136.A.
//
// FindCommonAncestor walks the HLC lineage of two manifests (local
// node's history vs remote's history) and returns the most recent HLC
// they share. Without this, merge sync (136.B-D) can't compute the
// "what changed since common base" sets that drive conflict detection.
//
// The lineage is encoded in Manifest.MergedFrom — every merged manifest
// carries the HLCs of its parents. A non-merge snapshot has empty
// MergedFrom and contributes only its own HLC to the lineage.

package diff

import (
	"github.com/ensamblatec/neoanvil/pkg/brain"
)

// LineageProvider returns the HLC ancestry of one machine's snapshot
// history. Implementations typically wrap a BrainStore + walk every
// manifest's MergedFrom field, but tests pass a hand-built map.
type LineageProvider interface {
	// Lineage returns the set of HLCs that appear anywhere in this
	// machine's snapshot history (including the manifest itself).
	// Order is irrelevant — the caller intersects.
	Lineage() ([]brain.HLC, error)
}

// FindCommonAncestor returns the most recent HLC that appears in both
// lineages. Returns ok=false when the two lineages share no HLC — that
// happens when two machines pushed for the first time without a shared
// origin (a "fresh fork" scenario the caller handles by treating the
// remote as a full overlay).
//
// "Most recent" is the highest HLC under CompareHLC ordering — same
// rule the storage layer uses for "latest".
func FindCommonAncestor(local, remote LineageProvider) (brain.HLC, bool, error) {
	if local == nil || remote == nil {
		return brain.HLC{}, false, nil
	}
	localHLCs, err := local.Lineage()
	if err != nil {
		return brain.HLC{}, false, err
	}
	remoteHLCs, err := remote.Lineage()
	if err != nil {
		return brain.HLC{}, false, err
	}
	remoteSet := make(map[string]brain.HLC, len(remoteHLCs))
	for _, h := range remoteHLCs {
		remoteSet[h.String()] = h
	}
	var best brain.HLC
	found := false
	for _, h := range localHLCs {
		if _, ok := remoteSet[h.String()]; !ok {
			continue
		}
		if !found || brain.CompareHLC(h, best) > 0 {
			best = h
			found = true
		}
	}
	return best, found, nil
}

// StaticLineage is a LineageProvider backed by a precomputed slice.
// Useful for tests and for code that already collected the lineage out
// of band. Implements LineageProvider.
type StaticLineage struct {
	HLCs []brain.HLC
}

// Lineage returns the underlying slice (a copy isn't necessary — the
// LineageProvider contract treats it as read-only).
func (s StaticLineage) Lineage() ([]brain.HLC, error) {
	return s.HLCs, nil
}

// LineageFromManifest builds a StaticLineage that includes the
// manifest's own HLC plus every HLC in its MergedFrom slice. Useful
// when the only ancestry signal available is a single manifest (no
// store walk yet).
func LineageFromManifest(m *brain.Manifest) StaticLineage {
	if m == nil {
		return StaticLineage{}
	}
	hs := make([]brain.HLC, 0, 1+len(m.MergedFrom))
	hs = append(hs, m.HLC)
	hs = append(hs, m.MergedFrom...)
	return StaticLineage{HLCs: hs}
}
