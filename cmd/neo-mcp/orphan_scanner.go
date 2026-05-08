package main

// orphan_scanner.go — background goroutine that detects and records stalled
// delegate tasks (lifecycle_state="in_progress" past TaskOrphanTimeoutMin). [362.A]

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/kanban"
	"github.com/ensamblatec/neoanvil/pkg/state"
)

// startOrphanScanner launches a background goroutine that wakes every 15 minutes,
// scans in_progress tasks older than cfg.SRE.TaskOrphanTimeoutMin, marks them
// orphaned, and writes a tech-debt entry for each. Non-fatal: all errors are logged
// but never propagate. Goroutine exits when ctx is cancelled. [362.A]
func startOrphanScanner(ctx context.Context, workspace string, cfg *config.NeoConfig) {
	ticker := time.NewTicker(15 * time.Minute)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runOrphanScan(workspace, cfg)
			}
		}
	}()
}

func runOrphanScan(workspace string, cfg *config.NeoConfig) {
	timeoutMin := cfg.SRE.TaskOrphanTimeoutMin
	if timeoutMin <= 0 {
		timeoutMin = 60
	}
	orphans, err := state.MarkTasksOrphaned(timeoutMin)
	if err != nil {
		log.Printf("[SRE-362.A] orphan scan error: %v", err)
		return
	}
	for _, t := range orphans {
		log.Printf("[SRE-362.A] task orphaned after %dm: %s (%s)", timeoutMin, t.ID, t.Description)
		title := fmt.Sprintf("Orphaned delegate task: %s", t.ID)
		description := fmt.Sprintf(
			"Task %q was claimed (in_progress) but not completed within %d minutes.\n"+
				"Description: %s\nTarget: %s\n"+
				"**Action:** re-enqueue via neo_daemon PushTasks or mark resolved manually.",
			t.ID, timeoutMin, t.Description, t.TargetFile,
		)
		if kerr := kanban.AppendTechDebt(workspace, title, description, "P2"); kerr != nil {
			log.Printf("[SRE-362.A] failed to write debt for orphaned task %s: %v", t.ID, kerr)
		}
	}
}
