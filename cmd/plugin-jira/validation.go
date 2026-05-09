// cmd/plugin-jira/validation.go — input sanitization at the action
// boundary (Phase E / Area 3.4 follow-up).
//
// Rationale:
//   The MCP plugin is spawned by Nexus and trusted clients send actions
//   over JSON-RPC. Even with that trust boundary, defense in depth at
//   the dispatch layer prevents a compromised client (or a future bug
//   in Nexus's auth) from leveraging the plugin to read arbitrary host
//   files or hit unintended Jira REST endpoints.
//
//   DeepSeek pro audit on 2026-05-09 surfaced two pre-existing issues
//   in this surface (path traversal in folder_path/repo_root + ticket
//   ID injection in URL paths). This file closes both.

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ticketIDRe matches the canonical Jira issue key format
// "<PROJECT>-<NUMBER>" — e.g., MCPI-1, ABC-12345. Reject anything
// else to prevent URL-path injection like "MCPI-1/../rest/api/3/serverInfo".
//
// Project key spec (Atlassian): 2–10 chars, starts with letter, then
// alphanumerics. Number: 1+ digits.
// [DS-AUDIT 3.4 Finding 3]
var ticketIDRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}-[0-9]+$`)

// validateTicketID returns nil iff s is a syntactically-valid Jira
// issue key. Wrapping with this at the dispatch entry stops malicious
// clients from injecting URL path segments.
func validateTicketID(s string) error {
	if s == "" {
		return errors.New("ticket_id is required")
	}
	if !ticketIDRe.MatchString(s) {
		return fmt.Errorf("ticket_id %q is not a valid Jira issue key (expected like MCPI-1)", s)
	}
	return nil
}

// jiraDocsBase returns the operator's safe upload base
// (~/.neo/jira-docs/) which is the only directory `attach_artifact`
// is allowed to zip. The dir is created on first use so the operator
// doesn't have to mkdir before scripted uploads.
func jiraDocsBase() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("cannot resolve $HOME (set HOME env var)")
	}
	base := filepath.Join(home, ".neo", "jira-docs")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", base, err)
	}
	return base, nil
}

// validateSafeFolderPath resolves the operator-supplied folder_path
// (or the default ~/.neo/jira-docs/<ticketID>) and refuses any path
// that escapes the jira-docs base. This prevents the documented
// exfiltration vector where a client could request
// `folder_path: /etc/ssh` and have the plugin zip + upload host
// secrets to a Jira ticket.
//
// Returns the cleaned absolute path; caller passes it to
// jira.AttachZipFolder. [DS-AUDIT 3.4 Finding 2]
func validateSafeFolderPath(supplied, ticketID string) (string, error) {
	base, err := jiraDocsBase()
	if err != nil {
		return "", err
	}
	candidate := supplied
	if candidate == "" {
		candidate = filepath.Join(base, ticketID)
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("abs %s: %w", candidate, err)
	}
	abs = filepath.Clean(abs)
	if !pathInside(abs, base) {
		return "", fmt.Errorf("folder_path %q must live under %s", supplied, base)
	}
	return abs, nil
}

// validateSafeRepoRoot resolves the operator-supplied repo_root and
// requires it to be an existing absolute path (any directory the
// operator owns). Unlike folder_path we don't anchor under a single
// dir because the operator may run `prepare_doc_pack` against any
// of their git checkouts. Defense: the path MUST exist and be a dir
// (no `/etc/passwd` because that's not a directory; no `../etc`
// because filepath.Clean normalises).
func validateSafeRepoRoot(supplied string) (string, error) {
	if supplied == "" {
		return "", errors.New("repo_root is required")
	}
	abs, err := filepath.Abs(supplied)
	if err != nil {
		return "", fmt.Errorf("abs %s: %w", supplied, err)
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("repo_root %q: %w", supplied, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repo_root %q is not a directory", supplied)
	}
	return abs, nil
}

// pathInside returns true iff target == base or target is a strict
// descendant of base. Both arguments must already be cleaned + absolute.
// We use string-prefix because filepath.Rel can return relative paths
// that look "inside" but go through `..`; the explicit prefix check
// after Clean is the canonical safe pattern.
func pathInside(target, base string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, "..") && !strings.Contains(rel, string(filepath.Separator)+"..")
}
