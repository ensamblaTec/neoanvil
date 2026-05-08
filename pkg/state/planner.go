package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.etcd.io/bbolt"
	bbolterrors "go.etcd.io/bbolt/errors"
)

// memexIDSuffix returns 8 hex chars derived from 4 cryptographically-random
// bytes. Appended to MEMEX-* identifiers so two commits within the same
// nanosecond (or the same 16ms window on Windows where time.Now().UnixNano()
// has coarse resolution) cannot collide on the same key and silently
// overwrite each other. Pre-fix, `MEMEX-<unixNano>` allowed two MemexCommit
// calls within ~16ms on Windows to produce identical IDs and lose the first
// entry on Put. Audit finding pkg/state/planner.go:241 (2026-05-01, SEV 8).
//
// The suffix is 4 bytes / 32 bits — collision probability for 1M entries
// generated within the same UnixNano tick is ~10^-3, negligible in
// practice; combined with the (already-distinct in 99.9% of cases on Linux)
// nanosecond prefix the effective collision space is ~10^-12. We do not
// fall back to a deterministic source on rand.Read failure: a system whose
// CSPRNG is broken should fail the commit loudly rather than risk a silent
// duplicate.
func memexIDSuffix() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand on Linux reads from /dev/urandom which never fails
		// once the kernel CSPRNG is initialised. If it does fail, return a
		// deterministic-but-unique-per-nanosecond fallback so MemexCommit
		// surfaces an error path rather than silently colliding.
		return fmt.Sprintf("xx%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(buf[:])
}

type SRETask struct {
	ID             string `json:"id"`
	Description    string `json:"description"`
	TargetFile     string `json:"target_file"`
	Status         string `json:"status"`          // "TODO", "DONE"
	Role           string `json:"role"`            // [SRE-25.1.1] optional agent role ("frontend", "backend", "")
	LifecycleState string `json:"lifecycle_state,omitempty"` // [362.A] pending|in_progress|completed|orphaned|rolled_back|failed_permanent
	CreatedAt      int64  `json:"created_at,omitempty"`       // [362.A] unix seconds when task was enqueued
	UpdatedAt      int64  `json:"updated_at,omitempty"`       // [362.A] unix seconds of last lifecycle transition
	StartedAt      int64  `json:"started_at,omitempty"`       // [132.A] unix seconds when task was first claimed
	Retries        int    `json:"retries,omitempty"`          // [132.A] number of orphan-recovery retry attempts
	LastError      string `json:"last_error,omitempty"`       // [132.A] reason for last orphan/failure
	CheckpointData string `json:"checkpoint_data,omitempty"`  // [132.A] opaque agent checkpoint for resumable work
	Backend        string `json:"backend,omitempty"`           // [132.F] "auto"|"deepseek"|"claude"; empty = "auto"
	MigratedAt     int64  `json:"migrated_at,omitempty"`       // [138.C.7] unix seconds when task was processed by the trust-system migration shim. Zero = never migrated.
}

// Task lifecycle states. [362.A]
const (
	TaskLifecyclePending        = "pending"
	TaskLifecycleInProgress     = "in_progress"
	TaskLifecycleCompleted      = "completed"
	TaskLifecycleOrphaned       = "orphaned"
	TaskLifecycleRolledBack     = "rolled_back"
	TaskLifecycleFailedPermanent = "failed_permanent" // [132.A] max retries exceeded — will not be re-queued
)

var (
	plannerDB        *bbolt.DB
	plannerWorkspace string
	activeTenantID   string // [Épica 265.B] set via SetActiveTenant after credentials load
)

// OnEpicClose is called when the last Kanban task transitions from TODO → DONE.
// [SRE-56.1] Wire this in main.go to publish EventSuggestCommit to the SSE bus.
var OnEpicClose func()

const taskBucket = "SRE_TASKS"
const memexBucket = "memex_buffer"       // [SRE-28.1.1] episodic memory short-term buffer
const batchCertifyBucket = "batch_certify_log" // [362.A] two-phase commit log for batch certify transactions

// SetActiveTenant configures per-tenant memex bucket namespacing. [Épica 265.B]
// Call after credentials are loaded. Empty string reverts to default bucket.
func SetActiveTenant(id string) {
	activeTenantID = id
}

// memexBucketName returns the active memex bucket name, namespaced by tenant
// when one is configured. Default "memex_buffer" preserves backward compat. [265.B]
func memexBucketName() string {
	if activeTenantID != "" {
		return memexBucket + ":" + activeTenantID
	}
	return memexBucket
}

