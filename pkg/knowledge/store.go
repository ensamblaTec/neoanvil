// pkg/knowledge/store.go — Project Knowledge Store. [PILAR XXXIX / Épica 293]
//
// KnowledgeStore is a cross-workspace key-value store backed by BoltDB in
// .neo-project/db/knowledge.db. It stores declarative project knowledge
// (DTOs, API contracts, enums, rules, flows) that must be shared instantly
// between all member workspaces of a project.
//
// Advisory write-lock via syscall.Flock so multiple neo-mcp processes can
// safely co-exist on the same file. Readers never lock.
package knowledge

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"go.etcd.io/bbolt"
)

// ErrNotFound is returned by Get when the entry does not exist.
var ErrNotFound = errors.New("knowledge: entry not found")

// Predefined namespaces — any string is valid; these are conventions.
const (
	NSContracts  = "contracts"
	NSTypes      = "types"
	NSEnums      = "enums"
	NSRules      = "rules"
	NSFlows      = "flows"
	NSPatterns   = "patterns"
	NSDecisions  = "decisions" // [330.J] ADRs — architecture decisions
	NSIncidents  = "incidents" // [330.J] post-mortems compartidos
	NSDebt       = "debt"      // [330.J] cross-workspace tech debt (syncs with SHARED_DEBT.md)
	NSShared     = "shared"     // [330.J] general cross-workspace config
	NSInbox      = "inbox"      // [331.A] agent-to-agent messaging: key = to-<ws-id>-<topic>
	NSEpics      = "epics"      // [332.A] cross-workspace shared epics: key = <PILAR>-<ID> (e.g. LX-331.A)
	NSConflicts  = "conflicts"  // [342.A] LWW conflict log — entries overwritten within 10s by another workspace
)

// ReservedNamespaces lists namespaces created with .gitkeep at boot so they
// appear empty-but-present in the project knowledge/ tree. Prevents agents
// from concluding "namespace X doesn't exist" when it's just empty. [330.J/331.A]
func ReservedNamespaces() []string {
	return []string{
		NSContracts, NSTypes, NSEnums, NSRules, NSFlows, NSPatterns,
		NSDecisions, NSIncidents, NSDebt, NSShared, NSInbox, NSEpics,
	}
}

// Inbox entry constants — priority levels + quota. [331.A]
const (
	InboxPriorityLow    = "low"
	InboxPriorityNormal = "normal"
	InboxPriorityUrgent = "urgent"

	// InboxKeyPrefix guards validation: inbox keys MUST match to-<ws-id>-<topic>.
	InboxKeyPrefix = "to-"

	// InboxQuotaPerSenderPerHour is the default rate limit. Senders exceeding
	// this cap hit ErrInboxQuotaExceeded. Override via config.
	InboxQuotaPerSenderPerHour = 30
)

// Inbox error sentinels.
var (
	ErrInboxInvalidKey      = errors.New("inbox: key must match 'to-<workspace-id>-<topic>'")
	ErrInboxMissingFrom     = errors.New("inbox: From field is required (sender workspace ID)")
	ErrInboxQuotaExceeded   = errors.New("inbox: quota exceeded — too many messages from this sender in the last hour")
	ErrInboxInvalidPriority = errors.New("inbox: priority must be one of low|normal|urgent")
)

// Epic status and priority constants. [332.A]
const (
	EpicStatusOpen       = "open"
	EpicStatusInProgress = "in_progress"
	EpicStatusDone       = "done"
	EpicStatusBlocked    = "blocked"

	EpicPriorityP0 = "P0"
	EpicPriorityP1 = "P1"
	EpicPriorityP2 = "P2"
	EpicPriorityP3 = "P3"
)

// Epic error sentinels. [332.A]
var (
	ErrEpicInvalidID       = errors.New("epics: key must follow '<PILAR>-<id>' format (e.g. 'LX-331.A')")
	ErrEpicInvalidStatus   = errors.New("epics: status must be one of open|in_progress|done|blocked")
	ErrEpicInvalidPriority = errors.New("epics: priority must be one of P0|P1|P2|P3")
)

