// Package brain — snapshot.go discovers workspaces + projects + orgs that
// belong in a brain push/pull manifest. PILAR XXVI / 135.A.2-A.3.
//
// WalkWorkspaces  — iterates ~/.neo/workspaces.json, drops entries whose
//                   filesystem path no longer exists on this machine,
//                   resolves canonical_id for each surviving entry.
// WalkDependencies — for each workspace, walks up to discover .neo-project
//                    and .neo-org parents. Dedups so one project / org is
//                    returned regardless of how many member workspaces
//                    reference it.
//
// The function returns Walked* DTOs (not pointers to registry/config types)
// so the snapshot/manifest layer is decoupled from the on-disk schema —
// future schema migrations don't ripple into manifest serialization.

package brain

import (
	"os"
	"path/filepath"

	"github.com/ensamblatec/neoanvil/pkg/workspace"
)

// WalkedWorkspace is one workspace entry as it will appear in the snapshot
// manifest. Mirrors the operational fields from workspace.WorkspaceEntry
// plus the resolved CanonicalID + its source rule (for diagnostics).
type WalkedWorkspace struct {
	ID                string
	Path              string
	Name              string
	DominantLang      string
	Type              string // "workspace" | "project"
	CanonicalID       string
	CanonicalIDSource CanonicalSource
}

// WalkedProject is a project federation root referenced by one or more
// member workspaces. Path is the directory containing .neo-project/.
type WalkedProject struct {
	Path        string // absolute path of the project root (parent of .neo-project)
	CanonicalID string // project:<project_name>:_root or local:<hash> fallback
	Members     []string
}

// WalkedOrg is an org federation root referenced by one or more projects.
// Same shape as WalkedProject; .neo-org/ instead of .neo-project/.
type WalkedOrg struct {
	Path        string
	CanonicalID string
	Members     []string
}

// WalkWorkspaces returns every workspace in the registry whose path still
// exists on disk. Stale entries (path was deleted, machine moved, etc.)
// are silently dropped — they should not appear in a brain manifest
// because there's nothing to snapshot.
//
// reg must not be nil. When reg is empty, returns an empty slice.
//
// CanonicalID is resolved per-workspace via ResolveCanonicalID; when the
// resolution itself fails (extremely rare — fallback always succeeds) the
// entry is included with whatever ID was produced rather than dropped.
func WalkWorkspaces(reg *workspace.Registry) []WalkedWorkspace {
	if reg == nil || len(reg.Workspaces) == 0 {
		return nil
	}
	out := make([]WalkedWorkspace, 0, len(reg.Workspaces))
	for _, e := range reg.Workspaces {
		if !pathExists(e.Path) {
			continue
		}
		canon := ResolveCanonicalID(e.Path)
		out = append(out, WalkedWorkspace{
			ID:                e.ID,
			Path:              e.Path,
			Name:              e.Name,
			DominantLang:      e.DominantLang,
			Type:              e.Type,
			CanonicalID:       canon.ID,
			CanonicalIDSource: canon.Source,
		})
	}
	return out
}

// WalkDependencies walks up from each workspace to find the
// .neo-project/ and .neo-org/ parents that own it. Same parent referenced
// by multiple workspaces appears once in the returned slice (dedup by
// path). Order is deterministic — projects sorted by Path, orgs by Path,
// to make manifest hashes reproducible.
//
// Members lists the workspace IDs whose walk-up landed on that parent.
func WalkDependencies(workspaces []WalkedWorkspace) (projects []WalkedProject, orgs []WalkedOrg) {
	// Map by absolute parent path → aggregated member IDs. Two workspaces
	// pointing at the same project share one entry.
	projectMap := map[string]*WalkedProject{}
	orgMap := map[string]*WalkedOrg{}

	for _, ws := range workspaces {
		if root := walkUpFor(ws.Path, ".neo-project"); root != "" {
			p, ok := projectMap[root]
			if !ok {
				p = &WalkedProject{Path: root, CanonicalID: projectCanonical(root)}
				projectMap[root] = p
			}
			p.Members = append(p.Members, ws.ID)
		}
		if root := walkUpFor(ws.Path, ".neo-org"); root != "" {
			o, ok := orgMap[root]
			if !ok {
				o = &WalkedOrg{Path: root, CanonicalID: orgCanonical(root)}
				orgMap[root] = o
			}
			o.Members = append(o.Members, ws.ID)
		}
	}

	for _, p := range projectMap {
		projects = append(projects, *p)
	}
	for _, o := range orgMap {
		orgs = append(orgs, *o)
	}
	sortByPath(projects, func(p WalkedProject) string { return p.Path })
	sortByPath(orgs, func(o WalkedOrg) string { return o.Path })
	return projects, orgs
}

// projectCanonical returns the canonical_id for a .neo-project root path.
// Convention: "project:<project_name>:_root" so it differs from any
// member-workspace canonical_id ("project:<project_name>:<basename>").
// Falls back to "local:<hash>" when the project_name cannot be read.
func projectCanonical(root string) string {
	id := readProjectName(filepath.Join(root, "_synthetic_member"))
	if id == "" {
		return pathHash(root)
	}
	// readProjectName returns "project:<name>:<basename>" — replace the
	// basename with "_root" to denote the project itself.
	return replaceLastSegment(id, "_root")
}

// orgCanonical mirrors projectCanonical but for .neo-org/ roots. There is
// no walk-up reader for org_name today (PILAR LXVII implements it
// elsewhere); fall back to the path hash.
func orgCanonical(root string) string {
	return pathHash(root)
}

// replaceLastSegment swaps the trailing ":<x>" of an id with ":<seg>".
// Used to convert "project:foo:backend" into "project:foo:_root".
func replaceLastSegment(id, seg string) string {
	last := -1
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == ':' {
			last = i
			break
		}
	}
	if last < 0 {
		return id + ":" + seg
	}
	return id[:last+1] + seg
}

// pathExists is true when stat succeeds, irrespective of file type.
// Errors other than ErrNotExist are treated as "exists, just unreadable"
// — the manifest layer should still see the workspace and surface the
// error there if relevant.
func pathExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	if err == nil {
		return true
	}
	return !os.IsNotExist(err)
}

// sortByPath orders a slice in-place by the result of a key function.
// Avoids importing sort everywhere — local helper for snapshot.go only.
func sortByPath[T any](s []T, key func(T) string) {
	// Insertion sort — N is the number of projects/orgs which is small
	// (typically 1-5), so a quadratic scan beats package sort for setup.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && key(s[j]) < key(s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
