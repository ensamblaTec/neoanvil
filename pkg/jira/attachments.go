// Package jira — attachments.go
// PILAR XXIII / Épica 127.A — local-folder → ZIP → Jira attachment pipe.
//
// Two helpers:
//
//	ZipFolder(srcDir, outZipPath) — recursively zip a directory.
//	  Skips .DS_Store, .git, hidden dot-files at root level.
//
//	buildAttachmentBody(filePath) — wrap a file in a multipart/form-data
//	  body Atlassian's /attachments endpoint expects.
//
// Used together by Client.AttachFile + Client.AttachZipFolder, and
// driven by cmd/plugin-jira's `attach_artifact` action.

package jira

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
)

// ZipFolder packs srcDir recursively into outZipPath. The archive
// preserves the source directory's basename as the ROOT entry — so
// unzipping creates a folder named after the ticket/issue ID rather
// than dumping files into the current working directory.
//
// Example: srcDir=~/.neo/jira-docs/MCPI-3 → unzip yields MCPI-3/README.md,
// MCPI-3/code/..., MCPI-3/images/...
//
// Smart filtering (noise reduction):
//   - the destination zip itself (avoids self-inclusion)
//   - .DS_Store, Thumbs.db, .git directories
//   - images/<basename>.{png,html} when code/<basename>.<src-ext>
//     ALSO exists — these are codesnap auto-renders redundant with the
//     source. Standalone images (frontend screenshots, hand-drawn
//     diagrams) are kept because their basename has no twin in code/.
//   - empty subdirectories (design/ when no files inside)
//
// Returns an error when srcDir does not exist or is not a directory.
func ZipFolder(srcDir, outZipPath string) error {
	info, err := os.Stat(srcDir)
	if err != nil {
		return fmt.Errorf("stat %s: %w", srcDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", srcDir)
	}

	out, err := os.Create(outZipPath) //nolint:gosec // G304-CLI-CONSENT: outZipPath is operator-controlled (jira-docs scratch dir).
	if err != nil {
		return fmt.Errorf("create zip: %w", err)
	}
	defer out.Close()

	w := zip.NewWriter(out)
	defer w.Close()

	rootName := filepath.Base(srcDir)
	if _, err := w.Create(rootName + "/"); err != nil {
		return fmt.Errorf("write root dir entry: %w", err)
	}

	absOut, _ := filepath.Abs(outZipPath)
	codeBasenames := readCodeBasenames(srcDir)
	nonEmptyDirs := scanNonEmptyDirs(srcDir, codeBasenames)

	return filepath.Walk(srcDir, func(path string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == srcDir {
			return nil
		}
		if absPath, _ := filepath.Abs(path); absPath == absOut {
			return nil
		}
		if shouldSkipForZip(fi.Name()) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			// Skip directories that have no files we'll emit.
			if !nonEmptyDirs[rel] {
				return filepath.SkipDir
			}
		} else if isRedundantCodeSnap(rel, codeBasenames) {
			return nil
		}
		// Prefix every entry with rootName so the archive expands into
		// a single top-level folder named after the ticket/issue ID.
		entryPath := filepath.ToSlash(filepath.Join(rootName, rel))
		return writeZipEntry(w, path, entryPath, fi)
	})
}

// readCodeBasenames returns a set of basenames (without extension)
// for every file under srcDir/code/. Used to detect when an
// images/<base>.png is the codesnap render of code/<base>.<ext> and
// therefore redundant in the zip.
func readCodeBasenames(srcDir string) map[string]struct{} {
	out := make(map[string]struct{})
	codeDir := filepath.Join(srcDir, "code")
	entries, err := os.ReadDir(codeDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out[stripAllExt(e.Name())] = struct{}{}
	}
	return out
}

