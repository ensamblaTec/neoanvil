// Package otelx is a deliberately-thin OpenTelemetry wrapper.
//
// Why thin: full OTel SDK pulls ~30 transitive deps (proto, grpc,
// otlp exporter, …). Most NeoAnvil deployments never wire a tracing
// backend, so the default code path must be ZERO-alloc and zero-dep.
// Operators who DO want tracing pull the real SDK behind their own
// build tag and call SetTracer(realTracer) at boot.
//
// Design:
//   · Package-level NoopTracer is the default. StartSpan returns the
//     unchanged context + a noop span — no allocation, no map, no
//     lock contention.
//   · A real implementation (operator-supplied or future SDK
//     integration) implements the Tracer interface and is installed
//     via SetTracer.
//   · Span has only the operations we currently need: SetAttribute,
//     SetStatus, RecordError, End. Adding more is straightforward
//     when the production tracer requires them.
//
// [Area 6.1.A]

package otelx

import (
	"context"
	"maps"
	"sync"
	"sync/atomic"
	"time"
)

// SpanStatus mirrors the OTel canonical status codes without forcing
// a dep on the SDK. Real implementations map to ot.Code.
type SpanStatus int

const (
	StatusUnset SpanStatus = iota
	StatusOK
	StatusError
)

// Tracer creates spans tied to operation names. Implementations are
// concurrency-safe.
type Tracer interface {
	StartSpan(ctx context.Context, name string) (context.Context, Span)
	Shutdown(ctx context.Context) error
}

// Span is the per-operation handle. End MUST be called (typically
// `defer span.End()` after StartSpan).
type Span interface {
	SetAttribute(key string, value any)
	SetStatus(code SpanStatus, description string)
	RecordError(err error)
	End()
	// TraceID returns a stable identifier for this span's trace.
	// Noop tracer returns "" — operators who care wire the real tracer.
	TraceID() string
}

// noopTracer is the default. All operations are O(1), allocation-free.
type noopTracer struct{}

// noopSpan implements Span as no-ops.
type noopSpan struct{}

// AttributeRecorder is implemented by tracers that can be queried
// for span attributes after End — primarily for tests + the
// neo_tool_stats integration which surfaces last-N trace IDs per
// tool. The noop tracer doesn't implement it; SDK adapters do.
// [Area 6.2.B]
type AttributeRecorder interface {
	LastAttributes(spanID string) map[string]any
}

// RecordingSpan is the shape adapters expose for recording-only
// spans (no exporter). Useful in tests where we want to assert
// attributes were set without spinning up an OTLP collector.
//
// Adapters can implement this on top of their Span type so test
// helpers reach the underlying state.
// [Area 6.2.C]
type RecordingSpan interface {
	Span
	Attributes() map[string]any
}

func (noopTracer) StartSpan(ctx context.Context, name string) (context.Context, Span) {
	return ctx, noopSpan{}
}
func (noopTracer) Shutdown(ctx context.Context) error { return nil }

func (noopSpan) SetAttribute(key string, value any)         {}
func (noopSpan) SetStatus(code SpanStatus, description string) {}
func (noopSpan) RecordError(err error)                      {}
func (noopSpan) End()                                       {}
func (noopSpan) TraceID() string                            { return "" }

// ─── Recording adapter ───────────────────────────────────────────────
//
// RecordingTracer is a fully in-memory adapter — no exporter, no
// network. Useful in tests, for development sanity checks, and as
// a reference implementation for operators writing their own SDK
// adapter (e.g., one that wraps go.opentelemetry.io/otel).
//
// Storage is bounded: the most-recent N spans are kept (default 256
// per process) so a long-running test doesn't accumulate forever.
// [Area 6.2.B + 6.2.C]

const recordingDefaultCap = 256

// RecordingTracer keeps a ring of finished spans + their attributes.
// Each StartSpan returns a fresh recordingSpan; End appends to the
// ring (oldest evicted at cap).
type RecordingTracer struct {
	mu     sync.Mutex
	cap    int
	spans  []*recordingSpan // ring buffer of finished spans
	traceCounter uint64       // monotonic so traceIDs stay unique within one process
}

// NewRecordingTracer constructs a tracer that records up to cap
// finished spans in memory. Pass 0 for the default cap (256).
func NewRecordingTracer(cap int) *RecordingTracer {
	if cap <= 0 {
		cap = recordingDefaultCap
	}
	return &RecordingTracer{cap: cap}
}

// StartSpan returns a recordingSpan with a fresh trace ID. Real OTel
// SDKs would derive the parent span ID from the context here; we
// don't have a parent-handle yet (operator-supplied adapter does).
func (t *RecordingTracer) StartSpan(ctx context.Context, name string) (context.Context, Span) {
	t.mu.Lock()
	t.traceCounter++
	tc := t.traceCounter
	t.mu.Unlock()
	tid := traceIDFromCounter(tc)
	s := &recordingSpan{
		owner:     t,
		name:      name,
		traceID:   tid,
		startedAt: time.Now(),
		attrs:     map[string]any{},
	}
	return ctx, s
}

func (t *RecordingTracer) Shutdown(ctx context.Context) error { return nil }

// FinishedSpans returns a copy of the recorded ring, oldest first.
// Useful in tests.
func (t *RecordingTracer) FinishedSpans() []FinishedSpan {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]FinishedSpan, len(t.spans))
	for i, s := range t.spans {
		out[i] = FinishedSpan{
			Name:     s.name,
			TraceID:  s.traceID,
			Status:   s.status,
			Attrs:    cloneMap(s.attrs),
			Duration: s.endedAt.Sub(s.startedAt),
			Error:    s.errMsg,
		}
	}
	return out
}

