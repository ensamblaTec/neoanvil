// Package swarm — file locking helpers for multi-agent CRDT coordination. [SRE-25.2.1]
package swarm

import (
	"encoding/binary"
	"hash/fnv"
	"time"
)

// HashFile returns a deterministic uint64 key for a file path.
func HashFile(path string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(path))
	return h.Sum64()
}

// LockFile marks a file hash as locked in the LWWSet (isAdded=true).
func (s *LWWSet) LockFile(path string) {
	s.Add(HashFile(path), time.Now().UnixNano(), true)
}

// UnlockFile removes the lock for a file hash (isAdded=false, later timestamp).
func (s *LWWSet) UnlockFile(path string) {
	s.Add(HashFile(path), time.Now().UnixNano(), false)
}

// IsFileLocked checks if a file path is currently locked by another agent.
func (s *LWWSet) IsFileLocked(path string) bool {
	return s.Contains(HashFile(path))
}

// NowNano returns time.Now().UnixNano() as a little-endian byte slice — zero alloc helper.
func NowNano() []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(time.Now().UnixNano()))
	return b
}