// KnowledgeEntry is one record in the knowledge store.
// Inbox-specific fields (From/ReadAt/Priority/ThreadID) are optional — only
// populated for namespace=inbox entries. [331.A]
type KnowledgeEntry struct {
	Key       string   `json:"key"`
	Namespace string   `json:"namespace"`
	Content   string   `json:"content"`
	Tags      []string `json:"tags,omitempty"`
	Hot       bool     `json:"hot"`
	CreatedAt int64    `json:"created_at"`
	UpdatedAt int64    `json:"updated_at"`

	// [331.A] Inbox-only fields. Omitempty keeps the on-disk schema compact
	// for non-inbox namespaces.
	From     string `json:"from,omitempty"`      // sender workspace ID
	ReadAt   int64  `json:"read_at,omitempty"`   // unix seconds when receiver called fetch; 0 = unread
	Priority string `json:"priority,omitempty"`  // low | normal | urgent (inbox) or P0-P3 (epics)
	ThreadID string `json:"thread_id,omitempty"` // optional conversation thread

	// [332.A] Epics-only fields. Omitempty for non-epics namespaces.
	EpicTitle    string   `json:"epic_title,omitempty"`     // human-readable name
	EpicStatus   string   `json:"epic_status,omitempty"`    // open | in_progress | done | blocked
	EpicOwner    string   `json:"epic_owner,omitempty"`     // workspace ID driving this epic
	EpicAffected []string `json:"epic_affected,omitempty"`  // workspace IDs impacted
	EpicBlocks   []string `json:"epic_blocks,omitempty"`    // epic IDs this one blocks
	EpicCreatedBy string  `json:"epic_created_by,omitempty"` // attribution workspace ID

	// [336.A] Identity of the agent session that last wrote this entry.
	// Format: "<workspace-id>:<boot-unix>:<client-name@version>"
	SessionAgentID string `json:"session_agent_id,omitempty"`
}

// KnowledgeStore wraps BoltDB for the project knowledge base. [293.A/B]
type KnowledgeStore struct {
	db       *bbolt.DB
	path     string
	lockPath string
	lockFD   *os.File
	locked   bool
	syncDir  string // [297.C] when set, Put/Delete mirror to .md files in this dir
}

// SetSyncDir enables dual-layer sync: Put writes a companion .md file under dir,
// Delete removes it. Call after Open when .neo-project/knowledge/ exists. [297.C]
func (ks *KnowledgeStore) SetSyncDir(dir string) { ks.syncDir = dir }

// ErrLockBusy is returned by Open when the advisory lock is held by another
// process after all retry attempts are exhausted. [268.A]
var ErrLockBusy = errors.New("knowledge: lock busy")

// Open opens (or creates) the KnowledgeStore at path. [293.B]
// Acquires an exclusive advisory lock on path+".lock" to serialize writers
// across concurrent neo-mcp processes (readers share without locking).
// Retries the flock with exponential backoff (50ms→200ms→500ms) to handle
// simultaneous multi-workspace boots without hard failures. [268.A]
func Open(path string) (*KnowledgeStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("knowledge: mkdir: %w", err)
	}
	lockPath := path + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304-DIR-WALK: lockPath derived from controlled path arg
	if err != nil {
		return nil, fmt.Errorf("knowledge: open lock: %w", err)
	}

	// [268.A / 302.B] Retry flock with exponential backoff+jitter — handles the
	// race when multiple neo-mcp children boot simultaneously sharing the same
	// project DB. 5 attempts: 50→100→200→400→800ms base, ±20% jitter each.
	const maxAttempts = 5
	baseDelays := [maxAttempts]time.Duration{
		50 * time.Millisecond,
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
	}
	var lockErr error
	for i := range maxAttempts {
		lockErr = syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lockErr == nil {
			break
		}
		if i < maxAttempts-1 {
			base := baseDelays[i]
			jitter := time.Duration(rand.Int63n(int64(base) / 5)) //nolint:gosec // G404: non-crypto jitter for retry backoff
			delay := base + jitter
			log.Printf("[knowledge] lock busy (attempt %d/%d) — retrying in %dms", i+1, maxAttempts, delay.Milliseconds())
			time.Sleep(delay)
		}
	}
	if lockErr != nil {
		lf.Close()
		return nil, fmt.Errorf("%w: %s", ErrLockBusy, lockErr)
	}

	db, err := bbolt.Open(path, 0o600, bbolt.DefaultOptions) //nolint:gosec // G304-DIR-WALK: path under process control
	if err != nil {
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		lf.Close()
		return nil, fmt.Errorf("knowledge: open db: %w", err)
	}
	return &KnowledgeStore{
		db:       db,
		path:     path,
		lockPath: lockPath,
		lockFD:   lf,
		locked:   true,
	}, nil
}