// FinishedSpan is the read-only snapshot of a recorded span.
type FinishedSpan struct {
	Name     string
	TraceID  string
	Status   SpanStatus
	Attrs    map[string]any
	Duration time.Duration
	Error    string
}

// recordingSpan is the live span; appended to owner.spans on End.
type recordingSpan struct {
	owner     *RecordingTracer
	name      string
	traceID   string
	startedAt time.Time
	endedAt   time.Time
	status    SpanStatus
	errMsg    string
	mu        sync.Mutex
	attrs     map[string]any
}

func (s *recordingSpan) SetAttribute(key string, value any) {
	s.mu.Lock()
	s.attrs[key] = value
	s.mu.Unlock()
}

func (s *recordingSpan) SetStatus(code SpanStatus, description string) {
	s.mu.Lock()
	s.status = code
	if description != "" {
		s.errMsg = description
	}
	s.mu.Unlock()
}

func (s *recordingSpan) RecordError(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.status = StatusError
	s.errMsg = err.Error()
	s.mu.Unlock()
}

func (s *recordingSpan) End() {
	s.mu.Lock()
	s.endedAt = time.Now()
	s.mu.Unlock()
	s.owner.mu.Lock()
	s.owner.spans = append(s.owner.spans, s)
	if len(s.owner.spans) > s.owner.cap {
		s.owner.spans = s.owner.spans[len(s.owner.spans)-s.owner.cap:]
	}
	s.owner.mu.Unlock()
}

func (s *recordingSpan) TraceID() string { return s.traceID }

// Attributes returns a snapshot of the span's recorded attributes.
// [Area 6.2.B — span attributes bridge for neo_tool_stats]
func (s *recordingSpan) Attributes() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneMap(s.attrs)
}

// traceIDFromCounter renders a 32-hex trace ID from an atomic
// monotonic counter. Deterministic per process — good enough for
// tests + correlation. Real SDK adapters use crypto/rand.
func traceIDFromCounter(c uint64) string {
	const hex = "0123456789abcdef"
	var buf [32]byte
	for i := range buf {
		buf[i] = '0'
	}
	for i := 31; i >= 0 && c > 0; i-- {
		buf[i] = hex[c&0x0f]
		c >>= 4
	}
	return string(buf[:])
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	maps.Copy(out, m)
	return out
}

// tracerHolder wraps the Tracer interface in a concrete type so
// atomic.Value sees a consistent dynamic type across stores. Without
// this, storing noopTracer{} and a *realTracer alternately would
// panic ("store of inconsistently typed value into Value").
type tracerHolder struct{ t Tracer }

// activeTracer is the package-level tracer. Replaced by SetTracer.
// atomic.Value stores tracerHolder; reads are lock-free + concurrent.
var activeTracer atomic.Value

func init() {
	activeTracer.Store(tracerHolder{t: noopTracer{}})
}

// SetTracer swaps the active tracer. Pass nil to revert to the noop
// (useful for tests + clean shutdown). Safe to call from any goroutine.
func SetTracer(t Tracer) {
	if t == nil {
		t = noopTracer{}
	}
	activeTracer.Store(tracerHolder{t: t})
}

// CurrentTracer returns whatever SetTracer most recently installed.
// Used by callers that prefer to manage their own span lifecycles.
func CurrentTracer() Tracer {
	return activeTracer.Load().(tracerHolder).t
}

// StartSpan is the canonical entry point. Sugar over CurrentTracer
// so callers don't have to think about the global.
//
// Usage:
//
//	ctx, span := otelx.StartSpan(ctx, "nexus.handleSSEMessage")
//	defer span.End()
//	span.SetAttribute("workspace_id", wsID)
func StartSpan(ctx context.Context, name string) (context.Context, Span) {
	return CurrentTracer().StartSpan(ctx, name)
}

// Shutdown drains any buffered spans. Always safe to call (noop is
// a no-op). Pass a context with a deadline so a hung exporter doesn't
// block forever.
func Shutdown(ctx context.Context) error {
	return CurrentTracer().Shutdown(ctx)
}

// ParseTraceParent extracts the trace ID from a W3C traceparent
// header value. Returns "" when the header is empty or malformed.
// Used by neo-mcp to start a child span linked to the Nexus root.
//
// Format spec: `<version 2hex>-<traceID 32hex>-<spanID 16hex>-<flags 2hex>`
func ParseTraceParent(header string) (traceID string) {
	if len(header) < 55 {
		return ""
	}
	if header[2] != '-' || header[35] != '-' {
		return ""
	}
	tid := header[3:35]
	// Quick hex sanity check — no allocation, no regex.
	for i := range 32 {
		c := tid[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return ""
		}
	}
	return tid
}

// TraceParentHeader is the canonical name of the W3C Trace Context
// header. Use this constant to keep request injection + extraction
// in sync across packages.
const TraceParentHeader = "Traceparent"

// W3CTraceParent renders a span's TraceID as a W3C-compliant
// `traceparent` header value. Returns "" if TraceID is empty (noop
// tracer). Callers inject this into outbound HTTP requests so
// downstream services join the same trace.
//
// Format: `00-<32hex traceID>-<16hex spanID>-01` (sampled flag set).
// We don't have a real spanID in the noop, so the last segment is
// derived from the current nanosecond — good enough for downstream
// correlation when the real tracer is later installed.
func W3CTraceParent(s Span) string {
	tid := s.TraceID()
	if tid == "" {
		return ""
	}
	// 16-char hex span ID derived from the wall clock; replaced by
	// the real tracer's SpanID() if/when implemented.
	now := time.Now().UnixNano()
	const hex = "0123456789abcdef"
	var sid [16]byte
	for i := range sid {
		sid[i] = hex[now&0x0f]
		now >>= 4
	}
	return "00-" + tid + "-" + string(sid[:]) + "-01"
}
