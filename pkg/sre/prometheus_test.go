package sre

import (
	"bytes"
	"expvar"
	"strings"
	"testing"
)

func TestPrometheusName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"mes_ingested_total", "neo_mes_ingested_total"},
		{"Goroutines_Active", "neo_goroutines_active"},
		{"weird-chars.@here", "neo_weird_chars__here"},
	}
	for _, c := range cases {
		if got := prometheusName(c.in); got != c.want {
			t.Errorf("prometheusName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestInferKind(t *testing.T) {
	if inferKind("foo_total") != "counter" {
		t.Error("_total suffix should map to counter")
	}
	if inferKind("active_connections") != "gauge" {
		t.Error("non-total should map to gauge")
	}
}

func TestFormatFloat(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{42, "42"},
		{0, "0"},
		{3.14, "3.14"},
		{-1, "-1"},
	}
	for _, c := range cases {
		if got := formatFloat(c.in); got != c.want {
			t.Errorf("formatFloat(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestWritePrometheusMetrics_PicksUpExpvars verifies the endpoint emits
// Prometheus text format for the counters already registered by init(). [359.A]
func TestWritePrometheusMetrics_PicksUpExpvars(t *testing.T) {
	// The init() of diagnostics.go publishes mes_ingested_total + friends.
	// Set one value and verify it appears in the output.
	MetricsMESIngested.Store(123)
	defer MetricsMESIngested.Store(0)

	var buf bytes.Buffer
	WritePrometheusMetrics(&buf)
	out := buf.String()

	// Required elements: HELP, TYPE, and a line with the counter value.
	for _, needle := range []string{
		"# HELP neo_mes_ingested_total",
		"# TYPE neo_mes_ingested_total counter",
		"neo_mes_ingested_total 123",
		"neo_goroutines_active", // gauge from expvar.Func
	} {
		if !strings.Contains(out, needle) {
			t.Errorf("metrics missing %q in output:\n%s", needle, out)
		}
	}
}

// TestCoerceNumeric covers Int, Float, and Func expvar types. [359.A]
func TestCoerceNumeric(t *testing.T) {
	ci := new(expvar.Int)
	ci.Set(99)
	if v, ok := coerceNumeric(ci); !ok || v != 99 {
		t.Errorf("Int → (%v,%v), want (99,true)", v, ok)
	}
	cf := new(expvar.Float)
	cf.Set(3.5)
	if v, ok := coerceNumeric(cf); !ok || v != 3.5 {
		t.Errorf("Float → (%v,%v)", v, ok)
	}
	cfn := expvar.Func(func() any { return int64(7) })
	if v, ok := coerceNumeric(cfn); !ok || v != 7 {
		t.Errorf("Func(int64) → (%v,%v)", v, ok)
	}
}
