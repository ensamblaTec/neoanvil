package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	bolt "go.etcd.io/bbolt"

	"github.com/ensamblatec/neoanvil/pkg/deepseek"
	"github.com/ensamblatec/neoanvil/pkg/deepseek/cache"
)

const (
	bucketBGTasks   = "deepseek_bg_tasks"
	bucketBGSimilar = "deepseek_bg_similar"
	bgTaskIDPrefix  = "bgtask_"
	simThreshold    = 0.85
)

// bgTaskStatus tracks a background boilerplate generation task.
type bgTaskStatus struct {
	ID          string     `json:"id"`
	TargetFile  string     `json:"target_file"`
	BplType     string     `json:"boilerplate_type"`
	Status      string     `json:"status"` // pending|running|done|error|skipped
	FileWritten string     `json:"file_written,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	DoneAt      *time.Time `json:"done_at,omitempty"`
}

// similarFn checks content against previously stored content.
// Returns similarity score ∈ [0,1] and the matching source path (if any).
type similarFn func(db *bolt.DB, content string) (float64, string)

// generateBoilerplate implements the generate_boilerplate action.
//
// Two modes:
//   - Launch: target_file + boilerplate_type → returns task_id immediately; goroutine runs in background.
//   - Status: task_id → returns task status from BoltDB.
//
// HNSW dedup (131.I): Jaccard similarity against stored content — skips generation when ≥ 0.85.
func generateBoilerplate(s *state, id any, args map[string]any) map[string]any {
	return generateBoilerplateWithDB(s, id, args, "", nil, nil)
}

// generateBoilerplateWithDB is the testable variant. doneCh is closed when the background goroutine finishes.
func generateBoilerplateWithDB(s *state, id any, args map[string]any, dbPath string, simFn similarFn, doneCh chan struct{}) map[string]any {
	// Status query mode.
	if taskID, ok := args["task_id"].(string); ok && taskID != "" {
		return queryBGTaskStatus(id, dbPath, taskID)
	}

	targetFile, _ := args["target_file"].(string)
	bplType, _ := args["boilerplate_type"].(string)
	styleGuide, _ := args["style_guide"].(string)
	if styleGuide == "" {
		styleGuide, _ = args["target_prompt"].(string)
	}
	if styleGuide == "" {
		styleGuide = "Follow the project's existing code style and conventions."
	}

	if targetFile == "" {
		return rpcErr(id, -32602, "generate_boilerplate: target_file is required")
	}
	if bplType != "tests" && bplType != "docs" && bplType != "both" {
		return rpcErr(id, -32602, "generate_boilerplate: boilerplate_type must be tests|docs|both")
	}

	if s.client == nil {
		return ok(id, textContent(fmt.Sprintf(
			"[deepseek/generate_boilerplate] stub — client not initialised. target_file:%s type:%s",
			targetFile, bplType)))
	}

	db, err := getPluginDB(dbPath)
	if err != nil {
		return rpcErr(id, -32603, "generate_boilerplate: open db: "+err.Error())
	}
	if err := initBGBuckets(db); err != nil {
		return rpcErr(id, -32603, "generate_boilerplate: init buckets: "+err.Error())
	}

	taskID := newBGTaskID()
	task := bgTaskStatus{
		ID:         taskID,
		TargetFile: targetFile,
		BplType:    bplType,
		Status:     "pending",
		CreatedAt:  time.Now(),
	}
	if err := saveBGTask(db, task); err != nil {
		return rpcErr(id, -32603, "generate_boilerplate: save task: "+err.Error())
	}

	if simFn == nil {
		simFn = jaccardSimilarity
	}

	// Parse model + thinking overrides at the synchronous entry; the
	// background goroutine doesn't have args in scope. [Phase 4]
	model, thinking := parseModelAndThinking(args)

	go func() {
		if doneCh != nil {
			defer close(doneCh)
		}
		runBoilerplateTask(s, db, task, styleGuide, simFn, model, thinking)
	}()

	return ok(id, textContent(fmt.Sprintf("task_id=%s status=pending target_file=%s type=%s",
		taskID, targetFile, bplType)))
}

// runBoilerplateTask executes the generation pipeline in the background.
// model + thinking are the per-call overrides plumbed from the synchronous
// entry point (the goroutine itself has no access to the original args map).
func runBoilerplateTask(s *state, db *bolt.DB, task bgTaskStatus, styleGuide string, simFn similarFn, model string, thinking *deepseek.ThinkingConfig) {
	task.Status = "running"
	saveBGTask(db, task) //nolint:errcheck

	content, err := os.ReadFile(task.TargetFile) //nolint:gosec // G304-CLI-CONSENT: operator-supplied paths
	if err != nil {
		finishTask(db, &task, "", "", fmt.Sprintf("read file: %v", err))
		return
	}

	// HNSW similarity check — skip if duplicate content detected.
	sim, reason := simFn(db, string(content))
	if sim >= simThreshold {
		task.Status = "skipped"
		task.Reason = fmt.Sprintf("similar content detected (similarity=%.2f): %s", sim, reason)
		done := time.Now()
		task.DoneAt = &done
		saveBGTask(db, task) //nolint:errcheck
		return
	}

	builder := cache.NewBuilder(
		"You are a senior Go engineer writing high-quality boilerplate code.", "", 80000, time.Hour)
	block1, _, _ := builder.BuildBlock1([]string{task.TargetFile})

	outputFile, prompt := buildBoilerplatePrompt(task, string(content), styleGuide)

	assembled := builder.AssemblePrompt(block1, prompt)
	resp, err := s.client.Call(context.Background(), deepseek.CallRequest{
		Action:    "generate_boilerplate",
		Prompt:    assembled,
		Mode:      deepseek.SessionModeEphemeral,
		MaxTokens: 4096,
		Model:     model,
		Thinking:  thinking,
	})
	if err != nil {
		finishTask(db, &task, "", "", fmt.Sprintf("api call: %v", err))
		return
	}
	s.recordAPICall(resp) // [ÉPICA 151.E] cache discipline aggregate

	if err := os.WriteFile(outputFile, []byte(resp.Text), 0600); err != nil { //nolint:gosec // G306-output-file: operator-controlled path
		finishTask(db, &task, "", "", fmt.Sprintf("write output file: %v", err))
		return
	}

	// Store content for future similarity checks (HNSW insert).
	storeSimilarContent(db, task.TargetFile, string(content)) //nolint:errcheck

	finishTask(db, &task, outputFile, "", "")
}

// buildBoilerplatePrompt returns the output file path and assembled prompt for the given task.
func buildBoilerplatePrompt(task bgTaskStatus, content, styleGuide string) (outputFile, prompt string) {
	ext := filepath.Ext(task.TargetFile)
	base := strings.TrimSuffix(task.TargetFile, ext)

	switch task.BplType {
	case "tests":
		outputFile = base + "_test" + ext
		prompt = fmt.Sprintf(
			"Write comprehensive unit tests. Style guide: %s\n\nFile: %s\n\n%s",
			styleGuide, task.TargetFile, content)
	case "docs":
		outputFile = filepath.Join(filepath.Dir(task.TargetFile), "doc.go")
		prompt = fmt.Sprintf(
			"Write a doc.go with package documentation. Style guide: %s\n\nFile: %s\n\n%s",
			styleGuide, task.TargetFile, content)
	default: // "both"
		outputFile = base + "_test" + ext
		prompt = fmt.Sprintf(
			"Write comprehensive unit tests and godoc comments. Style guide: %s\n\nFile: %s\n\n%s",
			styleGuide, task.TargetFile, content)
	}
	return
}

// finishTask updates task status to done/error and persists it.
func finishTask(db *bolt.DB, task *bgTaskStatus, fileWritten, reason, errMsg string) {
	done := time.Now()
	task.DoneAt = &done
	if errMsg != "" {
		task.Status = "error"
		task.Error = errMsg
	} else {
		task.Status = "done"
		task.FileWritten = fileWritten
		task.Reason = reason
	}
	saveBGTask(db, *task) //nolint:errcheck
}

// queryBGTaskStatus returns the status of a background task.
func queryBGTaskStatus(id any, dbPath, taskID string) map[string]any {
	db, err := getPluginDB(dbPath)
	if err != nil {
		return rpcErr(id, -32603, "generate_boilerplate: open db: "+err.Error())
	}
	task, err := loadBGTask(db, taskID)
	if err != nil {
		return rpcErr(id, -32602, fmt.Sprintf("generate_boilerplate: task %s not found: %v", taskID, err))
	}
	text := fmt.Sprintf("task_id=%s status=%s", task.ID, task.Status)
	if task.FileWritten != "" {
		text += " file_written=" + task.FileWritten
	}
	if task.Error != "" {
		text += " error=" + task.Error
	}
	if task.Reason != "" {
		text += " reason=" + task.Reason
	}
	return ok(id, textContent(text))
}

// --- BoltDB helpers ---

func initBGBuckets(db *bolt.DB) error {
	return db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{bucketBGTasks, bucketBGSimilar} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		return nil
	})
}

func saveBGTask(db *bolt.DB, task bgTaskStatus) error {
	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketBGTasks))
		if b == nil {
			return fmt.Errorf("bucket missing: %s", bucketBGTasks)
		}
		v, err := json.Marshal(task)
		if err != nil {
			return err
		}
		return b.Put([]byte(task.ID), v)
	})
}

func loadBGTask(db *bolt.DB, taskID string) (bgTaskStatus, error) {
	var task bgTaskStatus
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketBGTasks))
		if b == nil {
			return fmt.Errorf("bucket missing: %s", bucketBGTasks)
		}
		v := b.Get([]byte(taskID))
		if v == nil {
			return fmt.Errorf("task not found")
		}
		return json.Unmarshal(v, &task)
	})
	return task, err
}

func storeSimilarContent(db *bolt.DB, path, content string) error {
	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketBGSimilar))
		if b == nil {
			return fmt.Errorf("bucket missing: %s", bucketBGSimilar)
		}
		return b.Put([]byte(path), []byte(content))
	})
}

// --- Similarity (Jaccard) ---

// jaccardSimilarity is the default similarFn. Scans bucketBGSimilar and returns the max Jaccard score.
func jaccardSimilarity(db *bolt.DB, content string) (float64, string) {
	tokensA := tokenizeWords(content)
	if len(tokensA) == 0 {
		return 0, ""
	}
	var maxSim float64
	var mostSimilarPath string
	db.View(func(tx *bolt.Tx) error { //nolint:errcheck
		b := tx.Bucket([]byte(bucketBGSimilar))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			tokensB := tokenizeWords(string(v))
			sim := jaccardCoeff(tokensA, tokensB)
			if sim > maxSim {
				maxSim = sim
				mostSimilarPath = string(k)
			}
			return nil
		})
	})
	return maxSim, mostSimilarPath
}

// tokenizeWords splits text into a word-token set (lowercase, min 3 chars).
func tokenizeWords(s string) map[string]struct{} {
	tokens := make(map[string]struct{})
	var sb strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			sb.WriteRune(r)
		} else if sb.Len() > 0 {
			if tok := sb.String(); len(tok) >= 3 {
				tokens[tok] = struct{}{}
			}
			sb.Reset()
		}
	}
	if sb.Len() >= 3 {
		tokens[sb.String()] = struct{}{}
	}
	return tokens
}

// jaccardCoeff computes |A∩B| / |A∪B|.
func jaccardCoeff(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersection := 0
	for tok := range a {
		if _, ok := b[tok]; ok {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func newBGTaskID() string {
	var buf [8]byte
	rand.Read(buf[:]) //nolint:errcheck
	return bgTaskIDPrefix + hex.EncodeToString(buf[:])
}