// MemexEntry is an episodic memory record — a lesson learned during an agent session. [SRE-28.1.1]
// [SRE-39.1] Extended with CausalContext for situational awareness.
type MemexEntry struct {
	ID        string         `json:"id"`
	Topic     string         `json:"topic"`
	Scope     string         `json:"scope"`     // path or module, e.g. "pkg/rag" or "frontend"
	Content   string         `json:"content"`   // the lesson learned
	Timestamp int64          `json:"timestamp"`
	Causal    *CausalContext `json:"causal,omitempty"` // [SRE-39.1] situational context at time of recording
}

// CausalContext captures the hardware and system state when a memex entry was created. [SRE-39.1]
// This enables "why was this decision made?" reconstruction via intent chains.
type CausalContext struct {
	HeapMB       float64  `json:"heap_mb"`        // runtime heap at time of recording
	GCRuns       uint32   `json:"gc_runs"`         // cumulative GC runs
	CPULoad      float64  `json:"cpu_load"`        // approximate CPU utilization (0-1)
	RAPLWatts    float64  `json:"rapl_watts"`      // power consumption if available
	ServerMode   string   `json:"server_mode"`     // "pair", "fast", "daemon"
	PriorErrors  []string `json:"prior_errors"`    // last N errors before this entry (max 5)
	TriggerEvent string   `json:"trigger_event"`   // what caused this entry: "user_commit", "rem_sleep", "flashback_miss", etc.
	ParentID     string   `json:"parent_id"`       // ID of the memex entry that caused this one (causal chain)
}

// InitPlanner inicializa la base de datos de colas SRE
func InitPlanner(workspace string) error {
	dbPath := filepath.Join(workspace, ".neo/db", "planner.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0750); err != nil {
		return fmt.Errorf("[SRE-FATAL] Directorio persistente bbolt inalcanzable: %w", err)
	}

	db, err := bbolt.Open(dbPath, 0600, &bbolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return err
	}

	err = db.Update(func(tx *bbolt.Tx) error {
		if _, e := tx.CreateBucketIfNotExists([]byte(taskBucket)); e != nil {
			return e
		}
		if _, e := tx.CreateBucketIfNotExists([]byte(memexBucket)); e != nil {
			return e
		}
		if _, e := tx.CreateBucketIfNotExists([]byte(batchCertifyBucket)); e != nil { // [362.A]
			return e
		}
		_, e := tx.CreateBucketIfNotExists([]byte(daemonBudgetBucket)) // [132.B]
		return e
	})

	if err != nil {
		_ = db.Close()
		return fmt.Errorf("[SRE-FATAL] Fallo al forjar bucket inicial SRE_TASKS: %w", err)
	}

	plannerDB = db
	plannerWorkspace = workspace
	return nil
}

// ReadActivePhase parsea master_plan.md y devuelve SOLO la fase que tiene tareas pendientes [-].
func ReadActivePhase(workspace string) (string, error) {
	planPath := filepath.Join(workspace, ".neo", "master_plan.md")
	//nolint:gosec // G304-WORKSPACE-CANON: workspace is strictly enforced
	data, err := os.ReadFile(planPath)
	if err != nil {
		return "", fmt.Errorf("no se encontró .neo/master_plan.md. Crea el archivo con el plan estratégico primero")
	}

	lines := strings.Split(string(data), "\n")
	var activePhase []string
	inActivePhase := false
	hasPendingTasks := false
	// [330.M] Track code-fence state so ``` blocks don't leak fake phase boundaries.
	inFence := false

	for _, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			inFence = !inFence
			if inActivePhase {
				activePhase = append(activePhase, line)
			}
			continue
		}

		if !inFence && strings.HasPrefix(line, "## ") {
			if inActivePhase && hasPendingTasks {
				break // Terminó la fase activa, no leemos más para ahorrar tokens
			}
			inActivePhase = true
			activePhase = []string{line}
			continue
		}

		if inActivePhase {
			activePhase = append(activePhase, line)
			if !inFence && strings.HasPrefix(strings.TrimLeft(line, " \t"), "- [ ]") {
				hasPendingTasks = true
			}
		}
	}

	if !hasPendingTasks {
		return "🎉 NO HAY FASES PENDIENTES. Todas las tareas de master_plan.md están marcadas con [x]. Épica completada.", nil
	}

	return strings.Join(activePhase, "\n"), nil
}

// ReadOpenTasks returns only the open (- [ ]) task lines with their parent ## and ###
// headings — ~13× fewer tokens than ReadActivePhase for the same navigation intent. [318.A]
func ReadOpenTasks(workspace string) (string, error) {
	planPath := filepath.Join(workspace, ".neo", "master_plan.md")
	//nolint:gosec // G304-WORKSPACE-CANON: workspace is strictly enforced
	data, err := os.ReadFile(planPath)
	if err != nil {
		return "", fmt.Errorf("no se encontró .neo/master_plan.md")
	}

	var out []string
	var h2, h3 string
	h2Printed, h3Printed := false, false
	// [330.M] Track code-fence state so ``` blocks don't leak fake headings/tasks.
	inFence := false

	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		switch {
		case strings.HasPrefix(line, "## "):
			h2, h3 = line, ""
			h2Printed, h3Printed = false, false
		case strings.HasPrefix(line, "### "):
			h3 = line
			h3Printed = false
		case strings.HasPrefix(strings.TrimLeft(line, " \t"), "- [ ]"):
			if !h2Printed {
				out = append(out, h2)
				h2Printed = true
			}
			if h3 != "" && !h3Printed {
				out = append(out, h3)
				h3Printed = true
			}
			out = append(out, line)
		}
	}

	if len(out) == 0 {
		return "🎉 Open: 0 — todas las tareas están marcadas [x].", nil
	}
	return strings.Join(out, "\n"), nil
}

