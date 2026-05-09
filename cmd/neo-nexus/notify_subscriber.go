// cmd/neo-nexus/notify_subscriber.go — SSE event reader per managed
// child workspace. Translates each frame to a notify.Event and
// dispatches via the package-level notifier built in notify_wire.go.
//
// Wire format: each child neo-mcp exposes GET /events as an SSE
// stream. We connect once per child, classify event types, map
// severities, and dispatch. Reconnect with backoff if the child dies
// or restarts.
//
// [Area 5.2.B]
//
// Disabled when notifier is nil (default — config knob lands later).
// Spawned by main.go after pool.StartAll() so children are bound
// before we try to connect.

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/notify"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// subscriberManager tracks one active subscriber per workspace ID.
// Reconnect / shutdown coordinated via context cancel.
type subscriberManager struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

var subMgr = &subscriberManager{
	cancels: map[string]context.CancelFunc{},
}

// startNotifySubscribers connects to each managed workspace's /events
// endpoint and dispatches incoming frames as notify Events. Idempotent:
// re-call after pool reconcile to pick up new workspaces.
func startNotifySubscribers(ctx context.Context, workspaces []workspaceTuple, internalToken string) {
	if notifier == nil {
		return // notifier not wired (config disabled) — nothing to do
	}
	for _, ws := range workspaces {
		startSubscriber(ctx, ws.ID, ws.Port, internalToken)
	}
}

// stopNotifySubscriber cancels an active subscriber. Called when a
// workspace is removed from the managed set.
func stopNotifySubscriber(wsID string) {
	subMgr.mu.Lock()
	defer subMgr.mu.Unlock()
	if cancel, ok := subMgr.cancels[wsID]; ok {
		cancel()
		delete(subMgr.cancels, wsID)
	}
}

// stopAllNotifySubscribers cancels every active stream. Called on
// Nexus shutdown.
func stopAllNotifySubscribers() {
	subMgr.mu.Lock()
	defer subMgr.mu.Unlock()
	for id, cancel := range subMgr.cancels {
		cancel()
		delete(subMgr.cancels, id)
	}
}

// workspaceTuple is the minimum surface we need from the registry +
// pool to subscribe.
type workspaceTuple struct {
	ID   string
	Port int
}

// startSubscriber spawns the read loop for one workspace. The loop
// reconnects with exponential backoff (1s → 30s cap) on read failure
// or child restart. Child unreachable (port not listening) is the
// most common failure mode — we keep trying until the operator
// stops the workspace.
func startSubscriber(parent context.Context, wsID string, port int, internalToken string) {
	subMgr.mu.Lock()
	if _, exists := subMgr.cancels[wsID]; exists {
		subMgr.mu.Unlock()
		return // already running
	}
	ctx, cancel := context.WithCancel(parent)
	subMgr.cancels[wsID] = cancel
	subMgr.mu.Unlock()

	go func() {
		defer func() {
			subMgr.mu.Lock()
			delete(subMgr.cancels, wsID)
			subMgr.mu.Unlock()
		}()
		backoff := time.Second
		for {
			if ctx.Err() != nil {
				return
			}
			err := streamFromChild(ctx, wsID, port, internalToken)
			if err != nil && ctx.Err() == nil {
				log.Printf("[NEXUS-NOTIFY] subscriber %s disconnected: %v (reconnect in %s)", wsID, err, backoff)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}()
}

// streamFromChild opens the SSE stream and parses frames. Returns
// nil on clean EOF (connection closed) or err on read failure. The
// outer loop decides whether to reconnect.
func streamFromChild(ctx context.Context, wsID string, port int, internalToken string) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/events", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if internalToken != "" {
		req.Header.Set("X-Neo-Internal-Token", internalToken)
	}
	// Long-lived stream — use the safe internal client with a generous
	// per-call timeout so we don't time out a healthy idle connection.
	client := sre.SafeInternalHTTPClient(3600)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return parseSSEStream(ctx, wsID, resp.Body)
}

// parseSSEStream is a minimal SSE parser. We only care about the
// `event:` and `data:` fields (no id retry); the spec permits
// multi-line data joined by newlines but neo-mcp's writer emits
// single-line JSON so we collapse that case. Unknown frame types
// are logged + skipped.
func parseSSEStream(ctx context.Context, wsID string, body io.Reader) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var eventType, data string
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			// Blank line = frame boundary; dispatch what we have.
			if eventType != "" {
				dispatchSSEFrame(wsID, eventType, data)
			}
			eventType, data = "", ""
		}
	}
	return scanner.Err()
}

// dispatchSSEFrame maps an SSE event type to a notify Event and
// fires it via the package-level notifier. Severity defaults to 5;
// known critical types (oom_guard, thermal_rollback, policy_veto)
// promote to 9.
func dispatchSSEFrame(wsID, eventType, data string) {
	severity := 5
	switch eventType {
	case "oom_guard", "thermal_rollback", "policy_veto":
		severity = 9
	case "heartbeat", "inference":
		severity = 1 // chatty, low signal
	}
	if severity < 5 {
		return // skip chatty events from notify dispatch
	}
	dispatchNexusEvent(eventType, severity,
		fmt.Sprintf("[%s] %s", wsID, eventType),
		truncatePayload(data, 800),
		map[string]any{
			"workspace_id": wsID,
			"event_type":   eventType,
		},
	)
}

func truncatePayload(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
}

// Anchor the subscriber API surface to package-level vars so the
// linter doesn't drop the helpers as "unused" — they're the public
// integration points main.go will call once notifications.enabled
// lands in nexus.yaml. Without anchors, U1000 trips audit-ci even
// though every helper has a clear future caller.
var (
	_ = (*notify.Notifier)(nil)
	_ = startNotifySubscribers
	_ = ensureNotifySubscribers
)

// ensureNotifySubscribers reconciles the active subscriber set with
// the running pool. Called on a polling cadence (every 30s) so new
// workspaces are picked up without a Nexus restart and stopped
// workspaces shed their subscribers automatically. [Area 5.2.B/C]
//
// nil notifier is documented no-op — initNotifier installs the
// notifier only when config enables it.
func ensureNotifySubscribers(ctx context.Context, pool subscriberPool) {
	const reconcileInterval = 30 * time.Second
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	defer stopAllNotifySubscribers()

	reconcile := func() {
		if notifier == nil {
			return
		}
		desired := pool.SnapshotForSubscribe()
		// Start any missing.
		for _, ws := range desired {
			startSubscriber(ctx, ws.ID, ws.Port, "")
		}
		// Stop any active that aren't in the desired set.
		desiredSet := make(map[string]bool, len(desired))
		for _, ws := range desired {
			desiredSet[ws.ID] = true
		}
		subMgr.mu.Lock()
		toStop := make([]string, 0)
		for id := range subMgr.cancels {
			if !desiredSet[id] {
				toStop = append(toStop, id)
			}
		}
		subMgr.mu.Unlock()
		for _, id := range toStop {
			stopNotifySubscriber(id)
		}
	}
	reconcile()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}

// subscriberPool is the minimum surface ensureNotifySubscribers
// needs from the production pool. Defined as an interface so we
// can mock it in tests + decouple from pkg/nexus's full ProcessPool
// shape. ProcessPool already has the public method we need below;
// when wired, an adapter satisfies this interface.
type subscriberPool interface {
	// SnapshotForSubscribe returns currently-running workspaces with
	// their child ports. Implementations should return a fresh slice
	// per call (no internal locking concerns for the caller).
	SnapshotForSubscribe() []workspaceTuple
}
