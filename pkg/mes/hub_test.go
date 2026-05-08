package mes

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewHub_Fields(t *testing.T) {
	db := createTestDB(t)
	d := NewEventDispatcher(10, db, nil)
	h := NewHub(d)
	if h == nil {
		t.Fatal("NewHub returned nil")
	}
	if h.clients == nil {
		t.Error("clients map should be initialized")
	}
	if h.register == nil {
		t.Error("register channel should be initialized")
	}
	if h.unregister == nil {
		t.Error("unregister channel should be initialized")
	}
	if h.done == nil {
		t.Error("done channel should be initialized")
	}
}

func TestHub_Shutdown_Idempotent(t *testing.T) {
	db := createTestDB(t)
	d := NewEventDispatcher(10, db, nil)
	h := NewHub(d)
	// shutdown twice should not panic
	h.shutdown()
	h.shutdown()
}

func TestHub_RemoveClient_NoClients(t *testing.T) {
	db := createTestDB(t)
	d := NewEventDispatcher(10, db, nil)
	h := NewHub(d)
	// removeClient on a client not in the map should be a no-op
	fakeClient := &Client{hub: h, send: make(chan []byte, 1)}
	h.removeClient(fakeClient) // should not panic
}

func TestHub_Start_CancelCtx(t *testing.T) {
	db := createTestDB(t)
	d := NewEventDispatcher(10, db, nil)
	ctx, cancel := context.WithCancel(context.Background())
	h := NewHub(d)
	done := make(chan struct{})
	go func() {
		h.Start(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("Hub.Start did not exit after ctx cancelled")
	}
}

func TestHandleMCTSSync_ValidJSON(t *testing.T) {
	db := createTestDB(t)
	d := NewEventDispatcher(10, db, nil)
	srv := NewIngestionServer(":0", d, nil, nil, nil, "")

	body := []byte(`{"nodes":42,"depth":3}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sre/mcts_sync", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleMCTSSync(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestHandleMCTSSync_InvalidMethod(t *testing.T) {
	db := createTestDB(t)
	d := NewEventDispatcher(10, db, nil)
	srv := NewIngestionServer(":0", d, nil, nil, nil, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sre/mcts_sync", nil)
	w := httptest.NewRecorder()
	srv.handleMCTSSync(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleMCTSSync_InvalidJSON(t *testing.T) {
	db := createTestDB(t)
	d := NewEventDispatcher(10, db, nil)
	srv := NewIngestionServer(":0", d, nil, nil, nil, "")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sre/mcts_sync", bytes.NewReader([]byte(`{bad`)))
	w := httptest.NewRecorder()
	srv.handleMCTSSync(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleToolsSync_InvalidMethod(t *testing.T) {
	db := createTestDB(t)
	d := NewEventDispatcher(10, db, nil)
	srv := NewIngestionServer(":0", d, nil, nil, nil, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sre/tools_sync", nil)
	w := httptest.NewRecorder()
	srv.handleToolsSync(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleToolsSync_InvalidJSON(t *testing.T) {
	db := createTestDB(t)
	d := NewEventDispatcher(10, db, nil)
	srv := NewIngestionServer(":0", d, nil, nil, nil, "")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sre/tools_sync", bytes.NewReader([]byte(`{bad`)))
	w := httptest.NewRecorder()
	srv.handleToolsSync(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}
