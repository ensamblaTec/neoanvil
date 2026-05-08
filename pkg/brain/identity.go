// Package brain provides cross-machine workspace identity and (later)
// snapshot/manifest/storage operations for PILAR XXVI Brain Portable.
//
// identity.go — ResolveCanonicalID determines a stable workspace identifier
// that survives clones, machine swaps, and fresh boots. Same physical
// workspace on Mac and Linux must resolve to the same canonical_id so the
// snapshot manifest can match the two ends of a push/pull.
//
// Resolution order (first match wins):
//
//  1. neo.yaml override         workspace.canonical_id (string, free-form)
//  2. git remote.origin.url     normalize SSH↔HTTPS to <host>/<owner>/<repo>
//  3. .neo-project/neo.yaml     project:<project_name>:<workspace_basename>
//  4. fallback                  local:<sha256-prefix(absolute path)>
//
// All paths walk-up — a workspace nested several directories below its
// .git/ or .neo-project/ root is still resolved correctly.
package brain

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/config"
)

// gitSubprocessTimeout caps `git config` calls. NFS locks, FUSE filesystems,
// and corrupt .git/index.lock files can hang git indefinitely; bound the
// subprocess so canonical_id resolution always completes. [DS round 3 F4]
const gitSubprocessTimeout = 5 * time.Second

// CanonicalSource identifies which resolution rule produced a CanonicalID.
// Useful for diagnostics + tests. Stable string values — do not rename.
type CanonicalSource string

const (
	SourceConfigOverride CanonicalSource = "config_override"
	SourceGitRemote      CanonicalSource = "git_remote"
	SourceProjectName    CanonicalSource = "project_name"
	SourcePathHash       CanonicalSource = "path_hash" // legacy; superseded by SourceDeviceKey [146.J]
)
// SourceDeviceKey is declared in identity_key.go alongside LoadOrCreateDeviceKey.

// CanonicalID is the resolved identifier plus the rule that produced it.
// Callers who only need the string can use Resolution.ID directly.
type Resolution struct {
	ID     string
	Source CanonicalSource
}

// ResolveCanonicalID returns a stable cross-machine identifier for the
// workspace at workspacePath. workspacePath should be absolute; relative
// values are converted via filepath.Abs.
//
// Resolution order:
//  1. neo.yaml::workspace.canonical_id override
//  2. Git remote URL (normalised)
//  3. .neo-project/neo.yaml::project_name
//  4. Ed25519 device key fingerprint ("dev:<24 hex>") — [146.J] stable across
//     path renames, replaces the path-derived hash used in earlier builds.
//  5. Path hash fallback (SourcePathHash) — only when device key unavailable.
//
// The function never returns an error — every failure path falls through
// to the next rule; at worst returns the path-hash sentinel.
func ResolveCanonicalID(workspacePath string) Resolution {
	abs, err := filepath.Abs(workspacePath)
	if err != nil || abs == "" {
		// Path is unusable; degrade to a hash of whatever we got.
		return Resolution{ID: pathHash(workspacePath), Source: SourcePathHash}
	}

	if id := readConfigOverride(abs); id != "" {
		return Resolution{ID: id, Source: SourceConfigOverride}
	}

	if id := readGitRemote(abs); id != "" {
		return Resolution{ID: id, Source: SourceGitRemote}
	}

	if id := readProjectName(abs); id != "" {
		return Resolution{ID: id, Source: SourceProjectName}
	}

	// [146.J] Prefer the Ed25519 device fingerprint over the path-derived hash.
	// The device key is stable even when the workspace is moved or migrated to
	// a different machine with the same key material in ~/.neo/identity.key.
	if key, err := LoadOrCreateDeviceKey(); err == nil {
		pub := key.Public().(ed25519.PublicKey)
		return Resolution{ID: DeviceKeyFingerprint(pub), Source: SourceDeviceKey}
	}

	return Resolution{ID: pathHash(abs), Source: SourcePathHash}
}

// readConfigOverride walks up looking for neo.yaml, loads it, and returns
// workspace.canonical_id if set. Errors are silent — fall through to next
// rule. config.LoadConfig takes a file path (not a directory) so we
// resolve via walk-up first.
func readConfigOverride(workspace string) string {
	root := walkUpFor(workspace, "neo.yaml")
	if root == "" {
		return ""
	}
	cfg, err := config.LoadConfig(filepath.Join(root, "neo.yaml"))
	if err != nil || cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Workspace.CanonicalID)
}

