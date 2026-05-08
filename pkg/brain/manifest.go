// Package brain — manifest.go: snapshot manifest schema. PILAR XXVI / 135.A.4.
//
// A Manifest is the metadata blob that describes one brain snapshot:
// which workspaces, projects, and orgs it contains, where they came from,
// and at what logical time. Snapshots are content-addressable archives
// (135.A.5 BuildArchive) — the manifest is the index that lets the
// receiver decide what to restore where.
//
// Schema is versioned via SnapshotVersion; receivers reject blobs whose
// SnapshotVersion exceeds the version they were built for. The current
// version is 1 — the very first iteration. Future bumps document
// migration paths in `neo brain migrate <old-version>`.
//
// HLC (Hybrid Logical Clock): WallMS is the wall-clock time in
// milliseconds; LogicalCounter ticks within the same millisecond to
// disambiguate snapshots taken too close together for wall time to
// distinguish them. Compare with CompareHLC.
//
// NodeID is derived per-machine from hostname + best-effort machine-id
// fingerprint. Two pushes from the same machine carry the same NodeID,
// so the receiver can detect "this is just my own snapshot reflected
// back" and short-circuit no-op restores.

package brain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// CurrentSnapshotVersion advertises the schema version this build of
// neoanvil produces and accepts. Receivers MUST reject manifests with
// SnapshotVersion > this value (forward-incompatible) and SHOULD reject
// SnapshotVersion < 1 (corrupt). Backward-compatible additions inside
// the same version (new optional fields with default values) are
// permitted without a bump.
//
// When v2 ships:
//   - bump CurrentSnapshotVersion to 2 in the producing build first
//   - older clients reading a v2 manifest hit Validate → clear error
//     ("snapshot v2 requires neo ≥ X.Y, run `neo brain migrate v1`")
//   - implement `neo brain migrate <old-version>` to translate v1 → v2
//     manifests offline (the encrypted archive bytes are
//     forward-compatible — only the manifest changes shape)
//
// Edge case: a workspace where `git config remote.origin.url` is empty
// (freshly `git init`-ed local repo, no remote configured) still
// resolves canonically — ResolveCanonicalID falls through to
// `local:<sha256-prefix(absolute path)>` which always succeeds.
// See pkg/brain/identity.go.
const CurrentSnapshotVersion = 1

// Manifest is the top-level snapshot index. JSON-serializable. Persisted
// alongside the encrypted archive (135.B) and read first by the receiver
// to verify version + HLC + NodeID before decrypting the rest.
type Manifest struct {
	// SnapshotVersion is the schema version (see CurrentSnapshotVersion).
	SnapshotVersion int `json:"snapshot_version"`

	// HLC tags this snapshot at a logical instant. Strictly monotonic
	// across snapshots produced on the same node.
	HLC HLC `json:"hlc"`

	// NodeID is the stable per-machine identifier. See NodeFingerprint.
	NodeID string `json:"node_id"`

	// CreatedAt is the wall-clock timestamp the manifest was built —
	// human-readable counterpart to HLC.WallMS.
	CreatedAt time.Time `json:"created_at"`

	// Workspaces enumerates every workspace included in the snapshot.
	// Order matches the WalkWorkspaces output so manifest hashes stay
	// reproducible.
	Workspaces []WorkspaceManifest `json:"workspaces"`

	// Projects lists every .neo-project federation root referenced by
	// at least one workspace. Members holds workspace IDs.
	Projects []ProjectManifest `json:"projects,omitempty"`

	// Orgs lists every .neo-org root.
	Orgs []OrgManifest `json:"orgs,omitempty"`

	// Globals lists ~/.neo files that should travel with the snapshot
	// (workspaces.json, credentials.json, plugins.yaml, contexts.json).
	// Empty when the operator opted to push only workspaces.
	Globals []GlobalEntry `json:"globals,omitempty"`

	// MergedFrom is non-empty when this manifest is the result of
	// `neo brain merge` — lists the HLCs of the parent snapshots so the
	// linage forms a DAG. Drives 136.* merge sync.
	MergedFrom []HLC `json:"merged_from,omitempty"`
}

// WorkspaceManifest is one workspace's slot in the manifest. Files lists
// repo-relative paths included in the archive; absolute paths are derived
// at restore time by joining with the receiver's destination root.
type WorkspaceManifest struct {
	ID              string   `json:"id"`                           // local registry ID at origin (e.g. neoanvil-95248)
	LocalIDAtOrigin string   `json:"local_id_at_origin,omitempty"` // alias for ID, kept for forward-compat clarity
	Path            string   `json:"path"`                         // absolute path at origin (informational)
	Name            string   `json:"name"`
	DominantLang    string   `json:"dominant_lang,omitempty"`
	Type            string   `json:"type,omitempty"` // "workspace" | "project"
	CanonicalID     string   `json:"canonical_id"`   // resolved by 135.A.1
	Files           []string `json:"files"`          // repo-relative paths in the archive
}

