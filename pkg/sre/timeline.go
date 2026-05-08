package sre

// [SRE-89.A] Temporal Causal Replay — mutation timeline for time-travel debugging.
//
// Every successful neo_sre_certify_mutation call records a MutationSnapshot
// in the timeline. The timeline is indexed by timestamp and queryable by
// date range or target file. The neo_time_travel tool uses this to navigate
// the history of mutations and detect BLAST_RADIUS divergence.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// MutationSnapshot captures the state at the moment of a successful certification. [SRE-89.A.1]
type MutationSnapshot struct {
	ID           string   `json:"id"`                // SHA256(timestamp + files)
	Timestamp    int64    `json:"timestamp"`         // Unix epoch
	GitSHA       string   `json:"git_sha"`           // HEAD at snapshot time
	MutatedFiles []string `json:"mutated_files"`     // absolute paths
	ASTHashes    []string `json:"ast_hashes"`        // SHA256 of each file's AST
	BlastEdges   int      `json:"blast_edges"`       // impacted_count at certification time
	Complexity   string   `json:"complexity_intent"` // O(1), FEATURE_ADD, etc.
}

// Timeline is a thread-safe, in-memory chronological index of mutation snapshots. [SRE-89.A.2]
// Backed by BoltDB via Persist/Load methods for crash recovery.
type Timeline struct {
	mu        sync.RWMutex
	snapshots []MutationSnapshot
	maxSize   int
}

// NewTimeline creates a timeline with the given capacity.
func NewTimeline(maxSize int) *Timeline {
	if maxSize <= 0 {
		maxSize = 500
	}
	return &Timeline{
		snapshots: make([]MutationSnapshot, 0, maxSize),
		maxSize:   maxSize,
	}
}

// Record adds a mutation snapshot to the timeline. [SRE-89.A.1]
func (t *Timeline) Record(snap MutationSnapshot) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if snap.ID == "" {
		h := sha256.Sum256(fmt.Appendf(nil, "%d:%s", snap.Timestamp, strings.Join(snap.MutatedFiles, ",")))
		snap.ID = hex.EncodeToString(h[:8])
	}
	if snap.Timestamp == 0 {
		snap.Timestamp = time.Now().Unix()
	}
	if snap.GitSHA == "" {
		snap.GitSHA = currentGitSHA()
	}

	t.snapshots = append(t.snapshots, snap)
	if len(t.snapshots) > t.maxSize {
		t.snapshots = t.snapshots[len(t.snapshots)-t.maxSize:]
	}
}

// QueryRange returns snapshots between fromUnix and toUnix (inclusive). [SRE-89.A.3]
func (t *Timeline) QueryRange(fromUnix, toUnix int64) []MutationSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var result []MutationSnapshot
	for _, s := range t.snapshots {
		if s.Timestamp >= fromUnix && s.Timestamp <= toUnix {
			result = append(result, s)
		}
	}
	return result
}

// QueryFile returns snapshots that include the given file path. [SRE-89.A.3]
func (t *Timeline) QueryFile(filePath string) []MutationSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var result []MutationSnapshot
	for _, s := range t.snapshots {
		for _, f := range s.MutatedFiles {
			if f == filePath || strings.HasSuffix(f, filePath) {
				result = append(result, s)
				break
			}
		}
	}
	return result
}

// All returns a copy of all snapshots in chronological order.
func (t *Timeline) All() []MutationSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]MutationSnapshot, len(t.snapshots))
	copy(out, t.snapshots)
	return out
}

// Len returns the number of snapshots in the timeline.
func (t *Timeline) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.snapshots)
}

// MarshalJSON serializes the timeline for BoltDB persistence.
func (t *Timeline) MarshalJSON() ([]byte, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return json.Marshal(t.snapshots)
}

// UnmarshalJSON restores the timeline from BoltDB data.
func (t *Timeline) UnmarshalJSON(data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return json.Unmarshal(data, &t.snapshots)
}

// DetectBlastDivergence compares consecutive snapshots for the same file
// and returns pairs where blast_edges jumped more than 3×. [SRE-89.B.2]
func (t *Timeline) DetectBlastDivergence(filePath string) []DivergencePoint {
	snapshots := t.QueryFile(filePath)
	if len(snapshots) < 2 {
		return nil
	}

	var points []DivergencePoint
	for i := 1; i < len(snapshots); i++ {
		prev := snapshots[i-1]
		curr := snapshots[i]
		if prev.BlastEdges > 0 && curr.BlastEdges > prev.BlastEdges*3 {
			points = append(points, DivergencePoint{
				Before:   prev,
				After:    curr,
				Ratio:    float64(curr.BlastEdges) / float64(prev.BlastEdges),
				FilePath: filePath,
			})
		}
	}
	return points
}

// DivergencePoint marks where BLAST_RADIUS jumped unexpectedly. [SRE-89.B.2]
type DivergencePoint struct {
	Before   MutationSnapshot `json:"before"`
	After    MutationSnapshot `json:"after"`
	Ratio    float64          `json:"ratio"` // after.BlastEdges / before.BlastEdges
	FilePath string           `json:"file_path"`
}

