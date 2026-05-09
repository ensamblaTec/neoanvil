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

func (noopTracer) StartSpan(ctx context.Context, name string) (context.Context, Span) {
	return ctx, noopSpan{}
}
func (noopTracer) Shutdown(ctx context.Context) error { return nil }

func (noopSpan) SetAttribute(key string, value any)         {}
func (noopSpan) SetStatus(code SpanStatus, description string) {}
func (noopSpan) RecordError(err error)                      {}
func (noopSpan) End()                                       {}
func (noopSpan) TraceID() string                            { return "" }

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
