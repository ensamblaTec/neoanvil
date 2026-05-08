// Package jira — docpack.go
// PILAR XXIII / Épica 127.D — local-only doc pack builder.
//
// Designed to be called from cmd/plugin-jira via the prepare_doc_pack
// action so the operator (and Claude) can request a full pack with
// ONE tool call: list files in the repo + ticket key. The plugin reads
// the files itself, derives descriptors, runs git log, writes README,
// optionally renders PNGs, optionally zips + uploads. None of the file
// content passes through Claude's context.

package jira

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// PrepareDocPackInput drives the local pack builder.
type PrepareDocPackInput struct {
	TicketID    string   // required: e.g. "MCPI-5"
	RepoRoot    string   // required: absolute path to the git repo
	Files       []string // paths relative to RepoRoot
	CommitRange string   // optional: e.g. "HEAD~10..HEAD"; default = whole log of files
	Summary     string   // optional override; default = derived from get_context
	AutoRender  bool     // generate PNGs via codesnap (kept for inspection; ZipFolder
	// strips redundant code-snaps from the upload). Default false.
	AutoAttach bool // zip + upload to Jira after build
	// CommitHash, when set, drives auto_doc_from_commit semantics:
	// runs `git show --stat <hash>` to discover Files and reads the
	// commit message body for Summary. Skipped fields are auto-populated.
	CommitHash string
	// ExcludePaths is a list of substrings matched against repo paths.
	// Any file whose path contains one of these strings is dropped
	// before copying. Useful when a commit touches metadata that's
	// not part of the ticket (master_plan, technical_debt, etc.).
	// Defaults applied when empty: see defaultExcludes().
	ExcludePaths []string
}

// PrepareDocPackResult reports what the builder produced.
type PrepareDocPackResult struct {
	FolderPath  string   // ~/.neo/jira-docs/<TICKET_ID>
	ReadmePath  string
	CodeFiles   []string // descriptors written to code/
	Renders     []string // PNGs written to images/
	ZipPath     string   // empty when AutoAttach=false
	Uploaded    bool
}

// PrepareDocPack builds the documentation folder for one ticket and
// optionally attaches it. All work is done locally — no per-file IO
// passes through the agent's context.
//
// When CommitHash is set, derives Files + Summary from the commit
// before validating (auto_doc_from_commit pattern). Operator only
// passes ticket_id + commit_hash + repo_root.
func (c *Client) PrepareDocPack(ctx context.Context, in PrepareDocPackInput) (*PrepareDocPackResult, error) {
	if strings.TrimSpace(in.CommitHash) != "" {
		if strings.Contains(in.CommitHash, "..") { // [129.1] range mode: "hash_a..hash_b"
			if err := populateFromCommitRange(&in); err != nil {
				return nil, fmt.Errorf("populate from range %s: %w", in.CommitHash, err)
			}
		} else {
			if err := populateFromCommit(&in); err != nil {
				return nil, fmt.Errorf("populate from commit %s: %w", in.CommitHash, err)
			}
		}
	}
	if err := validatePrepareInput(in); err != nil {
		return nil, err
	}

	folderPath, err := initDocPackFolder(in.TicketID)
	if err != nil {
		return nil, err
	}

	res := &PrepareDocPackResult{FolderPath: folderPath}

	codeFiles, err := copyCodeSnippets(in.RepoRoot, in.Files, folderPath)
	if err != nil {
		return res, err
	}
	res.CodeFiles = codeFiles

	commits, err := gitCommitsForFiles(in.RepoRoot, in.Files, in.CommitRange)
	if err != nil {
		// Non-fatal: README will note absence
		commits = []string{fmt.Sprintf("(git log unavailable: %v)", err)}
	}

	readmePath, err := writeDocPackReadme(folderPath, in, commits, codeFiles)
	if err != nil {
		return res, err
	}
	res.ReadmePath = readmePath

	if in.AutoRender {
		renders, rerr := renderAllSnaps(ctx, folderPath)
		res.Renders = renders
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "doc-pack auto-render: %v\n", rerr)
		}
	}

	if in.AutoAttach {
		zipPath, attachErr := c.AttachZipFolder(ctx, in.TicketID, folderPath, AttachOptions{AutoRender: false})
		res.ZipPath = zipPath
		if attachErr != nil {
			return res, fmt.Errorf("attach: %w", attachErr)
		}
		res.Uploaded = true
	}

	return res, nil
}