// ProjectManifest mirrors WalkedProject for serialization.
type ProjectManifest struct {
	Path        string   `json:"path"`
	CanonicalID string   `json:"canonical_id"`
	Members     []string `json:"members"` // workspace IDs whose walk-up landed here
	Files       []string `json:"files,omitempty"`
}

// OrgManifest mirrors WalkedOrg for serialization.
type OrgManifest struct {
	Path        string   `json:"path"`
	CanonicalID string   `json:"canonical_id"`
	Members     []string `json:"members"`
	Files       []string `json:"files,omitempty"`
}

// GlobalEntry is one ~/.neo file. RelPath is relative to ~/.neo so the
// receiver can restore it under their own home dir without rewriting.
type GlobalEntry struct {
	RelPath string `json:"rel_path"`
	Mode    uint32 `json:"mode"` // file permissions (0o600 sensitive, 0o644 docs)
}

// HLC is a hybrid logical clock value. WallMS is unix milliseconds at
// the moment of issue; LogicalCounter ticks for ties (HLCSource called
// twice in the same millisecond on the same node). Total ordering: lex
// (WallMS, LogicalCounter).
type HLC struct {
	WallMS         int64 `json:"wall_ms"`
	LogicalCounter int64 `json:"logical_counter"`
}

// IsZero is true for the zero HLC. Used by callers to detect "no parent"
// in MergedFrom and similar.
func (h HLC) IsZero() bool { return h.WallMS == 0 && h.LogicalCounter == 0 }

// String returns a stable human-readable form: "<wall_ms>.<counter>".
func (h HLC) String() string {
	return fmt.Sprintf("%d.%d", h.WallMS, h.LogicalCounter)
}

// CompareHLC returns -1/0/+1 when a is before/equal/after b. Lexicographic
// over (WallMS, LogicalCounter). HLCs from different nodes are still
// comparable in this scheme — the receiver may produce a higher HLC than
// the sender if its clock is ahead, which is desirable: snapshot order
// follows wall-clock perception with tie-breaking by counter.
func CompareHLC(a, b HLC) int {
	switch {
	case a.WallMS < b.WallMS:
		return -1
	case a.WallMS > b.WallMS:
		return 1
	case a.LogicalCounter < b.LogicalCounter:
		return -1
	case a.LogicalCounter > b.LogicalCounter:
		return 1
	}
	return 0
}

// hlcSource is the local HLC issuer. Goroutine-safe via atomic counter.
// Reset on process restart — that's intentional, the wall-clock component
// guarantees ordering across restarts.
type hlcSource struct {
	lastWallMS atomic.Int64
	counter    atomic.Int64
}

var defaultHLCSource hlcSource

// reNodeID validates the NodeID format produced by NodeFingerprint:
// "node:" followed by exactly 16 lowercase hex characters.
// [146.I] Rejects malformed or truncated IDs received from untrusted manifests.
var reNodeID = regexp.MustCompile(`^node:[a-f0-9]{16}$`)

// NextHLC issues a fresh HLC value strictly greater than every prior
// NextHLC return on this process. Safe to call concurrently.
//
// The algorithm:
//
//  1. Read the current wall-clock in milliseconds.
//  2. If wall > last → store wall, reset counter to 0, return (wall, 0).
//  3. Else (clock didn't advance, or went backward) → increment counter,
//     return (last_wall, counter).
//
// Step 3 is the safety net for clocks that don't advance between rapid
// calls (common at high frequency).
func NextHLC() HLC {
	now := time.Now().UnixMilli()
	for {
		last := defaultHLCSource.lastWallMS.Load()
		if now > last {
			if defaultHLCSource.lastWallMS.CompareAndSwap(last, now) {
				defaultHLCSource.counter.Store(0)
				return HLC{WallMS: now, LogicalCounter: 0}
			}
			// Lost the CAS — retry; another goroutine just advanced.
			continue
		}
		// Wall didn't advance; bump the counter.
		c := defaultHLCSource.counter.Add(1)
		// Re-read last in case it advanced between Load() and Add().
		last = defaultHLCSource.lastWallMS.Load()
		return HLC{WallMS: last, LogicalCounter: c}
	}
}

// NodeFingerprint returns a stable per-machine identifier suitable for
// Manifest.NodeID. Format: "node:<16 hex chars of sha256(host || machine-id)>".
//
// Sources tried in order, first hit wins:
//  1. /etc/machine-id            (Linux systemd, persistent across boots)
//  2. /var/lib/dbus/machine-id   (older Linux fallback)
//  3. hostname                    (always available)
//
// On error or empty input, returns "node:unknown" so callers always have
// SOMETHING to write.
func NodeFingerprint() string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if data, err := os.ReadFile(p); err == nil && len(data) > 0 { //nolint:gosec // G304-WORKSPACE-CANON: hardcoded literal paths under /etc and /var/lib/dbus
			if id := fingerprintFrom(string(data)); id != "" {
				return id
			}
		}
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "node:unknown"
	}
	return fingerprintFrom(host)
}

