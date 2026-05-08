// Package brain — pathmap.go: cross-host path remapping for restore.
// PILAR XXVI / 135.E.1.
//
// When `neo brain pull` runs on a different machine than the original
// `neo brain push`, the absolute paths inside the manifest no longer
// exist (Mac /path/to/neoanvil vs Linux
// /home/ensamblatec/go/src/...). PathMap captures the operator's
// per-canonical_id remap so pulls can restore to the right place.
//
// Schema lives at ~/.neo/path_map.json:
//
//	{
//	  "version": 1,
//	  "entries": {
//	    "github.com/ensamblatec/neoanvil": {
//	      "path":   "/home/ensamblatec/go/src/github.com/ensamblatec/neoanvil",
//	      "action": "restore"
//	    },
//	    "github.com/ensamblatec/old-thing": {
//	      "action": "skip"
//	    }
//	  }
//	}
//
// Action values:
//
//	"restore" — extract files into the given Path (default when path set)
//	"skip"    — manifest entry ignored on this machine
//	"clone"   — git clone canonical_id (if it parses as a git URL) to Path
//	            then restore on top
//
// The local registry is consulted FIRST; only canonical_ids that don't
// match any registered workspace fall through to PathMap. That keeps
// the common path (one machine, one workspace per repo) zero-config.

package brain

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PathMapVersion is the on-disk schema version. Bump when fields change.
const PathMapVersion = 1

// PathAction enumerates how a pull should handle a workspace whose
// canonical_id maps to this entry.
type PathAction string

const (
	ActionRestore PathAction = "restore" // extract files into Path
	ActionSkip    PathAction = "skip"    // manifest entry ignored
	ActionClone   PathAction = "clone"   // git clone canonical_id, then restore
)

// PathMapEntry is one canonical_id → local path binding.
type PathMapEntry struct {
	Path   string     `json:"path,omitempty"`
	Action PathAction `json:"action,omitempty"`
}

// PathMap is the in-memory representation of ~/.neo/path_map.json.
type PathMap struct {
	Version int                     `json:"version"`
	Entries map[string]PathMapEntry `json:"entries"`
}

// NewPathMap returns an empty PathMap with the current version stamped.
func NewPathMap() *PathMap {
	return &PathMap{Version: PathMapVersion, Entries: map[string]PathMapEntry{}}
}

// DefaultPathMapPath returns ~/.neo/path_map.json (or "" if HOME isn't
// resolvable, which only happens in stripped containers).
func DefaultPathMapPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".neo", "path_map.json")
}

