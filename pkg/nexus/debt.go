package nexus

// debt.go — Nexus-level debt registry. Events detected by the Nexus dispatcher
// (verify_boot timeouts, watchdog trips, service_manager Ollama outages, port
// conflicts) are appended to a markdown file at `~/.neo/nexus_debt.md`. Source
// of truth is the embedded JSON block; tables below it are regenerated on each
// write for human readability. [PILAR LXVI / 351.A]

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// NexusDebtEvent is the persisted record of a single dispatcher-level issue.
type NexusDebtEvent struct {
	ID                 string    `json:"id"`                   // YYYY-MM-DD-xxxx
	Priority           string    `json:"priority"`             // P0 | P1 | P2 | P3
	Title              string    `json:"title"`                // short human-readable line
	AffectedWorkspaces []string  `json:"affected_workspaces"`  // workspace IDs or names
	Source             string    `json:"source"`               // watchdog | verify_boot | service_manager | port_allocator | contract_broadcast
	DetectedAt         time.Time `json:"detected_at"`
	LastSeenAt         time.Time `json:"last_seen_at"`
	OccurrenceCount   int       `json:"occurrence_count"`
	Recommended        string    `json:"recommended,omitempty"` // remediation hint shown in BRIEFING
	Resolution         string    `json:"resolution,omitempty"`  // present when ResolvedAt non-zero
	ResolvedAt         time.Time `json:"resolved_at"`
	DedupKey           string    `json:"dedup_key"`             // sha256(title + sorted affected)
}

// DebtFilter narrows ListOpen results.
type DebtFilter struct {
	Priority   string    // "" = all; else exact match
	SinceUnix  int64     // 0 = no bound; else only events with LastSeenAt >= Since
	AffectedWS string    // "" = any; else must contain this workspace id/name
	IncludeResolved bool // false = open only
}

// DebtConfig mirrors the `debt:` section of ~/.neo/nexus.yaml.
type DebtConfig struct {
	Enabled           bool   `yaml:"enabled"`
	File              string `yaml:"file"`                 // ~/.neo/nexus_debt.md by default
	MaxResolvedDays   int    `yaml:"max_resolved_days"`    // archive resolved older than N days; 0 = never
	DedupWindowMinutes int   `yaml:"dedup_window_minutes"` // 0 = no dedup
	BoltDBMirror      bool   `yaml:"boltdb_mirror"`        // reserved for future
}

// DebtRegistry is the in-memory view of the debt file. Open it once at Nexus
// boot via OpenDebtRegistry, then call its methods from any goroutine — methods
// are safe for concurrent use and serialize access through an internal mutex
// plus a POSIX advisory lock on the sidecar `.lock` file.
type DebtRegistry struct {
	mu       sync.Mutex
	path     string
	lockPath string
	cfg      DebtConfig
	events   []NexusDebtEvent
	lockFD   *os.File // held while AppendDebt/ResolveDebt is running
}

// debtFileHeader is the human-readable preamble.
const debtFileHeader = "# Nexus-Level Debt — Auto-Generated\n\n> Events detected by Nexus dispatcher. Read-only from workspace tools.\n> Resolve via `neo_debt(scope: \"nexus\", action: \"resolve\", id: \"...\")`.\n\n"

// debtJSONOpen / debtJSONClose delimit the embedded JSON block which is the
// authoritative storage. Parser hunts for these markers.
const debtJSONOpen = "<!-- neo-nexus-debt-db-v1\n"
const debtJSONClose = "\n-->\n"

var debtJSONBlock = regexp.MustCompile(`(?s)<!-- neo-nexus-debt-db-v1\n(.*?)\n-->`)

