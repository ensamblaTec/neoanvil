package knowledge

// org_store.go — Org-tier KnowledgeStore coordinator. [PILAR LXVII / 354.B]
//
// Mirrors the project-tier pattern (see ProjectConfig.CoordinatorWorkspace):
// one project per org opens `.neo-org/db/org.db` in read-write mode, all
// other projects proxy writes via the coordinator. MVP returns nil for
// non-coordinators — the proxy routing will be added in a follow-up commit
// on top of the Nexus shared HTTP pattern.

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
)

// ErrOrgStoreReadOnly is returned when a non-coordinator project tries to
// open the org store. In MVP non-coordinators should treat the org tier as
// read-only-via-proxy; once the proxy wiring exists they'll get a thin
// client instead of this error. [354.B]
var ErrOrgStoreReadOnly = errors.New("knowledge: org store is read-only from non-coordinator projects")

// OrgStoreConfig bundles the parameters OpenOrgStore needs to decide ownership
// without dragging the entire config package into pkg/knowledge.
type OrgStoreConfig struct {
	OrgDir                 string // absolute path to `.neo-org/`
	ProjectRoot            string // the current project's root (parent of `.neo-project/`)
	CoordinatorProject     string // OrgConfig.CoordinatorProject value
	DBPathOverride         string // optional explicit DB path; default = OrgDir/db/org.db
	IsStandaloneWorkspace  bool   // true when caller has no project (→ always no-op)
}

// OpenOrgStore returns a KnowledgeStore opened in RW mode iff the calling
// project is the designated coordinator. Non-coordinators return
// (nil, ErrOrgStoreReadOnly) — callers should route tier:"org" writes
// via Nexus to the coordinator once that wiring lands.
//
// IsStandaloneWorkspace short-circuits to (nil, nil) so a workspace without
// `.neo-project/` doesn't even attempt to participate in the org tier. [354.B]
func OpenOrgStore(cfg OrgStoreConfig) (*KnowledgeStore, error) {
	if cfg.IsStandaloneWorkspace || cfg.OrgDir == "" {
		return nil, nil
	}
	if !isCoordinatorProject(cfg.ProjectRoot, cfg.CoordinatorProject) {
		return nil, ErrOrgStoreReadOnly
	}
	path := cfg.DBPathOverride
	if path == "" {
		path = filepath.Join(cfg.OrgDir, "db", "org.db")
	}
	ks, err := Open(path)
	if err != nil {
		return nil, fmt.Errorf("org_store: open %s: %w", path, err)
	}
	log.Printf("[ORG-COORD] opened %s as coordinator (project=%s)", path, filepath.Base(cfg.ProjectRoot))
	return ks, nil
}

// isCoordinatorProject is the package-local mirror of config.IsCoordinatorProject
// — duplicated to avoid a circular import. Keep logic identical. [354.B]
func isCoordinatorProject(projectRoot, coordinator string) bool {
	if coordinator == "" {
		// Legacy mode: every project claims coordinator. Race-prone but backwards
		// compatible until an operator sets the field.
		return true
	}
	return projectRoot == coordinator ||
		filepath.Base(projectRoot) == coordinator ||
		filepath.Base(projectRoot) == filepath.Base(coordinator)
}
