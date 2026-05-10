# Observability Pipeline — neoanvil

Tres librerías nuevas (sesión 2026-05-09) forman una pipeline: **eventos del SSE bus** → **`pkg/notify` dispatcher** → **`pkg/otelx` span/trace** → **expuesto via `pkg/openapi`**. Sin un doc unificado se ven como features sueltas; en realidad son las tres capas de la misma observability story.

```
┌─────────────────────────────────────────────────────────────────┐
│                       neo-mcp child SSE bus                     │
│     (chaos_drill_fail, oracle_alert, policy_veto, ...)          │
└──────────────┬──────────────────────────────────────────────────┘
               │ SSE event (21 EventTypes)
               ▼
┌─────────────────────────────────────────────────────────────────┐
│   cmd/neo-nexus/notify_subscriber.go (per-child goroutine)      │
│       - filtra por allowlist en nexus.yaml::notifications       │
│       - severity mínima por route                               │
└──────────────┬──────────────────────────────────────────────────┘
               │ notify.Event
               ▼
┌─────────────────────────────────────────────────────────────────┐
│   pkg/notify Notifier (Slack + Discord)                         │
│       - rate-limited per route                                  │
│       - retry+backoff exponencial                               │
│       - SSRF guard via sre.SafeHTTPClient                       │
└──────────────┬──────────────────────────────────────────────────┘
               │ each dispatch wrapped in StartSpan()
               ▼
┌─────────────────────────────────────────────────────────────────┐
│   pkg/otelx Tracer interface                                    │
│       - noop default (zero-alloc)                               │
│       - SetTracer hook for prod backend                         │
│       - W3C traceparent propagation Nexus → child               │
└─────────────────────────────────────────────────────────────────┘

                   meanwhile, exposed for inspection:
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│   pkg/openapi → GET /openapi.json (auto-built from registry)    │
│   GET /docs (Swagger UI standalone)                             │
└─────────────────────────────────────────────────────────────────┘
```

---

## `pkg/notify` — Webhook dispatcher

### Schema (en `~/.neo/nexus.yaml`)

```yaml
notifications:
  enabled: true
  webhooks:
    - name: ops-channel
      provider: slack          # slack | discord
      url: https://hooks.slack.com/services/...
      timeout_ms: 5000
      retry_max: 3
  routes:
    - name: critical-events
      webhook: ops-channel
      event_kinds: [chaos_drill_fail, policy_veto, oom_guard, thermal_rollback]
      min_severity: 7          # 1-10 scale
  rate_limit:
    per_route_per_minute: 10   # drop excess instead of buffering
```

### Cuándo emite

| Event kind (allowlist) | Cuándo | Severidad típica |
|---|---|---|
| `chaos_drill_fail` | Status UNSTABLE tras chaos drill | 8 |
| `oracle_alert` | Predicción de fallo de regresión lineal | 7 |
| `policy_veto` | Bouncer rechazó certificación por policy | 9 |
| `thermal_rollback` | RAPL > 60W sostenido | 9 |
| `oom_guard` | Heap > limit absoluto | 10 |
| `cognitive_drift` | MCTS divergencia detectada | 6 |
| `kinetic_anomaly` | IO/RAM patrón fuera de baseline | 6 |
| `gc_pressure` | gcPerFile > 5 sostenido | 5 |

`pkg/pubsub/bus.go` define los 21 EventTypes; el dispatcher solo filtra por la allowlist del operador (no spamea por defecto).

### Test pattern

```go
notifier, _ := notify.New(cfg)
notifier.Dispatch(notify.Event{
    Kind: "chaos_drill_fail", Severity: 8,
    Title: "drill TPS=42 errors=12%", Body: "...",
})
// El dispatcher es síncrono dentro del Dispatch call;
// usa un goroutine interno solo para el retry+backoff.
```

---

## `pkg/otelx` — OpenTelemetry skeleton

### Diseño

- **Default:** `noopTracer` — `StartSpan` retorna struct vacío sin asignar memoria. Importar `pkg/otelx` no añade overhead a producción si no llamas `SetTracer`.
- **Test:** `RecordingTracer` captura todos los spans en memoria + `AttributeRecorder` para asserts en tests:

```go
rt := otelx.NewRecordingTracer(100)
otelx.SetTracer(rt)
defer otelx.SetTracer(nil)

// código bajo test...
notifier.Dispatch(event)

spans := rt.Finished()
if len(spans) != 1 {
    t.Fatalf("expected 1 span, got %d", len(spans))
}
if spans[0].Attributes["notify.kind"] != "chaos_drill_fail" {
    t.Errorf("attribute mismatch")
}
```

- **Prod:** un wrapper que puentea `pkg/otelx::Tracer` a `go.opentelemetry.io/otel`. No incluido en repo (depende del backend del operador: Tempo, Jaeger, Honeycomb, etc.).

### Traceparent propagation

