package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/nexus"
)

// testPool builds a ProcessPool whose DebtRegistry is backed by a temp file.
func testPoolWithDebt(t *testing.T) (*nexus.ProcessPool, *nexus.NexusConfig) {
	t.Helper()
	dir := t.TempDir()
	cfg := &nexus.NexusConfig{
		Nexus: nexus.NexusSection{
			Debt: nexus.DebtConfig{
				Enabled:            true,
				File:               filepath.Join(dir, "nexus_debt.md"),
				DedupWindowMinutes: 15,
				MaxResolvedDays:    30,
			},
		},
	}
	// Minimal pool — NewProcessPoolWithConfig auto-opens the registry.
	pool := nexus.NewProcessPoolWithConfig(nil, "", cfg)
	if pool.Debt == nil {
		t.Fatal("pool.Debt nil — registry did not open")
	}
	return pool, cfg
}

func TestDebtHandler_ListEmpty(t *testing.T) {
	pool, cfg := testPoolWithDebt(t)
	mux := http.NewServeMux()
	registerNexusDebtHandlers(mux, pool, cfg)

	req := httptest.NewRequest("GET", "/internal/nexus/debt", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []nexus.NexusDebtEvent
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %d events", len(got))
	}
}

func TestDebtHandler_ListWithEntries(t *testing.T) {
	pool, cfg := testPoolWithDebt(t)
	pool.Debt.AppendDebt(nexus.NexusDebtEvent{
		Priority: "P0", Title: "boot timeout",
		AffectedWorkspaces: []string{"ws-a"}, Source: "verify_boot",
	})
	pool.Debt.AppendDebt(nexus.NexusDebtEvent{
		Priority: "P1", Title: "ollama down",
		AffectedWorkspaces: []string{"ws-b"}, Source: "service_manager",
	})

	mux := http.NewServeMux()
	registerNexusDebtHandlers(mux, pool, cfg)

	req := httptest.NewRequest("GET", "/internal/nexus/debt?priority=P0", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var got []nexus.NexusDebtEvent
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if len(got) != 1 {
		t.Fatalf("priority=P0 filter returned %d, want 1", len(got))
	}
	if got[0].Priority != "P0" {
		t.Errorf("wrong priority: %s", got[0].Priority)
	}
}

func TestDebtHandler_Affecting(t *testing.T) {
	pool, cfg := testPoolWithDebt(t)
	pool.Debt.AppendDebt(nexus.NexusDebtEvent{Priority: "P0", Title: "a", AffectedWorkspaces: []string{"ws-a"}})
	pool.Debt.AppendDebt(nexus.NexusDebtEvent{Priority: "P1", Title: "b", AffectedWorkspaces: []string{"ws-b"}})
	pool.Debt.AppendDebt(nexus.NexusDebtEvent{Priority: "P2", Title: "c", AffectedWorkspaces: []string{"ws-a", "ws-b"}})

	mux := http.NewServeMux()
	registerNexusDebtHandlers(mux, pool, cfg)

	req := httptest.NewRequest("GET", "/internal/nexus/debt/affecting?workspace_id=ws-a", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var got []nexus.NexusDebtEvent
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if len(got) != 2 {
		t.Fatalf("ws-a affecting = %d, want 2", len(got))
	}

	// Missing workspace_id → 400.
	req = httptest.NewRequest("GET", "/internal/nexus/debt/affecting", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing workspace_id: status=%d, want 400", rec.Code)
	}
}

func TestDebtHandler_ResolveAuth(t *testing.T) {
	pool, cfg := testPoolWithDebt(t)
	cfg.Nexus.API.AuthToken = "secret123"
	ev, _ := pool.Debt.AppendDebt(nexus.NexusDebtEvent{
		Priority: "P0", Title: "x", AffectedWorkspaces: []string{"ws"},
	})

	mux := http.NewServeMux()
	registerNexusDebtHandlers(mux, pool, cfg)

	// Missing token → 401.
	body := mustMarshal(map[string]string{"id": ev.ID, "resolution": "fixed"})
	req := httptest.NewRequest("POST", "/internal/nexus/debt/resolve", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status=%d, want 401", rec.Code)
	}

	// Wrong token → 401.
	req = httptest.NewRequest("POST", "/internal/nexus/debt/resolve", bytes.NewReader(body))
	req.Header.Set("X-Nexus-Token", "wrong")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status=%d, want 401", rec.Code)
	}

	// Correct token → 200.
	req = httptest.NewRequest("POST", "/internal/nexus/debt/resolve", bytes.NewReader(body))
	req.Header.Set("X-Nexus-Token", "secret123")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("correct token: status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	// Second resolve → 409.
	req = httptest.NewRequest("POST", "/internal/nexus/debt/resolve", bytes.NewReader(body))
	req.Header.Set("X-Nexus-Token", "secret123")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("double resolve: status=%d, want 409", rec.Code)
	}
}

func TestDebtHandler_DisabledReturns404(t *testing.T) {
	// pool.Debt nil when registry disabled.
	pool := nexus.NewProcessPoolWithConfig(nil, "", &nexus.NexusConfig{})
	mux := http.NewServeMux()
	registerNexusDebtHandlers(mux, pool, &nexus.NexusConfig{})

	req := httptest.NewRequest("GET", "/internal/nexus/debt", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("disabled: status=%d, want 404", rec.Code)
	}
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
