package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNew_RefusesPlaintextHTTPInProd(t *testing.T) {
	cfg := NotificationsConfig{
		Enabled: true,
		Webhooks: []WebhookConfig{
			{Name: "leak", Provider: ProviderSlack, URL: "http://insecure.example.com/hooks/abc"},
		},
	}
	if _, err := New(cfg); err == nil {
		t.Errorf("expected refusal of http:// webhook")
	}
	cfg.AllowHTTP = true
	if _, err := New(cfg); err != nil {
		t.Errorf("with allow_http should pass, got %v", err)
	}
}

func TestNew_ExpandsEnvInURL(t *testing.T) {
	t.Setenv("NEO_TEST_HOOK", "https://hooks.example.com/abc")
	cfg := NotificationsConfig{
		Enabled:  true,
		Webhooks: []WebhookConfig{{Name: "x", Provider: ProviderSlack, URL: "${NEO_TEST_HOOK}"}},
	}
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n.cfg.Webhooks[0].URL != "https://hooks.example.com/abc" {
		t.Errorf("env expansion failed: %q", n.cfg.Webhooks[0].URL)
	}
}

func TestDispatch_RoutesAndDedups(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NotificationsConfig{
		Enabled: true,
		Webhooks: []WebhookConfig{
			{Name: "ops", Provider: ProviderSlack, URL: srv.URL},
		},
		Routes: []Route{
			{EventKind: "workspace_unhealthy", Webhooks: []string{"ops"}, MinSeverity: 0},
		},
		RateLimit: RateLimit{BurstPerMinute: 10, DedupWindowSec: 60},
		AllowHTTP: true, // httptest.NewTLSServer uses self-signed cert; allow http for unit test simplicity
	}
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the safe http.Client with one that trusts httptest's
	// self-signed cert.
	n.client = srv.Client()

	e := Event{Kind: "workspace_unhealthy", Severity: 7, Title: "ws down", Body: "details"}
	if err := n.Dispatch(e); err != nil {
		t.Errorf("dispatch 1: %v", err)
	}
	if err := n.Dispatch(e); err != nil { // dedup'd
		t.Errorf("dispatch 2 (dedup): %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("got %d calls, want 1 (second deduped)", got)
	}

	// Different event kind hits a NEW route lookup → no route → no dispatch.
	other := Event{Kind: "unknown", Severity: 1, Title: "x"}
	if err := n.Dispatch(other); err != nil {
		t.Errorf("dispatch unknown: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("unrouted event hit webhook: %d calls", got)
	}
}

func TestFormatSlack_HasHeaderAndFields(t *testing.T) {
	body, err := formatSlack(Event{
		Kind:     "k",
		Severity: 9,
		Title:    "Plugin zombie",
		Body:     "details",
		Fields:   map[string]any{"workspace_id": "neoanvil-9b272", "errors": 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	blocks, _ := parsed["blocks"].([]any)
	if len(blocks) < 2 {
		t.Errorf("expected at least 2 blocks (header + body/context), got %d\n%s", len(blocks), body)
	}
	if !strings.Contains(string(body), "Plugin zombie") {
		t.Errorf("title missing: %s", body)
	}
}

func TestFormatDiscord_HasEmbedAndColor(t *testing.T) {
	body, err := formatDiscord(Event{
		Kind:     "k",
		Severity: 5,
		Title:    "Title",
		Fields:   map[string]any{"a": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	embeds, _ := parsed["embeds"].([]any)
	if len(embeds) != 1 {
		t.Fatalf("expected 1 embed, got %d", len(embeds))
	}
	first, _ := embeds[0].(map[string]any)
	if first["color"] != float64(0xE67E22) { // orange for sev 5
		t.Errorf("color = %v, want orange (0xE67E22)", first["color"])
	}
}

func TestFormat_RejectsEmptyTitle(t *testing.T) {
	if _, err := formatSlack(Event{Kind: "k"}); err == nil {
		t.Errorf("slack: expected error for empty title")
	}
	if _, err := formatDiscord(Event{Kind: "k"}); err == nil {
		t.Errorf("discord: expected error for empty title")
	}
}

func TestTokenBucket_RefillsAfterMinute(t *testing.T) {
	b := newTokenBucket(2)
	if !b.take() {
		t.Fatal("first take should succeed")
	}
	if !b.take() {
		t.Fatal("second take should succeed")
	}
	if b.take() {
		t.Errorf("3rd take should fail (bucket empty)")
	}
	// Backdate lastFill to force refill.
	b.lastFill = time.Now().Add(-2 * time.Minute)
	if !b.take() {
		t.Errorf("after refill, take should succeed")
	}
}
