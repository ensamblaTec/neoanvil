package session

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func openDB(t *testing.T) *bolt.DB {
	t.Helper()
	db, err := bolt.Open(filepath.Join(t.TempDir(), "test.db"), 0600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newStore(t *testing.T) *ThreadStore {
	t.Helper()
	s, err := NewThreadStore(openDB(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRouteEphemeral(t *testing.T) {
	r := NewRouter(nil) // nil store — ephemeral tools must not need it
	mode, thread, err := r.Route("distill_payload", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mode != SessionModeEphemeral {
		t.Errorf("mode = %v, want Ephemeral", mode)
	}
	if thread != nil {
		t.Error("thread must be nil for ephemeral")
	}
}

func TestRouteThreadedNew(t *testing.T) {
	r := NewRouter(newStore(t))
	mode, thread, err := r.Route("red_team_audit", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mode != SessionModeThreaded {
		t.Errorf("mode = %v, want Threaded", mode)
	}
	if thread == nil {
		t.Fatal("thread must not be nil")
	}
	if !strings.HasPrefix(thread.ID, "ds_thread_") {
		t.Errorf("bad thread ID: %s", thread.ID)
	}
	if thread.Status != ThreadStatusActive {
		t.Errorf("status = %s, want active", thread.Status)
	}
}

func TestRouteThreadedExisting(t *testing.T) {
	store := newStore(t)
	created, err := store.Create(nil)
	if err != nil {
		t.Fatal(err)
	}

	r := NewRouter(store)
	mode, thread, err := r.Route("red_team_audit", created.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mode != SessionModeThreaded {
		t.Errorf("mode = %v, want Threaded", mode)
	}
	if thread.ID != created.ID {
		t.Errorf("thread ID mismatch: %s vs %s", thread.ID, created.ID)
	}
}

func TestRouteThreadedExpiredError(t *testing.T) {
	store := newStore(t)
	created, err := store.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Expire(created.ID); err != nil {
		t.Fatal(err)
	}

	r := NewRouter(store)
	_, _, routeErr := r.Route("red_team_audit", created.ID)
	if routeErr == nil {
		t.Error("expected error for expired thread")
	}
}

func TestAppendIncrementsTokenCount(t *testing.T) {
	store := newStore(t)
	created, err := store.Create(nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Append(created.ID, Message{Role: "user", Content: "hello", TokensUsed: 10}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(created.ID, Message{Role: "assistant", Content: "hi", TokensUsed: 5}); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TokenCount != 15 {
		t.Errorf("token count = %d, want 15", got.TokenCount)
	}
	if len(got.History) != 2 {
		t.Errorf("history len = %d, want 2", len(got.History))
	}
}

func TestListActiveFiltersExpired(t *testing.T) {
	store := newStore(t)
	a, _ := store.Create(nil) //nolint:errcheck
	b, _ := store.Create(nil) //nolint:errcheck
	store.Expire(b.ID)        //nolint:errcheck

	active, err := store.ListActive()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != a.ID {
		t.Errorf("expected 1 active thread %s, got %v", a.ID, active)
	}
}