W3C [traceparent](https://www.w3.org/TR/trace-context/) format `00-<32-hex-trace-id>-<16-hex-span-id>-<2-hex-flags>`. Nexus genera el root span al recibir un MCP request, propaga via `X-Neo-Traceparent` header al spawnar el call al child neo-mcp:

```go
// En Nexus dispatcher
ctx, span := otelx.StartSpan(r.Context(), "mcp.dispatch")
defer span.End()
proxyReq.Header.Set("X-Neo-Traceparent", otelx.W3CTraceParent(span))

// En child neo-mcp
traceID := otelx.ParseTraceParent(r.Header.Get("X-Neo-Traceparent"))
// ... usar traceID en todos los spans children
```

Round-trip cubierto por `pkg/otelx/otelx_test.go`.

### Shutdown

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
otelx.Shutdown(ctx)  // flush a backend si SetTracer no es noop
```

---

## `pkg/openapi` — Auto-generated spec

### Lo que genera

- **`GET /openapi.json`**: OpenAPI 3.0.3 con `x-mcp-tools` array (custom extension) listando los 15 tools registrados con sus `inputSchema`.
- **`GET /docs`**: Swagger UI standalone (CDN-served swagger-ui-dist) apuntando a `/openapi.json`.

### Cómo funciona

```
ToolRegistry (cmd/neo-mcp/registry.go)
    │
    │ por cada tool: { name, description, InputSchema() }
    ▼
pkg/openapi.BuildSpec(contracts, tools, opts)
    │
    │ - camina handlers via AST (HandlerScanner)
    │ - extrae response shapes
    │ - merge con InputSchema() del registry
    ▼
*Spec (struct), serializado a JSON
    │
    │ servido por handler.go en GET /openapi.json
    │ Nexus dispatcher proxea via /openapi.json + /docs
    ▼
operador → curl http://127.0.0.1:9000/openapi.json
operador → http://127.0.0.1:9000/docs (browser)
```

### Cuándo regenera

- En **boot del child neo-mcp**, una vez. La spec es immutable durante la vida del proceso.
- Si añades un nuevo tool al registry, `make rebuild-restart` para que aparezca en la spec.

### Verificación rápida

```bash
curl -s http://127.0.0.1:9000/openapi.json | jq '."x-mcp-tools" | length'
# → 14   (counted from live registry — single source of truth)

curl -s http://127.0.0.1:9000/openapi.json | jq '."x-mcp-tools"[].name'
# → "neo_apply_migration", "neo_cache", "neo_chaos_drill", ...
```

---

## Cómo testear los 3 juntos

```go
// Test que valida la pipeline completa con RecordingTracer
func TestObservabilityPipeline(t *testing.T) {
    rt := otelx.NewRecordingTracer(10)
    otelx.SetTracer(rt)
    defer otelx.SetTracer(nil)

    cfg := notify.NotificationsConfig{
        Enabled: true,
        Webhooks: []notify.WebhookConfig{{
            Name: "test", Provider: notify.ProviderSlack,
            URL: testServer.URL,  // httptest
        }},
        Routes: []notify.Route{{
            Name: "all", Webhook: "test",
            EventKinds: []string{"chaos_drill_fail"},
        }},
    }
    n, _ := notify.New(cfg)
    n.Dispatch(notify.Event{Kind: "chaos_drill_fail", Severity: 8, Title: "x"})

    // assert: testServer recibió 1 POST
    // assert: rt.Finished() tiene 1 span con Attributes["notify.kind"]="chaos_drill_fail"
}
```

---

## Threat model

- **`pkg/notify` URLs:** vienen del operador (`nexus.yaml`). No hay user input que llegue directo a `webhook.URL`. Pero `sre.SafeHTTPClient()` se usa de defensa adicional — anti-SSRF para URLs de terceros (Slack/Discord).
- **`pkg/otelx`:** sin secretos en spans (las attributes son tipos primitivos públicos). Al cambiar `SetTracer` a un backend prod, verificar que el wrapper no exporte `traceparent` headers a observability servers de terceros si hay info regulada.
- **`pkg/openapi`:** spec contiene tool schemas, no implementaciones. Sin riesgo de leak. Pero si añades un tool con `description` que incluya secretos accidentales, aparecerán en `/openapi.json` (= cualquier red trusted al loopback).

---

## Referencias

- [`pkg/notify/notify.go`](../../pkg/notify/notify.go) — 710 LOC, dispatcher + retry + rate limit
- [`pkg/otelx/otelx.go`](../../pkg/otelx/otelx.go) — Tracer interface + noop + RecordingTracer
- [`pkg/otelx/otelx_test.go`](../../pkg/otelx/otelx_test.go) — round-trip Nexus → child + RecordingTracer asserts
- [`pkg/openapi/builder.go`](../../pkg/openapi/builder.go) — BuildSpec entry point
- [`cmd/neo-nexus/notify_wire.go`](../../cmd/neo-nexus/notify_wire.go) — Notifier construction at Nexus boot
- [`cmd/neo-nexus/notify_subscriber.go`](../../cmd/neo-nexus/notify_subscriber.go) — per-child SSE subscriber goroutine
