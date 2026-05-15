// pkg/observability/store_test.go — Store round-trip + concurrency tests.
// [PILAR-XXVII/242.J]

package observability

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// newTestStore opens a fresh Store in a temp workspace.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestStore_OpenAndClose — Open creates the DB + buckets; Close is idempotent.
func TestStore_OpenAndClose(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := os.Stat(s.Path()); err != nil {
		t.Errorf("DB file missing: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	// Idempotent — second Close must not panic or error.
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestStore_RecordCall_FlushRoundTrip — RecordCall + forced flushNow persists
// to daily bucket + updates ToolAggregate.
func TestStore_RecordCall_FlushRoundTrip(t *testing.T) {
	s := newTestStore(t)
	s.RecordCall("neo_radar", "BRIEFING", 1500*time.Nanosecond, "ok", "", 42, 512)
	s.RecordCall("neo_radar", "BRIEFING", 2000*time.Nanosecond, "error", "timeout", 10, 0)

	if err := s.flushNow(); err != nil {
		t.Fatalf("flushNow: %v", err)
	}
	if s.TotalFlushes() == 0 {
		t.Error("TotalFlushes should be > 0 after flushNow")
	}

	aggs := s.ToolAggregates()
	agg, ok := aggs["neo_radar"]
	if !ok {
		t.Fatal("neo_radar aggregate missing")
	}
	if agg.Calls != 2 {
		t.Errorf("Calls = %d, want 2", agg.Calls)
	}
	if agg.Errors != 1 {
		t.Errorf("Errors = %d, want 1", agg.Errors)
	}
	if agg.ErrorRate() != 0.5 {
		t.Errorf("ErrorRate = %f, want 0.5", agg.ErrorRate())
	}
	if agg.TotalDurationNs != 3500 {
		t.Errorf("TotalDurationNs = %d, want 3500", agg.TotalDurationNs)
	}
}

// TestStore_RecordCall_PerActionAggregate covers the [Phase 0.B / Speed-First]
// dual-write: persistCall maintains both the bare tool-name aggregate AND the
// "<name>/<action>" composite, so neo_tool_stats can surface that
// neo_radar/BLAST_RADIUS has different p99 than neo_radar/AST_AUDIT without
// the operator needing to scrape per-intent token_spend data.
func TestStore_RecordCall_PerActionAggregate(t *testing.T) {
	s := newTestStore(t)
	// Two intents on the same tool — different durations.
	s.RecordCall("neo_radar", "BLAST_RADIUS", 400*time.Millisecond, "ok", "", 100, 200)
	s.RecordCall("neo_radar", "BLAST_RADIUS", 460*time.Millisecond, "ok", "", 100, 200)
	s.RecordCall("neo_radar", "AST_AUDIT", 5*time.Millisecond, "ok", "", 50, 100)
	// One call without action — should hit only the bare key, not produce
	// a "neo_radar/" composite entry.
	s.RecordCall("neo_radar", "", 10*time.Millisecond, "ok", "", 1, 1)

	if err := s.flushNow(); err != nil {
		t.Fatalf("flushNow: %v", err)
	}

	aggs := s.ToolAggregates()

	// Bare aggregate keeps existing semantics.
	if got := aggs["neo_radar"].Calls; got != 4 {
		t.Errorf("neo_radar (bare) Calls = %d, want 4", got)
	}

	// Per-action composites surface.
	blast, ok := aggs["neo_radar/BLAST_RADIUS"]
	if !ok {
		t.Fatal("neo_radar/BLAST_RADIUS aggregate missing — per-action dual-write not effective")
	}
	if blast.Calls != 2 {
		t.Errorf("neo_radar/BLAST_RADIUS Calls = %d, want 2", blast.Calls)
	}
	if blast.Name != "neo_radar/BLAST_RADIUS" {
		t.Errorf("BLAST_RADIUS agg.Name = %q, want neo_radar/BLAST_RADIUS", blast.Name)
	}

	audit, ok := aggs["neo_radar/AST_AUDIT"]
	if !ok {
		t.Fatal("neo_radar/AST_AUDIT aggregate missing")
	}
	if audit.Calls != 1 {
		t.Errorf("neo_radar/AST_AUDIT Calls = %d, want 1", audit.Calls)
	}

	// The whole point: per-action p99s diverge — BLAST_RADIUS slow tail,
	// AST_AUDIT fast — the bare aggregate hides this.
	if blast.P99Ns <= audit.P99Ns {
		t.Errorf("expected BLAST_RADIUS p99 (%dns) > AST_AUDIT p99 (%dns); the whole point of per-action stats is they differ",
			blast.P99Ns, audit.P99Ns)
	}

	// Empty action must NOT produce a "neo_radar/" entry.
	if _, has := aggs["neo_radar/"]; has {
		t.Error("aggregate keyed by empty action should not exist — would pollute the table")
	}
}

// TestStore_RingFlushOnCapacity — crossing ringCapacity triggers async flush.
func TestStore_RingFlushOnCapacity(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < ringCapacity+5; i++ {
		s.RecordCall("neo_tool_stats", "", 100*time.Nanosecond, "ok", "", 1, 1)
	}
	// Give the async flush goroutine a moment.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.TotalFlushes() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s.TotalFlushes() == 0 {
		t.Error("expected async flush after filling ring, got 0 flushes")
	}
	// Drain remaining.
	if err := s.flushNow(); err != nil {
		t.Fatalf("flushNow: %v", err)
	}
	aggs := s.ToolAggregates()
	if agg := aggs["neo_tool_stats"]; agg.Calls != ringCapacity+5 {
		t.Errorf("Calls = %d, want %d", agg.Calls, ringCapacity+5)
	}
}

// TestStore_ConcurrentRecordCall — 10 goroutines × 100 calls each must
// not race (run with -race).
func TestStore_ConcurrentRecordCall(t *testing.T) {
	s := newTestStore(t)
	const workers = 10
	const perWorker = 100
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			name := fmt.Sprintf("worker_%d", id)
			for i := 0; i < perWorker; i++ {
				s.RecordCall(name, "action", time.Duration(i+1)*time.Nanosecond, "ok", "", i, i*2)
			}
		}(w)
	}
	wg.Wait()
	if err := s.flushNow(); err != nil {
		t.Fatalf("flushNow: %v", err)
	}
	if got := s.TotalRecords(); got != uint64(workers*perWorker) {
		t.Errorf("TotalRecords = %d, want %d", got, workers*perWorker)
	}
	aggs := s.ToolAggregates()
	// [Phase 0.B / Speed-First] Sum only bare-tool aggregates. persistCall
	// dual-writes one row per record into both "worker_N" (bare) and
	// "worker_N/action" (per-action composite); counting both would
	// double-count. The bare aggregates carry the canonical 1:1 invariant.
	totalCalls := 0
	for name, a := range aggs {
		if strings.Contains(name, "/") {
			continue
		}
		totalCalls += a.Calls
	}
	if totalCalls != workers*perWorker {
		t.Errorf("sum of bare-tool aggregate Calls = %d, want %d", totalCalls, workers*perWorker)
	}
	// Sanity: the dual-write also produced one composite per call.
	composite := 0
	for name, a := range aggs {
		if strings.Contains(name, "/") {
			composite += a.Calls
		}
	}
	if composite != workers*perWorker {
		t.Errorf("sum of composite aggregate Calls = %d, want %d (one per record under action=%q)",
			composite, workers*perWorker, "action")
	}
}

