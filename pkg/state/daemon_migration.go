// Package state — daemon trust-system migration shim. [PILAR XXVII / 138.C.7]
//
// MigrateLegacyTasks marks every active SRETask (pending or in_progress)
// with a `migrated_at` timestamp so the daemon's iterative loop knows
// the task pre-existed the trust system (138.B). It also seeds the
// catch-all (pattern:"unknown", scope:"unknown:unknown") TrustScore
// with the uniform (1, 1) prior so legacy tasks have a non-empty trust
// bucket on the first execute_next call.
//
// Idempotent: re-runs are no-ops once `migrated_at` is set on every
// task. Safe to call on every boot — the typical path is to invoke
// once after InitPlanner and ignore the (migrated, err) return when
// migrated == 0.
//
// Skips completed and failed_permanent tasks — those are terminal and
// don't need a trust handshake.
package state

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"go.etcd.io/bbolt"
)

// legacyMigrationPattern + legacyMigrationScope is the (pattern, scope)
// bucket that all legacy un-classifiable tasks land under. Matches the
// fallback emitted by ResolvePatternScope for empty descriptions, so
// future execute_next calls against legacy tasks bucket consistently.
const (
	legacyMigrationPattern = "unknown"
	legacyMigrationScope   = "unknown:unknown"
)

// MigrateLegacyTasks runs the boot-time migration sweep. Returns the
// count of tasks newly stamped with migrated_at, plus the seed status
// for the unknown:unknown TrustScore.
//
// The returned `migrated` count is informational — operators can read
// it from the boot log to verify the migration ran. After the first
// successful run on any installation, all subsequent calls return 0.
// [138.C.7]
func MigrateLegacyTasks() (migrated int, err error) {
	if plannerDB == nil {
		return 0, fmt.Errorf("plannerDB offline")
	}
	now := time.Now().Unix()
	err = plannerDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var t SRETask
			if jerr := json.Unmarshal(v, &t); jerr != nil {
				log.Printf("[MIGRATE] skipping corrupt task key=%q: %v", string(k), jerr)
				return nil
			}
			if t.MigratedAt > 0 {
				return nil // already migrated
			}
			// Skip terminal lifecycle states — completed/failed tasks
			// don't need a trust handshake.
			if t.LifecycleState == TaskLifecycleCompleted || t.LifecycleState == TaskLifecycleFailedPermanent {
				return nil
			}
			t.MigratedAt = now
			val, merr := json.Marshal(t)
			if merr != nil {
				return merr
			}
			if perr := b.Put(k, val); perr != nil {
				return perr
			}
			migrated++
			return nil
		})
	})
	if err != nil {
		return migrated, err
	}

	// Seed the unknown:unknown TrustScore so the bucket is non-empty
	// when the first legacy task hits execute_next. TrustGet auto-creates
	// the prior on read (or returns the existing one), but going through
	// TrustUpdate persists it explicitly so trust_status surfaces it
	// even before any execution.
	if uerr := TrustUpdate(legacyMigrationPattern, legacyMigrationScope, func(s *TrustScore) {
		// No-op fn — we just want to ensure the entry exists with the
		// uniform prior. NewTrustScore was already called inside the
		// transaction by TrustUpdate when the entry was missing.
	}); uerr != nil {
		log.Printf("[MIGRATE] trust seed unknown:unknown failed: %v (non-fatal)", uerr)
	}

	if migrated > 0 {
		log.Printf("[MIGRATE] stamped migrated_at on %d legacy task(s); seeded %s:%s trust prior",
			migrated, legacyMigrationPattern, legacyMigrationScope)
	}
	return migrated, nil
}
