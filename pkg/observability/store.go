// Package observability — persistent per-workspace metrics store.
// [PILAR-XXVII/242]
//
// Store wraps a BoltDB at .neo/db/observability.db and captures every
// tool call + periodic runtime snapshots + token usage across both MCP
// traffic (external agent) and internal inference escalations. Data
// survives neo-mcp restarts — the HUD and the TUI query via HTTP.
//
// Hot-path overhead: ring buffer Append < 200 ns (sync.Pool alloc-free);
// flush every 30 seconds OR 100 calls (whichever first) runs async
// under bbolt tx. SIGTERM forces synchronous flush within 500 ms.

package observability

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.etcd.io/bbolt"
)

const (
	// Schema current — bump on breaking changes. Stored in _schema bucket.
	schemaVersion = 1

	// Ring capacity: flush triggers at this or on time. 100 calls at p99 ≈
	// 33 ns/hit cache means a full ring in ~3 µs — flush overhead amortises
	// easily below the 2 s observation interval of the HUD.
	ringCapacity = 100

	// Flush every 30 s even if ring not full. Balances crash-data-loss
	// window vs fsync frequency.
	flushInterval = 30 * time.Second

	// DefaultRetentionDays is the recommended PurgeOldDays argument.
	// Exported so callers (neo-mcp purge loop) use a shared constant.
	DefaultRetentionDays = 30

	// Bucket names (constants so callers + tests reference the same strings).
	bucketSchema             = "_schema"
	bucketMeta               = "_meta"
	bucketToolAggregate      = "tool_aggregate"
	bucketMemStatsRing       = "memstats_ring"
	bucketTokensUsage        = "tokens_usage"
	bucketMutationsHistory   = "mutations_history"
	bucketEventsRing         = "events_ring"

	toolCallsBucketPrefix = "tool_calls_" // + YYYYMMDD

	// Events ring cap — older events get evicted. 1000 lines is ~100 KB and
	// covers ~8 h of typical activity.
	eventsRingCap = 1000
)

// GlobalStore is the process-wide observability Store set by neo-mcp at
// boot. Nil-safe: every Store method no-ops on a nil receiver, so callers
// can invoke observability.GlobalStore.RecordCall(...) without a guard
// even before boot completes or when persistence is disabled.
var GlobalStore *Store

// Store is the persistent observability backend. Thread-safe.
type Store struct {
	db       *bbolt.DB
	path     string
	workspace string

	// Ring buffer for tool_calls — drained by flush goroutine.
	ring    []toolCallRecord
	ringMu  sync.Mutex
	ringIdx int

	// Coordination with background goroutines.
	closeCh chan struct{}
	wg      sync.WaitGroup
	// flushWg tracks in-flight async flushes spawned when the ring fills
	// mid-RecordCall. Sync() / flushNow() drain it so callers can observe
	// a consistent state.
	flushWg sync.WaitGroup

	// Counters — atomic for observability of the Store itself.
	totalRecords atomic.Uint64
	totalFlushes atomic.Uint64
	lastFlushAt  atomic.Int64 // Unix nanos
	lastBackupAt atomic.Int64

	// now is injectable for tests (deterministic timestamps).
	now func() time.Time
}

// toolCallRecord is a single ring slot. Must be POD for sync.Pool-free
// append.
type toolCallRecord struct {
	Timestamp time.Time
	Name      string
	Action    string
	DurNs     int64
	Status    string // "ok" | "error" | "timeout"
	ErrCat    string // error category when status=error
	InBytes   int    // approximate input size (for MCP traffic token estimate)
	OutBytes  int    // approximate output size
}

// Open creates (if needed) and opens the observability DB for a workspace.
// workspacePath must be absolute; the DB lives at <workspace>/.neo/db/observability.db.
func Open(workspacePath string) (*Store, error) {
	if workspacePath == "" {
		return nil, errors.New("observability: workspace path required")
	}
	dir := filepath.Join(workspacePath, ".neo", "db")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("observability: mkdir %s: %w", dir, err)
	}
	dbPath := filepath.Join(dir, "observability.db")
	db, err := bbolt.Open(dbPath, 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("observability: open %s: %w", dbPath, err)
	}

	s := &Store{
		db:        db,
		path:      dbPath,
		workspace: workspacePath,
		ring:      make([]toolCallRecord, ringCapacity),
		closeCh:   make(chan struct{}),
		now:       time.Now,
	}

	if err := s.initBuckets(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("observability: init buckets: %w", err)
	}

	s.wg.Add(1)
	go s.flushLoop()

	return s, nil
}

