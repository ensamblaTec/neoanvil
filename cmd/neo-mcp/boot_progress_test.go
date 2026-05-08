package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBootProgress_StartFinishCycle covers the basic state machine.
// Before Start: zero values + phase reads as "loading" (default until set).
// After Start with a known file: total = file size; phase still loading.
// After Finish: read jumps to total; phase = "ready"; pct = 1.0.
func TestBootProgress_StartFinishCycle(t *testing.T) {
	bp := &BootProgress{}

	// Synthetic WAL file so Stat works — 4096 bytes is fine, we only
	// need a known size for the percentage math.
	tmp := filepath.Join(t.TempDir(), "fake.db")
	if err := os.WriteFile(tmp, make([]byte, 4096), 0644); err != nil {
		t.Fatalf("create fake walfile: %v", err)
	}

	bp.StartHNSWLoad(tmp)
	if got := bp.hnswBytesTotal.Load(); got != 4096 {
		t.Errorf("total=%d want 4096", got)
	}
	if bp.hnswReady.Load() {
		t.Error("ready should be false right after StartHNSWLoad")
	}
	if bp.startedUnix.Load() == 0 {
		t.Error("startedUnix should be set")
	}

	bp.FinishHNSWLoad()
	if !bp.hnswReady.Load() {
		t.Error("ready should be true after FinishHNSWLoad")
	}
	if got := bp.hnswBytesReadMin.Load(); got != 4096 {
		t.Errorf("readMin=%d want 4096 (jumped to total)", got)
	}
}

// TestBootProgress_StartHNSWLoad_StatFailure covers the case where the
// WAL file doesn't exist yet — we don't want StartHNSWLoad to panic;
// total stays 0 and the snapshot reports pct=0 (no division by zero).
func TestBootProgress_StartHNSWLoad_StatFailure(t *testing.T) {
	bp := &BootProgress{}
	bp.StartHNSWLoad("/this/path/does/not/exist/xyz.db")
	if got := bp.hnswBytesTotal.Load(); got != 0 {
		t.Errorf("total=%d want 0 on stat failure", got)
	}
	if bp.startedUnix.Load() == 0 {
		t.Error("startedUnix should still be set even on stat failure")
	}
	// snapshot should not divide by zero
	snap := bp.snapshotForJSON()
	if pct := snap["hnsw_pct"].(float64); pct != 0 {
		t.Errorf("pct=%f want 0 when total=0", pct)
	}
}

// TestBootProgress_SnapshotShape verifies the JSON output keys + types
// match what /boot_progress consumers expect.
func TestBootProgress_SnapshotShape(t *testing.T) {
	bp := &BootProgress{}
	bp.startedUnix.Store(time.Now().Unix() - 10) // 10s ago
	bp.hnswBytesTotal.Store(1000)
	bp.hnswBytesReadMin.Store(500)

	snap := bp.snapshotForJSON()
	wantKeys := []string{"phase", "hnsw_bytes_total", "hnsw_bytes_read", "hnsw_pct", "started_at_unix", "elapsed_seconds"}
	for _, k := range wantKeys {
		if _, ok := snap[k]; !ok {
			t.Errorf("snapshot missing key %q", k)
		}
	}
	if phase := snap["phase"].(string); phase != "loading" {
		t.Errorf("phase=%q want loading (not finished)", phase)
	}
	if elapsed := snap["elapsed_seconds"].(int64); elapsed < 9 {
		t.Errorf("elapsed=%d want >= 9 (we set started 10s ago)", elapsed)
	}
}

// TestBootProgress_PercentageCappedAt100 — process-wide read_bytes can
// exceed file size (other reads inflate the counter). The percentage
// must cap at 100% so the operator doesn't see "120% loaded".
func TestBootProgress_PercentageCappedAt100(t *testing.T) {
	bp := &BootProgress{}
	bp.hnswBytesTotal.Store(1000)
	bp.hnswBytesReadMin.Store(2500) // exceeds total
	snap := bp.snapshotForJSON()
	if pct := snap["hnsw_pct"].(float64); pct != 1.0 {
		t.Errorf("pct=%f want 1.0 (capped)", pct)
	}
}

// TestReadProcSelfIORead_LinuxOrFail covers the parsing of /proc/self/io
// when present. On non-Linux the test returns a non-nil error and we
// skip — production code degrades gracefully too.
func TestReadProcSelfIORead_LinuxOrFail(t *testing.T) {
	n, err := readProcSelfIORead()
	if err != nil {
		t.Skipf("non-Linux or /proc unavailable: %v", err)
	}
	if n < 0 {
		t.Errorf("read_bytes=%d should be non-negative", n)
	}
}

// TestEmitBootProgressLog_FormatsCorrectly captures the log line via
// the bootProgressLogger override hook and verifies the format matches
// the contract documented in 148.A. Also verifies MB conversion math.
func TestEmitBootProgressLog_FormatsCorrectly(t *testing.T) {
	var captured string
	orig := bootProgressLogger
	bootProgressLogger = func(format string, args ...any) {
		captured = fmt.Sprintf(format, args...)
	}
	t.Cleanup(func() { bootProgressLogger = orig })

	// 1.7 GB read of 3.3 GB total at 67s elapsed.
	emitBootProgressLog(0.515, 1755668480, 3329933312, 67)
	if !strings.Contains(captured, "[BOOT-PROGRESS]") {
		t.Errorf("missing [BOOT-PROGRESS] tag: %q", captured)
	}
	if !strings.Contains(captured, "phase=hnsw_load") {
		t.Errorf("missing phase=hnsw_load: %q", captured)
	}
	if !strings.Contains(captured, "read=1674MB") {
		t.Errorf("expected read=1674MB (1755668480>>20): %q", captured)
	}
	if !strings.Contains(captured, "total=3175MB") {
		t.Errorf("expected total=3175MB (3329933312>>20): %q", captured)
	}
	if !strings.Contains(captured, "pct=51.5%") {
		t.Errorf("expected pct=51.5%%: %q", captured)
	}
	if !strings.Contains(captured, "elapsed=67s") {
		t.Errorf("expected elapsed=67s: %q", captured)
	}
}

// TestHandleBootProgress_HTTP verifies the endpoint returns a JSON
// object with the expected keys.
func TestHandleBootProgress_HTTP(t *testing.T) {
	// Reset to known state — globalBootProgress is shared with other
	// tests that may run later in the same package, so set values
	// explicitly rather than relying on zero defaults.
	globalBootProgress = &BootProgress{}
	globalBootProgress.hnswBytesTotal.Store(2048)
	globalBootProgress.hnswBytesReadMin.Store(1024)
	globalBootProgress.startedUnix.Store(time.Now().Unix() - 5)

	req := httptest.NewRequest("GET", "/boot_progress", nil)
	w := httptest.NewRecorder()
	handleBootProgress(w, req)

	if w.Code != 200 {
		t.Fatalf("status=%d want 200", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if _, ok := resp["phase"]; !ok {
		t.Error("missing phase in JSON")
	}
	if _, ok := resp["hnsw_pct"]; !ok {
		t.Error("missing hnsw_pct in JSON")
	}
}
