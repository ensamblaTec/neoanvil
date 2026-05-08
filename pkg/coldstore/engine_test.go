package coldstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/config"
)

func TestEngineLifecycle(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test_cold.db")

	engine, err := OpenEngine(dbPath, config.ColdstoreConfig{MaxOpenConns: 3, MaxIdleConns: 2, DefaultQueryLimit: 50})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	ctx := context.Background()

	// Archive metrics
	metrics := []MetricRecord{
		{Timestamp: time.Now().Unix(), Category: "tool_latency", MetricName: "neo_radar", Value: 42.5, WorkspaceID: "test"},
		{Timestamp: time.Now().Unix(), Category: "tool_latency", MetricName: "neo_certify", Value: 120.3, WorkspaceID: "test"},
		{Timestamp: time.Now().Add(-24 * time.Hour).Unix(), Category: "gc_pressure", MetricName: "num_gc", Value: 3, WorkspaceID: "test"},
	}
	n, err := engine.ArchiveMetrics(ctx, metrics)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("expected 3 archived, got %d", n)
	}

	// Archive memex
	memex := []MemexArchive{
		{Timestamp: time.Now().Unix(), Topic: "circuit-breaker", Scope: "pkg/sre", Content: "Breaker trips at 5 consecutive failures", WorkspaceID: "test"},
	}
	n, err = engine.ArchiveMemex(ctx, memex)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 memex archived, got %d", n)
	}

	// Summary
	summary, err := engine.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.MetricCount != 3 {
		t.Errorf("expected 3 metrics, got %d", summary.MetricCount)
	}
	if summary.MemexCount != 1 {
		t.Errorf("expected 1 memex, got %d", summary.MemexCount)
	}

	// Trend query
	trends, err := engine.QueryMetricTrend(ctx, "tool_latency", "neo_radar", "day",
		time.Now().Add(-48*time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(trends) == 0 {
		t.Error("expected at least 1 trend point")
	}

	// Memex search
	results, err := engine.QueryMemexByTopic(ctx, "circuit", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 memex result, got %d", len(results))
	}
}

func TestEngineEmpty(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "empty.db")

	engine, err := OpenEngine(dbPath, config.ColdstoreConfig{MaxOpenConns: 3, MaxIdleConns: 2, DefaultQueryLimit: 50})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	ctx := context.Background()

	// Empty archive should be no-op
	n, err := engine.ArchiveMetrics(ctx, nil)
	if err != nil || n != 0 {
		t.Errorf("expected 0/nil, got %d/%v", n, err)
	}

	summary, err := engine.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.MetricCount != 0 {
		t.Errorf("expected 0 metrics, got %d", summary.MetricCount)
	}
}
