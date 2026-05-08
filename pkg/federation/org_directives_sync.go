package federation

// org_directives_sync.go — Mirror `.neo-org/knowledge/directives/*.md`
// into each workspace's `.claude/rules/` with an `org-` prefix so the
// Claude Code rule loader picks them up alongside workspace-local rules.
// [PILAR LXVII / 355.B]
//
// Mechanics: idempotent copy — skips when destination content matches source.
// Overwrite semantics on drift: source wins (org directives are authoritative).
// Files removed from the org source are NOT auto-deleted from workspaces to
// avoid surprises; operator can prune via `neo doctor` (follow-up).

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const orgDirectivesSubdir = "knowledge/directives"
const orgRulePrefix = "org-"

// ErrOrgDirectivesDirMissing is returned when the org has no directives
// folder yet — callers should treat it as no-op, not failure.
var ErrOrgDirectivesDirMissing = errors.New("org: knowledge/directives/ not present")

// SyncResult summarizes what SyncOrgDirectivesToWorkspace did.
type SyncResult struct {
	Copied   int      // new files + files whose content differed
	Skipped  int      // identical content — no write
	OrphansDetected []string // `.claude/rules/org-*.md` whose source no longer exists
}

// SyncOrgDirectivesToWorkspace mirrors *.md files from
// `<orgDir>/knowledge/directives/` into `<workspaceDir>/.claude/rules/` with
// the `org-` filename prefix. Idempotent — files whose destination already
// matches source content are skipped (Copied count excludes them).
//
// Returns (nil, ErrOrgDirectivesDirMissing) when the org has no directives
// directory. Other errors bubble up. [355.B]
func SyncOrgDirectivesToWorkspace(orgDir, workspaceDir string) (*SyncResult, error) {
	if orgDir == "" || workspaceDir == "" {
		return nil, errors.New("org_directives_sync: empty orgDir or workspaceDir")
	}
	srcDir := filepath.Join(orgDir, orgDirectivesSubdir)
	if fi, err := os.Stat(srcDir); err != nil || !fi.IsDir() {
		return nil, ErrOrgDirectivesDirMissing
	}
	dstDir := filepath.Join(workspaceDir, ".claude", "rules")
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return nil, fmt.Errorf("org_directives_sync: mkdir dst: %w", err)
	}

	result := &SyncResult{}
	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		if err := copyIfDifferent(path, filepath.Join(dstDir, orgRulePrefix+d.Name()), result); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result.OrphansDetected = detectOrphanRules(srcDir, dstDir)
	return result, nil
}

// copyIfDifferent writes src to dst only if the content differs or dst absent.
// Updates the SyncResult counters accordingly. [355.B]
func copyIfDifferent(src, dst string, r *SyncResult) error {
	srcData, err := os.ReadFile(src) //nolint:gosec // G304-WORKSPACE-CANON: src under orgDir
	if err != nil {
		return fmt.Errorf("read src %s: %w", src, err)
	}
	if existing, err := os.ReadFile(dst); err == nil && bytesEqual(existing, srcData) { //nolint:gosec // G304-WORKSPACE-CANON: dst under workspaceDir
		r.Skipped++
		return nil
	}
	if err := os.WriteFile(dst, srcData, 0o640); err != nil {
		return fmt.Errorf("write dst %s: %w", dst, err)
	}
	r.Copied++
	return nil
}

// detectOrphanRules returns basenames of `.claude/rules/org-*.md` that no
// longer have a matching source in the org directives folder. Informational
// only — no auto-delete in MVP. [355.B]
func detectOrphanRules(srcDir, dstDir string) []string {
	entries, err := os.ReadDir(dstDir)
	if err != nil {
		return nil
	}
	var orphans []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, orgRulePrefix) || !strings.HasSuffix(name, ".md") {
			continue
		}
		// strip org- prefix → reconstruct src path
		srcName := strings.TrimPrefix(name, orgRulePrefix)
		if _, err := os.Stat(filepath.Join(srcDir, srcName)); os.IsNotExist(err) {
			orphans = append(orphans, name)
		}
	}
	return orphans
}

// bytesEqual is a small helper to avoid a bytes.Equal import round-trip
// when this is the only consumer in the package.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
