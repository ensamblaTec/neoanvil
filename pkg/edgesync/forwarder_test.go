package edgesync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// openTestDB opens a scratch BoltDB seeded with an empty sync_outbox bucket.
func openTestDB(t *testing.T) *bbolt.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := bbolt.Open(filepath.Join(dir, "test.db"), 0600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("bbolt.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_ = db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("sync_outbox"))
		return err
	})
	return db
}

// seed inserts a key/value pair into sync_outbox.
func seed(t *testing.T, db *bbolt.DB, key, val string) {
	t.Helper()
	err := db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("sync_outbox")).Put([]byte(key), []byte(val))
	})
	if err != nil {
		t.Fatal(err)
	}
}

// countOutbox returns how many keys remain in sync_outbox.
func countOutbox(t *testing.T, db *bbolt.DB) int {
	t.Helper()
	n := 0
	_ = db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("sync_outbox")).ForEach(func(_, _ []byte) error {
			n++
			return nil
		})
	})
	return n
}

// TestNewForwarder_ZeroValue [Épica 230.B]
func TestNewForwarder_ZeroValue(t *testing.T) {
	db := openTestDB(t)
	f := NewForwarder(db, "http://localhost:9999")
	if f == nil {
		t.Fatal("NewForwarder returned nil")
	}
	if f.cloudURL != "http://localhost:9999" {
		t.Errorf("cloudURL mismatch")
	}
}

// TestFetchNextPayload_EmptyBucket — empty outbox returns nil key/val,
// no error. [Épica 230.B]
func TestFetchNextPayload_EmptyBucket(t *testing.T) {
	db := openTestDB(t)
	f := NewForwarder(db, "http://noop")
	key, val, err := f.fetchNextPayload()
	if err != nil {
		t.Fatalf("fetchNextPayload: %v", err)
	}
	if key != nil || val != nil {
		t.Errorf("expected nil key/val on empty bucket, got key=%q val=%q", key, val)
	}
}

// TestFetchNextPayload_ReturnsFirst — BoltDB orders keys lexicographically.
// [Épica 230.B]
func TestFetchNextPayload_ReturnsFirst(t *testing.T) {
	db := openTestDB(t)
	seed(t, db, "b-second", `{"v":2}`)
	seed(t, db, "a-first", `{"v":1}`)
	f := NewForwarder(db, "http://noop")
	key, val, err := f.fetchNextPayload()
	if err != nil {
		t.Fatal(err)
	}
	if string(key) != "a-first" || string(val) != `{"v":1}` {
		t.Errorf("wrong first: key=%q val=%q", key, val)
	}
}

// TestSendToCloud_Success — 200 OK response returns nil error.
// [Épica 230.B]
func TestSendToCloud_Success(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b [1024]byte
		n, _ := r.Body.Read(b[:])
		got.Store(string(b[:n]))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	db := openTestDB(t)
	f := NewForwarder(db, srv.URL)
	if err := f.sendToCloud(context.Background(), http.DefaultClient, []byte(`{"payload":true}`)); err != nil {
		t.Fatalf("sendToCloud: %v", err)
	}
	if got.Load() != `{"payload":true}` {
		t.Errorf("server saw wrong body: %v", got.Load())
	}
}

// TestSendToCloud_ServerError — 5xx returns an error so retry logic fires.
// [Épica 230.B]
func TestSendToCloud_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	db := openTestDB(t)
	f := NewForwarder(db, srv.URL)
	if err := f.sendToCloud(context.Background(), http.DefaultClient, []byte("{}")); err == nil {
		t.Error("expected error on 502, got nil")
	}
}

// TestDeletePayload_RemovesKey — after delete, outbox shrinks by one.
// [Épica 230.B]
func TestDeletePayload_RemovesKey(t *testing.T) {
	db := openTestDB(t)
	seed(t, db, "gone", `{"x":1}`)
	seed(t, db, "kept", `{"y":2}`)
	if countOutbox(t, db) != 2 {
		t.Fatal("seed failed")
	}
	f := NewForwarder(db, "http://noop")
	if err := f.deletePayload([]byte("gone")); err != nil {
		t.Fatal(err)
	}
	if n := countOutbox(t, db); n != 1 {
		t.Errorf("expected 1 remaining, got %d", n)
	}
}

// TestFetchAndSend_RoundTrip ensures fetch → POST → delete cycle leaves
// the outbox empty. [Épica 230.B]
func TestFetchAndSend_RoundTrip(t *testing.T) {
	received := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Tick int `json:"tick"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		received <- "" // signal
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	db := openTestDB(t)
	seed(t, db, "tick1", `{"tick":1}`)
	f := NewForwarder(db, srv.URL)

	key, val, err := f.fetchNextPayload()
	if err != nil || key == nil {
		t.Fatalf("fetch: err=%v key=%v", err, key)
	}
	if err := f.sendToCloud(context.Background(), http.DefaultClient, val); err != nil {
		t.Fatalf("send: %v", err)
	}
	<-received
	if err := f.deletePayload(key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := countOutbox(t, db); n != 0 {
		t.Errorf("outbox not empty after round-trip: %d", n)
	}
}