// TestStore_RecordMemStats — snapshot round-trip via MemStatsHistory.
func TestStore_RecordMemStats(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Minute)
	s.RecordMemStats(MemStatsSnapshot{
		Timestamp: now,
		HeapMB:    42.5,
		Goroutines: 12,
	})
	s.RecordMemStats(MemStatsSnapshot{
		Timestamp: now.Add(1 * time.Minute),
		HeapMB:    50.0,
	})
	out := s.MemStatsHistory(now.Add(-1 * time.Hour))
	if len(out) != 2 {
		t.Fatalf("MemStatsHistory len = %d, want 2", len(out))
	}
	if out[0].HeapMB != 42.5 {
		t.Errorf("first HeapMB = %f, want 42.5", out[0].HeapMB)
	}
	if out[1].HeapMB != 50.0 {
		t.Errorf("second HeapMB = %f, want 50.0", out[1].HeapMB)
	}
}

// TestStore_RecordTokens_Aggregate — same key aggregates counters.
func TestStore_RecordTokens_Aggregate(t *testing.T) {
	s := newTestStore(t)
	day := "2026-04-18"
	s.RecordTokens(TokenEntry{
		Day: day, Source: SourceMCPTraffic, Agent: "claude-code",
		Tool: "neo_radar", Model: "opus-4-7",
		InputTokens: 100, OutputTokens: 200, Calls: 1, CostUSD: 0.005,
	})
	s.RecordTokens(TokenEntry{
		Day: day, Source: SourceMCPTraffic, Agent: "claude-code",
		Tool: "neo_radar", Model: "opus-4-7",
		InputTokens: 50, OutputTokens: 100, Calls: 1, CostUSD: 0.002,
	})
	// Different source — must NOT aggregate.
	s.RecordTokens(TokenEntry{
		Day: day, Source: SourceInternalInference, Agent: "qwen2.5-coder",
		Tool: "pkg/inference", PromptType: "Diagnose", Model: "qwen2.5-coder:7b",
		InputTokens: 30, OutputTokens: 60, Calls: 1, CostUSD: 0,
	})

	bySrc := s.TokensBySource(day)
	mcp := bySrc[SourceMCPTraffic]
	if len(mcp) != 1 {
		t.Fatalf("MCP entries = %d, want 1 (aggregated)", len(mcp))
	}
	if mcp[0].InputTokens != 150 {
		t.Errorf("MCP InputTokens = %d, want 150", mcp[0].InputTokens)
	}
	if mcp[0].Calls != 2 {
		t.Errorf("MCP Calls = %d, want 2", mcp[0].Calls)
	}
	if mcp[0].CostUSD < 0.006 || mcp[0].CostUSD > 0.008 {
		t.Errorf("MCP CostUSD = %f, want ≈0.007", mcp[0].CostUSD)
	}
	inf := bySrc[SourceInternalInference]
	if len(inf) != 1 {
		t.Fatalf("Internal entries = %d, want 1", len(inf))
	}
}

