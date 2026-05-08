package sre

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/telemetry"
)

// PostWriteHook is called after an INC-*.md file is successfully written.
// Receives the absolute path and full content. Used for CPG correlation injection.
type PostWriteHook func(path string, content []byte)

// TriageEngine intercepta el flujo de logs estándar para detectar anomalías
// y generar contexto cognitivo para el Agente IA.
type TriageEngine struct {
	mu            sync.Mutex
	ring          [150][4096]byte
	sizes         [150]int
	head          int
	capacity      int
	lastTrigger   time.Time
	workspace     string
	postWriteHook PostWriteHook // optional; set via SetPostWriteHook
}

// SetPostWriteHook injects a hook called after each INC-*.md is written. [PILAR-XXI/152.C]
func (t *TriageEngine) SetPostWriteHook(h PostWriteHook) {
	t.mu.Lock()
	t.postWriteHook = h
	t.mu.Unlock()
}

// NewTriageEngine inicializa el interceptor con un Ring Buffer pre-alocado.
func NewTriageEngine(workspace string, capacity int) *TriageEngine {
	return &TriageEngine{
		capacity:  150, // Respetar barrera estática
		workspace: workspace,
	}
}

// Write cumple la interfaz io.Writer para acoplarse al log.SetOutput
func (t *TriageEngine) Write(p []byte) (n int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	bytesCopied := copy(t.ring[t.head][:], p)
	t.sizes[t.head] = bytesCopied

	t.head = (t.head + 1) % 150

	// Fast-Path: Detección de anomalías sin Expresiones Regulares (CPU bound).
	// [AUDIT-2026-04-23] `panic:` removed — it only fires on LOGGED panic strings (e.g.
	// `[DREAM] panic: recovered=true notes=immunity learned`) which are SUCCESS training
	// events, never real panics. Runtime panics write to stderr, not log.Printf, so they
	// never reach this Writer. Keeping only explicit SRE-CRIT / SRE-FATAL markers.
	if bytes.Contains(p, []byte("[SRE-CRIT]")) ||
		bytes.Contains(p, []byte("[SRE-FATAL]")) {
		t.evaluateTriggerLocked()
	}

	return len(p), nil
}

func (t *TriageEngine) evaluateTriggerLocked() {
	now := time.Now()
	// Anti-Storm: Cooldown de 5 minutos entre incidentes críticos
	if now.Sub(t.lastTrigger) < 5*time.Minute {
		return
	}
	t.lastTrigger = now

	// Reconstruir el contexto cronológico exacto
	var contextBuf bytes.Buffer
	for i := range 150 {
		idx := (t.head + i) % 150
		size := t.sizes[idx]
		if size > 0 {
			contextBuf.Write(t.ring[idx][:size])
		}
	}

	// Delegar la I/O a disco a una goroutine para no bloquear el Write-Path del Logger
	go t.createIncidentTicket(contextBuf.Bytes(), now)
}

// reServiceTag matches uppercase component/module bracket tags in log lines. [153.C]
var reServiceTag = regexp.MustCompile(`\[([A-Z][A-Z0-9-]+)\]`)

// classifyIncidentSeverity scans log context for severity signals. [153.B]
// [AUDIT-2026-04-23] `panic:` token removed — see Write() rationale. The DREAM
// immunity learner logs `panic: recovered=true` as a success event, which used to
// misclassify every boot cycle as CRITICAL.
func classifyIncidentSeverity(logContext []byte) string {
	criticalTokens := [][]byte{
		[]byte("[SRE-FATAL]"), []byte("[SRE-CRIT]"),
		[]byte("OOM"), []byte("QUARANTINE"),
	}
	for _, tok := range criticalTokens {
		if bytes.Contains(logContext, tok) {
			return "CRITICAL"
		}
	}
	warnTokens := [][]byte{[]byte("[WARN]"), []byte("timeout"), []byte("retry")}
	for _, tok := range warnTokens {
		if bytes.Contains(logContext, tok) {
			return "WARNING"
		}
	}
	return "INFO"
}

// extractAffectedServices returns unique uppercase bracket tags from log context. [153.C]
func extractAffectedServices(logContext []byte) []string {
	skip := map[string]bool{
		"BOOT": true, "WARN": true, "ERROR": true, "CRITICAL": true,
		"INFO": true, "SRE-CRIT": true, "SRE-FATAL": true,
	}
	seen := make(map[string]bool)
	var result []string
	for _, m := range reServiceTag.FindAllSubmatch(logContext, -1) {
		if len(m) < 2 {
			continue
		}
		tag := string(m[1])
		if skip[tag] || seen[tag] {
			continue
		}
		seen[tag] = true
		result = append(result, tag)
		if len(result) >= 10 {
			break
		}
	}
	return result
}