// Close releases the advisory lock and closes the database. [293.D]
func (ks *KnowledgeStore) Close() error {
	if ks == nil {
		return nil
	}
	dbErr := ks.db.Close()
	if ks.locked && ks.lockFD != nil {
		_ = syscall.Flock(int(ks.lockFD.Fd()), syscall.LOCK_UN)
		_ = ks.lockFD.Close()
		ks.locked = false
	}
	return dbErr
}

// bucketKey returns the BoltDB bucket name for a given namespace. [293.B]
func bucketKey(ns string) []byte {
	return []byte("knowledge:" + ns)
}

// ValidateInboxKey enforces the `to-<workspace-id>-<topic>` format for inbox
// namespace keys. Returns ErrInboxInvalidKey on mismatch. [331.A]
//
// Examples:
//   valid:   to-strategos-32492-api-v2-breaking-change
//   invalid: strategos-32492/foo   (missing 'to-' prefix)
//   invalid: to--api-v2            (empty workspace id)
//   invalid: to-strategos          (missing topic)
func ValidateInboxKey(key string) error {
	if !strings.HasPrefix(key, InboxKeyPrefix) {
		return ErrInboxInvalidKey
	}
	// Strip `to-` then require at least one more `-` separating ws-id and topic.
	rest := key[len(InboxKeyPrefix):]
	if rest == "" {
		return ErrInboxInvalidKey
	}
	sep := strings.Index(rest, "-")
	if sep <= 0 || sep == len(rest)-1 {
		// No separator, empty ws-id, or empty topic.
		return ErrInboxInvalidKey
	}
	return nil
}

// ValidateInboxPriority accepts empty (defaults to normal) or one of the
// three defined priority levels. [331.A]
func ValidateInboxPriority(p string) error {
	switch p {
	case "", InboxPriorityLow, InboxPriorityNormal, InboxPriorityUrgent:
		return nil
	default:
		return ErrInboxInvalidPriority
	}
}

// ValidateEpicID enforces the `<PILAR>-<id>` key format for epics namespace
// (e.g. "LX-331.A", "LVII-328.C"). Both parts must be non-empty. [332.A]
func ValidateEpicID(key string) error {
	sep := strings.Index(key, "-")
	if sep <= 0 || sep == len(key)-1 {
		return ErrEpicInvalidID
	}
	return nil
}

// ValidateEpicStatus accepts empty (defaults to open) or one of the four
// defined status values. [332.A]
func ValidateEpicStatus(s string) error {
	switch s {
	case "", EpicStatusOpen, EpicStatusInProgress, EpicStatusDone, EpicStatusBlocked:
		return nil
	default:
		return ErrEpicInvalidStatus
	}
}

// ValidateEpicPriority accepts empty (unset) or one of P0–P3. [332.A]
func ValidateEpicPriority(p string) error {
	switch p {
	case "", EpicPriorityP0, EpicPriorityP1, EpicPriorityP2, EpicPriorityP3:
		return nil
	default:
		return ErrEpicInvalidPriority
	}
}

