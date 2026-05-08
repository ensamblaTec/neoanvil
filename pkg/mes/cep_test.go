package mes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewCEPEngine_EmptyMatrix [Épica 231.I]
func TestNewCEPEngine_EmptyMatrix(t *testing.T) {
	e := NewCEPEngine(5 * time.Second)
	if e == nil {
		t.Fatal("NewCEPEngine returned nil")
	}
	// Empty ruleset — any event is a no-op.
	e.Evaluate(context.Background(), TelemetryEvent{MachineID: "ignored"})
	// Nothing to assert; we're validating we don't panic on empty.
}

// TestUpdateRules_HotSwap [Épica 231.I]
func TestUpdateRules_HotSwap(t *testing.T) {
	e := NewCEPEngine(1 * time.Second)
	e.UpdateRules(map[string]CEPRule{
		"M1": {TriggerState: 3, PLCEndpoint: "http://noop", Payload: []byte("{}")},
	})
	rules := *e.rules.Load()
	if len(rules) != 1 {
		t.Errorf("expected 1 rule, got %d", len(rules))
	}
	if _, ok := rules["M1"]; !ok {
		t.Error("M1 rule missing after UpdateRules")
	}

	// Hot-swap with a new ruleset.
	e.UpdateRules(map[string]CEPRule{
		"M2": {TriggerState: 5, PLCEndpoint: "http://noop2", Payload: []byte("{}")},
	})
	rules = *e.rules.Load()
	if _, ok := rules["M1"]; ok {
		t.Error("M1 should have been replaced")
	}
	if _, ok := rules["M2"]; !ok {
		t.Error("M2 missing after hot-swap")
	}
}

// TestEvaluate_NoRuleForMachine [Épica 231.I]
func TestEvaluate_NoRuleForMachine(t *testing.T) {
	e := NewCEPEngine(1 * time.Second)
	e.UpdateRules(map[string]CEPRule{"known": {TriggerState: 3}})
	// "unknown" has no rule — early return.
	e.Evaluate(context.Background(), TelemetryEvent{MachineID: "unknown", State: 3})
}

// TestEvaluate_WrongStateNoFire [Épica 231.I]
func TestEvaluate_WrongStateNoFire(t *testing.T) {
	fired := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fired.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := NewCEPEngine(1 * time.Second)
	e.UpdateRules(map[string]CEPRule{
		"M1": {TriggerState: 3, PLCEndpoint: srv.URL, Payload: []byte("{}")},
	})
	// State=1 does NOT match trigger=3 — no fire.
	e.Evaluate(context.Background(), TelemetryEvent{MachineID: "M1", State: 1})
	time.Sleep(20 * time.Millisecond)
	if fired.Load() != 0 {
		t.Errorf("should NOT fire when state != TriggerState, got %d", fired.Load())
	}
}

// TestEvaluate_TrackingLastAction covers the rule-match + debounce state
// machine without depending on an HTTP round-trip (SafeHTTPClient refuses
// 127.0.0.1 by design). We assert the engine's internal state instead.
// [Épica 231.I]
func TestEvaluate_TrackingLastAction(t *testing.T) {
	e := NewCEPEngine(500 * time.Millisecond)
	e.UpdateRules(map[string]CEPRule{
		"M1": {TriggerState: 3, PLCEndpoint: "http://unreachable.invalid", Payload: []byte("{}")},
	})

	// First event matches — lastAction should be recorded.
	e.Evaluate(context.Background(), TelemetryEvent{MachineID: "M1", State: 3})
	v1, ok := e.lastAction.Load("M1")
	if !ok {
		t.Fatal("lastAction should be populated for M1 after matching event")
	}
	ts1 := v1.(int64)
	if ts1 == 0 {
		t.Error("lastAction timestamp should be non-zero")
	}

	// Immediately trigger again — within cooldown, so lastAction should
	// NOT be updated (stays at ts1).
	e.Evaluate(context.Background(), TelemetryEvent{MachineID: "M1", State: 3})
	v2, _ := e.lastAction.Load("M1")
	if v2.(int64) != ts1 {
		t.Errorf("within cooldown, lastAction should not advance (ts1=%d, ts2=%d)", ts1, v2.(int64))
	}
}

// TestEvaluate_CooldownExpirationAllowsNewFire [Épica 231.I]
func TestEvaluate_CooldownExpirationAllowsNewFire(t *testing.T) {
	e := NewCEPEngine(20 * time.Millisecond)
	e.UpdateRules(map[string]CEPRule{
		"M1": {TriggerState: 3, PLCEndpoint: "http://unreachable.invalid", Payload: []byte("{}")},
	})

	e.Evaluate(context.Background(), TelemetryEvent{MachineID: "M1", State: 3})
	v1, _ := e.lastAction.Load("M1")
	ts1 := v1.(int64)

	time.Sleep(40 * time.Millisecond) // past cooldown

	e.Evaluate(context.Background(), TelemetryEvent{MachineID: "M1", State: 3})
	v2, _ := e.lastAction.Load("M1")
	if v2.(int64) <= ts1 {
		t.Errorf("after cooldown expiry, lastAction should advance (ts1=%d, ts2=%d)", ts1, v2.(int64))
	}
}

// Silence unused imports.
var (
	_ = httptest.NewServer
	_ = http.StatusOK
	_ atomic.Int32
)
