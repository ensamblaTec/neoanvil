package federation

// org_debt.go — `.neo-org/DEBT.md` reader/writer for cross-project technical
// debt. [PILAR LXVII / 356.A]
//
// Mirrors the SHARED_DEBT pattern (.neo-project/SHARED_DEBT.md, see
// shared_debt.go) but one level up: items here affect multiple projects
// within an organisation. Examples: "Go 1.26 upgrade pending across all
// projects", "Shared TLS cert expires 2026-08-01", "Nomic-embed-text model
// upgrade needs coordinated rollout".
//
// File format:
//
//	# Org-Level Technical Debt — {OrgName}
//
//	<!-- neo-org-debt-v1
//	{"entries":[...]}
//	-->
//
//	## P0 — Blocker
//	| ID | Title | Affected Projects | Detected |
//	...
//
// JSON block is source of truth; tables below regenerated on each write.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// OrgDebtEntry is one cross-project debt item. Priority P0 blocks org-wide
// coordination; P3 is observational. [356.A]
type OrgDebtEntry struct {
	ID                string    `json:"id"` // YYYY-MM-DD-xxxx
	Priority          string    `json:"priority"` // P0 | P1 | P2 | P3
	Title             string    `json:"title"`
	Description       string    `json:"description,omitempty"`
	AffectedProjects  []string  `json:"affected_projects"`
	Source            string    `json:"source,omitempty"` // e.g. "manual", "auto-detected"
	DetectedAt        time.Time `json:"detected_at"`
	ResolvedAt        time.Time `json:"resolved_at"`
	ResolvedBy        string    `json:"resolved_by,omitempty"`
	Resolution        string    `json:"resolution,omitempty"`
}

// ErrOrgDebtNotFound / ErrOrgDebtAlreadyResolved are returned by ResolveOrgDebt.
var (
	ErrOrgDebtNotFound        = errors.New("org_debt: entry not found")
	ErrOrgDebtAlreadyResolved = errors.New("org_debt: already resolved")
)

const orgDebtFile = "DEBT.md"
const odJSONOpen = "<!-- neo-org-debt-v1\n"
const odJSONClose = "\n-->\n"

var odJSONBlock = regexp.MustCompile(`(?s)<!-- neo-org-debt-v1\n(.*?)\n-->`)

var odMu sync.Mutex

// AppendOrgDebt records a new entry in `.neo-org/DEBT.md`. ID auto-generated.
// [356.A]
func AppendOrgDebt(orgDir string, e OrgDebtEntry) (OrgDebtEntry, error) {
	odMu.Lock()
	defer odMu.Unlock()
	if orgDir == "" {
		return OrgDebtEntry{}, errors.New("org_debt: empty orgDir")
	}
	if e.Title == "" {
		return OrgDebtEntry{}, errors.New("org_debt: title required")
	}
	path := filepath.Join(orgDir, orgDebtFile)
	existing, err := loadOrgDebt(path)
	if err != nil {
		return OrgDebtEntry{}, err
	}
	if e.ID == "" {
		e.ID = newOrgDebtID(time.Now().UTC())
	}
	if e.Priority == "" {
		e.Priority = "P2"
	}
	if e.DetectedAt.IsZero() {
		e.DetectedAt = time.Now().UTC()
	}
	existing = append(existing, e)
	if err := writeOrgDebt(path, existing); err != nil {
		return OrgDebtEntry{}, err
	}
	return e, nil
}

// ListOrgDebt returns all entries, or (nil, nil) when file absent. [356.A]
func ListOrgDebt(orgDir string) ([]OrgDebtEntry, error) {
	odMu.Lock()
	defer odMu.Unlock()
	if orgDir == "" {
		return nil, nil
	}
	return loadOrgDebt(filepath.Join(orgDir, orgDebtFile))
}

// ResolveOrgDebt transitions an open entry to resolved with resolvedBy +
// note. [356.A]
func ResolveOrgDebt(orgDir, id, resolvedBy, note string) error {
	odMu.Lock()
	defer odMu.Unlock()
	if orgDir == "" || id == "" {
		return errors.New("org_debt: empty orgDir or id")
	}
	path := filepath.Join(orgDir, orgDebtFile)
	all, err := loadOrgDebt(path)
	if err != nil {
		return err
	}
	for i := range all {
		if all[i].ID != id {
			continue
		}
		if !all[i].ResolvedAt.IsZero() {
			return ErrOrgDebtAlreadyResolved
		}
		all[i].ResolvedAt = time.Now().UTC()
		all[i].ResolvedBy = resolvedBy
		all[i].Resolution = note
		return writeOrgDebt(path, all)
	}
	return ErrOrgDebtNotFound
}