// PutInbox is the preferred high-level API for writing inbox messages.
// Validates key + priority + enforces per-sender quota, then delegates to Put.
// [331.A]
//
//   from:     sender workspace ID (e.g. "strategos-32492")
//   key:      must match `to-<target-ws-id>-<topic>`
//   content:  message body
//   priority: "" | "low" | "normal" | "urgent" (empty → normal)
//   quotaPerHour: max messages from this sender in the rolling last hour.
//                 0 → use default InboxQuotaPerSenderPerHour.
func (ks *KnowledgeStore) PutInbox(from, key, content, priority string, quotaPerHour int) error {
	if from == "" {
		return ErrInboxMissingFrom
	}
	if err := ValidateInboxKey(key); err != nil {
		return err
	}
	if err := ValidateInboxPriority(priority); err != nil {
		return err
	}
	if priority == "" {
		priority = InboxPriorityNormal
	}
	if quotaPerHour <= 0 {
		quotaPerHour = InboxQuotaPerSenderPerHour
	}

	// Quota check: count entries from this sender in the last hour.
	// Exempt re-writes of the SAME key (idempotent updates) from the count.
	count, countErr := ks.countInboxFromSince(from, key, time.Now().Add(-time.Hour).Unix())
	if countErr != nil {
		return fmt.Errorf("inbox: quota check: %w", countErr)
	}
	if count >= quotaPerHour {
		return fmt.Errorf("%w (sender=%s count=%d cap=%d)", ErrInboxQuotaExceeded, from, count, quotaPerHour)
	}

	entry := KnowledgeEntry{
		Content:  content,
		Tags:     []string{"inbox", "from:" + from},
		From:     from,
		Priority: priority,
		// ReadAt intentionally 0 — fetch flips it to time.Now().Unix() [331.C].
	}
	return ks.Put(NSInbox, key, entry)
}

// countInboxFromSince counts inbox entries with From==sender and UpdatedAt
// >= sinceUnix. Excludes the exempt key (idempotent re-writes don't count).
// [331.A]
func (ks *KnowledgeStore) countInboxFromSince(sender, exemptKey string, sinceUnix int64) (int, error) {
	count := 0
	err := ks.db.View(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(bucketKey(NSInbox))
		if bkt == nil {
			return nil // no inbox yet → 0
		}
		return bkt.ForEach(func(k, v []byte) error {
			if string(k) == exemptKey {
				return nil
			}
			var entry KnowledgeEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				return nil // malformed entry — skip
			}
			if entry.From == sender && entry.UpdatedAt >= sinceUnix {
				count++
			}
			return nil
		})
	})
	return count, err
}

// MarkInboxRead sets ReadAt=now() for an inbox entry. Idempotent — re-reading
// updates the timestamp. Returns ErrNotFound if the entry doesn't exist. [331.A/331.C]
func (ks *KnowledgeStore) MarkInboxRead(key string) error {
	if err := ValidateInboxKey(key); err != nil {
		return err
	}
	existing, err := ks.Get(NSInbox, key)
	if err != nil {
		return err
	}
	existing.ReadAt = time.Now().Unix()
	return ks.Put(NSInbox, key, *existing)
}

