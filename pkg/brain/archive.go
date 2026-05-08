// Package brain — archive.go: tar+zstd packaging of workspaces declared
// by a Manifest. PILAR XXVI / 135.A.5 + 135.A.6.
//
// BuildArchive walks every workspace + project + org listed in the
// manifest, streams the eligible files into a tar archive, compresses
// the tar with zstd, and writes the result to the caller's io.Writer.
// Eligibility is the negation of an exclude list (.git/, bin/, *.log,
// .neo/pki/, tmp/) plus an absolute size cap to prevent an accidental
// 100GB push.
//
// ApplyArchive reverses the operation. dryRun=true logs what would be
// written without touching the filesystem — operator validation step
// before a destructive restore.
//
// The archive format is intentionally boring: standard ustar tar inside
// zstd. No proprietary container so older neoanvil builds (or external
// tools) can still inspect a snapshot if needed for forensics.

package brain

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// archiveMaxBytes caps the total bytes written into one archive. 4 GiB
// matches the practical R2 multipart upload sweet spot (still streamable,
// won't overrun a single PUT session). Operators with truly larger
// workspaces should split the snapshot.
const archiveMaxBytes int64 = 4 << 30

// archiveMaxEntries caps the number of files extracted from one archive.
// Prevents zip-bomb style DoS via large numbers of tiny files.
const archiveMaxEntries = 100_000

// archivePerFileMaxBytes caps a single file. Even with a fat workspace,
// a single 1GB file is almost always a build artifact that should have
// been excluded — the cap defends against runaway includes.
const archivePerFileMaxBytes int64 = 1 << 30

// ErrHLCRollback is returned by ApplyArchive when the incoming manifest's
// HLC is not strictly greater than the currentHLC supplied by the caller.
// A non-zero currentHLC means "I already applied a snapshot at this logical
// instant; accepting anything ≤ would be a replay". Zero currentHLC (first
// pull or explicit override) bypasses the check.
var ErrHLCRollback = errors.New("ApplyArchive: manifest HLC is not strictly newer than current (replay rejected)")

// archiveExcludeDirs are directory basenames that BuildArchive never
// descends into. Lowercase compare. Walks resolve symlinks lazily so
// these match regardless of the absolute path the operator started from.
var archiveExcludeDirs = map[string]struct{}{
	".git":         {},
	"bin":          {},
	"node_modules": {},
	"vendor":       {},
	"dist":         {},
	"tmp":          {},
	"__pycache__":  {},
	".venv":        {},
	"venv":         {},
}

// archiveExcludeSuffixes is the file-extension blacklist. Lowercase.
// The .pki suffix is per-file rather than per-dir because pki/ lives
// at multiple paths.
var archiveExcludeSuffixes = []string{".log", ".pid"}

// archiveExcludePathFragments matches anywhere in the relative path.
// Used for nested patterns the basename rule can't express, like
// ".neo/pki/" and ".neo/db/" — those WALL paths must be excluded but
// the workspace root contains many unrelated files.
var archiveExcludePathFragments = []string{
	"/.neo/pki/",
	"/.neo/db/",
	"/.neo/logs/",
}

// BuildArchive walks every WorkspaceManifest.Path declared in m and
// streams eligible files as tar entries into a zstd-compressed stream
// written to w. After every workspace is visited, projects' and orgs'
// roots are walked too (for .neo-project/knowledge, .neo-org/knowledge,
// etc.) — Members fields are ignored here, the receiver re-resolves
// membership at restore time.
//
// The Files slice on each WorkspaceManifest is rewritten in place with
// the repo-relative paths actually included, so the manifest stored
// alongside the archive accurately lists what's inside.
//
// Returns the total uncompressed bytes written (for logging) and any
// error. On error the writer may have partial data — caller should
// discard the destination.
func BuildArchive(m *Manifest, w io.Writer) (int64, error) {
	if m == nil {
		return 0, errors.New("BuildArchive: nil manifest")
	}
	if w == nil {
		return 0, errors.New("BuildArchive: nil writer")
	}

	zw, err := zstd.NewWriter(w)
	if err != nil {
		return 0, fmt.Errorf("BuildArchive: zstd: %w", err)
	}
	defer func() { _ = zw.Close() }()

	tw := tar.NewWriter(zw)
	defer func() { _ = tw.Close() }()

	var totalBytes int64

	for i := range m.Workspaces {
		files, n, err := writeRoot(tw, "workspace/"+m.Workspaces[i].ID, m.Workspaces[i].Path, totalBytes)
		if err != nil {
			return totalBytes, err
		}
		m.Workspaces[i].Files = files
		totalBytes += n
	}
	for i := range m.Projects {
		files, n, err := writeRoot(tw, "project/"+sanitizeID(m.Projects[i].CanonicalID), m.Projects[i].Path, totalBytes)
		if err != nil {
			return totalBytes, err
		}
		m.Projects[i].Files = files
		totalBytes += n
	}
	for i := range m.Orgs {
		files, n, err := writeRoot(tw, "org/"+sanitizeID(m.Orgs[i].CanonicalID), m.Orgs[i].Path, totalBytes)
		if err != nil {
			return totalBytes, err
		}
		m.Orgs[i].Files = files
		totalBytes += n
	}

	if err := tw.Close(); err != nil {
		return totalBytes, fmt.Errorf("BuildArchive: tar close: %w", err)
	}
	if err := zw.Close(); err != nil {
		return totalBytes, fmt.Errorf("BuildArchive: zstd close: %w", err)
	}
	return totalBytes, nil
}