// OpenDebtRegistry loads (or creates) the debt file and returns a ready-to-use
// registry. Missing file is treated as empty. [351.A]
func OpenDebtRegistry(cfg DebtConfig) (*DebtRegistry, error) {
	path := expandDebtPath(cfg.File)
	if path == "" {
		return nil, errors.New("debt: empty file path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // G301 path under user home
		return nil, fmt.Errorf("debt: mkdir: %w", err)
	}
	r := &DebtRegistry{
		path:     path,
		lockPath: path + ".lock",
		cfg:      cfg,
	}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// reload re-parses the file from disk. Caller holds mu.
func (r *DebtRegistry) reload() error {
	data, err := os.ReadFile(r.path) //nolint:gosec // G304: path under home, controlled by caller
	if err != nil {
		if os.IsNotExist(err) {
			r.events = nil
			return nil
		}
		return fmt.Errorf("debt: read: %w", err)
	}
	m := debtJSONBlock.FindSubmatch(data)
	if m == nil {
		r.events = nil
		return nil
	}
	var payload struct {
		Events []NexusDebtEvent `json:"events"`
	}
	if err := json.Unmarshal(m[1], &payload); err != nil {
		return fmt.Errorf("debt: parse embedded json: %w", err)
	}
	r.events = payload.Events
	return nil
}

// AppendDebt records a new event or updates an existing one (dedup window).
// Returns the canonical event (with ID assigned and counters updated).
func (r *DebtRegistry) AppendDebt(ev NexusDebtEvent) (NexusDebtEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.lockFile(); err != nil {
		return NexusDebtEvent{}, err
	}
	defer r.unlockFile()
	if err := r.reload(); err != nil {
		return NexusDebtEvent{}, err
	}

	now := time.Now().UTC()
	if ev.DetectedAt.IsZero() {
		ev.DetectedAt = now
	}
	ev.LastSeenAt = now
	if ev.Priority == "" {
		ev.Priority = "P2"
	}
	ev.DedupKey = computeDedupKey(ev.Title, ev.AffectedWorkspaces)

	// Dedup: active open entry with same key within the configured window.
	if idx := r.findDedupMatch(ev.DedupKey, now); idx >= 0 {
		e := &r.events[idx]
		e.OccurrenceCount++
		e.LastSeenAt = now
		if ev.Recommended != "" && e.Recommended == "" {
			e.Recommended = ev.Recommended
		}
		if err := r.persist(); err != nil {
			return NexusDebtEvent{}, err
		}
		return *e, nil
	}

	if ev.ID == "" {
		ev.ID = newDebtID(now)
	}
	if ev.OccurrenceCount == 0 {
		ev.OccurrenceCount = 1
	}
	r.events = append(r.events, ev)
	if err := r.persist(); err != nil {
		return NexusDebtEvent{}, err
	}
	return ev, nil
}

// findDedupMatch scans events for an open entry with the same key whose
// LastSeenAt is within the configured dedup window. Returns -1 on miss.
// Caller holds mu + flock. [351.A]
func (r *DebtRegistry) findDedupMatch(key string, now time.Time) int {
	if r.cfg.DedupWindowMinutes <= 0 || key == "" {
		return -1
	}
	window := time.Duration(r.cfg.DedupWindowMinutes) * time.Minute
	for i := range r.events {
		e := &r.events[i]
		if !e.ResolvedAt.IsZero() || e.DedupKey != key {
			continue
		}
		if now.Sub(e.LastSeenAt) > window {
			continue
		}
		return i
	}
	return -1
}

// ResolveDebt marks the given ID resolved with the supplied note. Returns
// ErrDebtNotFound when the ID does not exist or is already resolved.
func (r *DebtRegistry) ResolveDebt(id, resolution string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.lockFile(); err != nil {
		return err
	}
	defer r.unlockFile()
	if err := r.reload(); err != nil {
		return err
	}
	for i := range r.events {
		e := &r.events[i]
		if e.ID != id {
			continue
		}
		if !e.ResolvedAt.IsZero() {
			return ErrDebtAlreadyResolved
		}
		e.ResolvedAt = time.Now().UTC()
		e.Resolution = resolution
		return r.persist()
	}
	return ErrDebtNotFound
}

// ListOpen returns events matching the filter. Sorted by priority ascending
// (P0 first) then by DetectedAt descending (newest first within each tier).
func (r *DebtRegistry) ListOpen(filter DebtFilter) []NexusDebtEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []NexusDebtEvent
	for _, e := range r.events {
		if !filter.IncludeResolved && !e.ResolvedAt.IsZero() {
			continue
		}
		if filter.Priority != "" && e.Priority != filter.Priority {
			continue
		}
		if filter.SinceUnix > 0 && e.LastSeenAt.Unix() < filter.SinceUnix {
			continue
		}
		if filter.AffectedWS != "" && !containsWS(e.AffectedWorkspaces, filter.AffectedWS) {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority // P0 < P1 < P2 < P3
		}
		return out[i].DetectedAt.After(out[j].DetectedAt)
	})
	return out
}

// Affecting returns open events whose AffectedWorkspaces include wsID.
func (r *DebtRegistry) Affecting(wsID string) []NexusDebtEvent {
	return r.ListOpen(DebtFilter{AffectedWS: wsID})
}

// persist writes the current in-memory state to disk. Caller holds mu + flock.
func (r *DebtRegistry) persist() error {
	// Archive stale resolved events.
	r.archiveOldResolved()
	payload := struct {
		Events []NexusDebtEvent `json:"events"`
	}{Events: r.events}
	jsonBytes, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("debt: marshal: %w", err)
	}
	var body strings.Builder
	body.WriteString(debtFileHeader)
	body.WriteString(debtJSONOpen)
	body.Write(jsonBytes)
	body.WriteString(debtJSONClose)
	body.WriteString("\n")
	body.WriteString(renderTables(r.events))
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body.String()), 0o600); err != nil { //nolint:gosec // G306 0o600 is tight
		return fmt.Errorf("debt: write tmp: %w", err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return fmt.Errorf("debt: rename: %w", err)
	}
	return nil
}