// ListInboxFor returns inbox entries whose key targets the given workspace ID.
// Key filter: prefix match "to-<wsID>-". If unreadOnly is true, also filters to
// entries where ReadAt == 0. Results sorted by UpdatedAt desc. [331.A]
func (ks *KnowledgeStore) ListInboxFor(targetWSID string, unreadOnly bool) ([]KnowledgeEntry, error) {
	if targetWSID == "" {
		return nil, fmt.Errorf("inbox list: targetWSID is required")
	}
	prefix := []byte(InboxKeyPrefix + targetWSID + "-")
	var results []KnowledgeEntry
	err := ks.db.View(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(bucketKey(NSInbox))
		if bkt == nil {
			return nil
		}
		return bkt.ForEach(func(k, v []byte) error {
			if !hasPrefix(k, prefix) {
				return nil
			}
			var entry KnowledgeEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				return nil
			}
			if unreadOnly && entry.ReadAt != 0 {
				return nil
			}
			results = append(results, entry)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	// Sort desc by UpdatedAt (most recent first).
	sort.Slice(results, func(i, j int) bool { return results[i].UpdatedAt > results[j].UpdatedAt })
	return results, nil
}

// hasPrefix — bytes equivalent of strings.HasPrefix. [331.A]
func hasPrefix(b, prefix []byte) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i := range prefix {
		if b[i] != prefix[i] {
			return false
		}
	}
	return true
}

// PutEpic is the preferred high-level API for writing cross-workspace epic
// records. Validates key, status and priority before delegating to Put. [332.A]
//
//   key:       must follow '<PILAR>-<id>' format (e.g. "LX-332.A")
//   title:     human-readable epic name
//   content:   description / acceptance criteria
//   status:    "" → "open"; or one of open|in_progress|done|blocked
//   priority:  "" → unset; or one of P0|P1|P2|P3
//   ownerWSID: workspace driving this epic (attribution)
//   affected:  workspace IDs impacted by this epic
//   blocks:    epic IDs (same format) that this epic must finish first
func (ks *KnowledgeStore) PutEpic(key, title, content, status, priority, ownerWSID string, affected, blocks []string) error {
	if err := ValidateEpicID(key); err != nil {
		return err
	}
	if err := ValidateEpicStatus(status); err != nil {
		return err
	}
	if err := ValidateEpicPriority(priority); err != nil {
		return err
	}
	if status == "" {
		status = EpicStatusOpen
	}
	tags := []string{"epic", "status:" + status}
	if priority != "" {
		tags = append(tags, "priority:"+priority)
	}
	entry := KnowledgeEntry{
		Content:       content,
		Tags:          tags,
		Priority:      priority,
		EpicTitle:     title,
		EpicStatus:    status,
		EpicOwner:     ownerWSID,
		EpicAffected:  affected,
		EpicBlocks:    blocks,
		EpicCreatedBy: ownerWSID,
	}
	return ks.Put(NSEpics, key, entry)
}

// ListEpicsByStatus returns all epics with the given status, sorted by key.
// Pass "" to return all epics regardless of status. [332.A]
func (ks *KnowledgeStore) ListEpicsByStatus(status string) ([]KnowledgeEntry, error) {
	all, err := ks.List(NSEpics, "")
	if err != nil {
		return nil, err
	}
	if status == "" {
		return all, nil
	}
	filtered := all[:0]
	for _, e := range all {
		if e.EpicStatus == status {
			filtered = append(filtered, e)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Key < filtered[j].Key })
	return filtered, nil
}

// Put upserts an entry in the store. [293.C]
// Sets CreatedAt on first write, always updates UpdatedAt.
// When syncDir is set, also writes a companion .md file (dual-layer). [297.C]
//
// [331.A] For NSInbox: if caller uses the generic Put (not PutInbox), the key
// format is still validated — but From/quota checks are NOT enforced. Prefer
// PutInbox() for agent-authored messages.
func (ks *KnowledgeStore) Put(ns, key string, e KnowledgeEntry) error {
	// [331.A] Inbox key format gate — prevents random keys polluting the namespace
	// even when callers bypass PutInbox().
	if ns == NSInbox {
		if err := ValidateInboxKey(key); err != nil {
			return err
		}
	}
	// [332.A] Epics schema gate — key format + status + priority validated even
	// when callers use generic Put instead of PutEpic.
	if ns == NSEpics {
		if err := ValidateEpicID(key); err != nil {
			return err
		}
		if e.EpicStatus != "" {
			if err := ValidateEpicStatus(e.EpicStatus); err != nil {
				return err
			}
		}
		if e.Priority != "" {
			if err := ValidateEpicPriority(e.Priority); err != nil {
				return err
			}
		}
	}

	e.Namespace = ns
	e.Key = key
	now := time.Now().Unix()
	e.UpdatedAt = now

	var conflictEntry *KnowledgeEntry // set when a concurrent overwrite is detected [342.A]
	if err := ks.db.Update(func(tx *bbolt.Tx) error {
		bkt, err := tx.CreateBucketIfNotExists(bucketKey(ns))
		if err != nil {
			return err
		}
		// Preserve CreatedAt; detect LWW conflicts from different agent sessions. [342.A]
		if existing := bkt.Get([]byte(key)); existing != nil {
			var old KnowledgeEntry
			if json.Unmarshal(existing, &old) == nil {
				if old.CreatedAt != 0 {
					e.CreatedAt = old.CreatedAt
				}
				// Conflict: written <10s ago by a different session.
				if ns != NSConflicts && ns != NSInbox &&
					old.UpdatedAt > 0 && now-old.UpdatedAt < 10 &&
					old.SessionAgentID != "" && e.SessionAgentID != "" &&
					old.SessionAgentID != e.SessionAgentID {
					cp := old // capture the loser before overwrite
					conflictEntry = &cp
				}
			}
		}
		if e.CreatedAt == 0 {
			e.CreatedAt = now
		}
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		return bkt.Put([]byte(key), data)
	}); err != nil {
		return err
	}
	// Log conflict: store the overwritten entry in NSConflicts for review. [342.A]
	if conflictEntry != nil {
		log.Printf("[KNOWLEDGE-CONFLICT] ns=%s key=%s winner=%s loser=%s",
			ns, key, e.SessionAgentID, conflictEntry.SessionAgentID)
		conflictKey := fmt.Sprintf("%s:%s:%d", ns, key, now)
		_ = ks.db.Update(func(tx *bbolt.Tx) error {
			bkt, bErr := tx.CreateBucketIfNotExists(bucketKey(NSConflicts))
			if bErr != nil {
				return bErr
			}
			conflictEntry.Key = conflictKey
			conflictEntry.Namespace = NSConflicts
			// Tag with original ns+key so resolve_conflict can restore it.
			conflictEntry.Tags = append(conflictEntry.Tags, "conflict_of:"+ns+"/"+key)
			conflictEntry.Tags = append(conflictEntry.Tags, "winner:"+e.SessionAgentID)
			data, mErr := json.Marshal(conflictEntry)
			if mErr != nil {
				return mErr
			}
			return bkt.Put([]byte(conflictKey), data)
		})
	}
	// [297.C] Mirror to .md file. ExportEntry is idempotent — skips write when
	// content is unchanged, which breaks the watcher→Put→ExportEntry loop.
	if ks.syncDir != "" {
		if err := ExportEntry(ks.syncDir, e); err != nil {
			log.Printf("[297.C] knowledge export %s/%s: %v", ns, key, err)
		}
	}
	return nil
}

// Get retrieves a single entry by namespace + key. [293.C]
// Returns ErrNotFound when the entry does not exist.
func (ks *KnowledgeStore) Get(ns, key string) (*KnowledgeEntry, error) {
	var entry KnowledgeEntry
	err := ks.db.View(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(bucketKey(ns))
		if bkt == nil {
			return ErrNotFound
		}
		data := bkt.Get([]byte(key))
		if data == nil {
			return ErrNotFound
		}
		return json.Unmarshal(data, &entry)
	})
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

// Delete hard-deletes an entry. [293.C]
// Returns nil if the entry does not exist (idempotent).
// When syncDir is set, also removes the companion .md file. [297.C]
func (ks *KnowledgeStore) Delete(ns, key string) error {
	if err := ks.db.Update(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(bucketKey(ns))
		if bkt == nil {
			return nil
		}
		return bkt.Delete([]byte(key))
	}); err != nil {
		return err
	}
	if ks.syncDir != "" {
		mdPath := filepath.Join(ks.syncDir, ns, safeFilename(key)+".md")
		_ = os.Remove(mdPath) // idempotent — ignore not-found
	}
	return nil
}

