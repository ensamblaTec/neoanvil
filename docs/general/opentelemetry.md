# NeoAnvil — OpenTelemetry integration

[Area 6 — `pkg/otelx`]

NeoAnvil ships a deliberately-thin OTel wrapper. The default code path
is **zero-allocation noop**: every `StartSpan` returns the input
context unchanged and a span whose methods are inlined no-ops. No
tracer backend means no overhead.

Operators who want tracing install a real implementation via
`otelx.SetTracer(...)` once at boot. Two adapters ship in-tree; a
third (full OTLP via go.opentelemetry.io/otel) is the canonical
production option but requires the operator to add the dep.

---

## Wire diagram

```
┌────────┐   /mcp/sse   ┌─────────────┐  /mcp/message  ┌──────────┐
│ Client │ ───────────► │  Nexus      │ ─────────────► │  neo-mcp │
│ (Claude│              │ (dispatcher)│   Traceparent  │ (worker) │
│  Code) │              │             │   header       │          │
└────────┘              └──────┬──────┘                └─────┬────┘
                               │                             │
                       handleSSEMessage span     mcp.message span
                       (otelx root)              (otelx child, links via traceparent)
```

Every `/mcp/message` POST from Nexus to a child carries a W3C
`Traceparent` header derived from the active span. The child extracts
the trace ID via `otelx.ParseTraceParent` and stamps it as
`upstream.trace_id` on its own span — a real SDK adapter then links
the spans into a single trace.

---

## Adapters

### 1. Default (noop) — built-in

Zero deps, zero overhead. Active by default. Nothing to configure.

```go
// In application code:
ctx, span := otelx.StartSpan(ctx, "my.operation")
defer span.End()
span.SetAttribute("workspace_id", wsID)
// ... work ...
```

### 2. RecordingTracer — built-in (testing)

In-memory bounded-ring tracer. Useful in tests and dev to assert
spans were emitted with the right attributes.

```go
tracer := otelx.NewRecordingTracer(256) // ring cap
otelx.SetTracer(tracer)
defer otelx.SetTracer(nil) // restore noop

// ... run code that emits spans ...

for _, span := range tracer.FinishedSpans() {
    fmt.Printf("%s [%dµs] traceID=%s attrs=%v\n",
        span.Name, span.Duration.Microseconds(), span.TraceID, span.Attrs)
}
```

### 3. OTLP — operator-supplied

The production path. NeoAnvil doesn't ship the dep so you control
your supply chain. Sketch:

```go
// in your operator-managed init code, behind a build tag if you like:
import (
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/exporters/otlp/otlptrace"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
    "github.com/ensamblatec/neoanvil/pkg/otelx"
)

func setupRealTracer(endpoint string) error {
    exporter, err := otlptrace.New(ctx, otlptracegrpc.NewClient(
        otlptracegrpc.WithEndpoint(endpoint),
    ))
    if err != nil {
        return err
    }
    tp := sdktrace.NewTracerProvider(
        sdktrace.WithBatcher(exporter),
        sdktrace.WithResource(resource.NewSchemaless(
            attribute.String("service.name", "neoanvil"),
        )),
    )
    otel.SetTracerProvider(tp)
    otelx.SetTracer(&otelAdapter{tp: tp.Tracer("neoanvil")})
    return nil
}

// otelAdapter implements otelx.Tracer by delegating to the real SDK.
type otelAdapter struct {
    tp trace.Tracer
}

func (a *otelAdapter) StartSpan(ctx context.Context, name string) (context.Context, otelx.Span) {
    ctx, sp := a.tp.Start(ctx, name)
    return ctx, &otelSpanAdapter{sp: sp}
}

func (a *otelAdapter) Shutdown(ctx context.Context) error {
    return a.tp.(*sdktrace.TracerProvider).Shutdown(ctx)
}

type otelSpanAdapter struct{ sp trace.Span }

func (s *otelSpanAdapter) SetAttribute(key string, value any) {
    s.sp.SetAttributes(attribute.Any(key, value))
}
func (s *otelSpanAdapter) SetStatus(code otelx.SpanStatus, description string) {
    switch code {
    case otelx.StatusOK:
        s.sp.SetStatus(codes.Ok, description)
    case otelx.StatusError:
        s.sp.SetStatus(codes.Error, description)
    }
}
func (s *otelSpanAdapter) RecordError(err error)   { s.sp.RecordError(err) }
func (s *otelSpanAdapter) End()                    { s.sp.End() }
func (s *otelSpanAdapter) TraceID() string         { return s.sp.SpanContext().TraceID().String() }
```

Common backends:

| Backend  | Endpoint                                | Protocol  |
|----------|------------------------------------------|-----------|
| Jaeger   | `localhost:4318` (OTLP HTTP)             | http      |
| Tempo    | `tempo:4317` (gRPC)                      | grpc      |
| Honeycomb | `api.honeycomb.io:443` + auth header    | grpc      |
| Datadog  | `localhost:4318` (Datadog agent OTLP)    | http      |

---

## Span naming convention

Stable + searchable across versions:

```
<package>.<operation>
```

Examples in NeoAnvil:

| Span name              | Where                              |
|------------------------|-------------------------------------|
| `nexus.handleSSEMessage` | cmd/neo-nexus/sse.go              |
| `mcp.message`            | cmd/neo-mcp/main.go (handler wrap) |
| `radar.<intent>`         | (planned — radar_handlers.go)     |
| `cache.<action>`         | (planned — neo_cache dispatcher)  |
| `plugin.<name>.<action>` | (planned — Nexus plugin bridge)   |

---

## Standard attributes

| Key                | Type    | Set by                            |
|--------------------|---------|-----------------------------------|
| `workspace_id`     | string  | callers in workspace-scoped flows |
| `session_id`       | string  | `nexus.handleSSEMessage`          |
| `upstream.trace_id`| string  | `mcp.message` (from Traceparent)  |
| `tool.name`        | string  | tool dispatcher (planned)         |
| `tool.latency_ms`  | int     | tool dispatcher (planned)         |
| `error.message`    | string  | RecordError                       |

---

## Sampling

`pkg/otelx/Config.SampleRate` is the operator-facing knob (default
1.0 = sample all). Real SDK adapters honour it via the parent-based
sampler. The noop tracer ignores it (nothing to sample).

For high-volume production: drop to `0.05` (5%) and rely on
`upstream.trace_id` propagation to reconstruct sampled paths in your
backend.

---

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| No spans in backend | noop tracer is active | call `otelx.SetTracer(realAdapter)` at boot |
| Spans appear but unlinked | Traceparent header stripped by intermediary | verify `Traceparent` is in `req.Header` at the receiver |
| TraceID empty in logs | logging happens before adapter is installed | move `SetTracer` earlier in boot |
| OOM with high traffic | `BatchSpanProcessor` queue unbounded | set `WithBatchTimeout` + `WithMaxExportBatchSize` |
| Permission denied on /openapi.json | unrelated — see docs/onboarding/docker.md | this is OpenAPI, not OTel |
