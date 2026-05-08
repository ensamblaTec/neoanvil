package auth

// audit.go — Append-only hash-chained audit log for credential and tool
// usage. PILAR XXIII / Épica 124.6.
//
// Each entry is one JSON line containing:
//   - monotonic Seq
//   - RFC3339-nano timestamp
//   - the event payload (kind, actor, provider, tool, tenant, details)
//   - PrevHash (the previous entry's Hash, or "GENESIS" for seq=1)
//   - Hash = sha256(canonical JSON of the entry with Hash field cleared)
//
// Tampering with any entry invalidates that entry's Hash and breaks the
// chain at the next entry's PrevHash. Verify() re-reads the file and
// recomputes the chain end-to-end.
//
// This file IS the source of truth for credential lifecycle. It is
// intentionally NOT BoltDB — JSONL on disk survives BoltDB corruption,
// is greppable, and is trivially exportable. Cost: O(n) verify, ~150 ns
// hash per entry. For typical usage (<10k events/year) this is fine.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// genesisPrevHash is the PrevHash of the first entry in any chain.
// Sentinel rather than empty string so a corrupt prev_hash="" anywhere
// in the file is detected as a chain break (not mistaken for genesis).
const genesisPrevHash = "GENESIS"

// Event is the input to AuditLog.Append. Seq/TS/PrevHash/Hash are
// computed internally; the caller supplies the body.
type Event struct {
	Kind     string         // e.g. "credential_use", "tool_call", "credential_rotated"
	Actor    string         // e.g. "neo-mcp", "plugin-jira", "neo login CLI"
	Provider string         // optional: credential provider this event relates to
	Tool     string         // optional: tool name (e.g. "jira/transition")
	TenantID string         // optional: tenant scope
	Details  map[string]any // optional: arbitrary structured details
}

// AuditEntry is the persisted record — one line of JSON per entry.
type AuditEntry struct {
	Seq      uint64         `json:"seq"`
	TS       string         `json:"ts"`
	Actor    string         `json:"actor,omitempty"`
	Kind     string         `json:"kind"`
	Provider string         `json:"provider,omitempty"`
	Tool     string         `json:"tool,omitempty"`
	TenantID string         `json:"tenant_id,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
	PrevHash string         `json:"prev_hash"`
	Hash     string         `json:"hash"`
}

// AuditLog is an append-only hash-chained event log persisted to disk.
// Safe for concurrent Append from multiple goroutines (mutex-serialized).
// Each Append fsyncs — durability over throughput.
type AuditLog struct {
	mu       sync.Mutex
	path     string
	f        *os.File
	lastHash string
	lastSeq  uint64
}

// OpenAuditLog opens (or creates) the log at path. If the file exists,
// the chain head (lastSeq, lastHash) is loaded from the final line so
// subsequent Appends extend the chain correctly.
//
// Open does NOT fully verify the chain — that's Verify(). A corrupt
// final line is treated as parse error to avoid extending a broken log.
func OpenAuditLog(path string) (*AuditLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir audit log dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600) //nolint:gosec // G304-CLI-CONSENT: operator-managed audit log path.
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	a := &AuditLog{path: path, f: f, lastHash: genesisPrevHash}
	if err := a.loadHead(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("load head: %w", err)
	}
	return a, nil
}

// loadHead scans existing lines and sets lastHash + lastSeq from the final
// entry. Does NOT verify the full chain.
func (a *AuditLog) loadHead() error {
	if _, err := a.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	sc := bufio.NewScanner(a.f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("parse entry near seq=%d: %w", a.lastSeq, err)
		}
		a.lastSeq = e.Seq
		a.lastHash = e.Hash
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if _, err := a.f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	return nil
}

// Append writes a new entry to the log. Seq/TS/PrevHash/Hash are computed.
// Each call fsyncs the file before returning.
func (a *AuditLog) Append(ev Event) (*AuditEntry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return nil, errors.New("audit log closed")
	}

	entry := &AuditEntry{
		Seq:      a.lastSeq + 1,
		TS:       time.Now().UTC().Format(time.RFC3339Nano),
		Actor:    ev.Actor,
		Kind:     ev.Kind,
		Provider: ev.Provider,
		Tool:     ev.Tool,
		TenantID: ev.TenantID,
		Details:  ev.Details,
		PrevHash: a.lastHash,
	}
	entry.Hash = computeAuditHash(entry)

	raw, err := json.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf("marshal entry: %w", err)
	}
	raw = append(raw, '\n')
	if _, err := a.f.Write(raw); err != nil {
		return nil, fmt.Errorf("write entry: %w", err)
	}
	if err := a.f.Sync(); err != nil {
		return nil, fmt.Errorf("fsync: %w", err)
	}
	a.lastSeq = entry.Seq
	a.lastHash = entry.Hash
	return entry, nil
}

// Verify re-reads the log file and recomputes the hash chain end-to-end.
// Returns nil for a clean chain. On mismatch, returns an error naming the
// seq number where the chain breaks (parse error / sequence gap /
// prev_hash mismatch / hash tamper).
func (a *AuditLog) Verify() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	f, err := os.Open(a.path) //nolint:gosec // G304-CLI-CONSENT: same path opened in OpenAuditLog.
	if err != nil {
		return fmt.Errorf("open for verify: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	expectedSeq := uint64(1)
	expectedPrev := genesisPrevHash

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("parse seq=%d: %w", expectedSeq, err)
		}
		if e.Seq != expectedSeq {
			return fmt.Errorf("sequence gap: expected %d, got %d", expectedSeq, e.Seq)
		}
		if e.PrevHash != expectedPrev {
			return fmt.Errorf("prev_hash mismatch at seq=%d: expected %s, got %s", e.Seq, expectedPrev, e.PrevHash)
		}
		if recomputed := computeAuditHash(&e); recomputed != e.Hash {
			return fmt.Errorf("hash tampered at seq=%d", e.Seq)
		}
		expectedSeq++
		expectedPrev = e.Hash
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	return nil
}

// Close releases the file handle. Subsequent Appends fail.
func (a *AuditLog) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return nil
	}
	err := a.f.Close()
	a.f = nil
	return err
}

// computeAuditHash returns the SHA-256 hex of the entry's canonical JSON
// with Hash cleared. Stable across runs because Go's json.Marshal orders
// struct fields by declaration order and map keys alphabetically.
func computeAuditHash(e *AuditEntry) string {
	clone := *e
	clone.Hash = ""
	raw, _ := json.Marshal(clone)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
