// pkg/shadow/mirror.go — Shadow Traffic Mirroring middleware. [SRE-92.A.1]
//
// Duplicates incoming HTTP requests to a shadow target for comparison testing.
// The original request is never blocked — the shadow copy runs in a background
// goroutine with a bounded timeout. Only idempotent methods are mirrored by
// default; set UnsafeMethods=true to include POST/PUT/DELETE.
package shadow

import (
	"bytes"
	"io"
	"log"
	"math/rand"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// MirrorConfig configures the shadow traffic interceptor.
// All fields come from neo.yaml shadow section (Zero-Hardcoding). [SRE-92.A.3]
type MirrorConfig struct {
	Enabled          bool    `yaml:"enabled"`
	TargetURL        string  `yaml:"target_url"`
	SampleRate       float64 `yaml:"sample_rate"`        // 0.0–1.0, fraction of requests to mirror
	TimeoutMs        int     `yaml:"timeout_ms"`          // per-shadow-request timeout
	UnsafeMethods    bool    `yaml:"unsafe_methods"`      // mirror POST/PUT/DELETE
	DiffThresholdMs  int     `yaml:"diff_threshold_ms"`   // latency delta to flag as divergent
	BufferSize       int     `yaml:"buffer_size"`         // replay buffer ring size
	MaxInflight      int     `yaml:"max_inflight"`        // max concurrent shadow requests (default 100)
}

// DefaultMirrorConfig returns safe defaults for shadow traffic.
func DefaultMirrorConfig() MirrorConfig {
	return MirrorConfig{
		Enabled:         false,
		TargetURL:       "",
		SampleRate:      0.1,
		TimeoutMs:       5000,
		UnsafeMethods:   false,
		DiffThresholdMs: 500,
		BufferSize:      1000,
		MaxInflight:     100,
	}
}

// Stats tracks shadow mirror counters atomically.
type Stats struct {
	Mirrored  atomic.Int64
	Errors    atomic.Int64
	Divergent atomic.Int64
	Dropped   atomic.Int64 // requests dropped due to semaphore full
}

// Mirror is the shadow traffic middleware. [SRE-92.A.1]
type Mirror struct {
	cfg       MirrorConfig
	client    *http.Client
	Stats     Stats
	onVerdict func(report DiffReport) // set via NewMirror, immutable after construction
	sem       chan struct{}            // concurrency limiter [SRE-96.A.1]
}

// NewMirror creates a shadow traffic mirror with the given config.
// onVerdict is called (from a goroutine) with each shadow comparison result; may be nil.
func NewMirror(cfg MirrorConfig, onVerdict func(DiffReport)) *Mirror {
	timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	maxInflight := cfg.MaxInflight
	if maxInflight <= 0 {
		maxInflight = 100
	}
	// [SRE-110.D] Shadow target is operator-configured (target_url in
	// neo.yaml) — could be remote. SafeHTTPClient applies the standard
	// SSRF guard.
	httpClient := sre.SafeHTTPClient()
	httpClient.Timeout = timeout
	return &Mirror{
		cfg:       cfg,
		client:    httpClient,
		onVerdict: onVerdict,
		sem:       make(chan struct{}, maxInflight),
	}
}

// maxBodyCapture limits how much of the response body we capture for comparison.
const maxBodyCapture = 1 * 1024 * 1024 // 1 MB

// Middleware returns an http.Handler that wraps the given handler with shadow
// traffic mirroring. The original request is served normally; a copy is sent
// to the shadow target in a bounded goroutine. [SRE-92.A.1]
//
// NOTE: Do not wrap streaming handlers (SSE, WebSocket) with this middleware —
// the responseRecorder buffers the entire response.
func (m *Mirror) Middleware(next http.Handler) http.Handler {
	if !m.cfg.Enabled || m.cfg.TargetURL == "" {
		return next // no-op when disabled
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sample rate check — skip early if not selected.
		if m.cfg.SampleRate < 1.0 && rand.Float64() > m.cfg.SampleRate {
			next.ServeHTTP(w, r)
			return
		}
		// Only mirror idempotent methods unless unsafe is enabled.
		if !m.cfg.UnsafeMethods && !isIdempotent(r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		// Capture the request body for the shadow copy.
		var bodyBytes []byte
		if r.Body != nil {
			bodyBytes, _ = io.ReadAll(io.LimitReader(r.Body, maxBodyCapture))
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		// Snapshot values BEFORE serving — r.Header may be reused after ServeHTTP returns.
		method := r.Method
		path := r.URL.Path
		contentType := r.Header.Get("Content-Type") // [SRE-96.A.1] clone header value, not map

		// Serve the original request with a response recorder.
		rec := &responseRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		realLatency := time.Since(start)
		realBody := rec.capturedBody() // safe snapshot

		// Bounded fire-and-forget shadow request. [SRE-96.A.1]
		select {
		case m.sem <- struct{}{}:
			go func() {
				defer func() { <-m.sem }()
				m.sendShadow(method, path, contentType, bodyBytes, rec.statusCode, realBody, realLatency)
			}()
		default:
			m.Stats.Dropped.Add(1) // semaphore full, drop this shadow request
		}
	})
}

// sendShadow dispatches a copy of the request to the shadow target and compares
// the response. Runs in a bounded goroutine — never blocks the original request.
func (m *Mirror) sendShadow(method, path, contentType string, body []byte, realStatus int, realBody []byte, realLatency time.Duration) {
	m.Stats.Mirrored.Add(1)

	url := m.cfg.TargetURL + path
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		m.Stats.Errors.Add(1)
		return
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Shadow-Mirror", "true")

	start := time.Now()
	resp, err := m.client.Do(req)
	shadowLatency := time.Since(start)
	if err != nil {
		m.Stats.Errors.Add(1)
		log.Printf("[SHADOW] error mirroring %s %s: %v", method, path, err)
		return
	}
	defer resp.Body.Close()
	shadowBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyCapture))

	// Compare responses.
	report := CompareResponses(
		Response{Status: realStatus, Body: realBody, Latency: realLatency},
		Response{Status: resp.StatusCode, Body: shadowBody, Latency: shadowLatency},
		m.cfg.DiffThresholdMs,
	)

	if report.Divergent {
		m.Stats.Divergent.Add(1)
		log.Printf("[SHADOW] DIVERGENT %s %s: %s", method, path, report.Reason)
	}

	if m.onVerdict != nil {
		m.onVerdict(report)
	}
}

// isIdempotent returns true for HTTP methods safe to duplicate.
func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// responseRecorder captures the status code and body of a response.
// Body capture is bounded to maxBodyCapture bytes.
type responseRecorder struct {
	http.ResponseWriter
	statusCode  int
	body        bytes.Buffer
	bodyLimited bool // true once maxBodyCapture is reached
}

func (r *responseRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.bodyLimited {
		remaining := maxBodyCapture - r.body.Len()
		if remaining > 0 {
			if len(b) <= remaining {
				r.body.Write(b)
			} else {
				r.body.Write(b[:remaining])
				r.bodyLimited = true
			}
		}
	}
	return r.ResponseWriter.Write(b)
}

// Flush implements http.Flusher for streaming handlers. [SRE-96.B.1]
func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap implements the Go 1.20+ ResponseWriter unwrapping interface.
func (r *responseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// capturedBody returns a copy of the captured body bytes (safe for goroutine use).
func (r *responseRecorder) capturedBody() []byte {
	cp := make([]byte, r.body.Len())
	copy(cp, r.body.Bytes())
	return cp
}
