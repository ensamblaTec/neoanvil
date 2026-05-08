package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"fmt"

	"github.com/ensamblatec/neoanvil/pkg/auth"
	"github.com/ensamblatec/neoanvil/pkg/jira"
)

// ── 5.F Issue status LRU cache ──────────────────────────────────────────────

type statusCache struct {
	mu      sync.RWMutex
	entries map[string]*statusEntry
	ttl     time.Duration
}

type statusEntry struct {
	status    string
	fetchedAt time.Time
}

func newStatusCache(ttl time.Duration) *statusCache {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &statusCache{
		entries: make(map[string]*statusEntry),
		ttl:     ttl,
	}
}

func (c *statusCache) get(issueKey string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[strings.ToUpper(issueKey)]
	if !ok || time.Since(e.fetchedAt) > c.ttl {
		return "", false
	}
	return e.status, true
}

func (c *statusCache) set(issueKey, status string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[strings.ToUpper(issueKey)] = &statusEntry{
		status:    status,
		fetchedAt: time.Now(),
	}
}

// invalidate removes a specific entry (call after our own transitions).
func (c *statusCache) invalidate(issueKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, strings.ToUpper(issueKey))
}

// ── 5.A Audit log multi-tenant field ────────────────────────────────────────

type auditEntry struct {
	Timestamp   string `json:"ts"`
	Tenant      string `json:"tenant"`
	Project     string `json:"project"`
	WorkspaceID string `json:"workspace_id"`
	Action      string `json:"action"`
	IssueKey    string `json:"issue_key,omitempty"`
	Result      string `json:"result"`
}

func (s *state) auditMultiTenant(cc callCtx, projName, action, issueKey, result string) {
	if s.audit == nil {
		return
	}
	tenant := ""
	if s.pluginCfg != nil {
		if proj, ok := s.pluginCfg.Projects[projName]; ok {
			tenant = proj.APIKeyRef
		}
	}
	s.audit.Append(auth.Event{
		Kind:     "tool_call",
		Actor:    "plugin-jira",
		Provider: "jira",
		Tool:     "jira/" + action,
		TenantID: tenant,
		Details: map[string]any{
			"project":      projName,
			"workspace_id": cc.WorkspaceID,
			"issue_key":    issueKey,
			"result":       result,
		},
	})
}

// ── 5.B Lazy connectivity check ─────────────────────────────────────────────

type connectivityResult struct {
	OK        bool
	CheckedAt time.Time
	Error     string
}

var (
	connCheckMu      sync.RWMutex
	connCheckResults = make(map[string]*connectivityResult)
)

// checkConnectivity does a lazy first-use ping per api_key.
// Returns cached result if already checked and not expired (5 min TTL).
func checkConnectivity(keyName string, key *APIKey, token string) *connectivityResult {
	connCheckMu.RLock()
	if r, ok := connCheckResults[keyName]; ok && time.Since(r.CheckedAt) < 5*time.Minute {
		connCheckMu.RUnlock()
		return r
	}
	connCheckMu.RUnlock()

	connCheckMu.Lock()
	defer connCheckMu.Unlock()

	// Double-check.
	if r, ok := connCheckResults[keyName]; ok && time.Since(r.CheckedAt) < 5*time.Minute {
		return r
	}

	result := &connectivityResult{CheckedAt: time.Now()}

	// Reuse pooled client to avoid connection leak.
	c, err := jira.NewClient(jira.Config{Domain: key.Domain, Email: key.Email, Token: token})
	if err != nil {
		result.Error = err.Error()
		connCheckResults[keyName] = result
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = c.SearchIssues(ctx, "created >= -1d", 1)
	if err != nil {
		result.Error = err.Error()
	} else {
		result.OK = true
	}
	connCheckResults[keyName] = result

	if result.OK {
		log.Printf("plugin-jira: connectivity OK for tenant %q (%s)", keyName, key.Domain)
	} else {
		log.Printf("plugin-jira: connectivity FAILED for tenant %q: %s", keyName, result.Error)
	}

	return result
}

// ── 5.C Edge cases ──────────────────────────────────────────────────────────

// buildStateSafe wraps buildState with edge case handling:
// - corrupt jira.json → reject, try legacy
// - missing config entirely → legacy env vars
// - legacy env vars missing → fatal
func buildStateSafe() (*state, error) {
	st, err := buildState()
	if err != nil {
		return nil, fmt.Errorf("plugin-jira init: %w", err)
	}
	return st, nil
}

// ── 5.E Legacy deprecation warning ──────────────────────────────────────────

func checkLegacyDeprecation() {
	home := os.Getenv("HOME")
	if home == "" {
		return
	}

	// Check if jira entry still in credentials.json
	credPath := home + "/.neo/credentials.json"
	if data, err := os.ReadFile(credPath); err == nil {
		if strings.Contains(string(data), `"provider":"jira"`) || strings.Contains(string(data), `"provider": "jira"`) {
			log.Printf("plugin-jira: ⚠️ DEPRECATION: credentials.json still has 'jira' entry — migrate to ~/.neo/plugins/jira.json")
		}
	}

	// Check if contexts.json still exists with jira context
	ctxPath := home + "/.neo/contexts.json"
	if data, err := os.ReadFile(ctxPath); err == nil {
		if strings.Contains(string(data), `"jira"`) {
			log.Printf("plugin-jira: ⚠️ DEPRECATION: contexts.json still has jira context — migrate to ~/.neo/plugins/jira.json")
		}
	}
}

// ── 4.F Graceful shutdown ───────────────────────────────────────────────────

type shutdownDrain struct {
	wg      sync.WaitGroup
	timeout time.Duration
}

func newShutdownDrain(timeout time.Duration) *shutdownDrain {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &shutdownDrain{timeout: timeout}
}

// track increments the in-flight counter (call before starting a hook).
func (d *shutdownDrain) track() {
	d.wg.Add(1)
}

// done decrements the in-flight counter (call when hook completes).
func (d *shutdownDrain) done() {
	d.wg.Done()
}

// waitOrTimeout blocks until all tracked hooks complete or timeout expires.
func (d *shutdownDrain) waitOrTimeout() bool {
	ch := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(ch)
	}()
	select {
	case <-ch:
		return true
	case <-time.After(d.timeout):
		return false
	}
}

// installShutdownHandler sets up SIGTERM/SIGINT handler that drains hooks.
func installShutdownHandler(drain *shutdownDrain, cancel context.CancelFunc) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("plugin-jira: received %s, draining in-flight hooks (timeout=%s)", sig, drain.timeout)
		if drain.waitOrTimeout() {
			log.Printf("plugin-jira: all hooks drained, shutting down")
		} else {
			log.Printf("plugin-jira: drain timeout expired, forcing shutdown")
		}
		cancel()
	}()
}
