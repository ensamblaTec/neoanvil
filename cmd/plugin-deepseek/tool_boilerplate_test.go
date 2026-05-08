package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestBoilerplateHNSWSkipSimilar(t *testing.T) {
	resetPluginDB()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	goFile := filepath.Join(dir, "math.go")

	content := "package math\n\nfunc Add(a, b int) int { return a + b }\nfunc Sub(a, b int) int { return a - b }\n"
	os.WriteFile(goFile, []byte(content), 0600) //nolint:errcheck

	// Pre-populate BoltDB with identical content (Jaccard sim = 1.0 > 0.85 threshold).
	db, err := bolt.Open(dbPath, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := initBGBuckets(db); err != nil {
		t.Fatal(err)
	}
	if err := storeSimilarContent(db, filepath.Join(dir, "other.go"), content); err != nil {
		t.Fatal(err)
	}
	db.Close()

	srv := fakeServer(t, "package math_test\nfunc TestAdd(t *testing.T) {}")
	defer srv.Close()
	c, _ := newDeepSeekClient(t, srv.URL) //nolint:errcheck

	doneCh := make(chan struct{})
	st := &state{apiKey: "k", client: c}
	resp := generateBoilerplateWithDB(st, 1, map[string]any{
		"target_file":      goFile,
		"boilerplate_type": "tests",
	}, dbPath, nil, doneCh)

	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("unexpected error: %v", resp["error"])
	}

	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("background goroutine did not finish in time")
	}

	// Extract task_id and verify skipped status.
	result, _ := resp["result"].(map[string]any)
	content0, _ := result["content"].([]map[string]any)
	text, _ := content0[0]["text"].(string)
	taskID := extractBGTaskID(text)
	if taskID == "" {
		t.Fatalf("no task_id in response: %s", text)
	}

	// pluginDB is the singleton opened by getPluginDB inside generateBoilerplateWithDB.
	task, err := loadBGTask(pluginDB, taskID)
	if err != nil {
		t.Fatalf("load task: %v", err)
	}
	if task.Status != "skipped" {
		t.Errorf("expected status=skipped, got: %s (reason: %s)", task.Status, task.Reason)
	}
	if !strings.Contains(task.Reason, "similar content detected") {
		t.Errorf("expected similar content reason, got: %s", task.Reason)
	}
}

func TestBoilerplateLaunchReturnsTaskID(t *testing.T) {
	resetPluginDB()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	goFile := filepath.Join(dir, "calc.go")
	os.WriteFile(goFile, []byte("package calc\nfunc Mul(a, b int) int { return a * b }"), 0600) //nolint:errcheck

	srv := fakeServer(t, "// test stub")
	defer srv.Close()
	c, _ := newDeepSeekClient(t, srv.URL) //nolint:errcheck

	// Inject no-similarity function so generation is not blocked.
	noSim := func(_ *bolt.DB, _ string) (float64, string) { return 0, "" }

	start := time.Now()
	st := &state{apiKey: "k", client: c}
	resp := generateBoilerplateWithDB(st, 1, map[string]any{
		"target_file":      goFile,
		"boilerplate_type": "tests",
	}, dbPath, noSim, nil)
	elapsed := time.Since(start)

	// Response must be near-immediate (goroutine detached).
	if elapsed > time.Second {
		t.Errorf("expected immediate response, took %v", elapsed)
	}
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]map[string]any)
	text, _ := content[0]["text"].(string)

	if !strings.Contains(text, "task_id=") {
		t.Errorf("expected task_id= in response, got: %s", text)
	}
	if !strings.Contains(text, bgTaskIDPrefix) {
		t.Errorf("expected prefix %s in task_id, got: %s", bgTaskIDPrefix, text)
	}
}

func TestBoilerplateBackgroundWritesFile(t *testing.T) {
	resetPluginDB()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	goFile := filepath.Join(dir, "util.go")
	os.WriteFile(goFile, []byte("package util\nfunc Double(n int) int { return n * 2 }"), 0600) //nolint:errcheck

	srv := fakeServer(t, "package util_test\nfunc TestDouble(t *testing.T) {}")
	defer srv.Close()
	c, _ := newDeepSeekClient(t, srv.URL) //nolint:errcheck

	noSim := func(_ *bolt.DB, _ string) (float64, string) { return 0, "" }
	doneCh := make(chan struct{})
	st := &state{apiKey: "k", client: c}
	resp := generateBoilerplateWithDB(st, 1, map[string]any{
		"target_file":      goFile,
		"boilerplate_type": "tests",
	}, dbPath, noSim, doneCh)

	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("unexpected error: %v", resp["error"])
	}

	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("background goroutine did not finish in time")
	}

	expectedFile := filepath.Join(dir, "util_test.go")
	if _, err := os.Stat(expectedFile); err != nil {
		t.Errorf("expected %s to be written: %v", expectedFile, err)
	}
}

func TestBoilerplateTaskIDStatusQuery(t *testing.T) {
	resetPluginDB()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	goFile := filepath.Join(dir, "svc.go")
	os.WriteFile(goFile, []byte("package svc\nfunc Run() {}"), 0600) //nolint:errcheck

	srv := fakeServer(t, "// package svc documentation")
	defer srv.Close()
	c, _ := newDeepSeekClient(t, srv.URL) //nolint:errcheck

	noSim := func(_ *bolt.DB, _ string) (float64, string) { return 0, "" }
	doneCh := make(chan struct{})
	st := &state{apiKey: "k", client: c}
	resp := generateBoilerplateWithDB(st, 1, map[string]any{
		"target_file":      goFile,
		"boilerplate_type": "docs",
	}, dbPath, noSim, doneCh)

	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]map[string]any)
	text, _ := content[0]["text"].(string)
	taskID := extractBGTaskID(text)
	if taskID == "" {
		t.Fatalf("no task_id in response: %s", text)
	}

	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("background goroutine did not finish in time")
	}

	// Query by task_id — second call with task_id returns status.
	resp2 := generateBoilerplateWithDB(st, 2, map[string]any{
		"task_id": taskID,
	}, dbPath, nil, nil)

	if _, hasErr := resp2["error"]; hasErr {
		t.Fatalf("status query error: %v", resp2["error"])
	}
	result2, _ := resp2["result"].(map[string]any)
	content2, _ := result2["content"].([]map[string]any)
	text2, _ := content2[0]["text"].(string)
	if !strings.Contains(text2, "status=") {
		t.Errorf("expected status= in response, got: %s", text2)
	}
	if !strings.Contains(text2, taskID) {
		t.Errorf("expected task_id=%s in response, got: %s", taskID, text2)
	}
}

// extractBGTaskID extracts the task_id value from a response text.
func extractBGTaskID(text string) string {
	const prefix = "task_id="
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
