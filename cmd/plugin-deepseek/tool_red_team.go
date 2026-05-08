package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/ensamblatec/neoanvil/pkg/deepseek"
	"github.com/ensamblatec/neoanvil/pkg/deepseek/cache"
	"github.com/ensamblatec/neoanvil/pkg/deepseek/prompts"
	"github.com/ensamblatec/neoanvil/pkg/deepseek/session"
)

const autoPressureTokens = 30000

// pluginDB is the single BoltDB handle for the plugin process.
// Opened on first use, never closed (plugin owns it until exit).
var (
	pluginDBOnce sync.Once
	pluginDB     *bolt.DB
	pluginDBErr  error
)

// getPluginDB resolves the BoltDB path with this precedence:
//  1. explicit dbPath arg (tests inject TempDir paths)
//  2. DEEPSEEK_DB_PATH env var (operator override)
//  3. ~/.neo/db/deepseek.db (persistent default — survives reboots)
//  4. /tmp/deepseek_plugin.db (last-resort fallback if HOME is unresolvable)
//
// bbolt creates the FILE at first Open if missing, but does NOT create the
// parent directory. ~/.neo/db/ exists in workspaces (neo-mcp boots it for
// brain.db / hnsw.db / planner.db) but NOT in the operator's $HOME by
// default. We MkdirAll(parent, 0700) before Open to make the global path
// self-bootstrapping. [Épica 143 hardening — fix discovered post-bootstrap]
func getPluginDB(dbPath string) (*bolt.DB, error) {
	pluginDBOnce.Do(func() {
		path := dbPath
		if path == "" {
			path = os.Getenv("DEEPSEEK_DB_PATH") //nolint:gosec // G304-CLI-CONSENT
		}
		if path == "" {
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				path = filepath.Join(home, ".neo", "db", "deepseek.db")
			} else {
				path = filepath.Join(os.TempDir(), "deepseek_plugin.db")
			}
		}
		// Ensure parent dir exists — bbolt won't create it. 0700 because
		// ~/.neo/db/ holds session history + cached prompts which are
		// effectively secrets (could leak prompt context if read).
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if mkErr := os.MkdirAll(dir, 0700); mkErr != nil {
				pluginDBErr = mkErr
				return
			}
		}
		pluginDB, pluginDBErr = bolt.Open(path, 0600, &bolt.Options{Timeout: 3 * time.Second})
	})
	return pluginDB, pluginDBErr
}

// redTeamAudit implements the red_team_audit action.
//
// New thread: creates ThreadStore entry, computes initial FileDepsKey, posts adversarial prompt.
// Follow-up: retrieves thread, checks file mutation (FileDepsKey diff → error), checks context
// pressure (>30K tokens → auto-distill), appends and posts.
func redTeamAudit(s *state, id any, args map[string]any) map[string]any {
	return redTeamAuditWithDB(s, id, args, "")
}

// initNewThread creates a fresh audit thread for the given files and records
// the initial FileDepsKey for mutation detection.
func initNewThread(store *session.ThreadStore, tracker *cache.CacheKeyTracker, files []string) (session.Thread, error) {
	t, err := store.Create(files)
	if err != nil {
		return session.Thread{}, fmt.Errorf("create thread: %w", err)
	}
	initialKey := tracker.Snapshot(files)
	if err := store.SetFileDepsKey(t.ID, string(initialKey)); err != nil {
		return session.Thread{}, fmt.Errorf("set file deps key: %w", err)
	}
	t.FileDepsKey = string(initialKey)
	return t, nil
}

// resolveExistingThread loads a thread by ID, validates it is active and its
// files have not mutated, and applies context-pressure relief when the token
// count exceeds the auto-distill threshold. Returns the (possibly replaced)
// thread and the effective files list (falls back to thread.FileDeps).
func resolveExistingThread(store *session.ThreadStore, tracker *cache.CacheKeyTracker, threadID string, files []string) (session.Thread, []string, error) {
	t, derr := store.Get(threadID)
	if derr != nil {
		return session.Thread{}, nil, fmt.Errorf("thread not found: %w", derr)
	}
	if t.Status != session.ThreadStatusActive {
		return session.Thread{}, nil, fmt.Errorf("thread %s is expired", threadID)
	}
	if t.FileDepsKey != "" && len(t.FileDeps) > 0 {
		currentKey := tracker.Snapshot(t.FileDeps)
		if string(currentKey) != t.FileDepsKey {
			return session.Thread{}, nil, fmt.Errorf(
				"thread_invalidated — files mutated since thread %s was created; start a new audit", t.ID)
		}
	}
	if t.TokenCount > autoPressureTokens {
		summary := distillHistory(t.History)
		newT, berr := store.Create(t.FileDeps)
		if berr == nil {
			store.SetFileDepsKey(newT.ID, t.FileDepsKey) //nolint:errcheck
			store.Expire(t.ID)                           //nolint:errcheck
			store.Append(newT.ID, session.Message{       //nolint:errcheck
				Role:       "user",
				Content:    "[Context compressed]: " + summary,
				TokensUsed: len(summary) / 4,
				Timestamp:  time.Now(),
			})
			t = newT
		}
	}
	if len(files) == 0 {
		files = t.FileDeps
	}
	return t, files, nil
}