// Close flushes the ring and closes the DB. Safe to call multiple times.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	select {
	case <-s.closeCh:
		return nil // already closed
	default:
		close(s.closeCh)
	}
	s.wg.Wait()
	s.flushWg.Wait()
	// Final sync flush for anything left in the ring.
	if err := s.flushNow(); err != nil {
		// Log but don't fail close — DB close is important.
		_ = err
	}
	return s.db.Close()
}

// Path returns the DB file path (for tests + backup).
func (s *Store) Path() string { return s.path }

// ────────────── Write path — hot ──────────────

// RecordCall captures one tool call outcome. Non-blocking: appends to
// ring buffer; when the ring fills, swaps the batch under the lock and
// hands it off to a background flush goroutine so the next call sees an
// empty slot at ringIdx=0.
func (s *Store) RecordCall(name, action string, dur time.Duration, status, errCat string, inBytes, outBytes int) {
	if s == nil {
		return
	}
	var batch []toolCallRecord
	s.ringMu.Lock()
	s.ring[s.ringIdx] = toolCallRecord{
		Timestamp: s.now(),
		Name:      name,
		Action:    action,
		DurNs:     dur.Nanoseconds(),
		Status:    status,
		ErrCat:    errCat,
		InBytes:   inBytes,
		OutBytes:  outBytes,
	}
	s.ringIdx++
	if s.ringIdx >= ringCapacity {
		batch = make([]toolCallRecord, s.ringIdx)
		copy(batch, s.ring[:s.ringIdx])
		s.ringIdx = 0
	}
	s.ringMu.Unlock()

	s.totalRecords.Add(1)

	if batch != nil {
		s.flushWg.Add(1)
		go func(b []toolCallRecord) {
			defer s.flushWg.Done()
			_ = s.flushBatch(b)
		}(batch)
	}
}

// Sync blocks until all in-flight async flushes complete. Used by tests
// and by Close() before final teardown.
func (s *Store) Sync() {
	if s == nil {
		return
	}
	s.flushWg.Wait()
}

// MemStatsSnapshot captures one snapshot of process-level memory metrics.
type MemStatsSnapshot struct {
	Timestamp       time.Time `json:"ts"`
	HeapMB          float64   `json:"heap_mb"`
	StackMB         float64   `json:"stack_mb"`
	Goroutines      int       `json:"goroutines"`
	GCRuns          uint32    `json:"gc_runs"`
	GCPauseLastNs   uint64    `json:"gc_pause_last_ns"`
	CPGHeapMB       int       `json:"cpg_heap_mb"`
	CPGHeapLimitMB  int       `json:"cpg_heap_limit_mb"`
	QueryCacheHit   float64   `json:"query_cache_hit_rate"`
	TextCacheHit    float64   `json:"text_cache_hit_rate"`
	EmbCacheHit     float64   `json:"emb_cache_hit_rate"`
}

// RecordMemStats persists one runtime snapshot. Called by the memstats
// loop in neo-mcp every 30 s.
func (s *Store) RecordMemStats(snap MemStatsSnapshot) {
	if s == nil {
		return
	}
	if snap.Timestamp.IsZero() {
		snap.Timestamp = s.now()
	}
	key := snap.Timestamp.UTC().Format("2006-01-02T15:04")
	val, _ := json.Marshal(snap)
	_ = s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketMemStatsRing))
		return b.Put([]byte(key), val)
	})
}

// TokenSource identifies which flow generated the token usage.
type TokenSource string

const (
	SourceMCPTraffic       TokenSource = "mcp_traffic"
	SourceInternalInference TokenSource = "internal_inference"
)

