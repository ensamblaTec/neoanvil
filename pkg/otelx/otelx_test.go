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

// TestRecordingTracer_CapturesAttributes exercises the in-memory
// SDK adapter. End() flushes to FinishedSpans; Attributes() returns
// a snapshot for tests.
func TestRecordingTracer_CapturesAttributes(t *testing.T) {
	tracer := NewRecordingTracer(0) // default cap
	defer SetTracer(nil)
	SetTracer(tracer)

	_, span := StartSpan(context.Background(), "test.op")
	span.SetAttribute("workspace_id", "ws-1")
	span.SetAttribute("status", 200)
	span.End()

	finished := tracer.FinishedSpans()
	if len(finished) != 1 {
		t.Fatalf("expected 1 finished span, got %d", len(finished))
	}
	got := finished[0]
	if got.Name != "test.op" {
		t.Errorf("name = %q, want test.op", got.Name)
	}
	if got.Attrs["workspace_id"] != "ws-1" {
		t.Errorf("workspace_id attr missing or wrong: %v", got.Attrs)
	}
	if got.TraceID == "" || len(got.TraceID) != 32 {
		t.Errorf("traceID = %q, want 32-hex", got.TraceID)
	}
}

// TestRecordingTracer_ErrorStatus verifies RecordError + SetStatus
// both surface to FinishedSpan.Status.
func TestRecordingTracer_ErrorStatus(t *testing.T) {
	tracer := NewRecordingTracer(0)
	defer SetTracer(nil)
	SetTracer(tracer)

	_, span := StartSpan(context.Background(), "failing.op")
	span.RecordError(errors.New("boom"))
	span.End()

	got := tracer.FinishedSpans()[0]
	if got.Status != StatusError {
		t.Errorf("status = %d, want StatusError(%d)", got.Status, StatusError)
	}
	if got.Error != "boom" {
		t.Errorf("error msg = %q, want boom", got.Error)
	}
}

// TestRecordingTracer_RingCap verifies the bounded ring evicts
// oldest spans past the cap.
func TestRecordingTracer_RingCap(t *testing.T) {
	tracer := NewRecordingTracer(3)
	defer SetTracer(nil)
	SetTracer(tracer)

	for i := range 5 {
		_, span := StartSpan(context.Background(), "op")
		span.SetAttribute("i", i)
		span.End()
	}

	finished := tracer.FinishedSpans()
	if len(finished) != 3 {
		t.Fatalf("ring cap not enforced: %d spans, want 3", len(finished))
	}
	// Oldest (i=2) should be at index 0; newest (i=4) at index 2.
	if finished[0].Attrs["i"] != 2 {
		t.Errorf("oldest expected i=2, got %v", finished[0].Attrs["i"])
	}
	if finished[2].Attrs["i"] != 4 {
		t.Errorf("newest expected i=4, got %v", finished[2].Attrs["i"])
	}
}

type fakeSpan struct {
	name    string
	traceID string
}

func (f *fakeSpan) SetAttribute(key string, value any)         {}
func (f *fakeSpan) SetStatus(code SpanStatus, description string) {}
func (f *fakeSpan) RecordError(err error)                      {}
func (f *fakeSpan) End()                                       {}
func (f *fakeSpan) TraceID() string                            { return f.traceID }
