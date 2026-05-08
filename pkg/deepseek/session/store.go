// Package session manages DeepSeek conversation lifecycle (PILAR XXIV / 131.D).
//
// Two session modes:
//   - Ephemeral: fire-and-forget; no BoltDB state. Used by distill_payload, map_reduce_refactor.
//   - Threaded:  persistent ThreadID (ds_thread_<8-hex>); context window managed.
//     Used by red_team_audit for multi-turn conversations.
//
// ThreadStore wraps BoltDB bucket "deepseek_threads" with CRUD semantics.
// ThreadID format: ds_thread_<16 random hex chars> (8 crypto/rand bytes).
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// SessionMode controls thread lifecycle for a call.
type SessionMode int

const (
	SessionModeEphemeral SessionMode = iota // fire-and-forget; no state persisted
	SessionModeThreaded                     // persistent ThreadID; context window managed
)

// Message is a single turn in a threaded conversation.
type Message struct {
	Role       string    `json:"role"`        // "user" | "assistant"
	Content    string    `json:"content"`
	TokensUsed int       `json:"tokens_used"`
	Timestamp  time.Time `json:"timestamp"`
}

// ThreadStatus tracks lifecycle state.
type ThreadStatus string

const (
	ThreadStatusActive  ThreadStatus = "active"
	ThreadStatusExpired ThreadStatus = "expired"
)

// Thread represents a persistent multi-turn conversation.
type Thread struct {
	ID          string       `json:"id"`
	History     []Message    `json:"history"`
	CreatedAt   time.Time    `json:"created_at"`
	LastActive  time.Time    `json:"last_active"`
	TokenCount  int64        `json:"token_count"`
	FileDeps    []string     `json:"file_deps"`
	FileDepsKey string       `json:"file_deps_key"` // [131.H] SHA-256 snapshot of FileDeps at creation
	Status      ThreadStatus `json:"status"`
}

const bucketThreads = "deepseek_threads"

// ThreadStore provides thread CRUD operations over a BoltDB bucket.
type ThreadStore struct {
	db *bolt.DB
}

// NewThreadStore creates a ThreadStore backed by db.
// The caller is responsible for opening db and closing it.
func NewThreadStore(db *bolt.DB) (*ThreadStore, error) {
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(bucketThreads))
		return err
	}); err != nil {
		return nil, fmt.Errorf("session: init bucket: %w", err)
	}
	return &ThreadStore{db: db}, nil
}

// Create allocates a new Thread with a random ThreadID and persists it.
func (s *ThreadStore) Create(fileDeps []string) (Thread, error) {
	id, err := newThreadID()
	if err != nil {
		return Thread{}, fmt.Errorf("session: generate thread ID: %w", err)
	}
	now := time.Now()
	t := Thread{
		ID:         id,
		History:    []Message{},
		CreatedAt:  now,
		LastActive: now,
		FileDeps:   fileDeps,
		Status:     ThreadStatusActive,
	}
	if err := s.put(t); err != nil {
		return Thread{}, err
	}
	return t, nil
}

// Get retrieves a thread by ID. Returns an error if not found.
func (s *ThreadStore) Get(id string) (Thread, error) {
	var t Thread
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketThreads))
		if b == nil {
			return fmt.Errorf("session: bucket missing")
		}
		v := b.Get([]byte(id))
		if v == nil {
			return fmt.Errorf("session: thread %s not found", id)
		}
		return json.Unmarshal(v, &t)
	})
	return t, err
}

// Append adds a message to the thread and increments TokenCount.
func (s *ThreadStore) Append(id string, msg Message) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketThreads))
		if b == nil {
			return fmt.Errorf("session: bucket missing")
		}
		v := b.Get([]byte(id))
		if v == nil {
			return fmt.Errorf("session: thread %s not found", id)
		}
		var t Thread
		if err := json.Unmarshal(v, &t); err != nil {
			return err
		}
		t.History = append(t.History, msg)
		t.TokenCount += int64(msg.TokensUsed)
		t.LastActive = time.Now()
		data, err := json.Marshal(t)
		if err != nil {
			return err
		}
		return b.Put([]byte(id), data)
	})
}