// TestStore_RecordMutation_Hotspots — certified + bypassed are separated
// and hotspot ranking honours frequency.
func TestStore_RecordMutation_Hotspots(t *testing.T) {
	s := newTestStore(t)
	s.RecordMutation("pkg/rag/hnsw.go", false)
	s.RecordMutation("pkg/rag/hnsw.go", false)
	s.RecordMutation("pkg/rag/hnsw.go", false)
	s.RecordMutation("cmd/neo-mcp/main.go", false)
	s.RecordMutation("pkg/legacy/x.go", true) // bypassed

	certified, bypassed, hotspots := s.MutationsLast24h()
	if certified != 4 {
		t.Errorf("certified = %d, want 4", certified)
	}
	if bypassed != 1 {
		t.Errorf("bypassed = %d, want 1", bypassed)
	}
	if len(hotspots) == 0 || hotspots[0].Path != "pkg/rag/hnsw.go" || hotspots[0].Count != 3 {
		t.Errorf("top hotspot wrong: %+v", hotspots)
	}
}

// TestStore_RecordEvent_RingCap — once past cap, oldest entries evict.
func TestStore_RecordEvent_RingCap(t *testing.T) {
	s := newTestStore(t)
	// Push cap+50 events — we expect len == cap at the end.
	for i := 0; i < eventsRingCap+50; i++ {
		// Slight timestamp variation so keys differ.
		s.now = func() time.Time { return time.Unix(int64(i), 0).UTC() }
		s.RecordEvent("heartbeat", "info", map[string]any{"i": i})
	}
	// RecentEvents fetches last-N — asking for more than cap is fine.
	events := s.RecentEvents(eventsRingCap + 100)
	if len(events) > eventsRingCap {
		t.Errorf("events length = %d > cap %d (ring not enforced)", len(events), eventsRingCap)
	}
	if len(events) < eventsRingCap/2 {
		t.Errorf("events length = %d, suspiciously small", len(events))
	}
}

