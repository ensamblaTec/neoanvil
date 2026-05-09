// Package notify dispatches Nexus events (workspace failed, plugin
// zombie, debt detected, chaos drill) to Slack/Discord webhooks with
// dedup + per-webhook rate limiting + provider-specific formatting.
//
// Wire format: caller calls Notifier.Dispatch(Event) with kind +
// payload; the notifier picks a route by kind, formats per provider,
// rate-limits, sends. Idempotent dedup window keeps duplicate events
// from spamming the channel during a noisy outage.
//
// Integration: cmd/neo-nexus/notify_subscriber.go (Area 5.2) reads
// the SSE event bus from each managed workspace and calls Dispatch
// for events that match a configured route.
//
// [Area 5.1 — pkg/notify core]

package notify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// ProviderKind enumerates the supported webhook formats.
type ProviderKind string

const (
	ProviderSlack   ProviderKind = "slack"
	ProviderDiscord ProviderKind = "discord"
)

// WebhookConfig is one Slack/Discord webhook destination.
type WebhookConfig struct {
	Name     string       `yaml:"name"`     // operator-friendly label
	Provider ProviderKind `yaml:"provider"` // "slack" | "discord"
	URL      string       `yaml:"url"`      // resolved with os.ExpandEnv
}

// Route binds an event kind (e.g., "workspace_unhealthy", "debt_p0",
// "chaos_drill_done") to a list of webhook names.
type Route struct {
	EventKind string   `yaml:"event_kind"`
	Webhooks  []string `yaml:"webhooks"`
	MinSeverity int    `yaml:"min_severity"` // 0 = all; routes with min_severity:5 fire only on sev>=5
}

// RateLimit caps webhook QPS per destination so a flapping incident
// doesn't burn Slack/Discord rate quota.
type RateLimit struct {
	BurstPerMinute int `yaml:"burst_per_minute"` // default 10
	DedupWindowSec int `yaml:"dedup_window_sec"` // default 60
}

// NotificationsConfig is the top-level config consumed from
// nexus.yaml::notifications. Empty → notifier is a no-op.
type NotificationsConfig struct {
	Enabled    bool            `yaml:"enabled"`
	Webhooks   []WebhookConfig `yaml:"webhooks"`
	Routes     []Route         `yaml:"routes"`
	RateLimit  RateLimit       `yaml:"rate_limit"`
	AllowHTTP  bool            `yaml:"allow_http"` // override the no-HTTPS-in-prod guard
}

// Event is a single notification payload. Caller fills the basics;
// the formatter renders Slack blocks / Discord embeds from this.
type Event struct {
	Kind     string         // e.g., "workspace_unhealthy"
	Severity int            // 0..10; renders to color
	Title    string         // one-line summary
	Body     string         // optional Markdown body
	Fields   map[string]any // key/value details (workspace_id, error count, etc.)
}

// Notifier is the entrypoint. Construct with New(cfg). Calls to
// Dispatch are safe across goroutines.
type Notifier struct {
	cfg    NotificationsConfig
	client *http.Client

	mu      sync.Mutex
	dedup   map[string]time.Time
	buckets map[string]*tokenBucket // per-webhook QPS

	// hooks for tests — allows swapping the HTTP path without faking
	// a full http.Client.
	postFn func(ctx context.Context, url string, body []byte) error
}

// New returns a notifier honouring the supplied config. nil cfg or
// cfg.Enabled=false produces a Notifier whose Dispatch is a no-op.
// Webhook URLs go through os.ExpandEnv so operators can use
// `${SLACK_HOOK_URL}` rather than committing tokens to YAML.
func New(cfg NotificationsConfig) (*Notifier, error) {
	for i := range cfg.Webhooks {
		cfg.Webhooks[i].URL = os.ExpandEnv(cfg.Webhooks[i].URL)
	}
	if cfg.RateLimit.BurstPerMinute <= 0 {
		cfg.RateLimit.BurstPerMinute = 10
	}
	if cfg.RateLimit.DedupWindowSec <= 0 {
		cfg.RateLimit.DedupWindowSec = 60
	}
	if !cfg.AllowHTTP && cfg.Enabled {
		// In prod we refuse plaintext webhooks because they'd leak
		// the auth token (token is part of the URL for both providers).
		// AllowHTTP=true is opt-in for local testing.
		for _, w := range cfg.Webhooks {
			if !strings.HasPrefix(w.URL, "https://") && w.URL != "" {
				return nil, fmt.Errorf("webhook %q URL must be HTTPS (set notifications.allow_http=true to override)", w.Name)
			}
		}
	}
	n := &Notifier{
		cfg:     cfg,
		client:  sre.SafeHTTPClient(),
		dedup:   make(map[string]time.Time, 16),
		buckets: make(map[string]*tokenBucket, len(cfg.Webhooks)),
	}
	for _, w := range cfg.Webhooks {
		n.buckets[w.Name] = newTokenBucket(cfg.RateLimit.BurstPerMinute)
	}
	return n, nil
}