func loadOrgDebt(path string) ([]OrgDebtEntry, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304-WORKSPACE-CANON: orgDir under operator control
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("org_debt: read: %w", err)
	}
	m := odJSONBlock.FindSubmatch(data)
	if m == nil {
		return nil, nil
	}
	var payload struct {
		Entries []OrgDebtEntry `json:"entries"`
	}
	if err := json.Unmarshal(m[1], &payload); err != nil {
		return nil, fmt.Errorf("org_debt: parse: %w", err)
	}
	return payload.Entries, nil
}

func writeOrgDebt(path string, all []OrgDebtEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("org_debt: mkdir: %w", err)
	}
	payload := struct {
		Entries []OrgDebtEntry `json:"entries"`
	}{Entries: all}
	jsonBytes, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("org_debt: marshal: %w", err)
	}
	var sb strings.Builder
	sb.WriteString("# Org-Level Technical Debt\n\n")
	sb.WriteString("> Cross-project debt affecting multiple member projects.\n")
	sb.WriteString("> Managed by `neo_debt(scope:\"org\")`. Resolve with\n")
	sb.WriteString("> `neo_debt(action:\"resolve\", scope:\"org\", id, resolution)`.\n\n")
	sb.WriteString(odJSONOpen)
	sb.Write(jsonBytes)
	sb.WriteString(odJSONClose)
	sb.WriteString("\n")
	sb.WriteString(renderOrgDebtTables(all))

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o600); err != nil { //nolint:gosec // G306 0o600 tight
		return fmt.Errorf("org_debt: write tmp: %w", err)
	}
	return os.Rename(tmp, path)
}

func renderOrgDebtTables(all []OrgDebtEntry) string {
	buckets := map[string][]OrgDebtEntry{"P0": nil, "P1": nil, "P2": nil, "P3": nil}
	var resolved []OrgDebtEntry
	for _, e := range all {
		if !e.ResolvedAt.IsZero() {
			resolved = append(resolved, e)
			continue
		}
		p := e.Priority
		if _, ok := buckets[p]; !ok {
			p = "P2"
		}
		buckets[p] = append(buckets[p], e)
	}
	var sb strings.Builder
	renderOrgDebtBucket(&sb, "P0 — Blocker (prevents cross-project coordination)", buckets["P0"])
	renderOrgDebtBucket(&sb, "P1 — High (affects ≥2 projects)", buckets["P1"])
	renderOrgDebtBucket(&sb, "P2 — Medium", buckets["P2"])
	renderOrgDebtBucket(&sb, "P3 — Observational", buckets["P3"])
	fmt.Fprintf(&sb, "## Resolved (%d)\n\n", len(resolved))
	if len(resolved) == 0 {
		sb.WriteString("_none_\n")
		return sb.String()
	}
	sb.WriteString("| ID | Title | Resolved | By | Note |\n|----|-------|----------|----|------|\n")
	for _, e := range resolved {
		fmt.Fprintf(&sb, "| `%s` | %s | %s | %s | %s |\n",
			e.ID, e.Title, e.ResolvedAt.Format("2006-01-02 15:04"),
			e.ResolvedBy, e.Resolution)
	}
	return sb.String()
}

func renderOrgDebtBucket(sb *strings.Builder, title string, entries []OrgDebtEntry) {
	fmt.Fprintf(sb, "## %s\n\n", title)
	if len(entries) == 0 {
		sb.WriteString("_none_\n\n")
		return
	}
	sb.WriteString("| ID | Title | Affected Projects | Detected |\n|----|-------|-------------------|----------|\n")
	for _, e := range entries {
		fmt.Fprintf(sb, "| `%s` | %s | %s | %s |\n",
			e.ID, e.Title,
			strings.Join(e.AffectedProjects, ","),
			e.DetectedAt.Format("2006-01-02 15:04"))
	}
	sb.WriteString("\n")
}

func newOrgDebtID(t time.Time) string {
	suffix := rand.Intn(0xFFFF) //nolint:gosec // G404: non-crypto ID for display
	return fmt.Sprintf("%s-%04x", t.Format("2006-01-02"), suffix)
}