// archiveOldResolved drops resolved events older than MaxResolvedDays. Caller
// holds mu. Kept simple — no separate archive file yet (future follow-up).
func (r *DebtRegistry) archiveOldResolved() {
	if r.cfg.MaxResolvedDays <= 0 {
		return
	}
	cutoff := time.Now().UTC().Add(-time.Duration(r.cfg.MaxResolvedDays) * 24 * time.Hour)
	kept := r.events[:0]
	for _, e := range r.events {
		if !e.ResolvedAt.IsZero() && e.ResolvedAt.Before(cutoff) {
			continue
		}
		kept = append(kept, e)
	}
	r.events = kept
}

// renderTables produces the human-visible sections. [351.A]
func renderTables(events []NexusDebtEvent) string {
	buckets := map[string][]NexusDebtEvent{"P0": nil, "P1": nil, "P2": nil, "P3": nil}
	var resolved []NexusDebtEvent
	for _, e := range events {
		if !e.ResolvedAt.IsZero() {
			resolved = append(resolved, e)
			continue
		}
		key := e.Priority
		if _, ok := buckets[key]; !ok {
			key = "P2"
		}
		buckets[key] = append(buckets[key], e)
	}
	var sb strings.Builder
	renderBucket(&sb, "Open P0 — Blocker (prevents workspace boot)", buckets["P0"])
	renderBucket(&sb, "Open P1 — High (workspace degraded)", buckets["P1"])
	renderBucket(&sb, "Open P2 — Medium", buckets["P2"])
	renderBucket(&sb, "Open P3 — Low", buckets["P3"])
	renderResolved(&sb, resolved)
	return sb.String()
}

func renderBucket(sb *strings.Builder, title string, evs []NexusDebtEvent) {
	fmt.Fprintf(sb, "## %s\n\n", title)
	if len(evs) == 0 {
		sb.WriteString("_none_\n\n")
		return
	}
	sb.WriteString("| ID | Title | Affected | Detected | Count |\n|----|-------|----------|----------|-------|\n")
	for _, e := range evs {
		fmt.Fprintf(sb, "| %s | %s | %s | %s | %d |\n",
			e.ID, e.Title,
			strings.Join(e.AffectedWorkspaces, ","),
			e.DetectedAt.Format("2006-01-02 15:04:05"),
			e.OccurrenceCount)
	}
	sb.WriteString("\n")
}

func renderResolved(sb *strings.Builder, evs []NexusDebtEvent) {
	fmt.Fprintf(sb, "## Resolved (last 30 days)\n\n")
	if len(evs) == 0 {
		sb.WriteString("_none_\n")
		return
	}
	sb.WriteString("| ID | Title | Resolution | Resolved |\n|----|-------|-----------|----------|\n")
	for _, e := range evs {
		fmt.Fprintf(sb, "| %s | %s | %s | %s |\n",
			e.ID, e.Title, e.Resolution,
			e.ResolvedAt.Format("2006-01-02 15:04:05"))
	}
}

// lockFile acquires an exclusive POSIX advisory lock on the sidecar file.
// Must be paired with unlockFile.
func (r *DebtRegistry) lockFile() error {
	f, err := os.OpenFile(r.lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304 path under home
	if err != nil {
		return fmt.Errorf("debt: open lock: %w", err)
	}
	// Block up to 2s for the lock — contention is rare and short-lived.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			r.lockFD = f
			return nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return ErrDebtLockBusy
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (r *DebtRegistry) unlockFile() {
	if r.lockFD == nil {
		return
	}
	_ = syscall.Flock(int(r.lockFD.Fd()), syscall.LOCK_UN)
	_ = r.lockFD.Close()
	r.lockFD = nil
}

// Sentinel errors.
var (
	ErrDebtNotFound        = errors.New("debt: event not found")
	ErrDebtAlreadyResolved = errors.New("debt: event already resolved")
	ErrDebtLockBusy        = errors.New("debt: lock busy")
)

// computeDedupKey returns the sha256-prefix used to match repeat occurrences.
func computeDedupKey(title string, affected []string) string {
	sorted := append([]string(nil), affected...)
	sort.Strings(sorted)
	h := sha256.New()
	h.Write([]byte(title))
	h.Write([]byte{0x00})
	for _, w := range sorted {
		h.Write([]byte(w))
		h.Write([]byte{0x00})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// newDebtID returns a human-readable identifier of the form YYYY-MM-DD-xxxx.
func newDebtID(t time.Time) string {
	suffix := rand.Intn(0xFFFF) //nolint:gosec // G404: non-crypto ID for display
	return fmt.Sprintf("%s-%04x", t.Format("2006-01-02"), suffix)
}

// expandDebtPath resolves `~/...` against the user home directory.
func expandDebtPath(p string) string {
	if p == "" {
		return ""
	}
	if suffix, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, suffix)
		}
	}
	return p
}

func containsWS(list []string, ws string) bool {
	return slices.Contains(list, ws)
}
