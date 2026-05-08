package wms

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

const (
	bucketIdempotency = "idempotency_keys"
	keyTTL            = 24 * time.Hour
)

// Record define la persistencia de una transacción procesada.
type Record struct {
	Status    int   `json:"status"`
	Timestamp int64 `json:"timestamp"`
}

// InFlightManager protege contra Thundering Herd usando canales, NO Mutexes.
type InFlightManager struct {
	inFlight sync.Map
}

func (m *InFlightManager) Acquire(key string) (isLeader bool, waitFn func()) {
	done := make(chan struct{})
	actual, loaded := m.inFlight.LoadOrStore(key, done)

	if loaded {
		// Bloqueo pasivo (Goroutine en sleep)
		return false, func() { <-actual.(chan struct{}) }
	}

	// Líder activo: cierra el canal y limpia la memoria
	return true, func() {
		close(done)
		m.inFlight.Delete(key)
	}
}

var flightManager InFlightManager

// responseRecorder intercepta el StatusCode.
type responseRecorder struct {
	http.ResponseWriter
	status int
	body   *bytes.Buffer
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	r.status = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

func readIdempotencyRecord(db *bbolt.DB, idemKey string) (bool, Record, error) {
	var cachedRecord Record
	found := false
	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketIdempotency))
		if b != nil {
			if val := b.Get([]byte(idemKey)); val != nil {
				if err := json.Unmarshal(val, &cachedRecord); err == nil {
					found = true
				}
			}
		}
		return nil
	})
	return found, cachedRecord, err
}

func writeIdempotencyRecord(db *bbolt.DB, idemKey string, status int) {
	if status < 200 || status >= 300 {
		return
	}
	newRecord := Record{Status: status, Timestamp: time.Now().Unix()}
	val, err := json.Marshal(newRecord)
	if err != nil {
		log.Printf("[SRE-ERROR] Fallo emitiendo Record Idempotencia: %v", err)
		return
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucketIdempotency))
		if err != nil {
			return err
		}
		return b.Put([]byte(idemKey), val)
	}); err != nil {
		log.Printf("[SRE-FATAL] Fallo persistiendo estado transaccional: %v", err)
	}
}

// IdempotencyMiddleware SRE con Singleflight nativo.
func IdempotencyMiddleware(db *bbolt.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet || r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			idemKey := r.Header.Get("X-Idempotency-Key")
			if idemKey == "" {
				http.Error(w, "Missing X-Idempotency-Key header", http.StatusBadRequest)
				return
			}

			isLeader, syncAction := flightManager.Acquire(idemKey)
			if !isLeader {
				syncAction()
			} else {
				defer syncAction()
			}

			found, cachedRecord, err := readIdempotencyRecord(db, idemKey)
			if err != nil {
				http.Error(w, "Internal DB Error", http.StatusInternalServerError)
				return
			}
			if found {
				w.WriteHeader(http.StatusConflict)
				_, _ = fmt.Fprintf(w, "Idempotent hit: processed at %d with status %d", cachedRecord.Timestamp, cachedRecord.Status)
				return
			}

			rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK, body: &bytes.Buffer{}}
			next.ServeHTTP(rec, r)
			writeIdempotencyRecord(db, idemKey, rec.status)
		})
	}
}

// StartBackgroundSweeper inicializa la purga periódica de BoltDB.
// Ejecutar como Goroutine en el main.go: go StartBackgroundSweeper(ctx, db)
func StartBackgroundSweeper(ctx context.Context, db *bbolt.DB) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[SRE] Apagando WMS Idempotency Sweeper")
			return
		case <-ticker.C:
			sweepOldKeys(db)
		}
	}
}

func sweepOldKeys(db *bbolt.DB) {
	cutoff := time.Now().Add(-keyTTL).Unix()
	keysDeleted := 0

	err := db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketIdempotency))
		if b == nil {
			return nil
		}

		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var rec Record
			if err := json.Unmarshal(v, &rec); err != nil {
				// Corrupción o formato viejo, purgar por seguridad
				if err := c.Delete(); err != nil {
					log.Printf("[SRE-ERROR] Fallo purgando clave corrupta WMS: %v", err)
				} else {
					keysDeleted++
				}
				continue
			}

			if rec.Timestamp < cutoff {
				if err := c.Delete(); err != nil {
					log.Printf("[SRE-ERROR] Fallo purgando clave vieja WMS: %v", err)
				} else {
					keysDeleted++
				}
			}
		}
		return nil
	})

	if err != nil {
		log.Printf("[SRE-ERROR] Fallo en la purga de Idempotencia WMS: %v", err)
	} else if keysDeleted > 0 {
		log.Printf("[SRE] Purga WMS Idempotencia: %d claves oxidadas eliminadas", keysDeleted)
	}
}
