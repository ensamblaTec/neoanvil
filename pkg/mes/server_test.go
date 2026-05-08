package mes

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"go.etcd.io/bbolt"
	"github.com/ensamblatec/neoanvil/pkg/mesh"
)

func createTestDB(t *testing.T) *bbolt.DB {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := bbolt.Open(dbPath, 0600, nil)
	if err != nil {
		t.Fatalf("Failed to open bbolt: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestHandleTelemetry_Recorder(t *testing.T) {
	db := createTestDB(t)
	d := NewEventDispatcher(100, db, nil) // Mock CEP nil
	ctx := t.Context()
	d.Start(ctx)

	srv := NewIngestionServer(":0", d, nil, nil, mesh.NewRouter(100), "")

	validJSON := []byte(`{"MachineID":"M1", "State":2, "Payload":""}`)
	req := httptest.NewRequest("POST", "/api/v1/telemetry", bytes.NewReader(validJSON))
	w := httptest.NewRecorder()

	srv.httpSrv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("Expected 202 Accepted, got %d", w.Code)
	}
}

func TestHandleTelemetry_InvalidMethod(t *testing.T) {
	db := createTestDB(t)
	d := NewEventDispatcher(10, db, nil)
	srv := NewIngestionServer(":0", d, nil, nil, mesh.NewRouter(100), "")

	req := httptest.NewRequest("GET", "/api/v1/telemetry", nil)
	w := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected 405, got %d", w.Code)
	}
}

func TestHandleTelemetry_InvalidJSON(t *testing.T) {
	db := createTestDB(t)
	d := NewEventDispatcher(10, db, nil)
	srv := NewIngestionServer(":0", d, nil, nil, mesh.NewRouter(100), "")

	req := httptest.NewRequest("POST", "/api/v1/telemetry", bytes.NewReader([]byte(`{bad-json}`)))
	w := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400, got %d", w.Code)
	}
}
