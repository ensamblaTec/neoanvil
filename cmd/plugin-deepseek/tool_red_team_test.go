package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/ensamblatec/neoanvil/pkg/deepseek"
	"github.com/ensamblatec/neoanvil/pkg/deepseek/session"
)

// Each test gets its own BoltDB to avoid pluginDBOnce singleton conflicts.
// We reset the once and db pointers between tests.
func resetPluginDB() {
	pluginDBOnce = sync.Once{}
	if pluginDB != nil {
		pluginDB.Close()
		pluginDB = nil
	}
	pluginDBErr = nil
}

func TestRedTeamNewThread(t *testing.T) {
	resetPluginDB()
	srv := fakeServer(t, "VULNERABILITY: SQL injection in line 12")
	defer srv.Close()

	dir := t.TempDir()
	goFile := filepath.Join(dir, "auth.go")
	os.WriteFile(goFile, []byte("package auth\nfunc Login(q string) {}"), 0600) //nolint:errcheck
	dbPath := filepath.Join(dir, "test.db")

	c, _ := newDeepSeekClient(t, srv.URL) //nolint:errcheck

	st := &state{apiKey: "k", client: c}
	resp := redTeamAuditWithDB(st, 1, map[string]any{
		"audit_focus": "SQL injection vulnerabilities",
		"files":       []any{goFile},
	}, dbPath)

	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]map[string]any)
	text, _ := content[0]["text"].(string)

	if !strings.Contains(text, "thread_id=") {
		t.Errorf("expected thread_id in response, got: %s", text)
	}
	if !strings.Contains(text, "ds_thread_") {
		t.Errorf("expected ds_thread_ prefix, got: %s", text)
	}
}

func TestRedTeamFollowUpContinues(t *testing.T) {
	resetPluginDB()
	srv := fakeServer(t, "Further analysis: no additional issues")
	defer srv.Close()

	dir := t.TempDir()
	goFile := filepath.Join(dir, "main.go")
	os.WriteFile(goFile, []byte("package main\nfunc main() {}"), 0600) //nolint:errcheck
	dbPath := filepath.Join(dir, "test.db")

	c, _ := newDeepSeekClient(t, srv.URL) //nolint:errcheck

	// Open DB and create a thread manually.
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewThreadStore(db)
	if err != nil {
		t.Fatal(err)
	}
	thread, err := store.Create([]string{goFile})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	st := &state{apiKey: "k", client: c}
	resp := redTeamAuditWithDB(st, 1, map[string]any{
		"thread_id": thread.ID,
		"follow_up": "Are there any memory safety issues?",
	}, dbPath)

	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("unexpected error on follow-up: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]map[string]any)
	text, _ := content[0]["text"].(string)
	if !strings.Contains(text, thread.ID) {
		t.Errorf("response should include thread_id=%s, got: %s", thread.ID, text)
	}
}

func TestRedTeamInvalidThreadIDError(t *testing.T) {
	resetPluginDB()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Pre-create DB so getPluginDB can open it.
	db, _ := bolt.Open(dbPath, 0600, nil) //nolint:errcheck
	db.Close()

	srv := fakeServer(t, "ok")
	defer srv.Close()
	c, _ := newDeepSeekClient(t, srv.URL) //nolint:errcheck

	st := &state{apiKey: "k", client: c}
	resp := redTeamAuditWithDB(st, 1, map[string]any{
		"thread_id": "ds_thread_nonexistent",
		"follow_up": "Any issues?",
	}, dbPath)

	if _, hasErr := resp["error"]; !hasErr {
		t.Error("expected error for non-existent thread_id")
	}
}

func TestRedTeamFileMutationInvalidates(t *testing.T) {
	resetPluginDB()
	dir := t.TempDir()
	goFile := filepath.Join(dir, "target.go")
	os.WriteFile(goFile, []byte("package target\nfunc Foo() {}"), 0600) //nolint:errcheck
	dbPath := filepath.Join(dir, "test.db")

	srv := fakeServer(t, "audit result")
	defer srv.Close()
	c, _ := newDeepSeekClient(t, srv.URL) //nolint:errcheck

	st := &state{apiKey: "k", client: c}
	// Create thread.
	resp1 := redTeamAuditWithDB(st, 1, map[string]any{
		"audit_focus": "Security",
		"files":       []any{goFile},
	}, dbPath)
	if _, hasErr := resp1["error"]; hasErr {
		t.Fatalf("initial call error: %v", resp1["error"])
	}
	// Extract thread_id from response.
	content, _ := resp1["result"].(map[string]any)["content"].([]map[string]any)
	text, _ := content[0]["text"].(string)
	threadID := extractThreadID(text)
	if threadID == "" {
		t.Fatalf("could not extract thread_id from: %s", text)
	}

	// Mutate the file.
	os.WriteFile(goFile, []byte("package target\nfunc Foo() { /* CHANGED */ }"), 0600) //nolint:errcheck

	// Follow-up should fail with thread_invalidated.
	resp2 := redTeamAuditWithDB(st, 2, map[string]any{
		"thread_id": threadID,
		"follow_up": "Any more issues?",
	}, dbPath)
	if errMsg, hasErr := resp2["error"]; !hasErr {
		t.Errorf("expected thread_invalidated error, got success with: %v", resp2)
	} else {
		em, _ := errMsg.(map[string]any)
		msg, _ := em["message"].(string)
		if !strings.Contains(msg, "thread_invalidated") {
			t.Errorf("expected thread_invalidated in error message, got: %s", msg)
		}
	}
}

func TestRedTeamAutoDistill30K(t *testing.T) {
	resetPluginDB()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	goFile := filepath.Join(dir, "a.go")
	os.WriteFile(goFile, []byte("package a\nfunc A() {}"), 0600) //nolint:errcheck

	srv := fakeServer(t, "distilled audit")
	defer srv.Close()
	c, _ := newDeepSeekClient(t, srv.URL) //nolint:errcheck

	// Create a thread with artificially high token count via direct store manipulation.
	db, _ := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 3 * time.Second}) //nolint:errcheck
	store, _ := session.NewThreadStore(db)                                    //nolint:errcheck
	thread, _ := store.Create([]string{goFile})                               //nolint:errcheck
	// Add a fat message to push token count > 30K.
	store.Append(thread.ID, session.Message{ //nolint:errcheck
		Role:       "assistant",
		Content:    "Big audit",
		TokensUsed: autoPressureTokens + 1,
		Timestamp:  time.Now(),
	})
	db.Close()

	st := &state{apiKey: "k", client: c}
	resp := redTeamAuditWithDB(st, 1, map[string]any{
		"thread_id": thread.ID,
		"follow_up": "Are there injection bugs?",
	}, dbPath)

	// Should not error — auto-distill creates a new thread and continues.
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("unexpected error after auto-distill: %v", resp["error"])
	}
}

// --- helpers ---

func newDeepSeekClient(t *testing.T, baseURL string) (*deepseek.Client, error) {
	t.Helper()
	c, err := deepseek.New(deepseek.Config{APIKey: "k", BaseURL: baseURL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, nil
}

func extractThreadID(text string) string {
	const prefix = "thread_id="
	_, after, ok0 := strings.Cut(text, prefix)
	if !ok0 {
		return ""
	}
	rest := after
	end := strings.IndexAny(rest, " \n\t")
	if end < 0 {
		return rest
	}
	return rest[:end]
}
