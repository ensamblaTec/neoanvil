// cmd/neo-nexus/plugin_async.go — ÉPICA 376.A+B: async task store for
// fire-and-forget plugin calls. BoltDB-backed, goroutine-dispatched.
//
// Flow: Submit → go RunAsync → Complete/Fail → SSE event → poll Get.
// Prevents token waste from client-cancelled DeepSeek API calls.

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/plugin"

	bolt "go.etcd.io/bbolt"
)

const (
	asyncBucket     = "plugin_async_tasks"
	batchBucket     = "plugin_async_batches" // [Phase 4.B / Speed-First] batch_id → []taskID
	asyncMaxPending = 100
	asyncCallTTL    = 300 * time.Second

	// asyncIDPrefix / batchIDPrefix tag IDs minted by this store. They are the
	// routing discriminator in handleAsyncDispatch: a task_id carrying
	// asyncIDPrefix is unambiguously a Nexus async task (poll it); one without
	// belongs to a plugin-side mechanism (e.g. generate_boilerplate's bgtask_*)
	// and must fall through to the plugin untouched. Package-owned protocol
	// constants — newAsyncID / SubmitBatch are their sole minters.
	asyncIDPrefix = "async_"
	batchIDPrefix = "batch_"
)

type AsyncTaskStatus string

const (
	AsyncPending  AsyncTaskStatus = "pending"
	AsyncRunning  AsyncTaskStatus = "running"
	AsyncDone     AsyncTaskStatus = "done"
	AsyncError    AsyncTaskStatus = "error"
)

