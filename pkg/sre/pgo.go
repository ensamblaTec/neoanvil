package sre

// pgo.go — Continuous PGO profile capture with 24h rotation. [PILAR LXIX / 364.C]
//
// A long-running neo-mcp process periodically captures a 60-second CPU
// profile into `<workspace>/.neo/pgo/profile-<unix>.pgo`. Older profiles
// (> RotateMax) are pruned at each tick. The operator's build pipeline picks
// the most recent sample via `make build-pgo`.

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sort"
	"strings"
	"time"
)

const pgoDirName = "pgo"
const pgoFilePrefix = "profile-"
const pgoFileSuffix = ".pgo"
const pgoCaptureDuration = 60 * time.Second

// ContinuousPGOCapture captures one CPU profile every intervalMin minutes and
// rotates samples older than 24h. When intervalMin <= 0 the loop is a no-op —
// callers gate this behind `cfg.SRE.PGOCaptureIntervalMinutes > 0`. Safe to
// call from a goroutine; returns when ctx is canceled. [364.C]
func ContinuousPGOCapture(ctx context.Context, workspace string, intervalMin int) {
	if intervalMin <= 0 || workspace == "" {
		return
	}
	dir := filepath.Join(workspace, ".neo", pgoDirName)
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // workspace path under user control
		log.Printf("[364.C] pgo mkdir %s: %v", dir, err)
		return
	}
	interval := time.Duration(intervalMin) * time.Minute
	t := time.NewTicker(interval)
	defer t.Stop()
	log.Printf("[364.C] ContinuousPGOCapture enabled — interval=%s dir=%s", interval, dir)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := capturePGOProfile(ctx, dir); err != nil {
				log.Printf("[364.C] pgo capture failed: %v", err)
				continue
			}
			if removed, err := PrunePGOProfiles(dir, 24*time.Hour); err != nil {
				log.Printf("[364.C] pgo prune: %v", err)
			} else if removed > 0 {
				log.Printf("[364.C] pgo prune: removed %d stale profiles", removed)
			}
		}
	}
}

// capturePGOProfile writes a single pgoCaptureDuration-long CPU profile.
// Uses runtime/pprof StartCPUProfile/StopCPUProfile, which is mutex-guarded —
// a second call during this one's window returns an error which we log and skip.
func capturePGOProfile(ctx context.Context, dir string) error {
	stamp := time.Now().UTC().Unix()
	path := filepath.Join(dir, fmt.Sprintf("%s%d%s", pgoFilePrefix, stamp, pgoFileSuffix))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // workspace path
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("StartCPUProfile: %w", err)
	}
	select {
	case <-ctx.Done():
		pprof.StopCPUProfile()
		_ = f.Close()
		_ = os.Remove(path) // incomplete — delete
		return ctx.Err()
	case <-time.After(pgoCaptureDuration):
	}
	pprof.StopCPUProfile()
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	log.Printf("[364.C] pgo profile captured → %s", path)
	return nil
}

// PrunePGOProfiles deletes .pgo files whose mtime is older than maxAge. Returns
// the number of files removed. Exported for test coverage + manual invocation. [364.C]
func PrunePGOProfiles(dir string, maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, pgoFilePrefix) || !strings.HasSuffix(name, pgoFileSuffix) {
			continue
		}
		full := filepath.Join(dir, name)
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(full); err != nil {
			log.Printf("[364.C] pgo remove %s: %v", full, err)
			continue
		}
		removed++
	}
	return removed, nil
}

// LatestPGOProfile returns the newest .pgo file under dir, or "" when none
// exist. Used by `make build-pgo` via a helper CLI + by tests. [364.C]
func LatestPGOProfile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var candidates []os.DirEntry
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, pgoFilePrefix) && strings.HasSuffix(name, pgoFileSuffix) {
			candidates = append(candidates, e)
		}
	}
	if len(candidates) == 0 {
		return "", nil
	}
	// Newest first by mtime.
	sort.Slice(candidates, func(i, j int) bool {
		ii, _ := candidates[i].Info()
		ji, _ := candidates[j].Info()
		return ii.ModTime().After(ji.ModTime())
	})
	return filepath.Join(dir, candidates[0].Name()), nil
}
