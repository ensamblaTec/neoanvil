package main

// briefing_session_context.go — git state, tooling state, and recent epics
// for the BRIEFING compact and full modes. [127.1-127.4]

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// populateGitState runs `git status --porcelain -b` with a 200ms timeout. [127.1]
// Output: "git: <branch> ↔ origin (<ahead>/<behind>) <clean|N changes>"
// Returns "" on any error (fail-open).
func populateGitState(workspace string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	//nolint:gosec // G204-LITERAL-BIN: fixed "git" binary + workspace validated at boot
	cmd := exec.CommandContext(ctx, "git", "-C", workspace, "status", "--porcelain", "-b")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return parseGitStatus(out)
}

// parseGitStatus parses `git status --porcelain -b` output. [127.1]
func parseGitStatus(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "## ") {
		return ""
	}
	branch, ahead, behind := parseBranchLine(lines[0][3:])
	changed := 0
	for _, l := range lines[1:] {
		if l != "" {
			changed++
		}
	}
	state := "clean"
	if changed > 0 {
		state = fmt.Sprintf("%d changes", changed)
	}
	return fmt.Sprintf("git: %s ↔ origin (%d/%d) %s", branch, ahead, behind, state)
}

// parseBranchLine extracts (branch, ahead, behind) from the porcelain -b header. [127.1]
// Input example: "feature/neoanvil-v5...origin/feature/neoanvil-v5 [ahead 2, behind 1]"
func parseBranchLine(s string) (branch string, ahead, behind int) {
	parts := strings.SplitN(s, "...", 2)
	branch = strings.TrimSpace(parts[0])
	if len(parts) < 2 {
		return branch, 0, 0
	}
	rest := parts[1]
	reAhead := regexp.MustCompile(`ahead (\d+)`)
	reBehind := regexp.MustCompile(`behind (\d+)`)
	if m := reAhead.FindStringSubmatch(rest); len(m) == 2 {
		ahead, _ = strconv.Atoi(m[1])
	}
	if m := reBehind.FindStringSubmatch(rest); len(m) == 2 {
		behind, _ = strconv.Atoi(m[1])
	}
	return branch, ahead, behind
}

// populateToolingState checks hooks, skills, and output style. [127.2]
// Output: "hooks: post-commit:✓|✗ · style: <name> · skills: N (A auto, T task)"
// Returns "" on any error (fail-open).
func populateToolingState(workspace string) string {
	hookIcon := "✗"
	// Lstat instead of Stat: if .git/hooks/post-commit is a symlink, Stat would
	// follow it and fail when the target is momentarily unreachable (e.g. cold
	// filesystem cache right after make rebuild-restart), reporting ✗ even
	// though the hook is installed. Lstat verifies the symlink itself exists.
	if _, err := os.Lstat(filepath.Join(workspace, ".git/hooks/post-commit")); err == nil {
		hookIcon = "✓"
	}

	skillFiles, _ := filepath.Glob(filepath.Join(workspace, ".claude/skills/*/SKILL.md"))
	autoCount, taskCount := 0, 0
	for _, sf := range skillFiles {
		data, err := os.ReadFile(sf) //nolint:gosec // G304-DIR-WALK: path from filepath.Glob within workspace
		if err != nil {
			continue
		}
		if bytes.Contains(data, []byte("disable-model-invocation: true")) {
			taskCount++
		} else {
			autoCount++
		}
	}

	style := "default"
	settingsPath := filepath.Join(workspace, ".claude/settings.local.json")
	if data, err := os.ReadFile(settingsPath); err == nil { //nolint:gosec // G304-WORKSPACE-CANON: settings.local.json in workspace/.claude/
		const key = `"outputStyle"`
		if idx := bytes.Index(data, []byte(key)); idx >= 0 {
			rest := data[idx+len(key):]
			if colon := bytes.IndexByte(rest, ':'); colon >= 0 {
				rest = bytes.TrimSpace(rest[colon+1:])
				if len(rest) > 0 && rest[0] == '"' {
					rest = rest[1:]
					if end := bytes.IndexByte(rest, '"'); end >= 0 {
						style = string(rest[:end])
					}
				}
			}
		}
	}

	return fmt.Sprintf("hooks: post-commit:%s · style: %s · skills: %d (%d auto, %d task)",
		hookIcon, style, len(skillFiles), autoCount, taskCount)
}

// populateRecentEpics scans .neo/master_plan.md for closed épicas. [127.3]
// A section is closed when it has task lines and none are "- [ ]".
// Output: "last_epics: N, M, K" (last 3 closed, in order of appearance).
// Returns "" when no closed épicas found or on read error.
func populateRecentEpics(workspace string) string {
	planPath := filepath.Join(workspace, ".neo/master_plan.md")
	data, err := os.ReadFile(planPath) //nolint:gosec // G304-WORKSPACE-CANON: fixed .neo/master_plan.md path in workspace
	if err != nil {
		return ""
	}
	closed := collectClosedEpics(data)
	if len(closed) == 0 {
		return ""
	}
	if len(closed) > 3 {
		closed = closed[len(closed)-3:]
	}
	nums := make([]string, len(closed))
	for i, n := range closed {
		nums[i] = strconv.Itoa(n)
	}
	return "last_epics: " + strings.Join(nums, ", ")
}

// collectClosedEpics scans plan content for ## ÉPICA N sections where all
// task lines are [x] (no open [ ] tasks). Returns épica numbers in order. [127.3]
func collectClosedEpics(data []byte) []int {
	reEpica := regexp.MustCompile(`^## ÉPICA (\d+)`)
	var closed []int
	var currentEpic int
	hasOpen := false
	hasTasks := false

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if m := reEpica.FindStringSubmatch(line); len(m) == 2 {
			if currentEpic > 0 && hasTasks && !hasOpen {
				closed = append(closed, currentEpic)
			}
			n, _ := strconv.Atoi(m[1])
			currentEpic = n
			hasOpen = false
			hasTasks = false
			continue
		}
		if currentEpic == 0 {
			continue
		}
		if strings.HasPrefix(line, "- [x]") {
			hasTasks = true
		} else if strings.HasPrefix(line, "- [ ]") {
			hasTasks = true
			hasOpen = true
		}
	}
	if currentEpic > 0 && hasTasks && !hasOpen {
		closed = append(closed, currentEpic)
	}
	return closed
}

// appendSessionContextLines writes the 3 session-context lines when non-empty. [127.4]
// Used in compact mode and auto-compact mode.
func appendSessionContextLines(sb *strings.Builder, d *briefingData) {
	if d.gitState != "" {
		sb.WriteString(d.gitState + "\n")
	}
	if d.toolingState != "" {
		sb.WriteString(d.toolingState + "\n")
	}
	if d.recentEpics != "" {
		sb.WriteString(d.recentEpics + "\n")
	}
}

// appendSessionContextSection renders a ### Session context block for full mode. [127.4]
func appendSessionContextSection(sb *strings.Builder, d briefingData) {
	if d.gitState == "" && d.toolingState == "" && d.recentEpics == "" {
		return
	}
	sb.WriteString("\n### Session context\n\n")
	if d.gitState != "" {
		fmt.Fprintf(sb, "- **git:** %s\n", strings.TrimPrefix(d.gitState, "git: "))
	}
	if d.toolingState != "" {
		fmt.Fprintf(sb, "- **tooling:** %s\n", strings.TrimPrefix(d.toolingState, "hooks: "))
	}
	if d.recentEpics != "" {
		fmt.Fprintf(sb, "- **%s**\n", d.recentEpics)
	}
}