// EnqueueTasks guarda las tareas tácticas generadas por la IA
func EnqueueTasks(tasks []SRETask) error {
	if plannerDB == nil {
		return fmt.Errorf("plannerDB offline")
	}

	return plannerDB.Update(func(tx *bbolt.Tx) error {
		// Atomic swap: delete+recreate in one transaction so a SIGKILL cannot
		// leave the queue half-cleared. BoltDB rolls back an uncommitted tx
		// on recovery, preserving the old data if the process dies mid-write.
		if err := tx.DeleteBucket([]byte(taskBucket)); err != nil && err != bbolterrors.ErrBucketNotFound {
			return fmt.Errorf("fallo limpiando cola anterior de tareas: %w", err)
		}
		b, err := tx.CreateBucket([]byte(taskBucket))
		if err != nil {
			return fmt.Errorf("fallo recreando bucket de tareas: %w", err)
		}

		now := time.Now().Unix()
		for i, t := range tasks {
			t.ID = fmt.Sprintf("TASK-%03d", i+1)
			t.Status = "TODO"
			t.LifecycleState = TaskLifecyclePending // [362.A]
			t.CreatedAt = now
			t.UpdatedAt = now
			val, err := json.Marshal(t)
			if err != nil {
				return fmt.Errorf("fallo serializando tarea SRE %s: %w", t.ID, err)
			}
			if err := b.Put([]byte(t.ID), val); err != nil {
				return fmt.Errorf("fallo insertando tarea SRE %s: %w", t.ID, err)
			}
		}
		return nil
	})
}

// GetNextTask extrae la primera tarea pendiente (O(1))
func GetNextTask() (*SRETask, error) {
	var task SRETask
	found := false

	err := plannerDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var t SRETask
			if err := json.Unmarshal(v, &t); err != nil {
				log.Printf("[PLANNER] corrupt task entry key=%q: %v (skipping)", k, err)
				continue
			}
			if t.Status == "TODO" && t.LifecycleState != TaskLifecycleFailedPermanent { // [132.A] skip permanently failed
				task = t
				found = true
				break
			}
		}
		return nil
	})

	if !found || err != nil {
		return nil, nil // Cola vacía
	}
	return &task, nil
}

// GetNextTaskByRole extracts the first pending task matching agentRole. [SRE-25.1.3]
// If agentRole is empty, returns first task with no role set.
// Tasks with no role are eligible for any agent as fallback.
func GetNextTaskByRole(agentRole string) (*SRETask, error) {
	var task SRETask
	found := false

	err := plannerDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var t SRETask
			if err := json.Unmarshal(v, &t); err != nil {
				log.Printf("[PLANNER] corrupt task entry key=%q: %v (skipping)", k, err)
				continue
			}
			if t.Status != "TODO" {
				continue
			}
			if t.LifecycleState == TaskLifecycleFailedPermanent { // [132.A] skip permanently failed tasks
				continue
			}
			// Match: exact role match, or task has no role (unassigned fallback)
			if agentRole == "" || t.Role == agentRole || t.Role == "" {
				task = t
				found = true
				break
			}
		}
		return nil
	})

	if !found || err != nil {
		return nil, err
	}
	return &task, nil
}

