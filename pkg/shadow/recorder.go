// pkg/shadow/recorder.go — Request ring buffer for replay. [SRE-92.C.1]
//
// Records the last N HTTP requests in a thread-safe ring buffer for shadow
// replay testing. Requests are stored as compact RecordedRequest structs.
package shadow

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"time"
)

// RecordedRequest captures the essential fields of an HTTP request for replay.
type RecordedRequest struct {
	Method         string    `json:"method"`
	Path           string    `json:"path"`
	HeadersHash    string    `json:"headers_hash"`
	BodyHash       string    `json:"body_hash"`
	Body           []byte    `json:"-"` // not serialized to JSON by default
	Timestamp      time.Time `json:"timestamp"`
	ResponseStatus int       `json:"response_status"`
}

// Recorder is a thread-safe ring buffer of recent HTTP requests. [SRE-92.C.1]
type Recorder struct {
	mu      sync.RWMutex
	buf     []RecordedRequest
	size    int
	pos     int
	count   int
}

// NewRecorder creates a ring buffer of the given capacity.
func NewRecorder(size int) *Recorder {
	if size <= 0 {
		size = 1000
	}
	return &Recorder{
		buf:  make([]RecordedRequest, size),
		size: size,
	}
}

// Record adds a request to the ring buffer.
func (r *Recorder) Record(method, path string, headers, body []byte, responseStatus int) {
	rec := RecordedRequest{
		Method:         method,
		Path:           path,
		HeadersHash:    fmt.Sprintf("%x", sha256.Sum256(headers)),
		BodyHash:       fmt.Sprintf("%x", sha256.Sum256(body)),
		Body:           copyBytes(body),
		Timestamp:      time.Now(),
		ResponseStatus: responseStatus,
	}

	r.mu.Lock()
	r.buf[r.pos] = rec
	r.pos = (r.pos + 1) % r.size
	if r.count < r.size {
		r.count++
	}
	r.mu.Unlock()
}

// Last returns the last n recorded requests (newest first).
func (r *Recorder) Last(n int) []RecordedRequest {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if n > r.count {
		n = r.count
	}
	result := make([]RecordedRequest, n)
	for i := 0; i < n; i++ {
		idx := (r.pos - 1 - i + r.size) % r.size
		result[i] = r.buf[idx]
	}
	return result
}

// Count returns the number of recorded requests.
func (r *Recorder) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.count
}

// ReplayReport is the aggregated result of replaying recorded requests against a target.
type ReplayReport struct {
	Total             int   `json:"total"`
	Passed            int   `json:"passed"`
	Divergent         int   `json:"divergent"`
	AvgLatencyDeltaMs int64 `json:"avg_latency_delta_ms"`
}

func copyBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
}