type AsyncTask struct {
	ID          string          `json:"task_id"`
	Plugin      string          `json:"plugin"`
	Action      string          `json:"action"`
	Status      AsyncTaskStatus `json:"status"`
	CreatedAt   time.Time       `json:"created_at"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	ErrorMsg    string          `json:"error_msg,omitempty"`
	TokensIn    int             `json:"tokens_in,omitempty"`
	TokensOut   int             `json:"tokens_out,omitempty"`
	ElapsedMs   int64           `json:"elapsed_ms,omitempty"`
}

type AsyncTaskStore struct {
	db *bolt.DB
	mu sync.RWMutex
}

func NewAsyncTaskStore(dbPath string) (*AsyncTaskStore, error) {
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("async store: open %s: %w", dbPath, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists([]byte(asyncBucket)); err != nil {
			return err
		}
		// [Phase 4.B / Speed-First] batch mapping bucket — survives restart so
		// handleBatchPoll for a batch_id submitted before reboot doesn't return
		// "batch not found" while the AsyncTask rows themselves are intact.
		_, err := tx.CreateBucketIfNotExists([]byte(batchBucket))
		return err
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("async store: create bucket: %w", err)
	}
	return &AsyncTaskStore{db: db}, nil
}

// SaveBatchMapping persists batch_id → []taskID so handleBatchPoll can
// resolve batches across a Nexus restart. The mapping outlives the
// individual AsyncTask rows by design — Cleanup may reap stale tasks,
// but the batch entry stays until ReapBatchMappings sweeps it. Idempotent
// — overwrites any existing entry for the same batchID. [Phase 4.B]
func (s *AsyncTaskStore) SaveBatchMapping(batchID string, taskIDs []string) error {
	if s == nil {
		return nil
	}
	data, err := json.Marshal(taskIDs)
	if err != nil {
		return fmt.Errorf("batch mapping marshal: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(batchBucket)).Put([]byte(batchID), data)
	})
}

// GetBatchMapping returns the task IDs originally enqueued under batchID.
// (nil, false) means the batch is unknown to this store — caller surfaces
// a clean error. Restart-safe: a batch submitted before reboot still
// resolves after. [Phase 4.B]
func (s *AsyncTaskStore) GetBatchMapping(batchID string) ([]string, bool) {
	if s == nil {
		return nil, false
	}
	var taskIDs []string
	var found bool
	s.mu.RLock()
	defer s.mu.RUnlock()
	_ = s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(batchBucket)).Get([]byte(batchID))
		if raw == nil {
			return nil
		}
		if err := json.Unmarshal(raw, &taskIDs); err != nil {
			return err
		}
		found = true
		return nil
	})
	return taskIDs, found
}

func (s *AsyncTaskStore) Submit(pluginName, action string) (string, error) {
	id := newAsyncID()
	task := AsyncTask{
		ID:        id,
		Plugin:    pluginName,
		Action:    action,
		Status:    AsyncPending,
		CreatedAt: time.Now(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var pendingCount int
	if err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(asyncBucket))
		return b.ForEach(func(_, v []byte) error {
			var t AsyncTask
			if json.Unmarshal(v, &t) == nil && (t.Status == AsyncPending || t.Status == AsyncRunning) {
				pendingCount++
			}
			return nil
		})
	}); err != nil {
		return "", err
	}
	if pendingCount >= asyncMaxPending {
		return "", fmt.Errorf("async queue full (%d/%d pending)", pendingCount, asyncMaxPending)
	}

	data, err := json.Marshal(task)
	if err != nil {
		return "", err
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(asyncBucket)).Put([]byte(id), data)
	}); err != nil {
		return "", err
	}
	return id, nil
}

func (s *AsyncTaskStore) Get(id string) (*AsyncTask, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var task AsyncTask
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(asyncBucket)).Get([]byte(id))
		if v == nil {
			return fmt.Errorf("task %q not found", id)
		}
		return json.Unmarshal(v, &task)
	})
	if err != nil {
		return nil, err
	}
	return &task, nil
}

func (s *AsyncTaskStore) List(pluginFilter string, statusFilter AsyncTaskStatus) ([]AsyncTask, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []AsyncTask
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(asyncBucket)).ForEach(func(_, v []byte) error {
			var t AsyncTask
			if json.Unmarshal(v, &t) != nil {
				return nil
			}
			if pluginFilter != "" && t.Plugin != pluginFilter {
				return nil
			}
			if statusFilter != "" && t.Status != statusFilter {
				return nil
			}
			out = append(out, t)
			return nil
		})
	})
	return out, err
}

func (s *AsyncTaskStore) setStatus(id string, status AsyncTaskStatus, result json.RawMessage, errMsg string, elapsed time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(asyncBucket))
		v := b.Get([]byte(id))
		if v == nil {
			return fmt.Errorf("task %q not found", id)
		}
		var task AsyncTask
		if err := json.Unmarshal(v, &task); err != nil {
			return err
		}
		task.Status = status
		if status == AsyncDone || status == AsyncError {
			now := time.Now()
			task.CompletedAt = &now
			task.ElapsedMs = elapsed.Milliseconds()
		}
		if result != nil {
			task.Result = result
		}
		if errMsg != "" {
			task.ErrorMsg = errMsg
		}
		data, err := json.Marshal(task)
		if err != nil {
			return err
		}
		return b.Put([]byte(id), data)
	})
}

func (s *AsyncTaskStore) Complete(id string, result json.RawMessage, elapsed time.Duration) error {
	return s.setStatus(id, AsyncDone, result, "", elapsed)
}

func (s *AsyncTaskStore) Fail(id string, errMsg string, elapsed time.Duration) error {
	return s.setStatus(id, AsyncError, nil, errMsg, elapsed)
}

func (s *AsyncTaskStore) Cleanup(olderThan time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-olderThan)
	var removed int
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(asyncBucket))
		var toDelete [][]byte
		_ = b.ForEach(func(k, v []byte) error {
			var t AsyncTask
			if json.Unmarshal(v, &t) != nil {
				toDelete = append(toDelete, append([]byte{}, k...))
				return nil
			}
			if (t.Status == AsyncDone || t.Status == AsyncError) && t.CompletedAt != nil && t.CompletedAt.Before(cutoff) {
				toDelete = append(toDelete, append([]byte{}, k...))
			}
			return nil
		})
		for _, k := range toDelete {
			if err := b.Delete(k); err != nil {
				return err
			}
			removed++
		}
		return nil
	})
	return removed, err
}

func (s *AsyncTaskStore) Close() error {
	return s.db.Close()
}

// RunAsync executes a plugin call in a background goroutine and stores the
// result. The context is independent from the HTTP request — the call runs
// to completion even if the client disconnects. [376.B]
func RunAsync(store *AsyncTaskStore, conn *plugin.Connected, toolName string, args map[string]any, taskID string, onDone func(taskID string, task *AsyncTask)) {
	if err := store.setStatus(taskID, AsyncRunning, nil, "", 0); err != nil {
		log.Printf("[PLUGIN-ASYNC] task %s status→running write failed: %v", taskID, err)
	}
	start := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), asyncCallTTL)
	defer cancel()

	raw, err := conn.Client.CallTool(ctx, toolName, args)
	elapsed := time.Since(start)

	if err != nil {
		if storeErr := store.Fail(taskID, err.Error(), elapsed); storeErr != nil {
			log.Printf("[PLUGIN-ASYNC] task %s fail write error: %v", taskID, storeErr)
		}
		log.Printf("[PLUGIN-ASYNC] task %s failed: %v (%dms)", taskID, err, elapsed.Milliseconds())
	} else {
		if storeErr := store.Complete(taskID, raw, elapsed); storeErr != nil {
			log.Printf("[PLUGIN-ASYNC] task %s complete write error: %v", taskID, storeErr)
		}
		log.Printf("[PLUGIN-ASYNC] task %s done (%dms)", taskID, elapsed.Milliseconds())
	}

	if onDone != nil {
		if t, err := store.Get(taskID); err == nil {
			onDone(taskID, t)
		}
	}
}

// SubmitBatch creates N tasks and returns a batch_id + individual task_ids.
// Each task shares the same plugin/action but gets its own files entry from
// batchFiles. Semaphore limits concurrent goroutines to sem. [376.H]
func (s *AsyncTaskStore) SubmitBatch(pluginName, action string, count int) (string, []string, error) {
	batchID := batchIDPrefix + hex.EncodeToString(func() []byte { b := make([]byte, 6); rand.Read(b); return b }())
	taskIDs := make([]string, 0, count)
	for i := range count {
		id, err := s.Submit(pluginName, action)
		if err != nil {
			return batchID, taskIDs, fmt.Errorf("submit %d/%d: %w", i, count, err)
		}
		taskIDs = append(taskIDs, id)
	}
	return batchID, taskIDs, nil
}

// BatchStatus aggregates the state of multiple tasks by their IDs. [376.I]
func (s *AsyncTaskStore) BatchStatus(taskIDs []string) map[string]any {
	total := len(taskIDs)
	done, pending, errored := 0, 0, 0
	var results []map[string]any
	for _, id := range taskIDs {
		task, err := s.Get(id)
		if err != nil {
			errored++
			continue
		}
		switch task.Status {
		case AsyncDone:
			done++
			results = append(results, map[string]any{"task_id": id, "status": "done", "elapsed_ms": task.ElapsedMs})
		case AsyncError:
			errored++
			results = append(results, map[string]any{"task_id": id, "status": "error", "error": task.ErrorMsg})
		default:
			pending++
		}
	}
	return map[string]any{
		"total": total, "done": done, "pending": pending, "errored": errored,
		"all_done": done+errored == total,
		"results":  results,
	}
}

func newAsyncID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return asyncIDPrefix + hex.EncodeToString(b)
}

// StartReaper launches a background goroutine that purges completed/errored
// tasks older than ttl. Runs every ttl/2. Cancel via ctx. [376.J]
func (s *AsyncTaskStore) StartReaper(ctx context.Context, ttl time.Duration) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	interval := ttl / 2
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				removed, err := s.Cleanup(ttl)
				if err != nil {
					log.Printf("[PLUGIN-ASYNC-REAPER] cleanup error: %v", err)
				} else if removed > 0 {
					log.Printf("[PLUGIN-ASYNC-REAPER] purged %d stale tasks (ttl=%s)", removed, ttl)
				}
			}
		}
	}()
}