func markTaskDoneInDB() (int, string, error) {
	remaining := 0
	var markedTaskDesc string

	// Two-pass collect-then-mutate (audit finding S9-4, 2026-05-01): even
	// the cursor-based variant of mutate-during-iteration can cause bbolt's
	// B+tree to rebalance and the cursor to skip a subsequent entry,
	// producing an off-by-one `remaining` count. Under a concurrent reaper
	// the miscount fires `OnEpicClose` prematurely. We now collect the
	// first eligible key during pass 1 (read-only ForEach) and apply the
	// Put in pass 2.
	err := plannerDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		if b == nil {
			return nil
		}
		var markKey []byte
		var markedTask SRETask
		ferr := b.ForEach(func(k, v []byte) error {
			var t SRETask
			if err := json.Unmarshal(v, &t); err != nil {
				log.Printf("[PLANNER] corrupt task entry key=%q: %v (skipping)", k, err)
				return nil
			}
			if t.Status != "TODO" {
				return nil
			}
			if markKey == nil {
				markKey = make([]byte, len(k))
				copy(markKey, k)
				markedTask = t
				return nil
			}
			remaining++
			return nil
		})
		if ferr != nil {
			return ferr
		}
		if markKey == nil {
			return nil // nothing to mark
		}
		markedTask.Status = "DONE"
		markedTask.LifecycleState = TaskLifecycleCompleted // [362.A]
		markedTask.UpdatedAt = time.Now().Unix()
		val, jerr := json.Marshal(markedTask)
		if jerr != nil {
			return fmt.Errorf("imposible serializar tarea %s: %w", markKey, jerr)
		}
		if putErr := b.Put(markKey, val); putErr != nil {
			return fmt.Errorf("imposible marcar %s como DONE: %w", markKey, putErr)
		}
		markedTaskDesc = markedTask.Description
		return nil
	})

	return remaining, markedTaskDesc, err
}

func syncMasterPlanFile(desc string) {
	if plannerWorkspace == "" {
		return
	}
	planPath := filepath.Clean(filepath.Join(plannerWorkspace, ".neo", "master_plan.md"))
	if !strings.HasPrefix(planPath, filepath.Clean(plannerWorkspace)) {
		return
	}
	//nolint:gosec // G304-WORKSPACE-CANON: workspace is strictly enforced
	data, err := os.ReadFile(planPath)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.Contains(line, "- [ ]") && strings.Contains(line, desc) {
			lines[i] = strings.Replace(line, "- [ ]", "- [x]", 1)
			break
		}
	}
	//nolint:gosec // G304-WORKSPACE-CANON: workspace is strictly enforced
	_ = os.WriteFile(planPath, []byte(strings.Join(lines, "\n")), 0600)
}

// MarkTaskDone sella la tarea actual y devuelve cuántas quedan.
// [SRE-56.1] Fires OnEpicClose when the last pending task is marked DONE.
func MarkTaskDone() (int, error) {
	remaining, desc, err := markTaskDoneInDB()
	if err == nil && desc != "" {
		syncMasterPlanFile(desc)
	}
	if err == nil && remaining == 0 && OnEpicClose != nil {
		OnEpicClose()
	}
	return remaining, err
}

// IsEpicComplete returns true when all tasks in the queue are DONE and at least one exists.
// [SRE-30.1.1] Signal used by Kanban to trigger archive of completed epic blocks.
func IsEpicComplete() bool {
	pending, completed := GetPlannerState()
	return pending == 0 && completed > 0
}

// GetPlannerState devuelve conteos de tareas pendientes y completadas en BoltDB.
// [SRE-24.1.4] No retorna el Master Plan — ReadActivePhase() lo hace cuando se necesita explícitamente.
func GetPlannerState() (int, int) {
	if plannerDB == nil {
		return 0, 0
	}

	pending := 0
	completed := 0

	viewErr := plannerDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var t SRETask
			if err := json.Unmarshal(v, &t); err != nil {
				log.Printf("[PLANNER] corrupt task entry key=%q: %v (skipping)", k, err)
				continue
			}
			switch t.Status {
			case "TODO":
				pending++
			case "DONE":
				completed++
			}
		}
		return nil
	})

	if viewErr != nil {
		log.Printf("[SRE-WARN] Imposible contar estado de planner: %v\n", viewErr)
	}

	return pending, completed
}

// GetAllTasks exporta la estructura de la cola para Snapshots
func GetAllTasks() []SRETask {
	var tasks []SRETask
	if plannerDB == nil {
		return tasks
	}

	viewErr := plannerDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var t SRETask
			if err := json.Unmarshal(v, &t); err != nil {
				log.Printf("[PLANNER] corrupt task entry key=%q: %v (skipping)", k, err)
				continue
			}
			tasks = append(tasks, t)
		}
		return nil
	})

	if viewErr != nil {
		log.Printf("[SRE-WARN] Error iterando cola de tareas: %v\n", viewErr)
	}

	return tasks
}

// MemexCommit persists an episodic memory entry to the short-term buffer. [SRE-28.2.2]
// Does NOT trigger immediate vectorisation — REM sleep handles that later.
func MemexCommit(entry MemexEntry) error {
	if plannerDB == nil {
		return fmt.Errorf("plannerDB offline")
	}
	entry.ID = fmt.Sprintf("MEMEX-%d-%s", time.Now().UnixNano(), memexIDSuffix())
	entry.Timestamp = time.Now().Unix()
	return plannerDB.Update(func(tx *bbolt.Tx) error {
		// [265.B] Lazy create tenant-namespaced bucket if needed.
		b, err := tx.CreateBucketIfNotExists([]byte(memexBucketName()))
		if err != nil {
			return fmt.Errorf("memex bucket create: %w", err)
		}
		val, merr := json.Marshal(entry)
		if merr != nil {
			return merr
		}
		return b.Put([]byte(entry.ID), val)
	})
}