// BisectResult is the output of an automated git bisect run. [SRE-89.B.1]
type BisectResult struct {
	CulpritSHA  string `json:"culprit_sha"`
	CulpritMsg  string `json:"culprit_message"`
	TestedCount int    `json:"tested_count"`
	TotalRange  int    `json:"total_range"`
	Duration    string `json:"duration"`
	Error       string `json:"error,omitempty"`
}

// AutoBisect runs git bisect in the workspace to find the first failing commit
// between goodSHA and badSHA. Fuel limit: max 50 commits or 5 minutes. [SRE-89.B.1]
func AutoBisect(workspace, goodSHA, badSHA string) (*BisectResult, error) {
	start := time.Now()
	maxDuration := 5 * time.Minute
	maxCommits := 50

	// Get commit range.
	out, err := exec.Command("git", "-C", workspace, "rev-list", "--count", goodSHA+".."+badSHA).Output()
	if err != nil {
		return nil, fmt.Errorf("rev-list: %w", err)
	}
	var totalRange int
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &totalRange)
	if totalRange > maxCommits {
		totalRange = maxCommits
	}

	// Get the commit list.
	listOut, err := exec.Command("git", "-C", workspace, "rev-list", "--reverse", goodSHA+".."+badSHA).Output()
	if err != nil {
		return nil, fmt.Errorf("rev-list: %w", err)
	}
	commits := strings.Split(strings.TrimSpace(string(listOut)), "\n")
	if len(commits) > maxCommits {
		commits = commits[:maxCommits]
	}

	// Binary search for first failing commit.
	lo, hi := 0, len(commits)-1
	tested := 0
	lastGood := goodSHA

	for lo <= hi {
		if time.Since(start) > maxDuration {
			return &BisectResult{
				CulpritSHA:  "timeout",
				TestedCount: tested,
				TotalRange:  totalRange,
				Duration:    time.Since(start).String(),
				Error:       "bisect timed out after " + maxDuration.String(),
			}, nil
		}

		mid := (lo + hi) / 2
		sha := commits[mid]
		tested++

		// Checkout and test.
		if err := exec.Command("git", "-C", workspace, "checkout", "--quiet", sha).Run(); err != nil {
			// Restore HEAD before returning.
			_ = exec.Command("git", "-C", workspace, "checkout", "--quiet", "-").Run()
			return nil, fmt.Errorf("checkout %s: %w", sha[:8], err)
		}

		buildCmd := exec.Command("go", "build", "./...")
		buildCmd.Dir = workspace
		buildErr := buildCmd.Run()

		if buildErr != nil {
			hi = mid - 1
		} else {
			lastGood = sha
			lo = mid + 1
		}
	}

	// Restore HEAD.
	_ = exec.Command("git", "-C", workspace, "checkout", "--quiet", "-").Run()

	culprit := ""
	if lo < len(commits) {
		culprit = commits[lo]
	} else if lastGood != goodSHA {
		culprit = lastGood
	}

	// Get commit message.
	culpritMsg := ""
	if culprit != "" {
		if msgOut, err := exec.Command("git", "-C", workspace, "log", "-1", "--format=%s", culprit).Output(); err == nil {
			culpritMsg = strings.TrimSpace(string(msgOut))
		}
	}

	return &BisectResult{
		CulpritSHA:  culprit,
		CulpritMsg:  culpritMsg,
		TestedCount: tested,
		TotalRange:  totalRange,
		Duration:    time.Since(start).String(),
	}, nil
}

// currentGitSHA returns the current HEAD SHA (short) or "unknown".
func currentGitSHA() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// FormatTimelineReport produces a Markdown report of the mutation timeline. [SRE-89.A.3]
func FormatTimelineReport(snapshots []MutationSnapshot, divergences []DivergencePoint) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("## Temporal Causal Replay — %d mutation(s)\n\n", len(snapshots)))

	if len(divergences) > 0 {
		b.WriteString(fmt.Sprintf("### ⚠️ %d BLAST_RADIUS Divergence(s) Detected\n\n", len(divergences)))
		for _, d := range divergences {
			b.WriteString(fmt.Sprintf("- **%s** @ git %s → %s: blast edges %d → %d (%.1f×)\n",
				d.FilePath, d.Before.GitSHA, d.After.GitSHA,
				d.Before.BlastEdges, d.After.BlastEdges, d.Ratio))
		}
		b.WriteString("\n")
	}

	b.WriteString("| Timestamp | Git SHA | Files | Blast Edges | Complexity |\n")
	b.WriteString("|-----------|---------|-------|-------------|------------|\n")
	for _, s := range snapshots {
		ts := time.Unix(s.Timestamp, 0).Format("2006-01-02 15:04")
		files := strings.Join(s.MutatedFiles, ", ")
		if len(files) > 60 {
			files = files[:57] + "..."
		}
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %d | %s |\n", ts, s.GitSHA, files, s.BlastEdges, s.Complexity))
	}
	return b.String()
}