// redTeamAuditWithDB is the testable variant that accepts an explicit dbPath.
func redTeamAuditWithDB(s *state, id any, args map[string]any, dbPath string) map[string]any {
	focus, _ := args["audit_focus"].(string)
	if focus == "" {
		focus, _ = args["target_prompt"].(string)
	}
	threadID, _ := args["thread_id"].(string)
	followUp, _ := args["follow_up"].(string)

	var files []string
	if raw, ok := args["files"].([]any); ok {
		for _, f := range raw {
			if p, ok := f.(string); ok {
				files = append(files, p)
			}
		}
	}

	if s.client == nil {
		return ok(id, textContent(fmt.Sprintf(
			"[deepseek/red_team_audit] stub — client not initialised. thread_id:%q files:%d",
			threadID, len(files))))
	}

	db, err := getPluginDB(dbPath)
	if err != nil {
		return rpcErr(id, -32603, "red_team_audit: open db: "+err.Error())
	}
	store, err := session.NewThreadStore(db)
	if err != nil {
		return rpcErr(id, -32603, "red_team_audit: init store: "+err.Error())
	}

	tracker := cache.NewTracker()
	builder := cache.NewBuilder(
		"You are a senior security auditor. Find vulnerabilities, anti-patterns, and logic flaws.",
		"", 80000, time.Hour)

	// [375.B] Thread continuity suggest: if no thread_id and an active thread
	// exists for the same package prefix, suggest reusing it (30% cheaper).
	var threadSuggest string
	if threadID == "" {
		if existing := store.FindByPrefix(files, 15*time.Minute); existing != nil {
			threadSuggest = fmt.Sprintf("\n\n💡 THREAD_REUSE: active thread %s covers same package (last active %s ago). Pass thread_id:\"%s\" to save ~30%% on cache hits.",
				existing.ID, time.Since(existing.LastActive).Round(time.Second), existing.ID)
		}
	}

	var thread session.Thread
	if threadID == "" {
		t, terr := initNewThread(store, tracker, files)
		if terr != nil {
			return rpcErr(id, -32603, "red_team_audit: "+terr.Error())
		}
		thread = t
		atomic.AddInt64(&s.threadCount, 1)
	} else {
		t, ef, terr := resolveExistingThread(store, tracker, threadID, files)
		if terr != nil {
			return rpcErr(id, -32602, "red_team_audit: "+terr.Error())
		}
		thread, files = t, ef
	}

	block1, _, _ := builder.BuildBlock1(files)

	// [ÉPICA 151.D + 151.B] Domain-specific prompt augmentation. Path-based
	// picker selects {crypto, storage, auth, concurrency, network} templates
	// matching the audited files, plus the universal mechanical_trace
	// requirement. Encodes the empirical hallucination-reduction patterns
	// from PILAR XXVI/XXVIII audit sessions (was manually scaffolded
	// per-prompt; now standardized).
	domainPrefix := prompts.AssemblePrefix(prompts.PickTemplates(files))

	var userMsg string
	if followUp != "" {
		userMsg = followUp
	} else {
		userMsg = fmt.Sprintf("%s\n\nPerform a red-team security audit.\n\nFocus: %s\n\n"+
			"Be adversarial. Enumerate attack vectors, privilege escalations, injection paths, and logic bugs.",
			domainPrefix, focus)
	}

	fullPrompt := builder.AssemblePrompt(block1, userMsg)
	model, thinking := parseModelAndThinking(args)
	resp, err := s.client.Call(context.Background(), deepseek.CallRequest{
		Action:   "red_team_audit",
		Prompt:   fullPrompt,
		Mode:     deepseek.SessionModeThreaded,
		ThreadID: thread.ID,
		Model:    model,
		Thinking: thinking,
	})
	if err != nil {
		return rpcErr(id, -32603, "red_team_audit: call: "+err.Error())
	}
	s.recordAPICall(resp) // [ÉPICA 151.E] cache discipline aggregate

	store.Append(thread.ID, session.Message{ //nolint:errcheck
		Role:       "user",
		Content:    userMsg,
		TokensUsed: resp.InputTokens,
		Timestamp:  time.Now(),
	})
	store.Append(thread.ID, session.Message{ //nolint:errcheck
		Role:       "assistant",
		Content:    resp.Text,
		TokensUsed: resp.OutputTokens,
		Timestamp:  time.Now(),
	})

	header := formatUsageLine(resp, "thread_id="+thread.ID)
	out := header + "\n\n" + resp.Text + s.cacheColdAdvisory(resp) + threadSuggest
	return ok(id, textContent(out))
}

// distillHistory produces a short summary of a conversation for context pressure relief.
func distillHistory(history []session.Message) string {
	var sb strings.Builder
	sb.WriteString("Conversation summary:\n")
	n := min(len(history), 5)
	for _, m := range history[:n] {
		content := m.Content
		if len(content) > 200 {
			content = content[:200] + "..."
		}
		sb.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, content))
	}
	return sb.String()
}