// writeRoot walks rootPath, appends eligible files to tw under the given
// archivePrefix (e.g. "workspace/neoanvil-95248/"), returns the
// repo-relative paths included plus bytes written. running keeps the
// archive-wide running total for the size-cap check.
func writeRoot(tw *tar.Writer, archivePrefix, rootPath string, running int64) (files []string, written int64, err error) {
	walkErr := filepath.WalkDir(rootPath, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(rootPath, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		// Directory pruning — no descent into excluded basenames.
		if d.IsDir() {
			if _, skip := archiveExcludeDirs[strings.ToLower(d.Name())]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // skip symlinks, devices, fifos
		}
		if excludedByPath(rel) {
			return nil
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		if info.Size() > archivePerFileMaxBytes {
			return fmt.Errorf("BuildArchive: %q exceeds per-file cap (%d > %d)", rel, info.Size(), archivePerFileMaxBytes)
		}
		if running+written+info.Size() > archiveMaxBytes {
			return fmt.Errorf("BuildArchive: archive cap exceeded at %q (%d > %d)", rel, running+written+info.Size(), archiveMaxBytes)
		}

		hdr := &tar.Header{
			Name:    archivePrefix + "/" + filepath.ToSlash(rel),
			Mode:    int64(info.Mode().Perm()),
			Size:    info.Size(),
			ModTime: info.ModTime(),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("tar header %q: %w", rel, err)
		}
		f, err := os.Open(path) //nolint:gosec // G304-WORKSPACE-CANON: path is constrained by WalkDir under rootPath which the manifest trusted at NewManifest time
		if err != nil {
			return fmt.Errorf("open %q: %w", path, err)
		}
		n, copyErr := io.Copy(tw, f)
		_ = f.Close()
		if copyErr != nil {
			return fmt.Errorf("copy %q: %w", rel, copyErr)
		}
		written += n
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, written, walkErr
}

// excludedByPath returns true when rel matches one of the
// archiveExcludeSuffixes (file extension) or
// archiveExcludePathFragments (substring anywhere in path).
func excludedByPath(rel string) bool {
	relSlash := "/" + filepath.ToSlash(rel)
	lower := strings.ToLower(relSlash)
	for _, frag := range archiveExcludePathFragments {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	for _, suf := range archiveExcludeSuffixes {
		if strings.HasSuffix(lower, suf) {
			return true
		}
	}
	return false
}

// sanitizeID converts a canonical_id (which may contain ":" / "/") into a
// path-safe segment for use as the archive prefix. Non-alphanumeric
// characters become "_" so archive readers don't trip over edge cases.
func sanitizeID(id string) string {
	var b strings.Builder
	b.Grow(len(id))
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ApplyArchive extracts an archive previously produced by BuildArchive
// and restores the files to disk. m must match the archive — the caller
// is responsible for shipping the manifest alongside the bytes.
//
// currentHLC is the HLC of the last snapshot successfully applied on this
// receiver. When non-zero, ApplyArchive rejects manifests whose HLC is not
// strictly greater (returns ErrHLCRollback). Pass HLC{} (zero) for the
// first pull or when monotonicity tracking is not available at the call
// site.
//
// dryRun=true logs the operations that would happen (including
// destination paths derived from the destRoot) without touching disk.
// Returns the list of destination absolute paths that were (or would be)
// written.
//
// destRoot is a directory under which the archive prefixes (workspace/,
// project/, org/) are recreated. For a real restore, destRoot is
// typically derived from a path_map (135.E) — the receiver decides where
// each canonical_id should land. Here we accept whatever the caller
// provides and append the archive prefix verbatim.
func ApplyArchive(r io.Reader, m *Manifest, currentHLC HLC, destRoot string, dryRun bool) ([]string, error) {
	if err := validateApplyArgs(r, m, currentHLC, destRoot); err != nil {
		return nil, err
	}

	zr, err := zstd.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("ApplyArchive: zstd reader: %w", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	var written []string
	var entryCount int
	var totalRestored int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return written, nil
		}
		if err != nil {
			return written, fmt.Errorf("ApplyArchive: tar next: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		entryCount++
		if entryCount > archiveMaxEntries {
			return written, fmt.Errorf("ApplyArchive: entry count cap exceeded (%d > %d)", entryCount, archiveMaxEntries)
		}
		totalRestored += hdr.Size
		if totalRestored > archiveMaxBytes {
			return written, fmt.Errorf("ApplyArchive: aggregate size cap exceeded at %q (%d > %d bytes)", hdr.Name, totalRestored, archiveMaxBytes)
		}
		dest, err := resolveArchiveDest(destRoot, hdr.Name)
		if err != nil {
			return written, err
		}
		written = append(written, dest)
		if dryRun {
			continue
		}
		if err := writeArchiveEntry(tr, dest, hdr.Mode, hdr.Size); err != nil {
			return written, err
		}
	}
}

// validateApplyArgs checks the caller invariants ApplyArchive depends on.
// Extracted so the main loop's CC stays under the limit.
func validateApplyArgs(r io.Reader, m *Manifest, currentHLC HLC, destRoot string) error {
	switch {
	case r == nil:
		return errors.New("ApplyArchive: nil reader")
	case m == nil:
		return errors.New("ApplyArchive: nil manifest")
	case destRoot == "":
		return errors.New("ApplyArchive: destRoot required")
	}
	if err := m.Validate(); err != nil {
		return fmt.Errorf("ApplyArchive: invalid manifest: %w", err)
	}
	// HLC replay guard: reject when the manifest is not strictly newer than
	// the current receiver state. Zero currentHLC bypasses the check
	// (first pull or caller does not track monotonicity).
	if !currentHLC.IsZero() && CompareHLC(m.HLC, currentHLC) <= 0 {
		return fmt.Errorf("%w (manifest=%s current=%s)", ErrHLCRollback, m.HLC, currentHLC)
	}
	return nil
}

// resolveArchiveDest joins destRoot with a tar header Name after rejecting
// any path-traversal sequences, absolute paths, and NUL bytes.
// Returns the absolute destination on disk.
func resolveArchiveDest(destRoot, name string) (string, error) {
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("ApplyArchive: refusing NUL byte in archive entry name: %q", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("ApplyArchive: refusing absolute path in archive: %q", name)
	}
	if strings.HasPrefix(clean, "..") || strings.Contains(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("ApplyArchive: refusing path traversal in archive: %q", name)
	}
	return filepath.Join(destRoot, clean), nil
}

// writeArchiveEntry copies one tar entry's body to dest. hdrSize is the size
// declared in the tar header; if it exceeds archivePerFileMaxBytes the entry
// is refused — silent truncation via LimitReader would produce a corrupt file.
func writeArchiveEntry(tr *tar.Reader, dest string, mode int64, hdrSize int64) error {
	if hdrSize > archivePerFileMaxBytes {
		return fmt.Errorf("ApplyArchive: %q exceeds per-file cap (%d > %d bytes); restore aborted", dest, hdrSize, archivePerFileMaxBytes)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("ApplyArchive: mkdir for %q: %w", dest, err)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(mode)&0o7777) //nolint:gosec // G304-WORKSPACE-CANON: dest validated by resolveArchiveDest (".." rejected)
	if err != nil {
		return fmt.Errorf("ApplyArchive: open %q: %w", dest, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, io.LimitReader(tr, archivePerFileMaxBytes)); err != nil {
		return fmt.Errorf("ApplyArchive: copy %q: %w", dest, err)
	}
	return f.Close()
}
