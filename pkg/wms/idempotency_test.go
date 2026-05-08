package wms

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"go.etcd.io/bbolt"
	"golang.org/x/sync/errgroup"
)

func TestIdempotencyRaceCondition(t *testing.T) {
	// 1. Setup bbolt Test DB
	tmpDir, err := os.MkdirTemp("", "wms_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := bbolt.Open(dbPath, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	var processCount int32
	baseHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&processCount, 1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("Payload Guardado"))
	})

	middleware := IdempotencyMiddleware(db)
	handler := middleware(baseHandler)

	workers := 100
	idemKey := "sre-v4-idem-testing-key-100"

	var success201 int32
	var conflict409 int32

	var g errgroup.Group

	for range workers {
		g.Go(func() error {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/wms/scan", nil)
			req.Header.Set("X-Idempotency-Key", idemKey)

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			switch rr.Code {
			case http.StatusCreated:
				atomic.AddInt32(&success201, 1)
			case http.StatusConflict:
				atomic.AddInt32(&conflict409, 1)
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		t.Fatalf("Errgroup falló: %v", err)
	}

	if processCount != 1 {
		t.Fatalf("RACE CONDITION SRE-FATAL: El Handler de negocio se disparó %d veces. Esperado: 1", processCount)
	}
	if success201 != 1 {
		t.Fatalf("Esperábamos exactamente 1 status Created (201), obtuvimos %d", success201)
	}
	if conflict409 != int32(workers-1) {
		t.Fatalf("Esperábamos %d status Conflict (409), obtuvimos %d", workers-1, conflict409)
	}
}