// Expire marks a thread as expired (soft delete).
func (s *ThreadStore) Expire(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketThreads))
		if b == nil {
			return fmt.Errorf("session: bucket missing")
		}
		v := b.Get([]byte(id))
		if v == nil {
			return nil // already gone
		}
		var t Thread
		if err := json.Unmarshal(v, &t); err != nil {
			return err
		}
		t.Status = ThreadStatusExpired
		data, err := json.Marshal(t)
		if err != nil {
			return err
		}
		return b.Put([]byte(id), data)
	})
}

// ListActive returns all threads with Status == active.
func (s *ThreadStore) ListActive() ([]Thread, error) {
	var out []Thread
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketThreads))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var t Thread
			if err := json.Unmarshal(v, &t); err != nil {
				return nil // skip corrupt entries
			}
			if t.Status == ThreadStatusActive {
				out = append(out, t)
			}
			return nil
		})
	})
	return out, err
}

// FindByPrefix returns the most recently active thread whose FileDeps share
// a directory prefix with the given files. Returns nil if no match. Used for
// thread continuity auto-suggest: reuse an existing thread instead of paying
// cache miss on a fresh one. [375.B]
func (s *ThreadStore) FindByPrefix(files []string, maxAge time.Duration) *Thread {
	if len(files) == 0 {
		return nil
	}
	prefix := filepath.Dir(files[0])
	active, err := s.ListActive()
	if err != nil || len(active) == 0 {
		return nil
	}
	cutoff := time.Now().Add(-maxAge)
	var best *Thread
	for i := range active {
		t := &active[i]
		if t.LastActive.Before(cutoff) {
			continue
		}
		for _, dep := range t.FileDeps {
			if strings.HasPrefix(dep, prefix) {
				if best == nil || t.LastActive.After(best.LastActive) {
					best = t
				}
				break
			}
		}
	}
	return best
}

// SetFileDepsKey stores the initial CacheKey snapshot for file mutation detection. [131.H]
func (s *ThreadStore) SetFileDepsKey(id, key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketThreads))
		if b == nil {
			return fmt.Errorf("session: bucket missing")
		}
		v := b.Get([]byte(id))
		if v == nil {
			return fmt.Errorf("session: thread %s not found", id)
		}
		var t Thread
		if err := json.Unmarshal(v, &t); err != nil {
			return err
		}
		t.FileDepsKey = key
		data, err := json.Marshal(t)
		if err != nil {
			return err
		}
		return b.Put([]byte(id), data)
	})
}

// ExpireByFileDep marks expired all active threads that list path in FileDeps.
// Returns the count of threads expired.
func (s *ThreadStore) ExpireByFileDep(path string) (int, error) {
	count := 0
	return count, s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketThreads))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var t Thread
			if err := json.Unmarshal(v, &t); err != nil {
				return nil
			}
			if t.Status != ThreadStatusActive {
				return nil
			}
			for _, dep := range t.FileDeps {
				if dep == path {
					t.Status = ThreadStatusExpired
					data, err := json.Marshal(t)
					if err != nil {
						return err
					}
					if err := b.Put(k, data); err != nil {
						return err
					}
					count++
					break
				}
			}
			return nil
		})
	})
}

// put serializes and writes a thread. Must be called from writable context.
func (s *ThreadStore) put(t Thread) error {
	data, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("session: marshal thread: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketThreads))
		if b == nil {
			return fmt.Errorf("session: bucket missing")
		}
		return b.Put([]byte(t.ID), data)
	})
}

// newThreadID returns a random "ds_thread_<16hex>" identifier.
func newThreadID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "ds_thread_" + hex.EncodeToString(buf[:]), nil
}
