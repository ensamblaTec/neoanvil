package telemetry

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/finops"
)

func StartHardwareTelemetry(workspace string) {
	pid := fmt.Sprint(os.Getpid())
	ticker := time.NewTicker(1 * time.Second)
	dbPath := workspace + "/.neo/db"

	rapl, raplErr := finops.MountRAPL()
	var totalJoules float64 = 0.0

	for range ticker.C {
		// CPU
		cmd := exec.Command("ps", "-p", pid, "-o", "%cpu=")
		out, err := cmd.Output()
		var cpu float64
		if err == nil {
			cpu, _ = strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
		}

		// RAM & Goroutines (SRE Zero-Allocation Tracker)
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		ramMB := float64(m.Alloc) / 1024.0 / 1024.0
		goroutines := runtime.NumGoroutine()

		// DB Sizes
		dbSizes := make(map[string]float64)
		_ = filepath.Walk(dbPath, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				dbSizes[info.Name()] = float64(info.Size()) / 1024.0 / 1024.0
			}
			return nil
		})

		if raplErr == nil && rapl != nil {
			totalJoules += rapl.ReadWatts_O1(1.0)
		}

		// Unificando
		mu.Lock()
		globalState.CPUUsage = cpu
		globalState.AllocatedRAM_MB = ramMB
		globalState.ActiveGoroutines = goroutines
		globalState.DatabaseSizesMB = dbSizes
		globalState.JoulesConsumed = totalJoules
		globalState.Uptime = time.Since(startTime).Truncate(time.Second).String()
		mu.Unlock()

		b, _ := json.Marshal(GetSystemStats())

		EmitEvent("COGNITIVA", string(b))
	}
}
