package otelx

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// TestNoopTracer_DefaultIsZeroAlloc verifies that the default code
// path doesn't surprise hot-path callers. We can't directly assert
// "no allocations" without -benchmem, but we can verify the contract:
// StartSpan returns the input context unchanged and a span whose
// methods are all no-ops.
func TestNoopTracer_DefaultIsZeroAlloc(t *testing.T) {
	ctx := context.Background()
	gotCtx, span := StartSpan(ctx, "x")
	if gotCtx != ctx {
		t.Errorf("noop tracer should return input context unchanged")
	}
	span.SetAttribute("k", "v")
	span.SetStatus(StatusError, "fail")
	span.RecordError(errors.New("x"))
	span.End()
	if span.TraceID() != "" {
		t.Errorf("noop traceID should be empty, got %q", span.TraceID())
	}
}

func TestSetTracer_RoundTrip(t *testing.T) {
	defer SetTracer(nil) // restore noop after test

	called := atomic.Int64{}
	custom := &fakeTracer{
		startFn: func(ctx context.Context, name string) (context.Context, Span) {
			called.Add(1)
			return ctx, &fakeSpan{name: name}
		},
	}
	SetTracer(custom)

	if CurrentTracer() != custom {
		t.Errorf("SetTracer/CurrentTracer round-trip failed")
	}
	_, span := StartSpan(context.Background(), "op")
	if called.Load() != 1 {
		t.Errorf("custom tracer not called; got %d", called.Load())
	}
	if fs, ok := span.(*fakeSpan); !ok || fs.name != "op" {
		t.Errorf("span name plumbing broken: %v", span)
	}

	SetTracer(nil)
	if _, ok := CurrentTracer().(noopTracer); !ok {
		t.Errorf("SetTracer(nil) should reset to noop")
	}
}

func TestW3CTraceParent_EmptyForNoop(t *testing.T) {
	_, span := StartSpan(context.Background(), "op")
	if got := W3CTraceParent(span); got != "" {
		t.Errorf("noop traceparent should be empty, got %q", got)
	}
}

func TestW3CTraceParent_RealTracer(t *testing.T) {
	defer SetTracer(nil)
	SetTracer(&fakeTracer{
		startFn: func(ctx context.Context, name string) (context.Context, Span) {
			return ctx, &fakeSpan{traceID: "0123456789abcdef0123456789abcdef"}
		},
	})
	_, span := StartSpan(context.Background(), "op")
	got := W3CTraceParent(span)
	// Format: `00-<32hex>-<16hex>-01` = 3 + 32 + 1 + 16 + 1 + 2 = 55
	if len(got) != 55 {
		t.Errorf("traceparent length = %d, want 55: %q", len(got), got)
	}
	if got[:36] != "00-0123456789abcdef0123456789abcdef-" {
		t.Errorf("traceparent prefix wrong: %q", got)
	}
	if got[len(got)-3:] != "-01" {
		t.Errorf("traceparent suffix wrong: %q", got)
	}
}

func TestShutdown_NoopReturnsNil(t *testing.T) {
	if err := Shutdown(context.Background()); err != nil {
		t.Errorf("noop Shutdown returned %v, want nil", err)
	}
}

func TestConcurrentSetTracer_NoRace(t *testing.T) {
	defer SetTracer(nil)
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			SetTracer(&fakeTracer{startFn: func(ctx context.Context, name string) (context.Context, Span) {
				return ctx, &fakeSpan{name: name}
			}})
			_, _ = StartSpan(context.Background(), "x")
			_ = i
		}(i)
	}
	wg.Wait()
}

// ── fakes ─────────────────────────────────────────────────────────────

type fakeTracer struct {
	startFn func(ctx context.Context, name string) (context.Context, Span)
}

func (f *fakeTracer) StartSpan(ctx context.Context, name string) (context.Context, Span) {
	return f.startFn(ctx, name)
}
func (f *fakeTracer) Shutdown(ctx context.Context) error { return nil }

type fakeSpan struct {
	name    string
	traceID string
}

func (f *fakeSpan) SetAttribute(key string, value any)         {}
func (f *fakeSpan) SetStatus(code SpanStatus, description string) {}
func (f *fakeSpan) RecordError(err error)                      {}
func (f *fakeSpan) End()                                       {}
func (f *fakeSpan) TraceID() string                            { return f.traceID }
