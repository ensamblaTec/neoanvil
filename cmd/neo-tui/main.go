// neo-tui is the single-workspace observability TUI for NeoAnvil.
// [PILAR-XXVII/246.A]
//
// Invocations:
//   ./bin/neo-tui                     — auto-detect workspace (walk-up
//                                       from PWD looking for neo.yaml,
//                                       fall back to the single
//                                       running workspace in Nexus).
//   ./bin/neo-tui --workspace <id>    — explicit target.
//   ./bin/neo-tui --nexus <url>       — point at a non-default Nexus.
//
// Backend: Nexus on http://127.0.0.1:9000 (configurable). The TUI only
// ever reads — it never mutates state.

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("[neo-tui] ")

	fsWorkspace := flag.String("workspace", "", "workspace id or name (env: NEO_WORKSPACE_ID)")
	fsNexus := flag.String("nexus", "http://127.0.0.1:9000", "base URL of neo-nexus")
	flag.Parse()

	wsID := *fsWorkspace
	if wsID == "" {
		wsID = os.Getenv("NEO_WORKSPACE_ID")
	}

	client := NewClient(*fsNexus)

	if wsID == "" {
		resolved, err := resolveWorkspace(client)
		if err != nil {
			log.Fatalf("workspace resolution failed: %v\n\nOptions:\n  --workspace <id>\n  NEO_WORKSPACE_ID=<id>\n  run from inside a workspace (so walk-up finds neo.yaml)\n  start at least one workspace under Nexus", err)
		}
		wsID = resolved
	}

	// Human-readable label — derived from the walk-up filename when
	// available, otherwise the wsID itself.
	label := wsID
	if name := walkUpName(); name != "" {
		label = fmt.Sprintf("%s (%s)", name, wsID)
	}

	p := tea.NewProgram(newModel(client, wsID, label), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		log.Fatalf("bubbletea: %v", err)
	}
}

// resolveWorkspace tries, in order:
//  1. walk-up neo.yaml → derive hash-based id (not implemented here;
//     we fall back to Nexus).
//  2. GET /status — if exactly one is StatusRunning, use it.
func resolveWorkspace(c *Client) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	items, err := c.FetchStatus(ctx)
	if err != nil {
		return "", fmt.Errorf("fetch status: %w", err)
	}
	running := filterRunning(items)
	switch len(running) {
	case 0:
		return "", fmt.Errorf("no workspaces running in Nexus at %s", c.NexusBase)
	case 1:
		return running[0].ID, nil
	default:
		ids := make([]string, 0, len(running))
		for _, r := range running {
			ids = append(ids, fmt.Sprintf("%s (%s)", r.Name, r.ID))
		}
		return "", fmt.Errorf("multiple running workspaces — pick one with --workspace:\n  %s",
			strings.Join(ids, "\n  "))
	}
}

func filterRunning(items []WorkspaceSummary) []WorkspaceSummary {
	out := make([]WorkspaceSummary, 0, len(items))
	for _, it := range items {
		if it.Status == "running" {
			out = append(out, it)
		}
	}
	return out
}

// walkUpName returns the basename of the nearest directory containing
// a neo.yaml, or "" if none found.
func walkUpName() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	dir := cwd
	for i := 0; i < 12; i++ {
		if _, err := os.Stat(filepath.Join(dir, "neo.yaml")); err == nil {
			return filepath.Base(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}