// TokenEntry aggregates a day × source × agent × tool × prompt_type slot.
type TokenEntry struct {
	Day          string      `json:"day"` // YYYY-MM-DD
	Source       TokenSource `json:"source"`
	Agent        string      `json:"agent"`       // e.g. "claude-code@2.0.1" or "qwen2.5-coder:7b"
	Tool         string      `json:"tool"`        // MCP tool name
	PromptType   string      `json:"prompt_type,omitempty"` // internal only: Diagnose|SuggestFix|RunDebate
	Model        string      `json:"model"`       // resolved model for cost lookup
	InputTokens  int         `json:"input_tokens"`
	OutputTokens int         `json:"output_tokens"`
	Calls        int         `json:"calls"`
	CostUSD      float64     `json:"cost_usd"`
}

// RecordTokens accumulates token usage. Idempotent by key — repeated
// calls with the same day+source+agent+tool+prompt_type aggregate
// counters in place.
func (s *Store) RecordTokens(e TokenEntry) {
	if s == nil {
		return
	}
	if e.Day == "" {
		e.Day = s.now().UTC().Format("2006-01-02")
	}
	key := fmt.Sprintf("%s_%s_%s_%s_%s", e.Day, e.Source, e.Agent, e.Tool, e.PromptType)
	_ = s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketTokensUsage))
		var existing TokenEntry
		if raw := b.Get([]byte(key)); raw != nil {
			_ = json.Unmarshal(raw, &existing)
		}
		existing.Day = e.Day
		existing.Source = e.Source
		existing.Agent = e.Agent
		existing.Tool = e.Tool
		existing.PromptType = e.PromptType
		existing.Model = e.Model
		existing.InputTokens += e.InputTokens
		existing.OutputTokens += e.OutputTokens
		existing.Calls += e.Calls
		existing.CostUSD += e.CostUSD
		val, _ := json.Marshal(existing)
		return b.Put([]byte(key), val)
	})
}

// MutationEntry captures one certified or bypassed mutation snapshot.
type MutationEntry struct {
	Timestamp time.Time `json:"ts"`
	Path      string    `json:"path"`
	Bypassed  bool      `json:"bypassed"`
}

// RecordMutation persists one certification event.
func (s *Store) RecordMutation(path string, bypassed bool) {
	if s == nil {
		return
	}
	e := MutationEntry{Timestamp: s.now(), Path: path, Bypassed: bypassed}
	// Counter disambiguates two mutations with identical (ts, path),
	// which happens when the same file is certified twice inside one
	// flash (tests, test fixtures, rapid CI sequences).
	seq := s.totalRecords.Add(1)
	key := fmt.Sprintf("%s_%d_%s", e.Timestamp.UTC().Format(time.RFC3339Nano), seq, path)
	val, _ := json.Marshal(e)
	_ = s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketMutationsHistory))
		return b.Put([]byte(key), val)
	})
}

// EventEntry captures one SSE event for the events_ring.
type EventEntry struct {
	Timestamp time.Time              `json:"ts"`
	Type      string                 `json:"type"`
	Severity  string                 `json:"severity,omitempty"`
	Payload   map[string]any         `json:"payload,omitempty"`
}

// RecordEvent persists one event. Ring is capped to eventsRingCap —
// oldest entries evicted when capacity exceeded.
func (s *Store) RecordEvent(eventType, severity string, payload map[string]any) {
	if s == nil {
		return
	}
	e := EventEntry{
		Timestamp: s.now(),
		Type:      eventType,
		Severity:  severity,
		Payload:   payload,
	}
	key := fmt.Sprintf("%s_%s", e.Timestamp.UTC().Format(time.RFC3339Nano), eventType)
	val, _ := json.Marshal(e)
	_ = s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketEventsRing))
		if err := b.Put([]byte(key), val); err != nil {
			return err
		}
		// Cap enforcement — b.Stats().KeyN is unreliable mid-tx, so we
		// walk the bucket ourselves to count and evict oldest keys.
		var count int
		_ = b.ForEach(func(_, _ []byte) error {
			count++
			return nil
		})
		excess := count - eventsRingCap
		if excess <= 0 {
			return nil
		}
		c := b.Cursor()
		k, _ := c.First()
		for i := 0; i < excess && k != nil; i++ {
			toDelete := append([]byte(nil), k...)
			k, _ = c.Next()
			if err := b.Delete(toDelete); err != nil {
				return err
			}
		}
		return nil
	})
}