// TestStore_ToolAggregate_Percentiles — p50/p95/p99 computed correctly
// from RecentDurs.
func TestStore_ToolAggregate_Percentiles(t *testing.T) {
	s := newTestStore(t)
	// 100 calls with durations 1..100 ns. After sort: p50=50, p95=95, p99=99.
	for i := 1; i <= 100; i++ {
		s.RecordCall("perf", "x", time.Duration(i)*time.Nanosecond, "ok", "", 0, 0)
	}
	if err := s.flushNow(); err != nil {
		t.Fatalf("flushNow: %v", err)
	}
	aggs := s.ToolAggregates()
	agg := aggs["perf"]
	if agg.P50Ns != 51 {
		// sorted[50] is the 51st element, i.e. value 51.
		t.Errorf("P50Ns = %d, want 51", agg.P50Ns)
	}
	if agg.P95Ns != 96 {
		t.Errorf("P95Ns = %d, want 96", agg.P95Ns)
	}
	if agg.P99Ns != 100 {
		t.Errorf("P99Ns = %d, want 100", agg.P99Ns)
	}
}

// TestStore_TokensLast7Days — entries older than 7 days drop, younger
// ones aggregate per day across sources.
func TestStore_TokensLast7Days(t *testing.T) {
	s := newTestStore(t)
	today := time.Now().UTC()
	// 1 day-old, within window.
	day1 := today.AddDate(0, 0, -1).Format("2006-01-02")
	// 8 days-old, outside window.
	day8 := today.AddDate(0, 0, -8).Format("2006-01-02")
	s.RecordTokens(TokenEntry{Day: day1, Source: SourceMCPTraffic, Agent: "a", Tool: "t", InputTokens: 100, OutputTokens: 200, CostUSD: 0.01})
	s.RecordTokens(TokenEntry{Day: day1, Source: SourceInternalInference, Agent: "b", Tool: "u", InputTokens: 50, OutputTokens: 100, CostUSD: 0})
	s.RecordTokens(TokenEntry{Day: day8, Source: SourceMCPTraffic, Agent: "c", Tool: "v", InputTokens: 999, OutputTokens: 999, CostUSD: 9.9})
	out := s.TokensLast7Days()
	if len(out) != 1 {
		t.Fatalf("summaries = %d, want 1 (8-day entry should be filtered)", len(out))
	}
	got := out[0]
	if got.Day != day1 {
		t.Errorf("Day = %q, want %q", got.Day, day1)
	}
	if got.MCPInput != 100 || got.MCPOutput != 200 {
		t.Errorf("MCP sums wrong: %+v", got)
	}
	if got.InternalInput != 50 || got.InternalOutput != 100 {
		t.Errorf("Internal sums wrong: %+v", got)
	}
	if got.CostUSD < 0.009 || got.CostUSD > 0.011 {
		t.Errorf("CostUSD = %f, want ≈0.01", got.CostUSD)
	}
}