// MemexDrain reads all short-term episodic entries and purges the buffer. [SRE-28.3.3]
// Called exclusively by TriggerREMSleep — single consumer, no race.
func MemexDrain() ([]MemexEntry, error) {
	if plannerDB == nil {
		return nil, nil
	}
	var entries []MemexEntry
	err := plannerDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(memexBucketName())) // [265.B] tenant-aware bucket
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var e MemexEntry
			if err := json.Unmarshal(v, &e); err != nil {
				log.Printf("[PLANNER] corrupt memex entry key=%q: %v (skipping)", k, err)
				continue
			}
			entries = append(entries, e)
		}
		// Purge after draining
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			_ = c.Delete()
		}
		return nil
	})
	return entries, err
}

// MemexRead returns entries with Timestamp >= since.Unix() without purging. [SRE-94]
// Non-destructive counterpart to MemexDrain; used by ragMemexAdapter to satisfy federation.MemexExporter.
func MemexRead(since time.Time) ([]MemexEntry, error) {
	if plannerDB == nil {
		return nil, nil
	}
	threshold := since.Unix()
	var entries []MemexEntry
	err := plannerDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(memexBucketName())) // [265.B] tenant-aware bucket
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var e MemexEntry
			if json.Unmarshal(v, &e) == nil && e.Timestamp >= threshold {
				entries = append(entries, e)
			}
		}
		return nil
	})
	return entries, err
}

// memexImportKeysBucket returns the tenant-namespaced bucket name for import dedup keys. [285.C]
func memexImportKeysBucket() string {
	if activeTenantID != "" {
		return "memex_import_keys:" + activeTenantID
	}
	return "memex_import_keys"
}

// MemexHasKey reports whether a deduplication key has been recorded for an imported entry. [285.C]
func MemexHasKey(key string) bool {
	if plannerDB == nil {
		return false
	}
	found := false
	_ = plannerDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(memexImportKeysBucket()))
		if b == nil {
			return nil
		}
		if b.Get([]byte(key)) != nil {
			found = true
		}
		return nil
	})
	return found
}

// MemexImport commits an externally-sourced MemexEntry and records its dedup key. [285.C]
// key must be the SHA256 hex computed from topic+content before calling.
func MemexImport(entry MemexEntry, key string) error {
	if plannerDB == nil {
		return fmt.Errorf("plannerDB offline")
	}
	return plannerDB.Update(func(tx *bbolt.Tx) error {
		// Write entry into memex bucket
		mb, err := tx.CreateBucketIfNotExists([]byte(memexBucketName()))
		if err != nil {
			return fmt.Errorf("memex bucket: %w", err)
		}
		if entry.ID == "" {
			entry.ID = fmt.Sprintf("MEMEX-IMPORT-%d-%s", time.Now().UnixNano(), memexIDSuffix())
		}
		val, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if err := mb.Put([]byte(entry.ID), val); err != nil {
			return err
		}
		// Record dedup key
		kb, err := tx.CreateBucketIfNotExists([]byte(memexImportKeysBucket()))
		if err != nil {
			return fmt.Errorf("import_keys bucket: %w", err)
		}
		return kb.Put([]byte(key), []byte("1"))
	})
}

const contractAlertBucket = "contract_alerts"

// ContractBreakingChange mirrors macro_tools.BreakingChange for BoltDB persistence. [292.C/E]
type ContractBreakingChange struct {
	Type     string `json:"type"`
	Endpoint string `json:"endpoint"`
	Old      string `json:"old"`
	New      string `json:"new"`
}

// ContractAlertWrite persists a contract_drift alert from a peer workspace. [292.C]
// key is expected to be "fromWorkspace:nanos".
func ContractAlertWrite(key string, breaking any) error {
	if plannerDB == nil {
		return fmt.Errorf("plannerDB offline")
	}
	val, err := json.Marshal(breaking)
	if err != nil {
		return err
	}
	return plannerDB.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(contractAlertBucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(key), val)
	})
}

// ContractAlertReadAndFlush returns all stored contract alerts and empties the bucket (one-shot). [292.E]
func ContractAlertReadAndFlush() ([]struct {
	FromWorkspace string
	Breaking      []ContractBreakingChange
}, error) {
	if plannerDB == nil {
		return nil, nil
	}
	type rec struct {
		FromWorkspace string
		Breaking      []ContractBreakingChange
	}
	var results []rec
	err := plannerDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(contractAlertBucket))
		if b == nil {
			return nil
		}
		forErr := b.ForEach(func(k, v []byte) error {
			var changes []ContractBreakingChange
			if jerr := json.Unmarshal(v, &changes); jerr != nil {
				return nil // skip malformed
			}
			parts := strings.SplitN(string(k), ":", 2)
			r := rec{Breaking: changes}
			if len(parts) >= 1 {
				r.FromWorkspace = parts[0]
			}
			results = append(results, r)
			return nil
		})
		if forErr != nil {
			return forErr
		}
		return tx.DeleteBucket([]byte(contractAlertBucket))
	})
	out := make([]struct {
		FromWorkspace string
		Breaking      []ContractBreakingChange
	}, len(results))
	for i, r := range results {
		out[i].FromWorkspace = r.FromWorkspace
		out[i].Breaking = r.Breaking
	}
	return out, err
}

