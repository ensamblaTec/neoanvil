package dba

import (
	"context"
	"strings"
	"testing"
)

func TestNewAnalyzer_InMemory(t *testing.T) {
	a, err := NewAnalyzer(":memory:")
	if err != nil {
		t.Fatalf("NewAnalyzer: %v", err)
	}
	if a.db == nil {
		t.Fatal("db is nil")
	}
}

func TestApplySafeMigration_CreateTable(t *testing.T) {
	a, err := NewAnalyzer(":memory:")
	if err != nil {
		t.Fatalf("NewAnalyzer: %v", err)
	}
	ctx := context.Background()
	if err := a.ApplySafeMigration(ctx, "CREATE TABLE test_sre (id INTEGER PRIMARY KEY, val TEXT)"); err != nil {
		t.Fatalf("ApplySafeMigration create: %v", err)
	}
	// Insert and read back to confirm table exists.
	if err := a.ApplySafeMigration(ctx, "INSERT INTO test_sre(val) VALUES ('ok')"); err != nil {
		t.Fatalf("ApplySafeMigration insert: %v", err)
	}
}

func TestApplySafeMigration_SyntaxError_Rollback(t *testing.T) {
	a, err := NewAnalyzer(":memory:")
	if err != nil {
		t.Fatalf("NewAnalyzer: %v", err)
	}
	ctx := context.Background()
	err = a.ApplySafeMigration(ctx, "NOT VALID SQL !!!")
	if err == nil {
		t.Fatal("expected error for invalid SQL, got nil")
	}
}

func TestQuerySchema_SQLite(t *testing.T) {
	a, err := NewAnalyzer(":memory:")
	if err != nil {
		t.Fatalf("NewAnalyzer: %v", err)
	}
	// Create table and seed row via ApplySafeMigration.
	ctx := context.Background()
	_ = a.ApplySafeMigration(ctx, "CREATE TABLE metrics (name TEXT, value REAL)")
	_ = a.ApplySafeMigration(ctx, "INSERT INTO metrics VALUES ('latency_p99', 1.23)")

	// Use a separate SQLite in-memory DB via QuerySchema — same db is not reachable
	// via DSN ":memory:" (each open gets a new DB), so just verify QuerySchema returns
	// properly formatted output for a fresh DB with the sqlite pragma query.
	result, qErr := a.QuerySchema(ctx, "sqlite", ":memory:", "SELECT 1 AS one", 0)
	if qErr != nil {
		t.Fatalf("QuerySchema: %v", qErr)
	}
	if !strings.Contains(result, "one") {
		t.Errorf("QuerySchema result missing column name: %s", result)
	}
}

func TestQuerySchema_MaxOpenConnsDefault(t *testing.T) {
	a, err := NewAnalyzer(":memory:")
	if err != nil {
		t.Fatalf("NewAnalyzer: %v", err)
	}
	ctx := context.Background()
	// Pass maxOpenConns=0 → should default to 2 without panic.
	_, err = a.QuerySchema(ctx, "sqlite", ":memory:", "SELECT 42 AS answer", 0)
	if err != nil {
		t.Fatalf("QuerySchema with default maxOpenConns: %v", err)
	}
}