func fingerprintFrom(seed string) string {
	if seed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(seed))
	return "node:" + hex.EncodeToString(sum[:8])
}

// NewManifest builds a fresh Manifest from a walk result. The HLC is
// allocated via NextHLC, so two NewManifest calls on the same process
// always produce strictly ordered manifests.
//
// workspaces / projects / orgs may be nil; the manifest carries empty
// slices in that case. Globals must be filled by the caller (snapshot.go
// doesn't know which ~/.neo files the operator wants to bundle).
func NewManifest(workspaces []WalkedWorkspace, projects []WalkedProject, orgs []WalkedOrg) *Manifest {
	now := time.Now().UTC()
	m := &Manifest{
		SnapshotVersion: CurrentSnapshotVersion,
		HLC:             NextHLC(),
		NodeID:          NodeFingerprint(),
		CreatedAt:       now,
	}
	for _, w := range workspaces {
		m.Workspaces = append(m.Workspaces, WorkspaceManifest{
			ID:              w.ID,
			LocalIDAtOrigin: w.ID,
			Path:            w.Path,
			Name:            w.Name,
			DominantLang:    w.DominantLang,
			Type:            w.Type,
			CanonicalID:     w.CanonicalID,
		})
	}
	for _, p := range projects {
		m.Projects = append(m.Projects, ProjectManifest{
			Path:        p.Path,
			CanonicalID: p.CanonicalID,
			Members:     append([]string(nil), p.Members...),
		})
	}
	for _, o := range orgs {
		m.Orgs = append(m.Orgs, OrgManifest{
			Path:        o.Path,
			CanonicalID: o.CanonicalID,
			Members:     append([]string(nil), o.Members...),
		})
	}
	return m
}

// Validate checks the manifest's structural invariants. Returns an
// aggregated error describing every violation; nil when the manifest is
// well-formed. Callers receiving a manifest from disk MUST run Validate
// before trusting any field. Producers can call it as a self-check.
func validateWorkspaceManifests(ws []WorkspaceManifest) []string {
	var errs []string
	for i, w := range ws {
		if w.ID == "" {
			errs = append(errs, fmt.Sprintf("workspaces[%d].id empty", i))
		}
		if w.CanonicalID == "" {
			errs = append(errs, fmt.Sprintf("workspaces[%d].canonical_id empty", i))
		}
	}
	return errs
}

func validateProjectManifests(ps []ProjectManifest) []string {
	var errs []string
	for i, p := range ps {
		if p.CanonicalID == "" {
			errs = append(errs, fmt.Sprintf("projects[%d].canonical_id empty", i))
		}
	}
	return errs
}

func validateOrgManifests(os []OrgManifest) []string {
	var errs []string
	for i, o := range os {
		if o.CanonicalID == "" {
			errs = append(errs, fmt.Sprintf("orgs[%d].canonical_id empty", i))
		}
	}
	return errs
}

func (m *Manifest) Validate() error {
	if m == nil {
		return errors.New("manifest is nil")
	}
	var errs []string
	if m.SnapshotVersion < 1 {
		errs = append(errs, fmt.Sprintf("snapshot_version=%d (must be ≥1)", m.SnapshotVersion))
	}
	if m.SnapshotVersion > CurrentSnapshotVersion {
		errs = append(errs, fmt.Sprintf("snapshot v%d requires a newer neoanvil (this build supports up to v%d) — run `neo brain migrate v%d` after upgrading", m.SnapshotVersion, CurrentSnapshotVersion, CurrentSnapshotVersion))
	}
	if m.NodeID == "" {
		errs = append(errs, "node_id empty")
	} else if !reNodeID.MatchString(m.NodeID) {
		// [146.I] Reject malformed NodeIDs — guards against injection via crafted manifests.
		errs = append(errs, fmt.Sprintf("node_id %q does not match expected format node:<16 hex chars>", m.NodeID))
	}
	if m.HLC.WallMS == 0 && m.HLC.LogicalCounter == 0 {
		errs = append(errs, "hlc is zero — every manifest must carry an HLC")
	}
	errs = append(errs, validateWorkspaceManifests(m.Workspaces)...)
	errs = append(errs, validateProjectManifests(m.Projects)...)
	errs = append(errs, validateOrgManifests(m.Orgs)...)
	if len(errs) > 0 {
		return fmt.Errorf("manifest invalid: %d issue(s):\n  - %s", len(errs), joinLines(errs))
	}
	return nil
}

func joinLines(lines []string) string {
	var out strings.Builder
	for i, l := range lines {
		if i > 0 {
			out.WriteString("\n  - ")
		}
		out.WriteString(l)
	}
	return out.String()
}
