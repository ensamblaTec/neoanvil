// Package storage defines the BrainStore abstraction for snapshot
// persistence. PILAR XXVI / 135.C.1.
//
// BrainStore is intentionally narrow: PUT/GET object bytes, LIST keys
// under a prefix, DELETE, plus distributed Lock/Unlock for concurrent-
// pusher coordination. Higher-level concepts (snapshots, manifests,
// HLCs) live in pkg/brain itself; the store layer treats everything as
// opaque blobs keyed by string.
//
// Two production drivers are planned:
//
//   pkg/brain/storage/local.go   — local filesystem (this file's sibling).
//                                  Useful for offline backups, NAS targets,
//                                  and the tsnet-mounted directory in 137.E.
//
//   pkg/brain/storage/r2.go      — Cloudflare R2 via aws-sdk-go-v2 (S3 API).
//                                  Multipart uploads for >5 MiB; conditional
//                                  PUT for the lease lock semantics in 135.F.
//
// Tests live next to each driver. The interface itself is exercised
// indirectly through the driver suites — there's no benefit to a mock
// store at this layer.

package storage

import (
	"errors"
	"io"
	"time"
)

// ErrLockHeld is returned by BrainStore.Lock when another holder owns the
// named lock and its lease has not expired.
var ErrLockHeld = errors.New("storage: lock held by another node")

// ErrNotFound is returned by Get/Delete when the key does not exist. The
// driver layer normalizes its native NotFound errors into this single
// sentinel so callers can use errors.Is uniformly.
var ErrNotFound = errors.New("storage: object not found")

// ChunkRef describes one object surfaced by BrainStore.List. Size is
// authoritative when known; drivers that can't report it without a
// roundtrip leave it at -1. ETag (or the driver's equivalent) is opaque
// to callers — used only for replay/dedup checks.
type ChunkRef struct {
	Key       string
	Size      int64
	ETag      string
	UpdatedAt time.Time
}

// Lease represents a held lock. The holder MUST call Unlock(lease) when
// done; the driver may also auto-expire after the TTL passed to Lock.
// Holder is the node identifier (typically NodeFingerprint) so an
// operator inspecting the store can see who's pushing.
type Lease struct {
	Name      string
	Holder    string
	ExpiresAt time.Time
	// OpaqueToken is the driver's mechanism for verifying Unlock comes
	// from the original Lock holder (e.g. an S3 object ETag, a UUID, a
	// monotonic version counter).
	OpaqueToken string
}

// BrainStore is the snapshot persistence contract. Implementations MUST
// be safe for concurrent use by goroutines belonging to different
// callers; serialization where needed is the driver's responsibility.
//
// Object bodies are streamed — implementations must not materialize the
// full bytes in RAM unless explicitly bounded by the caller's reader.
type BrainStore interface {
	// Put writes the body of r under key. Overwrites any prior object
	// at that key. Returns the byte count written.
	Put(key string, r io.Reader) (int64, error)

	// Get returns a reader for the object at key. Caller MUST Close it.
	// Returns ErrNotFound when the key does not exist.
	Get(key string) (io.ReadCloser, error)

	// List enumerates objects whose key has the given prefix. Empty
	// prefix lists every object. Order is implementation-defined —
	// callers that need sorting must sort the result.
	List(prefix string) ([]ChunkRef, error)

	// Delete removes the object at key. Idempotent: returns nil when
	// the object did not exist (callers may use ErrNotFound semantics
	// only with Get; Delete is fire-and-forget).
	Delete(key string) error

	// Lock acquires a named distributed lock with the given TTL.
	// Returns ErrLockHeld when another holder has an unexpired lease.
	// Successful Lock returns a Lease that must be passed to Unlock.
	Lock(name, holder string, ttl time.Duration) (Lease, error)

	// Unlock releases the lock identified by lease. A second Unlock
	// with the same lease is a no-op (idempotent).
	Unlock(lease Lease) error

	// Close releases driver-level resources (file handles, HTTP clients,
	// in-memory state). After Close, subsequent operations return
	// errors. Implementations MUST be safe to call Close multiple
	// times.
	Close() error
}
