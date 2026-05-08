package integrations

import (
	"bufio"
	"strings"
	"sync/atomic"
	"testing"
)

// TestNewSREListener_Initialization verifies zero-value is sane — not
// active until Start is called. [Épica 230.C]
func TestNewSREListener_Initialization(t *testing.T) {
	var called atomic.Int32
	l := NewSREListener("127.0.0.1:0", func(payload string) { called.Add(1) })
	if l == nil {
		t.Fatal("NewSREListener returned nil")
	}
	if l.IsActive() {
		t.Error("listener should not be active before Start")
	}
	if called.Load() != 0 {
		t.Error("callback should not fire on construction")
	}
}

// TestParseAndAlert_TextField covers the "text" JSON path — the most
// common Slack/Discord/generic webhook format. [Épica 230.C]
func TestParseAndAlert_TextField(t *testing.T) {
	var got string
	l := NewSREListener("unused", func(payload string) { got = payload })
	l.parseAndAlert(`{"text": "database timeout spike"}`)
	if got != "database timeout spike" {
		t.Errorf("expected 'database timeout spike', got %q", got)
	}
}

// TestParseAndAlert_TitleFallback verifies the "title" fallback when
// "text" is absent. [Épica 230.C]
func TestParseAndAlert_TitleFallback(t *testing.T) {
	var got string
	l := NewSREListener("unused", func(payload string) { got = payload })
	l.parseAndAlert(`{"title": "[ALERT] Disk 95%"}`)
	if got != "[ALERT] Disk 95%" {
		t.Errorf("expected '[ALERT] Disk 95%%', got %q", got)
	}
}

// TestParseAndAlert_BothEmpty — both fields empty → default message.
// [Épica 230.C]
func TestParseAndAlert_BothEmpty(t *testing.T) {
	var got string
	l := NewSREListener("unused", func(payload string) { got = payload })
	l.parseAndAlert(`{}`)
	if got != "Alerta silenciosa" {
		t.Errorf("expected default 'Alerta silenciosa', got %q", got)
	}
}

// TestParseAndAlert_InvalidJSON — malformed input must not panic and
// must not invoke the callback. [Épica 230.C]
func TestParseAndAlert_InvalidJSON(t *testing.T) {
	invoked := false
	l := NewSREListener("unused", func(payload string) { invoked = true })
	l.parseAndAlert(`not json at all {`)
	if invoked {
		t.Error("callback should NOT fire on invalid JSON")
	}
}

// TestReadBody_HTTPLike ensures the "headers then blank then body"
// scanner logic extracts only the body. [Épica 230.C]
func TestReadBody_HTTPLike(t *testing.T) {
	raw := "POST /alert HTTP/1.1\nHost: example\n\n{\"text\":\"ok\"}"
	l := NewSREListener("unused", func(string) {})
	scanner := bufio.NewScanner(strings.NewReader(raw))
	body := l.readBody(scanner)
	if body != `{"text":"ok"}` {
		t.Errorf("expected body %q, got %q", `{"text":"ok"}`, body)
	}
}