// List returns all entries in a namespace, optionally filtered by tag. [293.C]
// tag="" means no tag filter. Returns empty slice (not nil) on empty result.
func (ks *KnowledgeStore) List(ns, tag string) ([]KnowledgeEntry, error) {
	var result []KnowledgeEntry
	err := ks.db.View(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(bucketKey(ns))
		if bkt == nil {
			return nil
		}
		return bkt.ForEach(func(_, v []byte) error {
			var e KnowledgeEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return nil // skip corrupt entries
			}
			if tag != "" && !hasTag(e.Tags, tag) {
				return nil
			}
			result = append(result, e)
			return nil
		})
	})
	if result == nil {
		result = []KnowledgeEntry{}
	}
	return result, err
}

// ListNamespaces returns all namespace names present in the store.
func (ks *KnowledgeStore) ListNamespaces() ([]string, error) {
	var nss []string
	prefix := "knowledge:"
	err := ks.db.View(func(tx *bbolt.Tx) error {
		return tx.ForEach(func(name []byte, _ *bbolt.Bucket) error {
			if ns, ok := strings.CutPrefix(string(name), prefix); ok {
				nss = append(nss, ns)
			}
			return nil
		})
	})
	return nss, err
}

// Search finds entries by substring match over Key and Content. [293.C]
// Returns up to k results. If k <= 0, returns all matches.
func (ks *KnowledgeStore) Search(ns, query string, k int) ([]KnowledgeEntry, error) {
	q := strings.ToLower(query)
	var result []KnowledgeEntry

	searchBucket := func(bkt *bbolt.Bucket) error {
		return bkt.ForEach(func(_, v []byte) error {
			if k > 0 && len(result) >= k {
				return nil
			}
			var e KnowledgeEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return nil
			}
			if strings.Contains(strings.ToLower(e.Key), q) ||
				strings.Contains(strings.ToLower(e.Content), q) {
				result = append(result, e)
			}
			return nil
		})
	}

	err := ks.db.View(func(tx *bbolt.Tx) error {
		if ns != "" && ns != "*" {
			bkt := tx.Bucket(bucketKey(ns))
			if bkt == nil {
				return nil
			}
			return searchBucket(bkt)
		}
		// ns == "*" — search all namespaces.
		prefix := "knowledge:"
		return tx.ForEach(func(name []byte, bkt *bbolt.Bucket) error {
			if k > 0 && len(result) >= k {
				return nil
			}
			if strings.HasPrefix(string(name), prefix) {
				return searchBucket(bkt)
			}
			return nil
		})
	})
	return result, err
}