// isRedundantCodeSnap reports whether a relative path inside the
// docpack folder is an images/<base>.{png,html} whose source lives at
// code/<base>.<src-ext>. Such files are byproducts of auto-render and
// don't add information to the attachment.
func isRedundantCodeSnap(rel string, codeBasenames map[string]struct{}) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 2 || parts[0] != "images" {
		return false
	}
	ext := strings.ToLower(filepath.Ext(parts[1]))
	if ext != ".png" && ext != ".html" {
		return false
	}
	base := stripAllExt(parts[1])
	_, exists := codeBasenames[base]
	return exists
}

// scanNonEmptyDirs returns the set of subdirectory paths (relative to
// srcDir) that will have at least one file emitted to the zip. Used
// to skip directory entries with no kept content (e.g. empty design/).
//
// Always includes the implicit srcDir root so the walk starts cleanly.
func scanNonEmptyDirs(srcDir string, codeBasenames map[string]struct{}) map[string]bool {
	out := map[string]bool{".": true}
	_ = filepath.Walk(srcDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		if shouldSkipForZip(fi.Name()) {
			return nil
		}
		rel, _ := filepath.Rel(srcDir, path)
		if isRedundantCodeSnap(rel, codeBasenames) {
			return nil
		}
		// Mark every parent as non-empty.
		for dir := filepath.Dir(rel); dir != "." && dir != "/"; dir = filepath.Dir(dir) {
			out[dir] = true
		}
		return nil
	})
	return out
}

func shouldSkipForZip(name string) bool {
	switch name {
	case ".DS_Store", "Thumbs.db", ".git":
		return true
	}
	return false
}

// writeZipEntry writes one file or directory entry into w.
func writeZipEntry(w *zip.Writer, path, rel string, fi os.FileInfo) error {
	if fi.IsDir() {
		_, err := w.Create(rel + "/")
		return err
	}
	header, err := zip.FileInfoHeader(fi)
	if err != nil {
		return err
	}
	header.Name = rel
	header.Method = zip.Deflate
	zw, err := w.CreateHeader(header)
	if err != nil {
		return err
	}
	src, err := os.Open(path) //nolint:gosec // G304-CLI-CONSENT: path comes from filepath.Walk under operator-supplied srcDir.
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = io.Copy(zw, src)
	return err
}

// buildAttachmentBody constructs a multipart/form-data body wrapping a
// single file under the form field name "file" — the name Atlassian
// expects on /attachments.
func buildAttachmentBody(filePath string) (io.Reader, string, error) {
	f, err := os.Open(filePath) //nolint:gosec // G304-CLI-CONSENT: filePath is the operator-supplied artifact ZIP.
	if err != nil {
		return nil, "", fmt.Errorf("open %s: %w", filePath, err)
	}
	defer f.Close()

	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	part, err := mw.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return nil, "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return nil, "", fmt.Errorf("copy: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart: %w", err)
	}
	return buf, mw.FormDataContentType(), nil
}

// AttachOptions controls AttachZipFolder behavior.
type AttachOptions struct {
	// AutoRender, when true, scans <folderPath>/code/ for source files
	// and generates code-snap PNGs in <folderPath>/images/ before
	// zipping. Existing PNGs are kept (idempotent). Files without a
	// recognized syntax-highlightable extension are skipped silently.
	AutoRender bool
}

