package edgesync

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func seedOutbox(t *testing.T, db *bbolt.DB, count int) {
	t.Helper()
	err := db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("sync_outbox"))
		if err != nil {
			return err
		}
		for i := range count {
			key := make([]byte, 8)
			binary.BigEndian.PutUint64(key, uint64(i))
			if err := b.Put(key, []byte(`{"event":"test"}`)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seedOutbox: %v", err)
	}
}

func outboxCount(t *testing.T, db *bbolt.DB) int {
	t.Helper()
	var n int
	_ = db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("sync_outbox"))
		if b != nil {
			n = b.Stats().KeyN
		}
		return nil
	})
	return n
}

func TestEnforceRetentionPolicy_UnderLimit(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	seedOutbox(t, db, 5)

	enforceRetentionPolicy(db, 10)

	if got := outboxCount(t, db); got != 5 {
		t.Errorf("expected 5 records (under limit), got %d", got)
	}
}

func TestEnforceRetentionPolicy_OverLimit(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	seedOutbox(t, db, 20)

	enforceRetentionPolicy(db, 10)

	if got := outboxCount(t, db); got >= 20 {
		t.Errorf("expected records to be reduced from 20, got %d", got)
	}
}

func TestEnforceRetentionPolicy_NoBucket(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	// sync_outbox bucket was already created by openTestDB, but no rows
	enforceRetentionPolicy(db, 5) // must not panic
}

func TestStartRetentionSweeper_ExitsOnCancel(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		StartRetentionSweeper(ctx, db, 100)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("StartRetentionSweeper did not exit after ctx cancelled")
	}
}
