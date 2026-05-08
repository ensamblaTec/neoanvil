// Package state — daemon TTL seal auto-renew helpers. [132.D]
package state

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GetSealedFilesNeedingRenewal reads the certify lock file and returns paths
// whose seals are expired or within bufferSec seconds of expiring.
//
// The lock file format is one entry per line: "path|unixSeconds".
// Only the most recent stamp per path is considered. [132.D]
func GetSealedFilesNeedingRenewal(lockPath string, ttlSeconds, bufferSec int) ([]string, error) {
	data, err := os.ReadFile(lockPath) //nolint:gosec // G304-WORKSPACE-CANON: lockPath derived from workspace/.neo/db/certified_state.lock
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("daemon_certify: read lock %s: %w", lockPath, err)
	}

	latest := make(map[string]int64)
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 {
			continue
		}
		ts, perr := strconv.ParseInt(parts[1], 10, 64)
		if perr != nil {
			continue
		}
		if ts > latest[parts[0]] {
			latest[parts[0]] = ts
		}
	}

	now := time.Now().Unix()
	threshold := int64(ttlSeconds - bufferSec) // age at which renewal is needed
	var stale []string
	for file, sealTime := range latest {
		if now-sealTime >= threshold {
			stale = append(stale, file)
		}
	}
	sort.Strings(stale)
	return stale, nil
}

// MaybeSealedFilesNeedingRenewal returns stale files when autoRecertify is true,
// otherwise returns nil without reading the lock. [132.D]
func MaybeSealedFilesNeedingRenewal(lockPath string, ttlSeconds, bufferSec int, autoRecertify bool) ([]string, error) {
	if !autoRecertify {
		return nil, nil
	}
	return GetSealedFilesNeedingRenewal(lockPath, ttlSeconds, bufferSec)
}