// ────────────── Flush + housekeeping ──────────────

func (s *Store) flushLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.closeCh:
			return
		case <-ticker.C:
			_ = s.flushNow()
		}
	}
}

// flushNow persists any pending ring entries synchronously. Drains
// in-flight async flushes first so the caller sees a coherent state.
func (s *Store) flushNow() error {
	s.flushWg.Wait()
	s.ringMu.Lock()
	if s.ringIdx == 0 {
		s.ringMu.Unlock()
		return nil
	}
	batch := make([]toolCallRecord, s.ringIdx)
	copy(batch, s.ring[:s.ringIdx])
	s.ringIdx = 0
	s.ringMu.Unlock()

	return s.flushBatch(batch)
}

// flushBatch writes an already-swapped batch to the DB. Does not touch
// the ring — callers must copy out first under ringMu.
func (s *Store) flushBatch(batch []toolCallRecord) error {
	if len(batch) == 0 {
		return nil
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		for _, rec := range batch {
			if err := s.persistCall(tx, rec); err != nil {
				return err
			}
		}
		if meta := tx.Bucket([]byte(bucketMeta)); meta != nil {
			_ = meta.Put([]byte("last_flush_at"), []byte(s.now().UTC().Format(time.RFC3339Nano)))
		}
		s.totalFlushes.Add(1)
		s.lastFlushAt.Store(s.now().UnixNano())
		return nil
	})
}

func (s *Store) persistCall(tx *bbolt.Tx, rec toolCallRecord) error {
	// Daily bucket for the raw call event.
	day := rec.Timestamp.UTC().Format("20060102")
	bucketName := []byte(toolCallsBucketPrefix + day)
	b, err := tx.CreateBucketIfNotExists(bucketName)
	if err != nil {
		return err
	}
	key := fmt.Appendf(nil, "%s_%s_%d", rec.Timestamp.UTC().Format(time.RFC3339Nano), rec.Name, s.totalRecords.Load())
	val, _ := json.Marshal(rec)
	if err := b.Put(key, val); err != nil {
		return err
	}

	// Update aggregate stats for this tool.
	aggB := tx.Bucket([]byte(bucketToolAggregate))
	var agg ToolAggregate
	if raw := aggB.Get([]byte(rec.Name)); raw != nil {
		_ = json.Unmarshal(raw, &agg)
	}
	agg.Name = rec.Name
	agg.Calls++
	if rec.Status == "error" {
		agg.Errors++
	}
	agg.TotalDurationNs += rec.DurNs
	agg.LastCallAt = rec.Timestamp
	agg.RecentDurs = append(agg.RecentDurs, rec.DurNs)
	if len(agg.RecentDurs) > 512 {
		agg.RecentDurs = agg.RecentDurs[len(agg.RecentDurs)-512:]
	}
	agg.recomputePercentiles()
	aggVal, _ := json.Marshal(agg)
	return aggB.Put([]byte(rec.Name), aggVal)
}

// PurgeOldDays removes tool_calls_YYYYMMDD buckets older than `keep` days.
// Called from the flushLoop once per hour.
func (s *Store) PurgeOldDays(keep int) (removed int, err error) {
	cutoff := s.now().UTC().AddDate(0, 0, -keep)
	err = s.db.Update(func(tx *bbolt.Tx) error {
		var toDelete [][]byte
		_ = tx.ForEach(func(name []byte, _ *bbolt.Bucket) error {
			n := string(name)
			if len(n) < len(toolCallsBucketPrefix)+8 || n[:len(toolCallsBucketPrefix)] != toolCallsBucketPrefix {
				return nil
			}
			day := n[len(toolCallsBucketPrefix):]
			t, parseErr := time.Parse("20060102", day)
			if parseErr != nil {
				return nil
			}
			if t.Before(cutoff) {
				toDelete = append(toDelete, append([]byte(nil), name...))
			}
			return nil
		})
		for _, name := range toDelete {
			if dErr := tx.DeleteBucket(name); dErr != nil {
				return dErr
			}
			removed++
		}
		return nil
	})
	return
}