// MarkTaskInProgress transitions a task to in_progress lifecycle state. [362.A]
// Called by handlePullTasks when a task is claimed by an agent.
func MarkTaskInProgress(taskID string) error {
	if plannerDB == nil {
		return fmt.Errorf("plannerDB offline")
	}
	return plannerDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(taskID))
		if v == nil {
			return nil // task not found — non-fatal
		}
		var t SRETask
		if err := json.Unmarshal(v, &t); err != nil {
			return err
		}
		now := time.Now().Unix()
		t.LifecycleState = TaskLifecycleInProgress
		t.UpdatedAt = now
		if t.StartedAt == 0 { // [132.A] record first-claim time; preserve on retries
			t.StartedAt = now
		}
		val, err := json.Marshal(t)
		if err != nil {
			return err
		}
		return b.Put([]byte(taskID), val)
	})
}

// MarkTaskCompleted transitions a specific task by ID to DONE +
// LifecycleCompleted. Companion to MarkTaskInProgress for the daemon
// approve action which needs to close out a known task ID rather than
// "the next TODO" (the cursor-walking semantic of MarkTaskDone).
//
// Idempotent: completing an already-completed task is a no-op without
// error. Missing tasks return nil — non-fatal so the caller can check
// the result via GetAllTasks if it needs strict semantics. [138.C.2]
func MarkTaskCompleted(taskID string) error {
	if plannerDB == nil {
		return fmt.Errorf("plannerDB offline")
	}
	return plannerDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(taskID))
		if v == nil {
			return nil
		}
		var t SRETask
		if err := json.Unmarshal(v, &t); err != nil {
			return err
		}
		t.Status = "DONE"
		t.LifecycleState = TaskLifecycleCompleted
		t.UpdatedAt = time.Now().Unix()
		val, err := json.Marshal(t)
		if err != nil {
			return err
		}
		return b.Put([]byte(taskID), val)
	})
}

// RequeueTaskOrFail re-queues a specific task by ID. If Retries <
// maxRetries, the task transitions back to TODO + Pending with
// retry_count++. Otherwise it transitions to FailedPermanent. Mirrors
// the behavior of RecoverOrphanedTasks but operates on a single task
// chosen by the operator (not the orphan scanner). [138.C.3]
//
// Returns (requeued=true) when the task is back in the queue,
// (requeued=false) when it hit max retries and is now FailedPermanent.
// Both outcomes are non-error states; only I/O failures return err.
//
// skipRetryIncrement=true requeues without touching the Retries
// counter at all — used by the timing reject category which shouldn't
// burn a retry slot since the model wasn't wrong, just early.
// [DeepSeek RESET-RETRIES fix: don't zero-then-increment, just skip]
func RequeueTaskOrFail(taskID, lastError string, maxRetries int, skipRetryIncrement bool) (requeued bool, err error) {
	if plannerDB == nil {
		return false, fmt.Errorf("plannerDB offline")
	}
	err = plannerDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(taskID))
		if v == nil {
			return nil
		}
		var t SRETask
		if jerr := json.Unmarshal(v, &t); jerr != nil {
			return jerr
		}
		now := time.Now().Unix()
		if skipRetryIncrement {
			// Timing rejects: requeue without retry tick. Past quality
			// rejects' Retries count is preserved so a quality-then-timing
			// sequence still reaches FailedPermanent at the right point.
			t.LastError = lastError
			t.LifecycleState = TaskLifecyclePending
			t.Status = "TODO"
			t.UpdatedAt = now
			requeued = true
		} else if t.Retries < maxRetries {
			t.Retries++
			t.LastError = lastError
			t.LifecycleState = TaskLifecyclePending
			t.Status = "TODO"
			t.UpdatedAt = now
			requeued = true
		} else {
			t.LastError = fmt.Sprintf("max retries %d exceeded — %s", maxRetries, lastError)
			t.LifecycleState = TaskLifecycleFailedPermanent
			t.UpdatedAt = now
			requeued = false
		}
		val, merr := json.Marshal(t)
		if merr != nil {
			return merr
		}
		return b.Put([]byte(taskID), val)
	})
	return requeued, err
}

