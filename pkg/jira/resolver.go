package jira

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// MasterPlanCache is an optional in-memory or persisted cache for resolver
// lookups. Implementations should be safe for concurrent use. Pass nil to
// resolve without caching.
//
// The KnowledgeStore namespace `jira_resolver` is the canonical persistent
// backing — wire it in the caller; this package stays decoupled from the
// store interface to keep tests fast.
type MasterPlanCache interface {
	Get(epicID string) (ticketID string, ok bool)
	Put(epicID, ticketID string)
}

// ErrAmbiguous is returned when SearchIssues yields more than one candidate
// and the resolver cannot pick one deterministically. The caller should
// pass a more specific epicID or filter the result manually.
var ErrAmbiguous = errors.New("master_plan ID matches multiple Jira tickets")

// ResolveMasterPlanID maps a master_plan epic ID (e.g. "130", "134.A.1") to
// its corresponding Jira ticket key (e.g. "MCPI-52") within projectKey.
//
// Lookup strategy:
//  1. cache.Get(epicID) — if hit, return immediately
//  2. JQL search: project = <projectKey> AND summary ~ "<epicID>"
//  3. If exactly one match: cache + return key
//  4. If multiple matches: cache + return the FIRST match, BUT also wrap
//     ErrAmbiguous so the caller can decide whether the heuristic is good
//     enough. Most epic IDs are unique within a project, so 1-match is
//     the common path.
//  5. If zero matches: return "" + ErrNotFound (no caching to avoid
//     poisoning negatives if the ticket is created later).
//
// projectKey is the Jira project key (e.g. "MCPI"). epicID is the
// canonical master_plan identifier as it appears in the .neo/master_plan.md
// checkbox: numeric prefix optionally followed by dotted alphanumeric
// segments. Whitespace is trimmed; empty input returns an error.
func ResolveMasterPlanID(ctx context.Context, c *Client, projectKey, epicID string, cache MasterPlanCache) (string, error) {
	epicID = strings.TrimSpace(epicID)
	projectKey = strings.TrimSpace(projectKey)
	if epicID == "" {
		return "", errors.New("epicID is required")
	}
	if projectKey == "" {
		return "", errors.New("projectKey is required")
	}
	if c == nil {
		return "", errors.New("jira client is required")
	}

	if cache != nil {
		if hit, ok := cache.Get(epicID); ok {
			return hit, nil
		}
	}

	// Force literal-substring match by escaping the term in nested quotes.
	// JQL backslash-escape: \" inside a JQL string yields a literal quote.
	jql := fmt.Sprintf(`project = %s AND summary ~ "\"%s\""`, projectKey, epicID)
	results, err := c.SearchIssues(ctx, jql, 5)
	if err != nil {
		return "", fmt.Errorf("search %s: %w", epicID, err)
	}
	if len(results) == 0 {
		return "", fmt.Errorf("epicID %q in project %s: %w", epicID, projectKey, ErrNotFound)
	}

	picked := results[0].Key
	if cache != nil {
		cache.Put(epicID, picked)
	}
	if len(results) > 1 {
		return picked, fmt.Errorf("%w: epicID %q yielded %d tickets, picked %s",
			ErrAmbiguous, epicID, len(results), picked)
	}
	return picked, nil
}

// MemoryCache is a tiny in-memory MasterPlanCache for callers that don't
// want to persist resolver output. Safe for concurrent use via the same
// rules as the embedded map (the caller wraps with sync.Mutex if needed).
type MemoryCache struct{ data map[string]string }

// NewMemoryCache returns an empty MemoryCache.
func NewMemoryCache() *MemoryCache { return &MemoryCache{data: make(map[string]string)} }

// Get implements MasterPlanCache.
func (m *MemoryCache) Get(epicID string) (string, bool) {
	v, ok := m.data[epicID]
	return v, ok
}

// Put implements MasterPlanCache.
func (m *MemoryCache) Put(epicID, ticketID string) { m.data[epicID] = ticketID }
