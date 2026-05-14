package deepseek

import (
	"strings"
	"testing"
	"time"
)

// TestHTTPTimeout_BackfillsDefault verifies the zero-value HTTPTimeoutSeconds
// is backfilled to the documented default rather than producing a 0s (= no
// timeout) http.Client.
func TestHTTPTimeout_BackfillsDefault(t *testing.T) {
	c, err := New(Config{APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.HTTPTimeout(), defaultHTTPTimeoutSec*time.Second; got != want {
		t.Errorf("HTTPTimeout() = %v, want %v (default backfill)", got, want)
	}
}

// TestHTTPTimeout_HonorsConfig verifies an explicit HTTPTimeoutSeconds flows
// through to the http.Client — this is the knob that kills the old hardcoded
// 120s and lets operators size the budget for v4-pro+max workloads.
func TestHTTPTimeout_HonorsConfig(t *testing.T) {
	c, err := New(Config{APIKey: "test-key", HTTPTimeoutSeconds: 300})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.HTTPTimeout(); got != 300*time.Second {
		t.Errorf("HTTPTimeout() = %v, want 300s", got)
	}
}

// TestExceedsSyncBudget covers the fail-fast gate: only v4-pro +
// reasoning_effort:max under the minimum budget should trip it.
func TestExceedsSyncBudget(t *testing.T) {
	maxT := &ThinkingConfig{ReasoningEffort: ReasoningEffortMax}
	highT := &ThinkingConfig{ReasoningEffort: ReasoningEffortHigh}
	cases := []struct {
		name     string
		timeout  int
		model    string
		thinking *ThinkingConfig
		want     bool
	}{
		{"v4pro+max under budget", 120, ModelV4Pro, maxT, true},
		{"v4pro+max at min budget", v4ProMaxMinTimeoutSec, ModelV4Pro, maxT, false},
		{"v4pro+max above budget", 600, ModelV4Pro, maxT, false},
		{"v4pro+high never trips", 120, ModelV4Pro, highT, false},
		{"v4flash+max never trips", 120, ModelV4Flash, maxT, false},
		{"v4pro+nil-thinking never trips", 120, ModelV4Pro, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(Config{APIKey: "test-key", HTTPTimeoutSeconds: tc.timeout})
			if err != nil {
				t.Fatal(err)
			}
			if got := c.ExceedsSyncBudget(tc.model, tc.thinking); got != tc.want {
				t.Errorf("ExceedsSyncBudget(%s, %+v) = %v, want %v", tc.model, tc.thinking, got, tc.want)
			}
			hint := c.SyncBudgetExceededHint(tc.model, tc.thinking)
			switch {
			case tc.want && hint == "":
				t.Error("expected a non-empty remediation hint when budget exceeded")
			case tc.want && !strings.Contains(hint, "background:true"):
				t.Errorf("hint should point the caller at background:true, got %q", hint)
			case !tc.want && hint != "":
				t.Errorf("expected empty hint when budget OK, got %q", hint)
			}
		})
	}
}
