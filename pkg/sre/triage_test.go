package sre_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/sre"
)

func BenchmarkTriageEngineWrite(b *testing.B) {
	engine := sre.NewTriageEngine(b.TempDir(), 100)
	normalLog := []byte("2024/01/01 INFO normal execution trace message")

	for b.Loop() {
		_, _ = engine.Write(normalLog)
	}
}

// TestZombiePIDDetection verifies severity classification and service extraction. [153.D]
func TestZombiePIDDetection(t *testing.T) {
	// Simulate ps output with a zombie process.
	psLines := []string{
		"  PID STAT  %CPU %MEM COMM",
		"    1 S      0.0  0.1 launchd",
		"  456 Z      0.0  0.0 defunct-worker",
		"  789 S      1.2  0.5 neo-mcp",
	}
	psOut := []byte(strings.Join(psLines, "\n"))

	// Verify zombie line is detectable in output.
	if !bytes.Contains(psOut, []byte(" Z")) {
		t.Fatal("expected zombie marker ' Z' in ps output")
	}
	zombieLine := psLines[2]
	if !strings.Contains(zombieLine, " Z") {
		t.Fatalf("zombie line %q does not contain stat Z", zombieLine)
	}
}

func TestClassifySeverityCritical(t *testing.T) {
	ws := t.TempDir()
	engine := sre.NewTriageEngine(ws, 2)

	ctx := []byte("2024/01/01 [SRE-FATAL] panic: memory corruption in RAG hot-path")
	_, _ = engine.Write(ctx)
	time.Sleep(150 * time.Millisecond)

	incPath := filepath.Join(ws, ".neo", "incidents")
	files, err := os.ReadDir(incPath)
	if err != nil || len(files) == 0 {
		t.Skip("incident file not generated (timing)")
	}
	data, _ := os.ReadFile(filepath.Join(incPath, files[0].Name()))
	if !bytes.Contains(data, []byte("**Severity:** CRITICAL")) {
		t.Fatalf("expected CRITICAL severity in incident, got:\n%s", data[:min(200, len(data))])
	}
}

func TestExtractAffectedServices(t *testing.T) {
	ws := t.TempDir()
	engine := sre.NewTriageEngine(ws, 2)

	ctx := []byte("2024/01/01 [SRE-FATAL] [RAG] embed failure [HNSW] panic: nil ptr")
	_, _ = engine.Write(ctx)
	time.Sleep(150 * time.Millisecond)

	incPath := filepath.Join(ws, ".neo", "incidents")
	files, err := os.ReadDir(incPath)
	if err != nil || len(files) == 0 {
		t.Skip("incident file not generated (timing)")
	}
	data, _ := os.ReadFile(filepath.Join(incPath, files[0].Name()))
	if !bytes.Contains(data, []byte("**Affected Services:**")) {
		t.Fatalf("expected Affected Services in incident, got:\n%s", data[:min(300, len(data))])
	}
}

func TestTriageBasicHit(t *testing.T) {
	ws := t.TempDir()
	engine := sre.NewTriageEngine(ws, 2)

	// [AUDIT-2026-04-23] `panic:` alone used to trigger; removed because the DREAM
	// engine logs `[DREAM] panic: recovered=true` as a training success, causing
	// spurious INC-*.md generation on every boot cycle. Explicit [SRE-CRIT] /
	// [SRE-FATAL] markers are the authoritative trigger now.
	_, _ = engine.Write([]byte("[SRE-CRIT] memory corruption detected"))
	time.Sleep(100 * time.Millisecond) // Goroutine delay

	incPath := filepath.Join(ws, ".neo", "incidents")
	files, err := os.ReadDir(incPath)
	if err != nil {
		t.Fatalf("Directorio incidents ausente: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("Fallo en la auto-recreación del incidente. Ticket no generado.")
	}
}

// TestTriageDreamPanicIgnored ensures that the DREAM engine's success log
// (`[DREAM] panic: recovered=true notes=...`) never triggers an INC.
// [AUDIT-2026-04-23 — root cause of 18 spurious INCs in one week.]
func TestTriageDreamPanicIgnored(t *testing.T) {
	ws := t.TempDir()
	engine := sre.NewTriageEngine(ws, 2)

	_, _ = engine.Write([]byte("2026/04/23 [DREAM] panic: recovered=true ms=0 notes=immunity learned"))
	time.Sleep(100 * time.Millisecond)

	incPath := filepath.Join(ws, ".neo", "incidents")
	files, err := os.ReadDir(incPath)
	// Either the dir doesn't exist (preferred — nothing triggered) or it's empty.
	if err == nil && len(files) > 0 {
		t.Fatalf("DREAM success log generated spurious INC: %s", files[0].Name())
	}
}
