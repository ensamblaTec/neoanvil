// cmd/neo-nexus/notify_wire.go — minimal notify wire-up for Nexus.
// [Area 5.2.A + 5.2.C]
//
// Phase 1 deliverable: a package-level Notifier built at boot from
// config and a Dispatch helper that callers (watchdog status changes,
// debt-detected handler, plugin zombie reaper) invoke directly with
// an Event.
//
// The full SSE-subscriber-per-child fan-in (5.2.B) is a follow-up —
// it requires reading each managed workspace's /events endpoint and
// translating SSE frames to Events. The skeleton here lets call sites
// migrate one-by-one without waiting for the streaming infra.

package main

import (
	"log"

	"github.com/ensamblatec/neoanvil/pkg/nexus"
	"github.com/ensamblatec/neoanvil/pkg/notify"
)

// notifyConfigFromNexus extracts a notify.NotificationsConfig from
// the parsed nexus.yaml. The two structs intentionally live in
// different packages (pkg/nexus.NotificationsSection mirrors
// pkg/notify.NotificationsConfig 1:1) to avoid pkg/nexus depending
// on pkg/notify's HTTP/retry stack — see the comment on
// NotificationsSection in pkg/nexus/config.go.
//
// When the pkg/notify schema evolves, update this translator AND
// the mirror struct in pkg/nexus/config.go in the same commit.
func notifyConfigFromNexus(cfg *nexus.NexusConfig) notify.NotificationsConfig {
	if cfg == nil || !cfg.Nexus.Notifications.Enabled {
		return notify.NotificationsConfig{Enabled: false}
	}
	src := cfg.Nexus.Notifications
	hooks := make([]notify.WebhookConfig, len(src.Webhooks))
	for i, h := range src.Webhooks {
		hooks[i] = notify.WebhookConfig{
			Name:     h.Name,
			Provider: notify.ProviderKind(h.Provider),
			URL:      h.URL,
		}
	}
	routes := make([]notify.Route, len(src.Routes))
	for i, r := range src.Routes {
		routes[i] = notify.Route{
			EventKind:   r.EventKind,
			Webhooks:    r.Webhooks,
			MinSeverity: r.MinSeverity,
		}
	}
	return notify.NotificationsConfig{
		Enabled:   true,
		Webhooks:  hooks,
		Routes:    routes,
		RateLimit: notify.RateLimit{
			BurstPerMinute: src.RateLimit.BurstPerMinute,
			DedupWindowSec: src.RateLimit.DedupWindowSec,
		},
		AllowHTTP: src.AllowHTTP,
	}
}

// notifier is the package-level dispatcher initialised by
// initNotifier at Nexus boot. nil-safe (notify.Dispatch tolerates a
// nil receiver — see pkg/notify/notify.go).
var notifier *notify.Notifier

// initNotifier constructs the package-level Notifier from the supplied
// config (typically nexus.cfg.Notifications). Logs and continues on
// validation errors so a misconfigured webhook doesn't block boot.
func initNotifier(cfg notify.NotificationsConfig) {
	if !cfg.Enabled {
		return // notifier stays nil; Dispatch is a no-op
	}
	n, err := notify.New(cfg)
	if err != nil {
		log.Printf("[NEXUS-NOTIFY] init failed: %v (notifications disabled)", err)
		return
	}
	notifier = n
	log.Printf("[NEXUS-NOTIFY] initialized: %d webhook(s), %d route(s), dedup=%ds",
		len(cfg.Webhooks), len(cfg.Routes), cfg.RateLimit.DedupWindowSec)
}

// dispatchNexusEvent is the convenience wrapper call sites use to
// emit a notification without having to know if the notifier is wired.
// Errors are swallowed (logged) — webhook failures should never break
// the dispatcher.
func dispatchNexusEvent(kind string, severity int, title, body string, fields map[string]any) {
	if notifier == nil {
		return
	}
	if err := notifier.Dispatch(notify.Event{
		Kind:     kind,
		Severity: severity,
		Title:    title,
		Body:     body,
		Fields:   fields,
	}); err != nil {
		log.Printf("[NEXUS-NOTIFY] dispatch %q failed: %v", kind, err)
	}
}
