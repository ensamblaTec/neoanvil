package main

import (
	"testing"
	"time"
)

// ── 5.F Status cache ────────────────────────────────────────────────────────

func TestStatusCache_SetAndGet(t *testing.T) {
	c := newStatusCache(1 * time.Minute)
	c.set("STRATIA-1", "In Progress")
	got, ok := c.get("STRATIA-1")
	if !ok || got != "In Progress" {
		t.Errorf("got %q ok=%v", got, ok)
	}
}

func TestStatusCache_CaseInsensitive(t *testing.T) {
	c := newStatusCache(1 * time.Minute)
	c.set("stratia-1", "Done")
	got, ok := c.get("STRATIA-1")
	if !ok || got != "Done" {
		t.Errorf("got %q ok=%v", got, ok)
	}
}

func TestStatusCache_Expired(t *testing.T) {
	c := newStatusCache(1 * time.Millisecond)
	c.set("STRATIA-1", "Done")
	time.Sleep(5 * time.Millisecond)
	_, ok := c.get("STRATIA-1")
	if ok {
		t.Error("expired entry should miss")
	}
}

func TestStatusCache_Invalidate(t *testing.T) {
	c := newStatusCache(1 * time.Minute)
	c.set("STRATIA-1", "In Progress")
	c.invalidate("STRATIA-1")
	_, ok := c.get("STRATIA-1")
	if ok {
		t.Error("invalidated entry should miss")
	}
}

func TestStatusCache_Miss(t *testing.T) {
	c := newStatusCache(1 * time.Minute)
	_, ok := c.get("NONEXISTENT-1")
	if ok {
		t.Error("nonexistent key should miss")
	}
}

// ── Shutdown drain ──────────────────────────────────────────────────────────

func TestShutdownDrain_AllComplete(t *testing.T) {
	d := newShutdownDrain(1 * time.Second)
	d.track()
	go func() {
		time.Sleep(10 * time.Millisecond)
		d.done()
	}()
	if !d.waitOrTimeout() {
		t.Error("should have drained")
	}
}

func TestShutdownDrain_Timeout(t *testing.T) {
	d := newShutdownDrain(10 * time.Millisecond)
	d.track()
	// Never call done — should timeout.
	if d.waitOrTimeout() {
		t.Error("should have timed out")
	}
	d.done() // cleanup
}

func TestShutdownDrain_Empty(t *testing.T) {
	d := newShutdownDrain(100 * time.Millisecond)
	if !d.waitOrTimeout() {
		t.Error("empty drain should return immediately")
	}
}

// ── 5.G Concurrent pool access (performance proxy) ──────────────────────────

func TestConcurrentResolveCall(t *testing.T) {
	cfg := minimalConfig()
	cfg.WorkspaceMapping["ws-a"] = "test"
	cfg.WorkspaceMapping["ws-b"] = "test"

	st := &state{
		pluginCfg: cfg,
		pool:      newClientPool(),
		ctx:       t.Context(),
	}

	errs := make(chan error, 20)
	for i := range 20 {
		go func(idx int) {
			ws := "ws-a"
			if idx%2 == 0 {
				ws = "ws-b"
			}
			_, _, _, err := st.resolveCall(callCtx{WorkspaceID: ws})
			errs <- err
		}(i)
	}
	for range 20 {
		if err := <-errs; err != nil {
			t.Errorf("resolveCall error: %v", err)
		}
	}
}