// snapshotProcesses captures the process table with zombie detection. [153.A]
// Uses GOOS-conditional ps flags: --sort is Linux-only; macOS uses BSD ps.
func snapshotProcesses() string {
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("ps", "-eo", "pid,stat,pcpu,pmem,comm")
	} else {
		cmd = exec.Command("ps", "-eo", "pid,stat,pcpu,pmem,comm", "--sort=-pcpu")
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("Fallo de captura OS: %v", err)
	}
	lines := bytes.Split(out, []byte("\n"))
	// Always include header; prioritise zombie lines; then fill up to 16 total.
	var result [][]byte
	for i, line := range lines {
		if len(line) == 0 {
			continue
		}
		isHeader := i == 0
		isZombie := bytes.Contains(line, []byte(" Z")) || bytes.Contains(line, []byte("<defunct>"))
		if isHeader || isZombie {
			result = append(result, line)
		} else if len(result) < 16 {
			result = append(result, line)
		}
	}
	return string(bytes.Join(result, []byte("\n")))
}

func (t *TriageEngine) createIncidentTicket(logContext []byte, ts time.Time) {
	ticketID := fmt.Sprintf("INC-%s", ts.Format("20060102-150405"))
	incidentDir := filepath.Join(t.workspace, ".neo", "incidents")
	_ = os.MkdirAll(incidentDir, 0750)

	// [153.B] Severity classification + [153.C] affected services extraction.
	severity := classifyIncidentSeverity(logContext)
	services := extractAffectedServices(logContext)

	report := fmt.Sprintf("# TICKET SRE: %s\n\n", ticketID)
	report += fmt.Sprintf("**Severity:** %s\n", severity)
	if len(services) > 0 {
		report += fmt.Sprintf("**Affected Services:** %s\n", strings.Join(services, ", "))
	}
	report += "\n## ANOMALÍA CRÍTICA DETECTADA\n"
	report += "> El motor Auto-Triage ha capturado un evento crítico de infraestructura. "
	report += "Analiza el contexto a continuación o usa `neo_log_analyzer` para formular una hipótesis médica.\n\n"

	// [153.A] GOOS-conditional zombie PID snapshot.
	report += "### SNAPSHOT DE HARDWARE (Detección de Zombies PIDs)\n```text\n"
	report += snapshotProcesses()
	report += "\n```\n\n"

	// Extracción de TOPOLOGÍA DE RED BPF si existe PID anómalo
	re := regexp.MustCompile(`HANG DE RED NÚCLEO - PID: (\d+)`)
	pidMatch := re.FindSubmatch(logContext)
	//nolint:nestif // SRE log structure handling requires depth
	if len(pidMatch) > 1 {
		pidStr := string(pidMatch[1])
		report += fmt.Sprintf("### TOPOLOGÍA BPF: SOCKETS DEL PID %s\n```text\n", pidStr)
		//nolint:gosec // G304-WORKSPACE-CANON: workspace-pinned path
		ssCmd := exec.Command("lsof", "-i", "-P", "-n", "-p", pidStr)
		ssOut, _ := ssCmd.CombinedOutput()
		if len(ssOut) > 0 {
			report += string(ssOut)
		} else {
			// Fallback Go-Native para ss (Zero-Trust Shell)
			ssFallback := exec.Command("ss", "-tapn")
			if fallbackOut, err := ssFallback.CombinedOutput(); err == nil {
				found := false
				for line := range bytes.SplitSeq(fallbackOut, []byte("\n")) {
					if bytes.Contains(line, []byte(pidStr)) {
						report += string(line) + "\n"
						found = true
					}
				}
				if !found {
					report += "No se obtuvieron descriptores TCP activos.\n"
				}
			} else {
				report += "No se obtuvieron descriptores TCP activos o socket ya cerrado.\n"
			}
		}
		report += "```\n\n"
	}

	report += "### CONTEXTO DE LOGS (Telemetry Ring Buffer)\n```text\n"
	report += string(logContext)
	report += "```\n"

	reportPath := filepath.Join(incidentDir, ticketID+".md")
	if err := os.WriteFile(reportPath, []byte(report), 0600); err == nil {
		// Biosensor: Activar la atención visual y del Agente
		telemetry.SetActiveTicket(ticketID)
		telemetry.LogAction("AUTO-TRIAGE: " + ticketID)

		// [PILAR-XXI/152.C] CPG correlation hook — appends ## CPG Blast Radius if wired.
		t.mu.Lock()
		hook := t.postWriteHook
		t.mu.Unlock()
		if hook != nil {
			hook(reportPath, []byte(report))
		}
	}
}