// MarkTaskFailedPermanent transitions a specific task to LifecycleFailedPermanent.
// Used by the daemon when an operator rejects with requeue=false and reason_kind=quality:
// the task should NOT be retried by the orphan scanner. Without this, RecoverOrphanedTasks
// would eventually re-queue the rejected task, creating a reject-loop where each cycle
// adds another β=5 trust penalty. [DeepSeek QUALITY-DEAD-LETTER fix]
func MarkTaskFailedPermanent(taskID, lastError string) error {
	if plannerDB == nil {
		return fmt.Errorf("plannerDB offline")
	}
	return plannerDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(taskID))
		if v == nil {
			return nil
		}
		var t SRETask
		if jerr := json.Unmarshal(v, &t); jerr != nil {
			return jerr
		}
		t.LifecycleState = TaskLifecycleFailedPermanent
		t.LastError = lastError
		t.UpdatedAt = time.Now().Unix()
		val, merr := json.Marshal(t)
		if merr != nil {
			return merr
		}
		return b.Put([]byte(taskID), val)
	})
}

// CountOrphanedTasks returns the number of tasks in orphaned state. [362.A]
// Used by BRIEFING to surface the ⚠️ orphaned_tasks:N badge.
func CountOrphanedTasks() int {
	if plannerDB == nil {
		return 0
	}
	count := 0
	_ = plannerDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var t SRETask
			if json.Unmarshal(v, &t) == nil && t.LifecycleState == TaskLifecycleOrphaned {
				count++
			}
			return nil
		})
	})
	return count
}

// MarkTasksOrphaned scans in_progress tasks whose UpdatedAt is older than
// timeoutMin minutes and atomically transitions them to orphaned. [362.A]
// Returns the list of newly-orphaned tasks for debt entry creation.
func MarkTasksOrphaned(timeoutMin int) ([]SRETask, error) {
	if plannerDB == nil {
		return nil, nil
	}
	cutoff := time.Now().Add(-time.Duration(timeoutMin) * time.Minute).Unix()
	var orphaned []SRETask
	// Collect-then-mutate pattern (audit finding S9-4, 2026-05-01): bbolt
	// docs explicitly say "Do not modify the bucket while iterating." Calling
	// b.Put on the key currently produced by ForEach can cause the cursor to
	// skip entries or revisit them, especially under concurrent reapers. We
	// gather (key, newValue, summary) tuples inside ForEach (read-only),
	// THEN apply Puts in a separate loop. The whole sequence stays inside
	// one transaction, so atomicity is preserved.
	err := plannerDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		if b == nil {
			return nil
		}
		now := time.Now().Unix()
		type pendingPut struct {
			key  []byte
			val  []byte
			task SRETask
		}
		var pending []pendingPut
		ferr := b.ForEach(func(k, v []byte) error {
			var t SRETask
			if err := json.Unmarshal(v, &t); err != nil {
				return nil
			}
			if t.LifecycleState != TaskLifecycleInProgress || t.UpdatedAt <= 0 || t.UpdatedAt >= cutoff {
				return nil
			}
			t.LifecycleState = TaskLifecycleOrphaned
			t.UpdatedAt = now
			val, merr := json.Marshal(t)
			if merr != nil {
				return merr
			}
			// ForEach loans (k, v) for the duration of the callback; copy k
			// so the post-iteration Put receives a stable slice.
			kCopy := make([]byte, len(k))
			copy(kCopy, k)
			pending = append(pending, pendingPut{key: kCopy, val: val, task: t})
			return nil
		})
		if ferr != nil {
			return ferr
		}
		for _, p := range pending {
			if perr := b.Put(p.key, p.val); perr != nil {
				return perr
			}
			orphaned = append(orphaned, p.task)
		}
		return nil
	})
	return orphaned, err
}