// populateFromCommit fills Files and Summary from `git show <hash>`.
// Files: every path with status A (added), M (modified), D (deleted)
// from `git show --name-status`. Summary: the commit message body
// (subject + body), used as the README summary when the input field
// is empty. Repo path comes from RepoRoot.
func populateFromCommit(in *PrepareDocPackInput) error {
	if strings.TrimSpace(in.RepoRoot) == "" {
		return errors.New("RepoRoot required when CommitHash is set")
	}
	files, err := gitFilesFromCommit(in.RepoRoot, in.CommitHash)
	if err != nil {
		return err
	}
	if len(in.Files) == 0 {
		in.Files = filterExcludedPaths(files, in.ExcludePaths)
	}
	if strings.TrimSpace(in.Summary) == "" {
		summary, msgErr := gitCommitMessage(in.RepoRoot, in.CommitHash)
		if msgErr == nil {
			in.Summary = summary
		}
	}
	if strings.TrimSpace(in.CommitRange) == "" {
		in.CommitRange = in.CommitHash + "~1.." + in.CommitHash
	}
	return nil
}

// populateFromCommitRange fills Files and Summary from a git log range
// (e.g. "abc123..def456"). Files: union of all paths added/modified in the
// range. Summary: commit message of the last commit in the range. [129.2/129.3]
func populateFromCommitRange(in *PrepareDocPackInput) error {
	if strings.TrimSpace(in.RepoRoot) == "" {
		return errors.New("RepoRoot required when CommitHash range is set")
	}
	files, err := deriveFilesFromCommitRange(in.RepoRoot, in.CommitHash)
	if err != nil {
		return err
	}
	if len(in.Files) == 0 {
		in.Files = filterExcludedPaths(files, in.ExcludePaths)
	}
	if strings.TrimSpace(in.Summary) == "" {
		if lastHash, hashErr := gitLastCommitInRange(in.RepoRoot, in.CommitHash); hashErr == nil {
			if summary, msgErr := gitCommitMessage(in.RepoRoot, lastHash); msgErr == nil {
				in.Summary = summary
			}
		}
	}
	if strings.TrimSpace(in.CommitRange) == "" {
		in.CommitRange = in.CommitHash
	}
	return nil
}

// deriveFilesFromCommitRange runs `git log --name-status <rangeSpec>` and
// returns the union of all added/modified files in the range. Deleted files
// and directories are excluded (same rules as gitFilesFromCommit). [129.2]
func deriveFilesFromCommitRange(repoRoot, rangeSpec string) ([]string, error) {
	cmd := exec.Command("git", "-C", repoRoot, "log", "--name-status", "--pretty=format:", rangeSpec) //nolint:gosec // G204-LITERAL-BIN
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git log --name-status: %w (stderr: %s)", err, stderr.String())
	}
	seen := make(map[string]bool)
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(stdout.String()), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		status := fields[0]
		path := fields[len(fields)-1]
		if strings.HasPrefix(status, "D") {
			continue
		}
		if seen[path] {
			continue
		}
		fullPath := filepath.Join(repoRoot, path)
		if info, err := os.Stat(fullPath); err != nil || info.IsDir() {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out, nil
}