// TestStore_RecentEvents_Ordering — events come back newest-first.
func TestStore_RecentEvents_Ordering(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 5; i++ {
		s.now = func() time.Time { return time.Unix(int64(i), 0).UTC() }
		s.RecordEvent("ev", "info", map[string]any{"i": i})
	}
	evs := s.RecentEvents(10)
	if len(evs) != 5 {
		t.Fatalf("got %d events, want 5", len(evs))
	}
	// First returned must be the latest (i=4).
	got, ok := evs[0].Payload["i"]
	if !ok {
		t.Fatalf("payload missing 'i' field: %+v", evs[0])
	}
	// JSON unmarshals ints as float64.
	if f, _ := got.(float64); int(f) != 4 {
		t.Errorf("newest event i = %v, want 4", got)
	}
}

// TestStore_SchemaVersion_FailOpen — if the on-disk version is in the
// future (e.g. v999), Open still succeeds; callers can read raw.
func TestStore_SchemaVersion_FailOpen(t *testing.T) {
	dir := t.TempDir()
	// First open to create the DB.
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	_ = s.Close()

	// Corrupt the schema version to something unknown.
	dbPath := filepath.Join(dir, ".neo", "db", "observability.db")
	db, err := bbolt.Open(dbPath, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatalf("reopen raw: %v", err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(bucketSchema)).Put([]byte("version"), []byte("999"))
	}); err != nil {
		t.Fatalf("bump schema: %v", err)
	}
	_ = db.Close()

	// Re-open with Store — must succeed (fail-open).
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("re-open with v999: %v", err)
	}
	defer s2.Close()
	// And be usable.
	s2.RecordCall("post_mig", "", 1*time.Nanosecond, "ok", "", 0, 0)
	if err := s2.flushNow(); err != nil {
		t.Errorf("flushNow after fail-open: %v", err)
	}
}

// TestStore_PurgeOldDays — old tool_calls buckets are deleted.
func TestStore_PurgeOldDays(t *testing.T) {
	s := newTestStore(t)
	// Seed three day buckets directly: today, 5d ago, 40d ago.
	now := time.Now().UTC()
	days := []string{
		now.Format("20060102"),
		now.AddDate(0, 0, -5).Format("20060102"),
		now.AddDate(0, 0, -40).Format("20060102"),
	}
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		for _, d := range days {
			if _, err := tx.CreateBucketIfNotExists([]byte(toolCallsBucketPrefix + d)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed buckets: %v", err)
	}

	removed, err := s.PurgeOldDays(30)
	if err != nil {
		t.Fatalf("PurgeOldDays: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (only the 40d-old bucket)", removed)
	}

	// Verify the 40d bucket is gone, the others survive.
	_ = s.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket([]byte(toolCallsBucketPrefix + days[2])) != nil {
			t.Errorf("40d bucket survived purge")
		}
		for _, d := range days[:2] {
			if tx.Bucket([]byte(toolCallsBucketPrefix + d)) == nil {
				t.Errorf("recent bucket %s was wrongly purged", d)
			}
		}
		return nil
	})
}

// TestStore_RecordCall_NilStore — nil Store must not panic.
func TestStore_RecordCall_NilStore(t *testing.T) {
	var s *Store
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil RecordCall panicked: %v", r)
		}
	}()
	s.RecordCall("x", "", 1, "ok", "", 0, 0)
	s.RecordMemStats(MemStatsSnapshot{})
	s.RecordTokens(TokenEntry{})
	s.RecordMutation("p", false)
	s.RecordEvent("t", "", nil)
	if got := s.TotalRecords(); got != 0 {
		t.Errorf("nil TotalRecords = %d, want 0", got)
	}
}

// TestStore_CaptureRuntimeMemStats — sanity check the helper.
func TestStore_CaptureRuntimeMemStats(t *testing.T) {
	snap := CaptureRuntimeMemStats()
	if snap.Timestamp.IsZero() {
		t.Error("Timestamp not set")
	}
	if snap.HeapMB <= 0 {
		t.Errorf("HeapMB = %f, want > 0", snap.HeapMB)
	}
	if snap.Goroutines < 1 {
		t.Errorf("Goroutines = %d, want ≥ 1", snap.Goroutines)
	}
}

