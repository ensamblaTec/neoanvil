package main

// tool_compress_export.go — export/import actions for neo_compress_context. [130.1]

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/observability"
	"github.com/ensamblatec/neoanvil/pkg/rag"
)

// sessionExportV1 is the schema for a portable session snapshot. [130.1]
const sessionExportSchemaVersion = "v1"

type sessionExportV1 struct {
	SchemaVersion    string                    `json:"schema_version"`
	ExportedAt       time.Time                 `json:"exported_at"`
	Workspace        string                    `json:"workspace"`
	SessionID        string                    `json:"session_id"`
	SessionMutations []string                  `json:"session_mutations"`
	MasterPlanSlice  string                    `json:"master_plan_slice"`
	ToolCallLog      []observability.CallExport `json:"tool_call_log"`
}

// compressExport serializes session state to path. [130.1.1]
func compressExport(ctx context.Context, w *rag.WAL, workspace, path string) (string, error) {
	_ = ctx
	sessionID := briefingSessionID(workspace)
	muts, err := w.GetSessionMutations(sessionID)
	if err != nil {
		return "", fmt.Errorf("get session mutations: %w", err)
	}

	planSlice := readActiveMasterPlanSlice(workspace)

	var callLog []observability.CallExport
	if observability.GlobalStore != nil {
		callLog = observability.GlobalStore.RecentCalls(50)
	}

	snap := sessionExportV1{
		SchemaVersion:    sessionExportSchemaVersion,
		ExportedAt:       time.Now().UTC(),
		Workspace:        workspace,
		SessionID:        sessionID,
		SessionMutations: muts,
		MasterPlanSlice:  planSlice,
		ToolCallLog:      callLog,
	}

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal export: %w", err)
	}

	if path == "" {
		path = filepath.Join(os.TempDir(), fmt.Sprintf("neo-session-%d.json", time.Now().Unix()))
	}
	if err := os.WriteFile(path, data, 0o600); err != nil { //nolint:gosec // G306-shared: user-specified path, 0o600 is restrictive
		return "", fmt.Errorf("write export %s: %w", path, err)
	}
	return path, nil
}

// compressImport merges a previously exported session snapshot. [130.1.2]
// Merge semantics: existing session_mutations are preserved; the import only
// appends paths not already present. tool_call_log is loaded to a buffer
// (in-memory only, not persisted back to observability DB to avoid rewriting
// history). Fails if schema version is incompatible.
func compressImport(ctx context.Context, w *rag.WAL, workspace, path string) (int, error) {
	_ = ctx
	data, err := os.ReadFile(path) //nolint:gosec // G304-CLI-CONSENT: path from operator arg
	if err != nil {
		return 0, fmt.Errorf("read import file %s: %w", path, err)
	}

	var snap sessionExportV1
	if err := json.Unmarshal(data, &snap); err != nil {
		return 0, fmt.Errorf("parse import file: %w", err)
	}
	if snap.SchemaVersion != sessionExportSchemaVersion {
		return 0, fmt.Errorf("incompatible schema version %q (want %q)", snap.SchemaVersion, sessionExportSchemaVersion)
	}

	sessionID := briefingSessionID(workspace)
	existing, _ := w.GetSessionMutations(sessionID)
	existingSet := make(map[string]bool, len(existing))
	for _, p := range existing {
		existingSet[p] = true
	}

	added := 0
	for _, p := range snap.SessionMutations {
		if existingSet[p] {
			continue
		}
		if appendErr := w.AppendSessionCertified(sessionID, p); appendErr == nil {
			added++
		}
	}
	return added, nil
}

// readActiveMasterPlanSlice reads the first open phase block from master_plan.md. [130.1]
func readActiveMasterPlanSlice(workspace string) string {
	planPath := filepath.Join(workspace, ".neo", "master_plan.md")
	data, err := os.ReadFile(planPath) //nolint:gosec // G304-WORKSPACE-CANON: path via filepath.Join(workspace,...)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	var slice []string
	inPhase := false
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			if inPhase {
				break
			}
			inPhase = strings.Contains(line, "[ ]") || containsOpenTask(lines, line)
		}
		if inPhase {
			slice = append(slice, line)
			if len(slice) > 80 {
				slice = append(slice, "... [truncated]")
				break
			}
		}
	}
	return strings.Join(slice, "\n")
}

func containsOpenTask(lines []string, header string) bool {
	inSection := false
	for _, l := range lines {
		if strings.HasPrefix(l, "## ") {
			if inSection {
				break
			}
			if l == header {
				inSection = true
				continue
			}
		}
		if inSection && strings.Contains(l, "- [ ]") {
			return true
		}
	}
	return false
}
