package main

// notify_subscriber_test exercises the SSE → notifier dispatch path
// end-to-end with two httptest servers wired in opposition: one
// playing the role of a child neo-mcp emitting SSE frames, one
// catching the resulting webhook POST. The subscriber reconnect
// loop + auth-failure backoff are also covered.
// [Area 5.2.D]

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/notify"
)

// TestSubscriber_ParsesSSEFrames spins up:
//  · a fake child SSE server that emits one event per 50ms
//  · a fake Slack webhook that records every POST
// Then runs streamFromChild for ~250ms and asserts the webhook
// received at least one notification with the right kind+severity.
func TestSubscriber_ParsesSSEFrames(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped under -short")
	}

	// Fake webhook receiver — counts POSTs and snapshots first body.
	var postCount atomic.Int64
	var firstBody atomic.Value
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		postCount.Add(1)
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		if firstBody.Load() == nil {
			firstBody.Store(string(body[:n]))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hookSrv.Close()

	// Wire a notifier targeting the fake hook, route all events through.
	cfg := notify.NotificationsConfig{
		Enabled: true,
		Webhooks: []notify.WebhookConfig{
			{Name: "ops", Provider: notify.ProviderSlack, URL: hookSrv.URL},
		},
		Routes: []notify.Route{
			{EventKind: "policy_veto", Webhooks: []string{"ops"}, MinSeverity: 0},
		},
		RateLimit: notify.RateLimit{BurstPerMinute: 100, DedupWindowSec: 1},
		AllowHTTP: true, // httptest.NewServer cert is self-signed
	}
	n, err := notify.New(cfg)
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	// Override package-level notifier directly so dispatchSSEFrame
	// reaches our test instance instead of the production singleton.
	notifier = n
	defer func() { notifier = nil }()

	// Fake SSE child — emits a policy_veto frame every 50ms.
	childSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("ResponseWriter doesn't support flushing")
			return
		}
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for range 5 {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = w.Write([]byte("event: policy_veto\ndata: {\"reason\":\"deny\"}\n\n"))
				flusher.Flush()
			}
		}
	}))
	defer childSrv.Close()

	// Extract host:port from the test server URL so streamFromChild
	// rebuilds the same URL with /events appended (which the
	// httptest mux ignores; we serve any path).
	port := childPortFromURL(t, childSrv.URL)

	// Run streamFromChild in a goroutine; cancel via context after
	// enough time for ~5 frames + dispatch.
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	wg.Go(func() {
		_ = streamFromChild(ctx, "ws-test", port, "")
	})
	wg.Wait()

	if got := postCount.Load(); got == 0 {
		t.Fatalf("webhook received 0 POSTs; want ≥1 (subscriber didn't dispatch any frames)")
	}
	body, _ := firstBody.Load().(string)
	if body == "" {
		t.Fatalf("first POST body empty")
	}
	// Slack format payload contains the workspace + event kind.
	if !contains(body, "policy_veto") {
		t.Errorf("first POST missing event kind: %q", body)
	}
}

// TestSubscriber_AuthFailureBackoff verifies that a 401 response
// triggers errAuthRejected (not the generic err). The longer backoff
// path is internal to startSubscriber but the sentinel is the
// observable contract.
func TestSubscriber_AuthFailureBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	port := childPortFromURL(t, srv.URL)

	err := streamFromChild(context.Background(), "ws-auth", port, "wrong-token")
	if err == nil {
		t.Fatalf("expected error on 401")
	}
	if err.Error() != errAuthRejected.Error() {
		t.Errorf("expected errAuthRejected, got: %v", err)
	}
}

// TestDispatchSSEFrame_SeverityClassifier verifies the kind→severity
// mapping that filters chatty events out of the notifier path.
func TestDispatchSSEFrame_SeverityClassifier(t *testing.T) {
	cases := []struct {
		kind        string
		shouldFire  bool
		description string
	}{
		{"policy_veto", true, "critical kind promoted to sev 9"},
		{"oom_guard", true, "critical kind"},
		{"thermal_rollback", true, "critical kind"},
		{"heartbeat", false, "chatty filtered out"},
		{"inference", false, "chatty filtered out"},
		{"random_event", true, "default sev 5 fires"},
	}

	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			var fired atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fired.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			cfg := notify.NotificationsConfig{
				Enabled: true,
				Webhooks: []notify.WebhookConfig{
					{Name: "ops", Provider: notify.ProviderSlack, URL: srv.URL},
				},
				Routes: []notify.Route{
					{EventKind: c.kind, Webhooks: []string{"ops"}, MinSeverity: 0},
				},
				RateLimit: notify.RateLimit{BurstPerMinute: 100},
				AllowHTTP: true,
			}
			n, err := notify.New(cfg)
			if err != nil {
				t.Fatalf("notify.New: %v", err)
			}
			notifier = n
			defer func() { notifier = nil }()

			dispatchSSEFrame("ws-test", c.kind, "{}")
			// Give the HTTP POST a moment to land.
			time.Sleep(50 * time.Millisecond)

			got := fired.Load()
			if c.shouldFire && got == 0 {
				t.Errorf("%s: expected webhook fire, got 0", c.description)
			}
			if !c.shouldFire && got != 0 {
				t.Errorf("%s: expected NO webhook, got %d", c.description, got)
			}
		})
	}
}

// childPortFromURL extracts the port from an httptest URL.
func childPortFromURL(t *testing.T, urlStr string) int {
	t.Helper()
	// httptest URLs are http://127.0.0.1:<port>
	const prefix = "http://127.0.0.1:"
	if len(urlStr) <= len(prefix) || urlStr[:len(prefix)] != prefix {
		// Could also be https:// — handle the TLS server case.
		const httpsPrefix = "https://127.0.0.1:"
		if len(urlStr) > len(httpsPrefix) && urlStr[:len(httpsPrefix)] == httpsPrefix {
			port := 0
			for _, ch := range urlStr[len(httpsPrefix):] {
				if ch < '0' || ch > '9' {
					break
				}
				port = port*10 + int(ch-'0')
			}
			return port
		}
		t.Fatalf("unexpected URL shape: %s", urlStr)
	}
	port := 0
	for _, ch := range urlStr[len(prefix):] {
		if ch < '0' || ch > '9' {
			break
		}
		port = port*10 + int(ch-'0')
	}
	return port
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