// TestStore_Persistence_Reopen — data survives close+reopen.
func TestStore_Persistence_Reopen(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s1.RecordCall("persistent_tool", "", 1000*time.Nanosecond, "ok", "", 1, 2)
	if err := s1.flushNow(); err != nil {
		t.Fatalf("flushNow: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer s2.Close()
	aggs := s2.ToolAggregates()
	if agg, ok := aggs["persistent_tool"]; !ok || agg.Calls != 1 {
		t.Errorf("aggregate did not survive restart: %+v", aggs)
	}
}

// TestStore_TokenEntry_KeyUniqueness — ensure PromptType differentiates
// internal inference rows (Diagnose vs SuggestFix must not overwrite).
func TestStore_TokenEntry_KeyUniqueness(t *testing.T) {
	s := newTestStore(t)
	day := "2026-04-18"
	s.RecordTokens(TokenEntry{
		Day: day, Source: SourceInternalInference, Agent: "a", Tool: "infer",
		PromptType: "Diagnose", InputTokens: 10, Calls: 1,
	})
	s.RecordTokens(TokenEntry{
		Day: day, Source: SourceInternalInference, Agent: "a", Tool: "infer",
		PromptType: "SuggestFix", InputTokens: 20, Calls: 1,
	})
	entries := s.TokensBySource(day)[SourceInternalInference]
	if len(entries) != 2 {
		t.Errorf("entries = %d, want 2 (Diagnose + SuggestFix distinct)", len(entries))
	}
}

// TestStore_RawBucketContent — sanity: raw BoltDB key exists for a
// recorded call.
func TestStore_RawBucketContent(t *testing.T) {
	s := newTestStore(t)
	s.RecordCall("raw_probe", "", 7*time.Nanosecond, "ok", "", 1, 1)
	if err := s.flushNow(); err != nil {
		t.Fatalf("flushNow: %v", err)
	}
	day := time.Now().UTC().Format("20060102")
	var found bool
	_ = s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(toolCallsBucketPrefix + day))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var rec toolCallRecord
			if err := json.Unmarshal(v, &rec); err == nil && rec.Name == "raw_probe" {
				found = true
			}
			return nil
		})
	})
	if !found {
		t.Error("raw_probe record not found in daily bucket")
	}
}

// ────── Benchmarks ──────

// BenchmarkRecordCall — realistic workload including amortized async
// flushes. This is the number the hot path actually observes under load.
func BenchmarkRecordCall(b *testing.B) {
	dir := b.TempDir()
	s, err := Open(dir)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer s.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.RecordCall("bench", "x", 100*time.Nanosecond, "ok", "", 1, 1)
	}
}

// BenchmarkRecordCall_Append — pure append cost (no flush). We reset
// the ring after every ringCapacity-1 calls to guarantee the fill path
// never trips. Target: < 200 ns/op (the hot-path claim).
func BenchmarkRecordCall_Append(b *testing.B) {
	dir := b.TempDir()
	s, err := Open(dir)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer s.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.RecordCall("bench", "x", 100*time.Nanosecond, "ok", "", 1, 1)
		if (i+1)%(ringCapacity-1) == 0 {
			b.StopTimer()
			s.ringMu.Lock()
			s.ringIdx = 0
			s.ringMu.Unlock()
			b.StartTimer()
		}
	}
}

// BenchmarkFlush100 — flush a full ring; target < 10 ms.
func BenchmarkFlush100(b *testing.B) {
	dir := b.TempDir()
	s, err := Open(dir)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer s.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		for j := 0; j < ringCapacity; j++ {
			s.RecordCall("bench", "x", 100*time.Nanosecond, "ok", "", 1, 1)
		}
		b.StartTimer()
		if err := s.flushNow(); err != nil {
			b.Fatalf("flushNow: %v", err)
		}
	}
}
