package main

// backup_loop.go — hourly tick that ensures observability.db is backed
// up at least once every 20 h. Rolls retention to 7 dated snapshots.
// [PILAR-XXVII/243.D]

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/observability"
)

const (
	backupCheckInterval = 1 * time.Hour
	backupMinInterval   = 20 * time.Hour
	backupRetentionKeep = 7
	purgeInterval       = 6 * time.Hour
)

// startBackupLoop launches the backup + daily bucket purge goroutine.
// No-op when the Store wasn't initialised.
func startBackupLoop(ctx context.Context, workspace string) {
	s := observability.GlobalStore
	if s == nil {
		log.Printf("[SRE-WARN] backup loop disabled — observability store not initialised")
		return
	}
	go func() {
		backupTick := time.NewTicker(backupCheckInterval)
		purgeTick := time.NewTicker(purgeInterval)
		defer backupTick.Stop()
		defer purgeTick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-backupTick.C:
				maybeRunBackup(s, workspace)
			case <-purgeTick.C:
				if removed, err := s.PurgeOldDays(observability.DefaultRetentionDays); err != nil {
					log.Printf("[SRE-WARN] purge old buckets: %v", err)
				} else if removed > 0 {
					log.Printf("[OBS] purged %d daily buckets older than %d days", removed, observability.DefaultRetentionDays)
				}
			}
		}
	}()
}

// maybeRunBackup creates a snapshot only when the last one is older than
// backupMinInterval. First-ever backup runs immediately on first tick.
func maybeRunBackup(s *observability.Store, workspace string) {
	last := s.LastBackupAt()
	if !last.IsZero() && time.Since(last) < backupMinInterval {
		return
	}
	dir := filepath.Join(workspace, ".neo", "db")
	stamp := time.Now().UTC().Format("20060102")
	dest := filepath.Join(dir, fmt.Sprintf("observability-%s.db.bak", stamp))
	if err := s.CreateBackup(dest); err != nil {
		log.Printf("[SRE-WARN] observability backup failed: %v", err)
		return
	}
	log.Printf("[OBS] backup written: %s", dest)
	if removed, err := s.RotateBackups(backupRetentionKeep); err != nil {
		log.Printf("[SRE-WARN] rotate backups: %v", err)
	} else if removed > 0 {
		log.Printf("[OBS] rotated %d old backup(s)", removed)
	}
}