// AttachZipFolder is the high-level operator helper: optionally
// auto-render code-snap PNGs, zip the folder, upload to the issue.
// Returns the path of the zip created (kept on disk for inspection).
//
// Backward-compatible signature: if you don't need auto-render, the
// AttachOptions zero value disables it and behavior matches the
// original implementation.
func (c *Client) AttachZipFolder(ctx context.Context, issueKey, folderPath string, opts AttachOptions) (string, error) {
	if strings.TrimSpace(issueKey) == "" {
		return "", errors.New("issue key is required")
	}
	folderPath = expandHome(folderPath)
	info, err := os.Stat(folderPath)
	if err != nil {
		return "", fmt.Errorf("stat folder: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", folderPath)
	}

	if opts.AutoRender {
		if err := autoRenderCodeSnaps(ctx, folderPath); err != nil {
			// Non-fatal: log via returned error but still proceed with
			// whatever images/ already contains.
			return "", fmt.Errorf("auto-render: %w", err)
		}
	}

	// Zip filename is <ticket-id>.zip — uploaded to Jira with that exact
	// name so the attachment list shows the ticket key directly. Drop
	// the legacy "-artifacts" suffix; the prefix folder inside the zip
	// already conveys what it contains.
	parent := filepath.Dir(folderPath)
	zipName := filepath.Base(folderPath) + ".zip"
	zipPath := filepath.Join(parent, zipName)

	if err := ZipFolder(folderPath, zipPath); err != nil {
		return "", fmt.Errorf("zip: %w", err)
	}
	if err := c.AttachFile(ctx, issueKey, zipPath); err != nil {
		return zipPath, fmt.Errorf("upload: %w", err)
	}
	return zipPath, nil
}

// autoRenderCodeSnaps walks <folderPath>/code/ and generates PNGs in
// <folderPath>/images/ for any source file that doesn't have a
// matching .png yet. Idempotent: re-runs are no-ops when output is
// up-to-date. Skips files whose extension chroma can't lex.
//
// Naming: code/<descriptor>.<ext> → images/<descriptor>.png +
// images/<descriptor>.html. Same basename stripped of .ext, then
// .png/.html appended (per the doc-pack naming convention).
func autoRenderCodeSnaps(ctx context.Context, folderPath string) error {
	codeDir := filepath.Join(folderPath, "code")
	imagesDir := filepath.Join(folderPath, "images")
	if _, err := os.Stat(codeDir); err != nil {
		if os.IsNotExist(err) {
			return nil // no code/ subfolder, nothing to render
		}
		return fmt.Errorf("stat code dir: %w", err)
	}
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		return fmt.Errorf("mkdir images: %w", err)
	}

	entries, err := os.ReadDir(codeDir)
	if err != nil {
		return fmt.Errorf("read code dir: %w", err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := stripAllExt(e.Name())
		srcPath := filepath.Join(codeDir, e.Name())
		pngPath := filepath.Join(imagesDir, base+".png")

		if isUpToDate(pngPath, srcPath) {
			continue
		}
		if _, err := RenderCodeSnap(ctx, CodeSnapInput{
			SourcePath: srcPath,
			Title:      e.Name(),
			OutPNG:     pngPath,
		}); err != nil {
			// Best-effort: keep going, surface error after the loop.
			fmt.Fprintf(os.Stderr, "auto-render %s: %v\n", srcPath, err)
		}
	}
	return nil
}

// stripAllExt removes ALL extensions from a filename so multi-suffix
// inputs like "manifest_permissions_check.go" become
// "manifest_permissions_check" (single .ext) and
// "loader.go.snippet" becomes "loader" (multi .ext).
//
// Conservative: only strips a recognized whitelist (.go, .ts, .tsx,
// .js, .jsx, .py, .rs, .java, .rb, .css, .html, .yaml, .yml, .md,
// .toml, .json, .snippet) so unusual filenames are preserved as-is.
func stripAllExt(name string) string {
	known := map[string]bool{
		".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
		".py": true, ".rs": true, ".java": true, ".rb": true, ".css": true,
		".html": true, ".yaml": true, ".yml": true, ".md": true,
		".toml": true, ".json": true, ".snippet": true,
	}
	for {
		ext := filepath.Ext(name)
		if !known[ext] {
			return name
		}
		name = strings.TrimSuffix(name, ext)
	}
}

// isUpToDate reports whether outPath exists and is newer than srcPath.
func isUpToDate(outPath, srcPath string) bool {
	out, err := os.Stat(outPath)
	if err != nil {
		return false
	}
	src, err := os.Stat(srcPath)
	if err != nil {
		return false
	}
	return out.ModTime().After(src.ModTime())
}

// expandHome resolves a leading ~ to the operator's home directory.
// Trivial helper kept here to avoid pulling in os/user across packages.
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}
