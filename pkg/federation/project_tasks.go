package federation

// project_tasks.go — File-based shared task queue for federation member
// workspaces. Tasks pushed with `scope:"project"` land here; each workspace's
// daemon PullTasks consumes those targeting it (or "*"). [PILAR LXV / 349.A MVP]
//
// Persistence: `.neo-project/PROJECT_TASKS.md` with JSON-in-HTML-comment +
// rendered markdown tables (same pattern as CONTRACT_PROPOSALS.md,
// SHARED_DEBT.md). Concurrency: package-level sync.Mutex serializes all
// writes across goroutines; atomic file-replace via tmp+rename prevents
// torn reads from readers. Locking across workspaces relies on SharedGraph's
// POSIX flock when both workspaces are on the same host.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ProjectTask is a unit of work posted to the federation shared queue. Mirrors
// state.SRETask but adds routing fields used by PullTasks. [349.A]
type ProjectTask struct {
	ID              string    `json:"id"`
	Description     string    `json:"description"`
	TargetFile      string    `json:"target_file,omitempty"`
	Role            string    `json:"role,omitempty"`
	TargetWorkspace string    `json:"target_workspace"` // workspace ID or "*" for any claimant
	AffinityTags    []string  `json:"affinity_tags,omitempty"`
	Status          string    `json:"status"` // pending | in_progress | completed
	ClaimedBy       string    `json:"claimed_by,omitempty"`
	ClaimedAt       time.Time `json:"claimed_at"`
	CreatedAt       time.Time `json:"created_at"`
	CompletedAt     time.Time `json:"completed_at"`
	Result          string    `json:"result,omitempty"`
}

var (
	// ErrProjectTaskNotFound is returned by ClaimProjectTask/CompleteProjectTask
	// when the referenced task is absent.
	ErrProjectTaskNotFound = errors.New("project_tasks: task not found")
	// ErrProjectTaskAlreadyClaimed is returned by ClaimProjectTask when the
	// task is no longer pending.
	ErrProjectTaskAlreadyClaimed = errors.New("project_tasks: already claimed")
)

const projectTasksFile = "PROJECT_TASKS.md"
const ptJSONOpen = "<!-- neo-project-tasks-v1\n"
const ptJSONClose = "\n-->\n"

var ptJSONBlock = regexp.MustCompile(`(?s)<!-- neo-project-tasks-v1\n(.*?)\n-->`)

var ptMu sync.Mutex

// AppendProjectTask persists a new pending task to PROJECT_TASKS.md. ID is
// auto-generated. [349.A]
func AppendProjectTask(projDir string, t ProjectTask) (ProjectTask, error) {
	ptMu.Lock()
	defer ptMu.Unlock()
	if projDir == "" {
		return ProjectTask{}, errors.New("project_tasks: empty projDir")
	}
	if t.Description == "" {
		return ProjectTask{}, errors.New("project_tasks: description required")
	}
	path := filepath.Join(projDir, projectTasksFile)
	existing, err := loadProjectTasks(path)
	if err != nil {
		return ProjectTask{}, err
	}
	if t.ID == "" {
		t.ID = newProjectTaskID(time.Now().UTC())
	}
	if t.Status == "" {
		t.Status = "pending"
	}
	if t.TargetWorkspace == "" {
		t.TargetWorkspace = "*"
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	existing = append(existing, t)
	if err := writeProjectTasks(path, existing); err != nil {
		return ProjectTask{}, err
	}
	return t, nil
}

// ClaimProjectTask atomically transitions the first matching pending task to
// in_progress. `workspaceID` is recorded as ClaimedBy. Tasks with
// TargetWorkspace=="*" match any claimant; others require an exact match. [349.A]
func ClaimProjectTask(projDir, workspaceID string) (ProjectTask, error) {
	ptMu.Lock()
	defer ptMu.Unlock()
	if projDir == "" || workspaceID == "" {
		return ProjectTask{}, errors.New("project_tasks: empty projDir or workspaceID")
	}
	path := filepath.Join(projDir, projectTasksFile)
	all, err := loadProjectTasks(path)
	if err != nil {
		return ProjectTask{}, err
	}
	for i := range all {
		t := &all[i]
		if t.Status != "pending" {
			continue
		}
		if t.TargetWorkspace != "*" && t.TargetWorkspace != workspaceID {
			continue
		}
		t.Status = "in_progress"
		t.ClaimedBy = workspaceID
		t.ClaimedAt = time.Now().UTC()
		if err := writeProjectTasks(path, all); err != nil {
			return ProjectTask{}, err
		}
		return *t, nil
	}
	return ProjectTask{}, ErrProjectTaskNotFound
}

// CompleteProjectTask transitions in_progress → completed with an optional
// result summary. [349.A]
func CompleteProjectTask(projDir, id, result string) error {
	ptMu.Lock()
	defer ptMu.Unlock()
	if projDir == "" || id == "" {
		return errors.New("project_tasks: empty projDir or id")
	}
	path := filepath.Join(projDir, projectTasksFile)
	all, err := loadProjectTasks(path)
	if err != nil {
		return err
	}
	for i := range all {
		if all[i].ID != id {
			continue
		}
		all[i].Status = "completed"
		all[i].Result = result
		all[i].CompletedAt = time.Now().UTC()
		return writeProjectTasks(path, all)
	}
	return ErrProjectTaskNotFound
}

// ListProjectTasks returns all tasks, or (nil, nil) when the file is absent. [349.A]
func ListProjectTasks(projDir string) ([]ProjectTask, error) {
	ptMu.Lock()
	defer ptMu.Unlock()
	if projDir == "" {
		return nil, nil
	}
	return loadProjectTasks(filepath.Join(projDir, projectTasksFile))
}

func loadProjectTasks(path string) ([]ProjectTask, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304-WORKSPACE-CANON: projDir under project root
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("project_tasks: read: %w", err)
	}
	m := ptJSONBlock.FindSubmatch(data)
	if m == nil {
		return nil, nil
	}
	var payload struct {
		Tasks []ProjectTask `json:"tasks"`
	}
	if err := json.Unmarshal(m[1], &payload); err != nil {
		return nil, fmt.Errorf("project_tasks: parse: %w", err)
	}
	return payload.Tasks, nil
}