// Dispatch sends e to every webhook matching its routes. Returns nil
// when notifier is disabled, no route matches, or all destinations
// were dedup'd. A route with zero webhooks is a documented no-op.
//
// Errors are aggregated per-destination — a single bad webhook
// doesn't abort the others.
func (n *Notifier) Dispatch(e Event) error {
	if n == nil || !n.cfg.Enabled {
		return nil
	}
	matched := n.lookupRoute(e)
	if len(matched) == 0 {
		return nil
	}
	if !n.shouldDispatch(e) {
		return nil // dedup window
	}
	var firstErr error
	for _, wname := range matched {
		w := n.findWebhook(wname)
		if w == nil {
			continue
		}
		if !n.takeToken(wname) {
			continue // rate-limited
		}
		body, err := n.format(*w, e)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := n.post(w.URL, body); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("dispatch %q: %w", w.Name, err)
			}
		}
	}
	return firstErr
}

func (n *Notifier) lookupRoute(e Event) []string {
	for _, r := range n.cfg.Routes {
		if r.EventKind == e.Kind && e.Severity >= r.MinSeverity {
			return r.Webhooks
		}
	}
	return nil
}

func (n *Notifier) findWebhook(name string) *WebhookConfig {
	for i := range n.cfg.Webhooks {
		if n.cfg.Webhooks[i].Name == name {
			return &n.cfg.Webhooks[i]
		}
	}
	return nil
}

// shouldDispatch returns true when the event hasn't been seen within
// dedupWindowSec. Dedup key = sha256(kind|title) so a flapping
// incident with identical wording doesn't spam.
func (n *Notifier) shouldDispatch(e Event) bool {
	key := dedupKey(e)
	now := time.Now()
	window := time.Duration(n.cfg.RateLimit.DedupWindowSec) * time.Second
	n.mu.Lock()
	defer n.mu.Unlock()
	if last, ok := n.dedup[key]; ok && now.Sub(last) < window {
		return false
	}
	n.dedup[key] = now
	// Sweep stale entries to bound map growth.
	for k, t := range n.dedup {
		if now.Sub(t) > window*2 {
			delete(n.dedup, k)
		}
	}
	return true
}

func dedupKey(e Event) string {
	sum := sha256.Sum256([]byte(e.Kind + "|" + e.Title))
	return hex.EncodeToString(sum[:8])
}

// takeToken returns true iff the per-webhook bucket has capacity.
// Drops the event silently otherwise — caller can log if it cares.
func (n *Notifier) takeToken(name string) bool {
	n.mu.Lock()
	b := n.buckets[name]
	n.mu.Unlock()
	if b == nil {
		return true
	}
	return b.take()
}

func (n *Notifier) format(w WebhookConfig, e Event) ([]byte, error) {
	switch w.Provider {
	case ProviderSlack:
		return formatSlack(e)
	case ProviderDiscord:
		return formatDiscord(e)
	default:
		return nil, fmt.Errorf("unsupported provider %q", w.Provider)
	}
}

// post sends body to url via the safe HTTP client. URL is verified
// (https + parseable) before send so a misconfigured webhook fails
// fast rather than silently 4xx-ing.
func (n *Notifier) post(rawURL string, body []byte) error {
	if n.postFn != nil {
		return n.postFn(nil, rawURL, body) //nolint:contextcheck // test hook
	}
	if _, err := url.ParseRequestURI(rawURL); err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("webhook returned %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

// errInvalidEvent is returned by formatters when a required field
// (Kind, Title) is missing.
var errInvalidEvent = errors.New("event missing Kind or Title")

// MarshalJSON exposes the notifier config in a form usable by tests
// and operator tooling without leaking the dedup/buckets state.
func (n *Notifier) MarshalJSON() ([]byte, error) {
	return json.Marshal(n.cfg)
}
