package sre

// prometheus.go — Minimal Prometheus exposition-format handler that wraps the
// expvar counters already registered in diagnostics.go (see init()). Adds a
// `/metrics` endpoint to the DiagnosticsServer without pulling a full
// Prometheus client dependency. [PILAR LXVIII / 359.A MVP]
//
// Format emitted:
//
//	# HELP neo_mes_ingested_total Messages ingested by the MES bus
//	# TYPE neo_mes_ingested_total counter
//	neo_mes_ingested_total 42
//
// Every expvar publication becomes a counter/gauge with the `neo_` prefix.
// Untyped values (non-Int, non-Float) are skipped silently. Additional series
// can be exposed by Publish-ing to expvar anywhere in the codebase — the
// handler picks them up automatically on the next scrape.

import (
	"expvar"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// WritePrometheusMetrics emits the Prometheus text-format exposition of every
// expvar publication. Value types handled: Int, Float, Func returning int64 /
// float64 / int / uint64. Untyped/unsupported values are skipped. [359.A]
func WritePrometheusMetrics(w io.Writer) {
	type entry struct {
		name string
		help string
		kind string // counter | gauge
		val  float64
		ok   bool
	}
	var entries []entry

	expvar.Do(func(kv expvar.KeyValue) {
		e := entry{name: prometheusName(kv.Key), kind: inferKind(kv.Key), help: prometheusHelp(kv.Key)}
		e.val, e.ok = coerceNumeric(kv.Value)
		if e.ok {
			entries = append(entries, e)
		}
	})

	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	for _, e := range entries {
		fmt.Fprintf(w, "# HELP %s %s\n", e.name, e.help)
		fmt.Fprintf(w, "# TYPE %s %s\n", e.name, e.kind)
		fmt.Fprintf(w, "%s %s\n", e.name, formatFloat(e.val))
	}
}

// prometheusName prepends "neo_" and sanitizes the expvar key to [a-z0-9_].
func prometheusName(k string) string {
	var b strings.Builder
	b.WriteString("neo_")
	for _, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// inferKind inspects the key for suffixes commonly used for counters vs
// gauges. Anything ending in `_total` is a counter; everything else is a gauge.
func inferKind(key string) string {
	if strings.HasSuffix(key, "_total") {
		return "counter"
	}
	return "gauge"
}

// prometheusHelp returns a generated description when expvar has no built-in
// help text. Kept short — operators care about the metric name, not prose.
func prometheusHelp(k string) string {
	return fmt.Sprintf("auto-generated from expvar %q", k)
}

// coerceNumeric extracts a float64 from an expvar.Var. Returns (0, false)
// when the underlying type isn't a supported numeric.
func coerceNumeric(v expvar.Var) (float64, bool) {
	switch x := v.(type) {
	case *expvar.Int:
		return float64(x.Value()), true
	case *expvar.Float:
		return x.Value(), true
	case expvar.Func:
		return coerceAny(x()), true
	}
	// Some custom types implement Value() via expvar.String via JSON — ignore.
	return 0, false
}

// coerceAny handles the return of expvar.Func(any), which may be int, int64,
// uint64, or float64 at runtime.
func coerceAny(raw any) float64 {
	switch n := raw.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case uint64:
		return float64(n)
	case float64:
		return n
	}
	return 0
}

// formatFloat renders a float without trailing zeros, using integer form when
// the value has no fractional part (Prometheus recommended idiom).
func formatFloat(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