// hasTag reports whether tags contains target.
func hasTag(tags []string, target string) bool {
	return slices.Contains(tags, target)
}

// CountConflicts returns the number of unresolved entries in the NSConflicts
// namespace. Used by BRIEFING to surface the knowledge_conflicts signal. [342.A]
func (ks *KnowledgeStore) CountConflicts() int {
	var n int
	_ = ks.db.View(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(bucketKey(NSConflicts))
		if bkt == nil {
			return nil
		}
		n = bkt.Stats().KeyN
		return nil
	})
	return n
}

// ResolveConflict settles a LWW conflict. winner must be "A" (keep current stored
// value) or "B" (restore the losing entry to its original namespace). The conflict
// entry is deleted from NSConflicts in either case. [342.A]
func (ks *KnowledgeStore) ResolveConflict(conflictKey, winner string) error {
	if winner != "A" && winner != "B" {
		return fmt.Errorf("resolve_conflict: winner must be 'A' or 'B', got %q", winner)
	}
	return ks.db.Update(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(bucketKey(NSConflicts))
		if bkt == nil {
			return fmt.Errorf("no conflicts namespace")
		}
		raw := bkt.Get([]byte(conflictKey))
		if raw == nil {
			return fmt.Errorf("conflict key %q not found", conflictKey)
		}
		if winner == "B" {
			// Restore loser to its original namespace/key (extracted from Tags).
			var loser KnowledgeEntry
			if err := json.Unmarshal(raw, &loser); err != nil {
				return err
			}
			origNS, origKey := "", ""
			for _, tag := range loser.Tags {
				if after, ok := strings.CutPrefix(tag, "conflict_of:"); ok {
					parts := strings.SplitN(after, "/", 2)
					if len(parts) == 2 {
						origNS, origKey = parts[0], parts[1]
					}
				}
			}
			if origNS == "" || origKey == "" {
				return fmt.Errorf("conflict entry missing conflict_of tag")
			}
			origBkt, err := tx.CreateBucketIfNotExists(bucketKey(origNS))
			if err != nil {
				return err
			}
			loser.Namespace = origNS
			loser.Key = origKey
			loser.UpdatedAt = time.Now().Unix()
			data, err := json.Marshal(loser)
			if err != nil {
				return err
			}
			if err := origBkt.Put([]byte(origKey), data); err != nil {
				return err
			}
		}
		return bkt.Delete([]byte(conflictKey))
	})
}
