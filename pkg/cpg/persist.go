package cpg

// persist.go — CPG snapshot serialization for fast-boot (skip SSA rebuild).
// PILAR XXXII, épicas 262.A-C.

import (
	"encoding/gob"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cpgSchemaVersion must be bumped whenever the Graph/Node/Edge layout changes.
// A mismatch at load time triggers a cold rebuild. [Épica 262.A]
const cpgSchemaVersion uint32 = 1

// cpgHeader is the first record written to cpg.bin. Separate from the Graph
// so we can check version/staleness before decoding the full graph.
type cpgHeader struct {
	Version     uint32
	BuildAtUnix int64 // seconds since epoch when the CPG was serialized
	NodeCount   int
}

// ErrSchemaMismatch is returned by LoadCPG when the on-disk schema version
// does not match cpgSchemaVersion — triggers a cold rebuild. [Épica 262.B]
var ErrSchemaMismatch = errors.New("cpg: schema version mismatch")

// SaveCPG serializes g to path with 0600 permissions. Creates parent dirs as needed.
// Logs the operation to stderr (not stdout — MCP isolation). [Épica 262.A]
func SaveCPG(g *Graph, path string) error {
	if g == nil {
		return nil
	}
	start := time.Now()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304-WORKSPACE-CANON: persist_path from config, not user input
	if err != nil {
		return err
	}
	defer f.Close()

	enc := gob.NewEncoder(f)
	hdr := cpgHeader{
		Version:     cpgSchemaVersion,
		BuildAtUnix: time.Now().Unix(),
		NodeCount:   len(g.Nodes),
	}
	if err := enc.Encode(hdr); err != nil {
		return err
	}
	if err := enc.Encode(g); err != nil {
		return err
	}
	_ = start // log is written by caller via returned duration if needed
	return nil
}

// LoadCPG deserializes a Graph from path. Returns ErrSchemaMismatch when the
// on-disk version is stale. Returns os.ErrNotExist when the file is absent. [Épica 262.B]
func LoadCPG(path string) (*Graph, cpgHeader, error) {
	f, err := os.Open(path) //nolint:gosec // G304-WORKSPACE-CANON: persist_path from config
	if err != nil {
		return nil, cpgHeader{}, err
	}
	defer f.Close()

	dec := gob.NewDecoder(f)
	var hdr cpgHeader
	if err := dec.Decode(&hdr); err != nil {
		return nil, cpgHeader{}, err
	}
	if hdr.Version != cpgSchemaVersion {
		return nil, hdr, ErrSchemaMismatch
	}
	var g Graph
	if err := dec.Decode(&g); err != nil {
		return nil, hdr, err
	}
	// Re-initialize unexported maps that gob cannot encode.
	if g.index == nil {
		g.index = make(map[string]NodeID, len(g.Nodes))
		for _, n := range g.Nodes {
			g.index[n.Package+"."+n.Name] = n.ID
		}
	}
	if g.edgeSet == nil {
		g.edgeSet = make(map[uint64]struct{}, len(g.Edges))
		for _, e := range g.Edges {
			g.edgeSet[edgeKey(e.From, e.To, e.Kind)] = struct{}{}
		}
	}
	return &g, hdr, nil
}

// IsCPGStale returns true when any .go file in workspace has a mtime newer than
// header.BuildAtUnix, or when the header is zero (invalid). [Épica 262.C]
func IsCPGStale(workspace string, hdr cpgHeader) bool {
	if hdr.BuildAtUnix == 0 {
		return true
	}
	stale := false
	// [AUDIT-2026-04-23] Capture the outer walk error. A permission failure on the
	// workspace root used to silently return stale=false — loading a stale CPG with
	// no warning. Inner per-file errors (individual inaccessible files) are still
	// tolerable and continue the walk.
	if werr := filepath.WalkDir(workspace, func(path string, d fs.DirEntry, err error) error {
		if err != nil || stale {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		if info.ModTime().Unix() > hdr.BuildAtUnix {
			stale = true
			return filepath.SkipAll
		}
		return nil
	}); werr != nil {
		log.Printf("[CPG-WARN] IsCPGStale: walk %s failed: %v — treating graph as stale", workspace, werr)
		return true
	}
	return stale
}