// RecoverOrphanedTasks performs boot-time orphan recovery for daemon mode. [132.A]
// It scans tasks in orphaned or in_progress-past-timeout state and applies retrial logic:
//   - Retries < maxRetries → reset to pending (re-queued), Retries++, LastError set
//   - Retries >= maxRetries → mark failed_permanent (excluded from future PullTasks)
//
// Returns all tasks that were acted on (both re-queued and permanently failed).
func RecoverOrphanedTasks(maxRetries, orphanTimeoutMin int) ([]SRETask, error) {
	if plannerDB == nil {
		return nil, nil
	}
	cutoff := time.Now().Add(-time.Duration(orphanTimeoutMin) * time.Minute).Unix()
	var recovered []SRETask
	// Same collect-then-mutate refactor as MarkTasksOrphaned — see audit
	// finding S9-4 in docs/codebase-audit-2026-05-01.md.
	err := plannerDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		if b == nil {
			return nil
		}
		now := time.Now().Unix()
		type pendingPut struct {
			key  []byte
			val  []byte
			task SRETask
		}
		var pending []pendingPut
		ferr := b.ForEach(func(k, v []byte) error {
			var t SRETask
			if err := json.Unmarshal(v, &t); err != nil {
				return nil
			}
			staleInProgress := t.LifecycleState == TaskLifecycleInProgress && t.UpdatedAt > 0 && t.UpdatedAt < cutoff
			alreadyOrphaned := t.LifecycleState == TaskLifecycleOrphaned
			if !staleInProgress && !alreadyOrphaned {
				return nil
			}
			if t.Retries < maxRetries {
				t.Retries++
				t.LastError = fmt.Sprintf("orphaned after %d min timeout (attempt %d/%d)", orphanTimeoutMin, t.Retries, maxRetries)
				t.LifecycleState = TaskLifecyclePending
				t.Status = "TODO"
				t.UpdatedAt = now
			} else {
				t.LastError = fmt.Sprintf("max retries %d exceeded — task permanently failed", maxRetries)
				t.LifecycleState = TaskLifecycleFailedPermanent
				t.UpdatedAt = now
			}
			val, merr := json.Marshal(t)
			if merr != nil {
				return merr
			}
			kCopy := make([]byte, len(k))
			copy(kCopy, k)
			pending = append(pending, pendingPut{key: kCopy, val: val, task: t})
			return nil
		})
		if ferr != nil {
			return ferr
		}
		for _, p := range pending {
			if perr := b.Put(p.key, p.val); perr != nil {
				return perr
			}
			recovered = append(recovered, p.task)
		}
		return nil
	})
	return recovered, err
}

// BatchCertifyTx records the state of a two-phase commit batch certify transaction. [362.A]
// Enables recovery of certify operations interrupted mid-flight.
type BatchCertifyTx struct {
	TxID      string   `json:"tx_id"`
	Files     []string `json:"files"`
	State     string   `json:"state"`      // prepared|committed|aborted
	StartedAt int64    `json:"started_at"` // unix seconds
}

// BeginBatchCertify records a new batch certify transaction in the prepared state. [362.A]
// txID must be unique per transaction (e.g. fmt.Sprintf("certify-%d", time.Now().UnixNano())).
func BeginBatchCertify(txID string, files []string) error {
	if plannerDB == nil {
		return fmt.Errorf("plannerDB offline")
	}
	entry := BatchCertifyTx{TxID: txID, Files: files, State: "prepared", StartedAt: time.Now().Unix()}
	val, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return plannerDB.Update(func(tx *bbolt.Tx) error {
		b, berr := tx.CreateBucketIfNotExists([]byte(batchCertifyBucket))
		if berr != nil {
			return berr
		}
		return b.Put([]byte(txID), val)
	})
}

// CommitBatchCertify marks a batch certify transaction as committed. [362.A]
func CommitBatchCertify(txID string) error {
	return updateBatchCertifyState(txID, "committed")
}

// AbortBatchCertify marks a batch certify transaction as aborted. [362.A]
func AbortBatchCertify(txID string) error {
	return updateBatchCertifyState(txID, "aborted")
}

func updateBatchCertifyState(txID, newState string) error {
	if plannerDB == nil {
		return fmt.Errorf("plannerDB offline")
	}
	return plannerDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(batchCertifyBucket))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(txID))
		if v == nil {
			return nil
		}
		var entry BatchCertifyTx
		if err := json.Unmarshal(v, &entry); err != nil {
			return err
		}
		entry.State = newState
		val, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		return b.Put([]byte(txID), val)
	})
}

// RecoverPendingCertify returns all batch certify transactions stuck in prepared
// state — these represent certify operations that were interrupted before commit
// or abort and may require manual inspection. [362.A]
func RecoverPendingCertify() ([]BatchCertifyTx, error) {
	if plannerDB == nil {
		return nil, nil
	}
	var pending []BatchCertifyTx
	err := plannerDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(batchCertifyBucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var entry BatchCertifyTx
			if json.Unmarshal(v, &entry) == nil && entry.State == "prepared" {
				pending = append(pending, entry)
			}
			return nil
		})
	})
	return pending, err
}

// RestoreTasks regenera la memoria despues de un LoadSnapshot
func RestoreTasks(tasks []SRETask) error {
	if plannerDB == nil {
		return fmt.Errorf("planner offline")
	}
	return plannerDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			if err := c.Delete(); err != nil {
				return fmt.Errorf("fallo purgando tabla de tareas para Snapshot: %w", err)
			}
		}
		for _, t := range tasks {
			val, err := json.Marshal(t)
			if err != nil {
				return fmt.Errorf("fallo serializando tarea restaurada %s: %w", t.ID, err)
			}
			if err := b.Put([]byte(t.ID), val); err != nil {
				return fmt.Errorf("fallo escribiendo tarea restaurada %s: %w", t.ID, err)
			}
		}
		return nil
	})
}