// LoadPathMap reads and parses path. Missing file returns an empty map
// (not an error) so first-time pulls don't need to pre-create the file.
// Schema-version mismatch is a hard error — refuse to operate on data
// produced by a future build.
func LoadPathMap(path string) (*PathMap, error) {
	if path == "" {
		return nil, errors.New("LoadPathMap: empty path")
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304-WORKSPACE-CANON: path is operator-pinned ~/.neo/path_map.json
	if err != nil {
		if os.IsNotExist(err) {
			return NewPathMap(), nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var pm PathMap
	if err := json.Unmarshal(data, &pm); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if pm.Version > PathMapVersion {
		return nil, fmt.Errorf("path_map.json version %d unsupported (this build supports %d) — upgrade neoanvil", pm.Version, PathMapVersion)
	}
	if pm.Entries == nil {
		pm.Entries = map[string]PathMapEntry{}
	}
	if pm.Version == 0 {
		pm.Version = PathMapVersion
	}
	return &pm, nil
}

// Save writes pm atomically to path. Mode 0o600 because path_map may
// reveal the operator's directory layout.
func (pm *PathMap) Save(path string) error {
	if path == "" {
		return errors.New("PathMap.Save: empty path")
	}
	if pm.Version == 0 {
		pm.Version = PathMapVersion
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("PathMap.Save: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(pm, "", "  ")
	if err != nil {
		return fmt.Errorf("PathMap.Save: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("PathMap.Save: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("PathMap.Save: rename: %w", err)
	}
	return nil
}

// Set installs an entry. Empty path with action != skip is rejected
// (skip is the only sensible "no path" outcome).
func (pm *PathMap) Set(canonicalID string, entry PathMapEntry) error {
	if canonicalID == "" {
		return errors.New("PathMap.Set: empty canonical_id")
	}
	if entry.Action == "" {
		// Default: restore to Path. Empty path → skip.
		if entry.Path == "" {
			entry.Action = ActionSkip
		} else {
			entry.Action = ActionRestore
		}
	}
	if entry.Action != ActionSkip && entry.Path == "" {
		return fmt.Errorf("PathMap.Set: action=%q requires a non-empty path", entry.Action)
	}
	if pm.Entries == nil {
		pm.Entries = map[string]PathMapEntry{}
	}
	pm.Entries[canonicalID] = entry
	return nil
}

// Lookup returns the entry for canonical_id and whether it was present.
func (pm *PathMap) Lookup(canonicalID string) (PathMapEntry, bool) {
	if pm == nil || pm.Entries == nil {
		return PathMapEntry{}, false
	}
	e, ok := pm.Entries[canonicalID]
	return e, ok
}

// Resolution describes the destination + action a pull should take for
// one workspace. Source records WHERE the resolution came from so the
// CLI can show the operator a readable trace.
type ResolutionSource string

const (
	ResolutionSourceLocalRegistry ResolutionSource = "local_registry"
	ResolutionSourcePathMap       ResolutionSource = "path_map"
	ResolutionSourceAutoClone     ResolutionSource = "auto_clone"
	ResolutionSourcePromptNeeded  ResolutionSource = "prompt_needed"
)

// PathResolution is the output of ResolveWorkspacePath.
type PathResolution struct {
	CanonicalID string
	Path        string
	Action      PathAction
	Source      ResolutionSource
	Reason      string // human-readable explanation when Action=skip or Source=prompt_needed
}

// ResolveWorkspacePath decides where to restore a workspace whose
// manifest entry has the given canonical_id and original path. Order:
//
//  1. Local registry contains a workspace with this canonical_id →
//     restore there (most common case, zero config).
//  2. PathMap has an entry → use it.
//  3. autoClone=true AND canonical_id parses as git URL AND fallbackRoot
//     is non-empty → propose auto-clone under fallbackRoot.
//  4. Otherwise: report "prompt needed" so the CLI can ask the operator.
func ResolveWorkspacePath(canonicalID, originalPath string, registryHits map[string]string, pm *PathMap, autoClone bool, fallbackRoot string) PathResolution {
	res := PathResolution{CanonicalID: canonicalID}
	if path, ok := registryHits[canonicalID]; ok {
		res.Path = path
		res.Action = ActionRestore
		res.Source = ResolutionSourceLocalRegistry
		return res
	}
	if e, ok := pm.Lookup(canonicalID); ok {
		res.Path = e.Path
		res.Action = e.Action
		res.Source = ResolutionSourcePathMap
		if e.Action == ActionSkip {
			res.Reason = "path_map says skip"
		}
		return res
	}
	if autoClone && fallbackRoot != "" && canonicalLooksGitClonable(canonicalID) {
		res.Path = filepath.Join(fallbackRoot, filepath.FromSlash(canonicalID))
		res.Action = ActionClone
		res.Source = ResolutionSourceAutoClone
		return res
	}
	res.Source = ResolutionSourcePromptNeeded
	res.Reason = fmt.Sprintf("canonical_id %q has no local registry entry, no path_map binding, and is not auto-clonable; original path was %q",
		canonicalID, originalPath)
	res.Action = ActionSkip
	return res
}

// canonicalLooksGitClonable returns true when canonical_id has the shape
// "<host>/<owner>/<repo>" produced by ResolveCanonicalID's git remote
// rule. Simple heuristic: contains exactly two slashes and starts with
// a hostname-ish segment.
func canonicalLooksGitClonable(id string) bool {
	if strings.HasPrefix(id, "local:") || strings.HasPrefix(id, "project:") {
		return false
	}
	parts := strings.Split(id, "/")
	if len(parts) < 3 {
		return false
	}
	// First segment must contain a "." (host) — rejects bare names.
	return strings.Contains(parts[0], ".")
}

// BuildRegistryHits inverts a list of (canonical_id, path) pairs into a
// map for fast Lookup during ResolveWorkspacePath. Callers who already
// have a Walked* slice in hand should use this rather than rebuilding
// a map themselves.
func BuildRegistryHits(walked []WalkedWorkspace) map[string]string {
	hits := make(map[string]string, len(walked))
	for _, w := range walked {
		if w.CanonicalID != "" && w.Path != "" {
			hits[w.CanonicalID] = w.Path
		}
	}
	return hits
}
