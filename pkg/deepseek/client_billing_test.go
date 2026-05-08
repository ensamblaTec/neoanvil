package deepseek

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// TestBillingCircuitOpen verifies that the session circuit breaker trips after the limit is reached.
func TestBillingCircuitOpen(t *testing.T) {
	srv := fakeDeepSeekServer(t, "result", 80, 40) // 120 tokens per call
	defer srv.Close()

	dir := t.TempDir()
	// MaxTokensPerSession set to 100 — circuit trips when session total ≥ 100.
	c, err := New(Config{APIKey: "k", BaseURL: srv.URL, DBPath: filepath.Join(dir, "b.db"), MaxTokensPerSession: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// First call: 0 tokens recorded at check time → passes, records 120.
	_, err = c.Call(context.Background(), CallRequest{
		Action: "distill_payload", Prompt: "first", Mode: SessionModeEphemeral,
	})
	if err != nil {
		t.Fatalf("first call should succeed: %v", err)
	}

	// Second call: 120 ≥ 100 → BILLING_CIRCUIT_OPEN.
	_, err = c.Call(context.Background(), CallRequest{
		Action: "distill_payload", Prompt: "second", Mode: SessionModeEphemeral,
	})
	if err == nil {
		t.Fatal("expected BILLING_CIRCUIT_OPEN error, got nil")
	}
	if !strings.Contains(err.Error(), "BILLING_CIRCUIT_OPEN") {
		t.Errorf("expected BILLING_CIRCUIT_OPEN in error, got: %v", err)
	}
}

// TestBillingCircuitAllow verifies that calls succeed when session total is below the limit.
func TestBillingCircuitAllow(t *testing.T) {
	srv := fakeDeepSeekServer(t, "ok", 50, 50)
	defer srv.Close()

	dir := t.TempDir()
	c, err := New(Config{APIKey: "k", BaseURL: srv.URL, DBPath: filepath.Join(dir, "b.db"), MaxTokensPerSession: 500000})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	for i := range 3 {
		_, err := c.Call(context.Background(), CallRequest{
			Action: "distill_payload", Prompt: "call", Mode: SessionModeEphemeral,
		})
		if err != nil {
			t.Fatalf("call %d should succeed: %v", i, err)
		}
	}

	tokens, _ := c.BillingStats()
	if tokens == 0 {
		t.Error("expected non-zero billing stats after calls")
	}
}

// TestCheckpointMiss verifies that a fresh request (no matching checkpoint) executes the API call.
func TestCheckpointMiss(t *testing.T) {
	srv := fakeDeepSeekServer(t, "fresh result", 10, 5)
	defer srv.Close()

	dir := t.TempDir()
	c, err := New(Config{APIKey: "k", BaseURL: srv.URL, DBPath: filepath.Join(dir, "c.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	resp, err := c.Call(context.Background(), CallRequest{
		Action:        "distill_payload",
		Prompt:        "unique prompt that has no checkpoint",
		Mode:          SessionModeEphemeral,
		CheckpointKey: "unique-key-no-match",
	})
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if resp.CacheHit {
		t.Error("fresh call should not be a cache hit")
	}
	if resp.Text != "fresh result" {
		t.Errorf("expected 'fresh result', got %q", resp.Text)
	}
}

// TestCheckpointExpired verifies that an expired checkpoint entry triggers a fresh API call.
func TestCheckpointExpired(t *testing.T) {
	srv := fakeDeepSeekServer(t, "refreshed", 10, 5)
	defer srv.Close()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "c.db")
	c, err := New(Config{APIKey: "k", BaseURL: srv.URL, DBPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Inject an expired checkpoint entry directly into BoltDB.
	expiredEntry := checkpointEntry{
		CreatedAt: time.Now().Unix() - checkpointTTL - 1, // 1 second past TTL
		Response:  CallResponse{Text: "stale cached result"},
	}
	data, _ := json.Marshal(expiredEntry)
	const expiredKey = "expired-ck-key"
	c.db.Update(func(tx *bolt.Tx) error { //nolint:errcheck
		b := tx.Bucket([]byte(bucketCheckpts))
		if b == nil {
			return nil
		}
		return b.Put([]byte(expiredKey), data)
	})

	resp, err := c.Call(context.Background(), CallRequest{
		Action:        "distill_payload",
		Prompt:        "some prompt",
		Mode:          SessionModeEphemeral,
		CheckpointKey: expiredKey,
	})
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if resp.CacheHit {
		t.Error("expired checkpoint should not yield a cache hit")
	}
	if resp.Text == "stale cached result" {
		t.Error("should not return stale cached result after expiry")
	}
	if resp.Text != "refreshed" {
		t.Errorf("expected 'refreshed', got %q", resp.Text)
	}
}