// gitLastCommitInRange returns the hash of the most recent commit in the range. [129.3]
func gitLastCommitInRange(repoRoot, rangeSpec string) (string, error) {
	cmd := exec.Command("git", "-C", repoRoot, "log", "-1", "--pretty=format:%H", rangeSpec) //nolint:gosec // G204-LITERAL-BIN
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// defaultExcludes returns the default substring patterns dropped from a
// commit's file list. Auto-managed metadata that's noise in a ticket
// pack: planning files, audit logs, build artifacts, lock files,
// committed yaml examples (rarely the focus of a story).
func defaultExcludes() []string {
	return []string{
		".neo/master_plan.md",
		".neo/master_done.md",
		".neo/technical_debt.md",
		".neo/.env",
		".neo/db/",
		"go.sum",
		".gitignore",
	}
}

// filterExcludedPaths drops paths matching any substring in patterns.
// When patterns is nil, defaultExcludes() is applied. To opt out,
// caller can pass an explicit empty slice — but check via len, not
// nil, so we honor the distinction.
func filterExcludedPaths(files, patterns []string) []string {
	if patterns == nil {
		patterns = defaultExcludes()
	}
	if len(patterns) == 0 {
		return files
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		drop := false
		for _, pat := range patterns {
			if strings.Contains(f, pat) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, f)
		}
	}
	return out
}

// gitFilesFromCommit returns paths touched by the commit (A/M added/
// modified — D deleted skipped because the source file is gone).
func gitFilesFromCommit(repoRoot, hash string) ([]string, error) {
	cmd := exec.Command("git", "-C", repoRoot, "show", "--name-status", "--pretty=format:", hash) //nolint:gosec // G204-LITERAL-BIN
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git show: %w (stderr: %s)", err, stderr.String())
	}
	out := make([]string, 0, 8)
	for line := range strings.SplitSeq(strings.TrimSpace(stdout.String()), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		status := fields[0]
		path := fields[len(fields)-1]
		// Skip deleted files (no source to copy) and binary diffs.
		if strings.HasPrefix(status, "D") {
			continue
		}
		// Skip files outside the repo (e.g. submodules).
		fullPath := filepath.Join(repoRoot, path)
		if info, err := os.Stat(fullPath); err != nil || info.IsDir() {
			continue
		}
		out = append(out, path)
	}
	return out, nil
}

// gitCommitMessage returns the commit message body (subject + blank
// line + body), suitable as a doc-pack summary.
func gitCommitMessage(repoRoot, hash string) (string, error) {
	cmd := exec.Command("git", "-C", repoRoot, "show", "-s", "--format=%B", hash) //nolint:gosec // G204-LITERAL-BIN
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func validatePrepareInput(in PrepareDocPackInput) error {
	if strings.TrimSpace(in.TicketID) == "" {
		return errors.New("TicketID is required")
	}
	if strings.TrimSpace(in.RepoRoot) == "" {
		return errors.New("RepoRoot is required")
	}
	if len(in.Files) == 0 {
		return errors.New("files list is required (at least one path)")
	}
	if _, err := os.Stat(in.RepoRoot); err != nil {
		return fmt.Errorf("RepoRoot %s: %w", in.RepoRoot, err)
	}
	return nil
}

// initDocPackFolder creates ~/.neo/jira-docs/<TICKET_ID>/{code,images,design}.
func initDocPackFolder(ticketID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	folder := filepath.Join(home, ".neo", "jira-docs", ticketID)
	for _, sub := range []string{"code", "images", "design"} {
		if err := os.MkdirAll(filepath.Join(folder, sub), 0o755); err != nil {
			return "", fmt.Errorf("mkdir %s: %w", sub, err)
		}
	}
	return folder, nil
}

// copyCodeSnippets reads each file from repoRoot and writes it under
// <folder>/code/<descriptor>.<ext>. Returns the list of descriptors.
//
// Descriptor derivation: take the last two path segments, replace
// "/" and ".", lowercase, strip common prefixes:
//
//	pkg/jira/client.go        → jira_client.go
//	pkg/auth/keystore.go      → auth_keystore.go
//	cmd/plugin-jira/main.go   → plugin_jira_main.go
//	pkg/nexus/plugin_pool.go  → nexus_plugin_pool.go
func copyCodeSnippets(repoRoot string, files []string, folder string) ([]string, error) {
	codeDir := filepath.Join(folder, "code")
	out := make([]string, 0, len(files))
	for _, f := range files {
		src := filepath.Join(repoRoot, f)
		data, err := os.ReadFile(src) //nolint:gosec // G304-WORKSPACE-CANON: repoRoot+rel under operator-supplied path; doc-pack scratch only
		if err != nil {
			return out, fmt.Errorf("read %s: %w", src, err)
		}
		desc := descriptorFromPath(f)
		dst := filepath.Join(codeDir, desc)
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return out, fmt.Errorf("write %s: %w", dst, err)
		}
		out = append(out, desc)
	}
	return out, nil
}