func writeProjectTasks(path string, all []ProjectTask) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("project_tasks: mkdir: %w", err)
	}
	payload := struct {
		Tasks []ProjectTask `json:"tasks"`
	}{Tasks: all}
	jsonBytes, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("project_tasks: marshal: %w", err)
	}
	var sb strings.Builder
	sb.WriteString("# Project Task Queue\n\n")
	sb.WriteString("> Cross-workspace tasks delegated via `neo_daemon(PushTasks, scope:\"project\")`.\n")
	sb.WriteString("> Each member daemon claims tasks with matching TargetWorkspace (or \"*\").\n\n")
	sb.WriteString(ptJSONOpen)
	sb.Write(jsonBytes)
	sb.WriteString(ptJSONClose)
	sb.WriteString("\n")
	sb.WriteString(renderProjectTasksTables(all))

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o600); err != nil { //nolint:gosec // G306 0o600 tight
		return fmt.Errorf("project_tasks: write tmp: %w", err)
	}
	return os.Rename(tmp, path)
}

func renderProjectTasksTables(all []ProjectTask) string {
	var pending, inProgress, completed []ProjectTask
	for _, t := range all {
		switch t.Status {
		case "pending":
			pending = append(pending, t)
		case "in_progress":
			inProgress = append(inProgress, t)
		case "completed":
			completed = append(completed, t)
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Pending (%d)\n\n", len(pending))
	if len(pending) == 0 {
		sb.WriteString("_none_\n\n")
	} else {
		sb.WriteString("| ID | Target | Role | Description | Created |\n|----|--------|------|-------------|---------|\n")
		for _, t := range pending {
			fmt.Fprintf(&sb, "| `%s` | %s | %s | %s | %s |\n",
				t.ID, t.TargetWorkspace, t.Role, truncate(t.Description, 60),
				t.CreatedAt.Format("2006-01-02 15:04"))
		}
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "## In Progress (%d)\n\n", len(inProgress))
	if len(inProgress) == 0 {
		sb.WriteString("_none_\n\n")
	} else {
		sb.WriteString("| ID | Claimed By | Claimed At | Description |\n|----|-----------|-----------|-------------|\n")
		for _, t := range inProgress {
			fmt.Fprintf(&sb, "| `%s` | %s | %s | %s |\n",
				t.ID, t.ClaimedBy, t.ClaimedAt.Format("2006-01-02 15:04"),
				truncate(t.Description, 60))
		}
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "## Completed (%d)\n\n", len(completed))
	if len(completed) == 0 {
		sb.WriteString("_none_\n")
	} else {
		sb.WriteString("| ID | Claimed By | Completed | Result |\n|----|-----------|-----------|--------|\n")
		for _, t := range completed {
			fmt.Fprintf(&sb, "| `%s` | %s | %s | %s |\n",
				t.ID, t.ClaimedBy, t.CompletedAt.Format("2006-01-02 15:04"),
				truncate(t.Result, 60))
		}
	}
	return sb.String()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func newProjectTaskID(t time.Time) string {
	suffix := rand.Intn(0xFFFF) //nolint:gosec // G404: non-crypto ID for display
	return fmt.Sprintf("pt-%s-%04x", t.Format("2006-01-02"), suffix)
}
