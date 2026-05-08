package session

import "fmt"

// toolModeMap maps DeepSeek action names to their SessionMode.
var toolModeMap = map[string]SessionMode{
	"distill_payload":    SessionModeEphemeral,
	"map_reduce_refactor": SessionModeEphemeral,
	"red_team_audit":     SessionModeThreaded,
}

// SessionRouter dispatches calls to the correct session mode and thread.
type SessionRouter struct {
	store *ThreadStore
}

// NewRouter creates a SessionRouter. store may be nil — in that case threaded
// tools return an error at route time (useful when BoltDB is unavailable).
func NewRouter(store *ThreadStore) *SessionRouter {
	return &SessionRouter{store: store}
}

// Route resolves the SessionMode for tool and, for threaded tools, returns the
// relevant Thread:
//
//   - Ephemeral tool  → (Ephemeral, nil, nil)
//   - Threaded + empty threadID  → (Threaded, &newThread, nil)
//   - Threaded + existing threadID → (Threaded, &thread, nil)  or error if expired/missing
//
// Returns an error if the store is nil for threaded tools, or if the requested
// thread is expired or not found.
func (r *SessionRouter) Route(tool, threadID string) (SessionMode, *Thread, error) {
	mode, ok := toolModeMap[tool]
	if !ok {
		// Unknown tools default to ephemeral.
		return SessionModeEphemeral, nil, nil
	}
	if mode == SessionModeEphemeral {
		return SessionModeEphemeral, nil, nil
	}

	// Threaded path.
	if r.store == nil {
		return 0, nil, fmt.Errorf("session: ThreadStore not initialised (no BoltDB path configured)")
	}
	if threadID == "" {
		t, err := r.store.Create(nil)
		if err != nil {
			return 0, nil, fmt.Errorf("session: create thread: %w", err)
		}
		return SessionModeThreaded, &t, nil
	}

	t, err := r.store.Get(threadID)
	if err != nil {
		return 0, nil, fmt.Errorf("session: get thread %s: %w", threadID, err)
	}
	if t.Status == ThreadStatusExpired {
		return 0, nil, fmt.Errorf("session: thread %s is expired", threadID)
	}
	return SessionModeThreaded, &t, nil
}