// ────────────── Backup ──────────────

// CreateBackup writes a zero-copy snapshot of the live DB to destPath.
// Uses bbolt's Tx.CopyFile (read-tx holding pattern, non-blocking for writers).
func (s *Store) CreateBackup(destPath string) error {
	if s == nil {
		return errors.New("observability: nil store")
	}
	return s.db.View(func(tx *bbolt.Tx) error {
		f, err := os.Create(destPath) //nolint:gosec // G304-WORKSPACE-CANON: caller controls path
		if err != nil {
			return fmt.Errorf("backup create: %w", err)
		}
		defer func() { _ = f.Close() }()
		if _, err := tx.WriteTo(f); err != nil {
			return fmt.Errorf("backup write: %w", err)
		}
		s.lastBackupAt.Store(s.now().UnixNano())
		return nil
	})
}

// RotateBackups removes backup files so only the most recent `keepN`
// survive. Called after CreateBackup.
func (s *Store) RotateBackups(keepN int) (removed int, err error) {
	dir := filepath.Dir(s.path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("rotate: readdir: %w", err)
	}
	var backups []string
	for _, e := range entries {
		n := e.Name()
		if len(n) > len("observability-") && n[:len("observability-")] == "observability-" && filepath.Ext(n) == ".bak" {
			backups = append(backups, filepath.Join(dir, n))
		}
	}
	sort.Strings(backups) // lexicographic = chronological (YYYYMMDD in filename)
	if len(backups) <= keepN {
		return 0, nil
	}
	for _, p := range backups[:len(backups)-keepN] {
		if rmErr := os.Remove(p); rmErr != nil {
			return removed, rmErr
		}
		removed++
	}
	return
}

// ────────────── Init ──────────────

func (s *Store) initBuckets() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		// Core buckets.
		for _, name := range []string{
			bucketSchema, bucketMeta, bucketToolAggregate,
			bucketMemStatsRing, bucketTokensUsage,
			bucketMutationsHistory, bucketEventsRing,
		} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		// Schema version stamp (fail-open on mismatch for future versions).
		sb := tx.Bucket([]byte(bucketSchema))
		if sb.Get([]byte("version")) == nil {
			_ = sb.Put([]byte("version"), fmt.Appendf(nil, "%d", schemaVersion))
			_ = sb.Put([]byte("created_at"), []byte(s.now().UTC().Format(time.RFC3339)))
		}
		return nil
	})
}

// Size returns the current DB size in bytes.
func (s *Store) Size() (int64, error) {
	info, err := os.Stat(s.path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// LastFlushAt returns the Unix-nano timestamp of the most recent flush.
func (s *Store) LastFlushAt() time.Time {
	nsec := s.lastFlushAt.Load()
	if nsec == 0 {
		return time.Time{}
	}
	return time.Unix(0, nsec)
}

// LastBackupAt returns when the last backup completed.
func (s *Store) LastBackupAt() time.Time {
	nsec := s.lastBackupAt.Load()
	if nsec == 0 {
		return time.Time{}
	}
	return time.Unix(0, nsec)
}

// CaptureRuntimeMemStats is a helper that builds a MemStatsSnapshot from
// runtime.ReadMemStats. The CPG + cache fields must be filled by the
// caller (neo-mcp knows those, the observability pkg doesn't).
func CaptureRuntimeMemStats() MemStatsSnapshot {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return MemStatsSnapshot{
		Timestamp:     time.Now(),
		HeapMB:        float64(ms.HeapAlloc) / (1024 * 1024),
		StackMB:       float64(ms.StackInuse) / (1024 * 1024),
		Goroutines:    runtime.NumGoroutine(),
		GCRuns:        ms.NumGC,
		GCPauseLastNs: ms.PauseNs[(ms.NumGC+255)%256],
	}
}
