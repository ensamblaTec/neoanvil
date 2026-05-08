package incidents

import (
	"testing"
	"time"
)

const fixtureINC = `# TICKET SRE: INC-20260417-083232

## ANOMALÍA CRÍTICA DETECTADA
> El motor Auto-Triage ha capturado un evento crítico de infraestructura.

### SNAPSHOT DE HARDWARE (Detección de Zombies PIDs)
` + "```" + `text
Fallo de captura OS: exit status 1
` + "```" + `

### CONTEXTO DE LOGS (Telemetry Ring Buffer)
` + "```" + `text
2026/04/17 08:27:19.026841 main.go:265: [BOOT] initialize NeoAnvil MCP Orchestrator
2026/04/17 08:32:11.000000 main.go:800: [WARN] high latency detected
` + "```"

func TestParseIncidentMeta_ID(t *testing.T) {
	m := ParseIncidentMeta("/some/path/.neo/incidents/INC-20260417-083232.md", []byte(fixtureINC))

	if m.ID != "INC-20260417-083232" {
		t.Errorf("ID: want INC-20260417-083232, got %q", m.ID)
	}
	want := time.Date(2026, 4, 17, 8, 32, 32, 0, time.UTC)
	if m.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	} else if !m.Timestamp.Equal(want) {
		t.Errorf("Timestamp: want %v, got %v", want, m.Timestamp)
	}
}

func TestParseIncidentMeta_Anomaly(t *testing.T) {
	m := ParseIncidentMeta("/p/INC-20260417-083232.md", []byte(fixtureINC))
	if m.Anomaly == "" {
		t.Error("Anomaly should be non-empty")
	}
	t.Logf("Anomaly: %q", m.Anomaly)
}

func TestParseIncidentMeta_Severity_Warning(t *testing.T) {
	m := ParseIncidentMeta("/p/INC-20260417-083232.md", []byte(fixtureINC))
	// fixture contains "[WARN]" keyword → WARNING (not CRITICAL)
	if m.Severity != "WARNING" {
		t.Errorf("Severity: want WARNING, got %q", m.Severity)
	}
}

func TestParseIncidentMeta_Severity_Critical(t *testing.T) {
	critContent := []byte(fixtureINC + "\npanic: OOM detected in heap")
	m := ParseIncidentMeta("/p/INC-20260417-083232.md", critContent)
	if m.Severity != "CRITICAL" {
		t.Errorf("Severity: want CRITICAL, got %q", m.Severity)
	}
}

func TestIncDocID_Stable(t *testing.T) {
	id1 := incDocID("/a/b/INC-20260417-083232.md")
	id2 := incDocID("/a/b/INC-20260417-083232.md")
	if id1 != id2 {
		t.Error("incDocID should be deterministic")
	}
	id3 := incDocID("/a/b/INC-20260416-180516.md")
	if id1 == id3 {
		t.Error("different paths should produce different docIDs")
	}
}

func TestParseIncidentMeta_AffectedServices(t *testing.T) {
	content := []byte("# TICKET SRE: INC-20260417-083232\n\n**Affected Services:** RAG, HNSW, MCP\n")
	m := ParseIncidentMeta("/p/INC-20260417-083232.md", content)
	if len(m.AffectedServices) != 3 {
		t.Fatalf("want 3 services, got %d: %v", len(m.AffectedServices), m.AffectedServices)
	}
	if m.AffectedServices[0] != "RAG" || m.AffectedServices[1] != "HNSW" || m.AffectedServices[2] != "MCP" {
		t.Errorf("unexpected services: %v", m.AffectedServices)
	}
}

func TestParseIncidentMeta_AffectedServices_Empty(t *testing.T) {
	m := ParseIncidentMeta("/p/INC-20260417-083232.md", []byte("# INC\nno services header\n"))
	if len(m.AffectedServices) != 0 {
		t.Errorf("want 0 services, got %d", len(m.AffectedServices))
	}
}

func TestParseIncidentMeta_ExplicitSeverity_Critical(t *testing.T) {
	m := ParseIncidentMeta("/p/INC-20260417-083232.md", []byte("# INC\n**Severity:** CRITICAL\n"))
	if m.Severity != "CRITICAL" {
		t.Errorf("want CRITICAL, got %q", m.Severity)
	}
}

func TestParseIncidentMeta_ExplicitSeverity_Warning(t *testing.T) {
	m := ParseIncidentMeta("/p/INC-20260417-083232.md", []byte("# INC\n**Severity:** WARNING\n"))
	if m.Severity != "WARNING" {
		t.Errorf("want WARNING, got %q", m.Severity)
	}
}

func TestParseIncidentMeta_ExplicitSeverity_Info(t *testing.T) {
	m := ParseIncidentMeta("/p/INC-20260417-083232.md", []byte("# INC\n**Severity:** INFO\n"))
	if m.Severity != "INFO" {
		t.Errorf("want INFO, got %q", m.Severity)
	}
}

func TestParseIncidentMeta_DefaultSeverity_Info(t *testing.T) {
	m := ParseIncidentMeta("/p/INC-20260417-083232.md", []byte("# INC\nno special keywords here\n"))
	if m.Severity != "INFO" {
		t.Errorf("default severity: want INFO, got %q", m.Severity)
	}
}

func TestAtomicCounters_Readable(t *testing.T) {
	_ = IndexedCount()
	_ = SkippedCount()
	_ = BM25IndexedCount()
}
