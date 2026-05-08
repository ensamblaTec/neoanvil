// Package workspace manages the multi-workspace registry for NeoAnvil. [SRE-34.1.1]
//
// Each workspace is a fully isolated project directory with its own .neo/ subtree.
// The registry lives at ~/.neo/workspaces.json and stores only lightweight metadata.
// A failure in Workspace A has zero impact on Workspace B — each reads/writes its
// own BoltDB, its own daemon.pid, and its own PKI. [SRE-34 — isolation guarantee]
package workspace

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// globalRegistryDir is the home-level directory for cross-workspace metadata.
	globalRegistryDir = ".neo"
	// registryFileName is the manifest tracking all registered workspaces.
	registryFileName = "workspaces.json"
)

// WorkspaceEntry describes a single registered project. [SRE-34.1.1]
type WorkspaceEntry struct {
	ID           string    `json:"id"`
	Path         string    `json:"path"`          // absolute path to project root
	Name         string    `json:"name"`          // basename of Path
	DominantLang string    `json:"dominant_lang"` // detected from file extensions
	Health       string    `json:"health"`        // "ok" | "degraded" | "unknown"
	AddedAt      time.Time `json:"added_at"`
	// [SRE-83.A.1] Transport declares how this workspace is served.
	// "sse"   → Nexus starts it as an SSE child process.
	// "stdio" → Nexus skips it (stdio-only project, e.g. strategos).
	// ""      → not set; Nexus falls back to managed_workspaces filter (backward compat).
	Transport string `json:"transport,omitempty"`
	// [Épica 261.C] Type distinguishes single workspace from project federation root.
	// "workspace" (default) | "project" (has .neo-project/neo.yaml with member_workspaces).
	Type string `json:"type,omitempty"`
}

// Registry is the in-memory view of ~/.neo/workspaces.json.
type Registry struct {
	Workspaces []WorkspaceEntry `json:"workspaces"`
	ActiveID   string           `json:"active_id"`
	filePath   string
}

// registryPath returns the path to workspaces.json in the user's home directory.
func registryPath() string {
	return DefaultRegistryPath()
}

// DefaultRegistryPath returns the canonical path to the global workspace registry.
func DefaultRegistryPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, globalRegistryDir, registryFileName)
}

// LoadRegistry reads ~/.neo/workspaces.json. If the file does not exist, an
// empty registry is initialised and persisted immediately so the file is always
// present after the first server boot. [SRE-34 — workspace isolation]
func LoadRegistry() (*Registry, error) {
	path := registryPath()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		r := &Registry{filePath: path}
		// Persist the empty registry so ~/.neo/workspaces.json exists on disk.
		// Errors here are non-fatal — the caller can still use the in-memory struct.
		_ = r.Save()
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workspace registry: read %s: %w", path, err)
	}
	var r Registry
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("workspace registry: parse: %w", err)
	}
	r.filePath = path
	return &r, nil
}

// Save persists the registry to disk atomically (write to temp, rename).
func (r *Registry) Save() error {
	if err := os.MkdirAll(filepath.Dir(r.filePath), 0700); err != nil {
		return fmt.Errorf("workspace registry: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("workspace registry: marshal: %w", err)
	}
	tmp := r.filePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("workspace registry: write tmp: %w", err)
	}
	return os.Rename(tmp, r.filePath)
}

// Add registers absPath as a new workspace. If the path is already registered,
// the existing entry is returned without modification (idempotent).
func (r *Registry) Add(absPath string) (*WorkspaceEntry, error) {
	absPath = filepath.Clean(absPath)
	if _, err := os.Stat(absPath); err != nil {
		return nil, fmt.Errorf("workspace path %q not accessible: %w", absPath, err)
	}
	// Idempotent: return existing entry if already registered.
	for i := range r.Workspaces {
		if r.Workspaces[i].Path == absPath {
			return &r.Workspaces[i], nil
		}
	}
	lang := DetectDominantLang(absPath)
	name := filepath.Base(absPath)
	// [SRE-106.A] Deterministic ID: same path → same ID across registry wipes.
	// Aligns with the port allocator (also hash(path)-based) and prevents
	// .mcp.json from breaking when ~/.neo/workspaces.json is rebuilt.
	id := slugify(name) + fmt.Sprintf("-%05d", fnvHash32(absPath)%100000)
	// [Épica 261.C] Detect project federation root by presence of .neo-project/neo.yaml.
	wsType := "workspace"
	if _, statErr := os.Stat(filepath.Join(absPath, ".neo-project", "neo.yaml")); statErr == nil {
		wsType = "project"
	}
	entry := WorkspaceEntry{
		ID:           id,
		Path:         absPath,
		Name:         name,
		DominantLang: lang,
		Health:       "unknown",
		AddedAt:      time.Now(),
		Type:         wsType,
	}
	r.Workspaces = append(r.Workspaces, entry)
	// Return pointer to the actual slice element (not the local var) so callers
	// that modify the returned entry (e.g. setting Transport) see their changes
	// reflected in the registry before Save(). [SRE-83 audit]
	return &r.Workspaces[len(r.Workspaces)-1], nil
}

// Select sets the active workspace by ID or name. Returns an error if not found.
func (r *Registry) Select(idOrName string) error {
	for _, e := range r.Workspaces {
		if e.ID == idOrName || e.Name == idOrName {
			r.ActiveID = e.ID
			return nil
		}
	}
	return fmt.Errorf("workspace %q not found — run `neo workspace list`", idOrName)
}

// Active returns the currently active workspace, or the first registered workspace
// if no active ID is set. Returns nil if the registry is empty.
func (r *Registry) Active() *WorkspaceEntry {
	for i := range r.Workspaces {
		if r.Workspaces[i].ID == r.ActiveID {
			return &r.Workspaces[i]
		}
	}
	if len(r.Workspaces) > 0 {
		return &r.Workspaces[0]
	}
	return nil
}

// NeoDir returns the .neo/ subdirectory for the given workspace entry.
// Each workspace owns its own .neo/ tree — isolation guarantee. [SRE-34]
func (e *WorkspaceEntry) NeoDir() string {
	return filepath.Join(e.Path, ".neo")
}

// DBDir returns the BoltDB directory for the given workspace.
func (e *WorkspaceEntry) DBDir() string {
	return filepath.Join(e.NeoDir(), "db")
}

// DetectDominantLang scans the top-level of root and counts file extensions to
// determine the dominant programming language. Skips vendor dirs. Public so the
// CLI can call it from outside the package.
func DetectDominantLang(root string) string {
	counts := map[string]int{}
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || name == "vendor" || name == ".git" || name == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".go":
			counts["go"]++
		case ".ts", ".tsx":
			counts["typescript"]++
		case ".js", ".jsx":
			counts["javascript"]++
		case ".py":
			counts["python"]++
		case ".rs":
			counts["rust"]++
		}
		return nil
	})
	if walkErr != nil {
		log.Printf("[WORKSPACE-WARN] DetectDominantLang: walk %s failed: %v", root, walkErr)
	}
	best, max := "unknown", 0
	for lang, n := range counts {
		if n > max {
			best, max = lang, n
		}
	}
	return best
}

// fnvHash32 returns the FNV-1a 32-bit hash of s. Used by Add() to derive a
// path-deterministic suffix for workspace IDs. [SRE-106.A]
func fnvHash32(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// slugify converts s to a lowercase hyphenated identifier safe for use as an ID.
func slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}