// readGitRemote walks up from workspace to find a .git/ directory then
// invokes `git -C <root> config --get remote.origin.url`. Empty result
// (no .git/, no remote, command failure) returns "" so the next rule fires.
//
// SSH and HTTPS URLs both normalize to "<host>/<owner>/<repo>" with .git
// suffix stripped, so the same repo cloned via either method matches.
func readGitRemote(workspace string) string {
	root := walkUpFor(workspace, ".git")
	if root == "" {
		return ""
	}
	// [DS round 3 F4] Bound the subprocess — NFS/FUSE/lock-file scenarios
	// can hang `git config` indefinitely; we'd rather fall through to the
	// next resolution rule than block the whole BRIEFING cycle.
	ctx, cancel := context.WithTimeout(context.Background(), gitSubprocessTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", root, "config", "--get", "remote.origin.url").Output() //nolint:gosec // G204-LITERAL-BIN: binary is "git" literal; root is workspace walk-up
	if err != nil {
		return ""
	}
	return normalizeGitRemote(strings.TrimSpace(string(out)))
}

// normalizeGitRemote converts an SSH or HTTPS git remote URL to the
// canonical "<host>/<owner>/<repo>" form. Returns "" when the input is
// not recognizable as a git URL.
//
// Examples:
//
//	git@github.com:foo/bar.git              → github.com/foo/bar
//	https://github.com/foo/bar.git          → github.com/foo/bar
//	https://gitlab.local:8443/x/y           → gitlab.local:8443/x/y
//	ssh://git@host/path/to/repo.git         → host/path/to/repo
func normalizeGitRemote(remote string) string {
	if remote == "" {
		return ""
	}
	// SSH form:  git@host:owner/repo.git
	if s, ok := strings.CutPrefix(remote, "git@"); ok && strings.Contains(s, ":") {
		if i := strings.Index(s, ":"); i > 0 {
			host := s[:i]
			path := strings.TrimPrefix(s[i+1:], "/")
			return host + "/" + strings.TrimSuffix(path, ".git")
		}
	}
	// ssh:// or https:// or http:// or git://
	for _, scheme := range []string{"ssh://", "https://", "http://", "git://"} {
		if s, ok := strings.CutPrefix(remote, scheme); ok {
			// Strip optional "user@" before host.
			if at := strings.Index(s, "@"); at >= 0 && at < strings.Index(s+"/", "/") {
				s = s[at+1:]
			}
			return strings.TrimSuffix(s, ".git")
		}
	}
	// Unknown shape — return as-is sans .git suffix; caller picks fallback.
	return ""
}

// readProjectName walks up looking for .neo-project/neo.yaml and returns
// "project:<project_name>:<workspace_basename>". The basename is appended
// so that two member workspaces of the same project (e.g. backend +
// frontend) get distinct canonical_ids.
//
// Returns "" when no .neo-project/ is found, project_name is empty, or
// the YAML cannot be parsed.
func readProjectName(workspace string) string {
	root := walkUpFor(workspace, ".neo-project")
	if root == "" {
		return ""
	}
	cfg, err := config.LoadProjectConfig(root)
	if err != nil || cfg == nil {
		return ""
	}
	name := strings.TrimSpace(cfg.ProjectName)
	if name == "" {
		return ""
	}
	return fmt.Sprintf("project:%s:%s", name, filepath.Base(workspace))
}

// pathHash returns local:<first 32 hex chars of sha256(path)>. Always
// succeeds — the absolute fallback when nothing else identifies the
// workspace. Two machines with the same absolute path produce the same
// hash, which is desirable for one-off local-only workspaces.
//
// [146.K] Widened from sum[:8] (64-bit) to sum[:16] (128-bit) to reduce
// collision probability when many workspaces exist on the same machine.
func pathHash(path string) string {
	if path == "" {
		path = "(unknown)"
	}
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return "local:" + hex.EncodeToString(sum[:16])
}

// walkUpFor returns the directory that contains `name` as a direct child
// (file or directory — both kinds match), searching from start up to
// filesystem root. Returns "" if not found. Bounded at 256 levels to
// defend against pathological symlink loops.
//
// [146.L] Raised from 32 to 256 — 32 was too conservative for deeply
// nested workspaces (monorepos with 6-10 levels are common; 32 blocks
// workspaces nested >32 directories deep from resolving federation roots).
func walkUpFor(start, name string) string {
	dir, err := filepath.Abs(start)
	if err != nil {
		return ""
	}
	for range 256 {
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}