// descriptorFromPath derives a snake_case descriptor from a repo path.
func descriptorFromPath(repoPath string) string {
	clean := filepath.ToSlash(repoPath)
	parts := strings.Split(clean, "/")
	// Strip leading common prefixes that add noise.
	for len(parts) > 0 {
		if parts[0] == "pkg" || parts[0] == "cmd" || parts[0] == "internal" {
			parts = parts[1:]
			continue
		}
		break
	}
	if len(parts) == 0 {
		return filepath.Base(repoPath)
	}
	// Take last 2 path segments (e.g. "jira/client.go") to give the
	// descriptor enough context without being noisy.
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	joined := strings.Join(parts, "_")
	return joined
}

// gitCommitsForFiles runs `git log --oneline [<commitRange>] -- <files>`
// in repoRoot and returns the lines. Empty range = full history of files.
func gitCommitsForFiles(repoRoot string, files []string, commitRange string) ([]string, error) {
	args := []string{"-C", repoRoot, "log", "--oneline"}
	if strings.TrimSpace(commitRange) != "" {
		args = append(args, commitRange)
	}
	args = append(args, "--")
	args = append(args, files...)

	cmd := exec.Command("git", args...) //nolint:gosec // G204-LITERAL-BIN: git binary literal, args from validated repo path + file list
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Dir = repoRoot
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git log: %w (stderr: %s)", err, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	if len(lines) > 50 {
		lines = lines[:50] // cap noise
	}
	return lines, nil
}

// writeDocPackReadme assembles a CONCISE README focusing on what
// changed and where to find it. Sections kept minimal:
//
//   1. Summary (1-2 sentences)
//   2. Cambios (file list with one-line "what" each)
//   3. Commits (1-3 hashes max)
//
// Removed from previous iteration: Snapshots section (redundant with
// images/ folder when present), verbose "Pack auto-generated"
// preamble, full commit log dumps. Goal: README is the at-a-glance
// summary, code/ has the detail.
func writeDocPackReadme(folder string, in PrepareDocPackInput, commits, codeFiles []string) (string, error) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n\n", in.TicketID)

	if strings.TrimSpace(in.Summary) != "" {
		sb.WriteString(in.Summary)
		sb.WriteString("\n\n")
	}

	if len(codeFiles) > 0 {
		sb.WriteString("## Cambios\n\n")
		for i, desc := range codeFiles {
			src := in.Files[i]
			fmt.Fprintf(&sb, "- `%s` (en pack como `code/%s`)\n", src, desc)
		}
		sb.WriteString("\n")
	}

	if len(commits) > 0 {
		sb.WriteString("## Commits\n\n")
		for i := range min(3, len(commits)) {
			fmt.Fprintf(&sb, "- `%s`\n", commits[i])
		}
		sb.WriteString("\n")
	}

	path := filepath.Join(folder, "README.md")
	return path, os.WriteFile(path, []byte(sb.String()), 0o600)
}

// renderAllSnaps walks <folder>/code/ and produces PNGs in <folder>/images/.
// Returns list of generated PNG paths.
func renderAllSnaps(ctx context.Context, folder string) ([]string, error) {
	codeDir := filepath.Join(folder, "code")
	imagesDir := filepath.Join(folder, "images")
	entries, err := os.ReadDir(codeDir)
	if err != nil {
		return nil, fmt.Errorf("read code dir: %w", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := stripAllExt(e.Name())
		srcPath := filepath.Join(codeDir, e.Name())
		pngPath := filepath.Join(imagesDir, base+".png")
		if isUpToDate(pngPath, srcPath) {
			out = append(out, pngPath)
			continue
		}
		if _, err := RenderCodeSnap(ctx, CodeSnapInput{
			SourcePath: srcPath,
			Title:      e.Name(),
			OutPNG:     pngPath,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "render %s: %v\n", srcPath, err)
			continue
		}
		out = append(out, pngPath)
	}
	return out, nil
}
