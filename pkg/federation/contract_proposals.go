package federation

// contract_proposals.go — Persistent pending-approval flow for breaking HTTP
// contract changes between federation member workspaces. [PILAR LXIII / 343.A]
//
// When `checkContractDrift` detects an actionable breaking change (route
// removed / method renamed / request schema changed) that affects frontend
// callers in sibling workspaces, instead of silently emitting an INC the
// backend appends a `ContractProposal` to `.neo-project/CONTRACT_PROPOSALS.md`
// with status=pending. The affected workspace sees it via BRIEFING or via
// `neo_debt(scope:"project")` and can approve/reject explicitly. This MVP
// covers the persistence + helpers; the CONTRACT_APPROVE intent and timeout /
// presence-based auto-approval are deferred to follow-up commits.

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

// ContractProposal represents a single breaking-contract change waiting for
// cross-workspace approval. Stored in
// `<projDir>/.neo-project/CONTRACT_PROPOSALS.md` as a JSON block embedded in
// an HTML comment (same pattern as Nexus debt registry, see pkg/nexus/debt.go).
type ContractProposal struct {
	ID              string    `json:"id"`              // YYYY-MM-DD-xxxx
	FromWorkspace   string    `json:"from_workspace"`  // origin (backend that certified the change)
	Endpoint        string    `json:"endpoint"`        // "POST /api/users"
	ChangeType      string    `json:"change_type"`     // route_removed | method_changed | schema_changed
	AffectedCallers []string  `json:"affected_callers"` // frontend file paths with call sites
	ProposedAt      time.Time `json:"proposed_at"`
	Status          string    `json:"status"` // pending | approved | rejected | auto_approved | timeout_rejected
	ResolvedAt      time.Time `json:"resolved_at"`
	ResolvedBy      string    `json:"resolved_by,omitempty"`
	Resolution      string    `json:"resolution,omitempty"`
}

// ErrProposalNotFound is returned when ResolveProposal is called with an ID
// that doesn't exist in the file.
var ErrProposalNotFound = errors.New("contract_proposals: id not found")

// ErrProposalAlreadyResolved is returned on double-resolve.
var ErrProposalAlreadyResolved = errors.New("contract_proposals: already resolved")

const contractProposalsFile = "CONTRACT_PROPOSALS.md"
const cpJSONOpen = "<!-- neo-contract-proposals-v1\n"
const cpJSONClose = "\n-->\n"

var cpJSONBlock = regexp.MustCompile(`(?s)<!-- neo-contract-proposals-v1\n(.*?)\n-->`)

var cpMu sync.Mutex // file-level serialization across goroutines

// AppendContractProposal atomically appends a new pending proposal to
// `.neo-project/CONTRACT_PROPOSALS.md`. ID is auto-generated. Missing file is
// created with header. [343.A]
func AppendContractProposal(projDir string, p ContractProposal) (ContractProposal, error) {
	cpMu.Lock()
	defer cpMu.Unlock()

	if projDir == "" {
		return ContractProposal{}, errors.New("contract_proposals: empty projDir")
	}
	path := filepath.Join(projDir, contractProposalsFile)
	existing, err := loadProposals(path)
	if err != nil {
		return ContractProposal{}, err
	}

	if p.ID == "" {
		p.ID = newProposalID(time.Now().UTC())
	}
	if p.ProposedAt.IsZero() {
		p.ProposedAt = time.Now().UTC()
	}
	if p.Status == "" {
		p.Status = "pending"
	}

	existing = append(existing, p)
	if err := writeProposals(path, existing); err != nil {
		return ContractProposal{}, err
	}
	return p, nil
}

// ListProposals returns all proposals in the file, or (nil, nil) when the
// file does not exist yet.
func ListProposals(projDir string) ([]ContractProposal, error) {
	cpMu.Lock()
	defer cpMu.Unlock()
	if projDir == "" {
		return nil, nil
	}
	return loadProposals(filepath.Join(projDir, contractProposalsFile))
}

// ListPendingProposals is a convenience that filters ListProposals by
// Status=="pending".
func ListPendingProposals(projDir string) ([]ContractProposal, error) {
	all, err := ListProposals(projDir)
	if err != nil {
		return nil, err
	}
	var pending []ContractProposal
	for _, p := range all {
		if p.Status == "pending" {
			pending = append(pending, p)
		}
	}
	return pending, nil
}

// ResolveProposal transitions a pending proposal to approved/rejected with a
// resolution note. Returns ErrProposalNotFound or ErrProposalAlreadyResolved
// on failure. [343.A]
func ResolveProposal(projDir, id, status, resolvedBy, note string) error {
	cpMu.Lock()
	defer cpMu.Unlock()
	if projDir == "" {
		return errors.New("contract_proposals: empty projDir")
	}
	if status != "approved" && status != "rejected" && status != "auto_approved" && status != "timeout_rejected" {
		return fmt.Errorf("contract_proposals: invalid status %q", status)
	}
	path := filepath.Join(projDir, contractProposalsFile)
	all, err := loadProposals(path)
	if err != nil {
		return err
	}
	for i := range all {
		if all[i].ID != id {
			continue
		}
		if all[i].Status != "pending" {
			return ErrProposalAlreadyResolved
		}
		all[i].Status = status
		all[i].ResolvedAt = time.Now().UTC()
		all[i].ResolvedBy = resolvedBy
		all[i].Resolution = note
		return writeProposals(path, all)
	}
	return ErrProposalNotFound
}

// loadProposals reads the JSON embedded in the markdown file. Empty / missing
// file → (nil, nil).
func loadProposals(path string) ([]ContractProposal, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304-WORKSPACE-CANON: projDir under project root
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("contract_proposals: read: %w", err)
	}
	m := cpJSONBlock.FindSubmatch(data)
	if m == nil {
		return nil, nil
	}
	var payload struct {
		Proposals []ContractProposal `json:"proposals"`
	}
	if err := json.Unmarshal(m[1], &payload); err != nil {
		return nil, fmt.Errorf("contract_proposals: parse: %w", err)
	}
	return payload.Proposals, nil
}

// writeProposals serializes proposals to the file with JSON-in-HTML-comment +
// rendered markdown tables for human readability.
func writeProposals(path string, all []ContractProposal) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("contract_proposals: mkdir: %w", err)
	}
	payload := struct {
		Proposals []ContractProposal `json:"proposals"`
	}{Proposals: all}
	jsonBytes, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("contract_proposals: marshal: %w", err)
	}
	var sb strings.Builder
	sb.WriteString("# Contract Proposals Pending Approval\n\n")
	sb.WriteString("> Breaking HTTP contract changes detected in a federation member workspace\n")
	sb.WriteString("> that affect callers in sibling workspaces. Review and resolve via\n")
	sb.WriteString("> `neo_radar(intent:\"CONTRACT_APPROVE\", proposal_id, decision:approve|reject)` (WIP).\n\n")
	sb.WriteString(cpJSONOpen)
	sb.Write(jsonBytes)
	sb.WriteString(cpJSONClose)
	sb.WriteString("\n")
	sb.WriteString(renderProposalsTables(all))

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o600); err != nil { //nolint:gosec // G306 0o600 tight
		return fmt.Errorf("contract_proposals: write tmp: %w", err)
	}
	return os.Rename(tmp, path)
}

// renderProposalsTables produces two human-readable sections: Pending and
// Resolved. Used for operator review of the file.
func renderProposalsTables(all []ContractProposal) string {
	var pending, resolved []ContractProposal
	for _, p := range all {
		if p.Status == "pending" {
			pending = append(pending, p)
		} else {
			resolved = append(resolved, p)
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Pending (%d)\n\n", len(pending))
	if len(pending) == 0 {
		sb.WriteString("_none_\n\n")
	} else {
		sb.WriteString("| ID | From | Endpoint | Change | Callers | Proposed |\n|----|------|----------|--------|---------|----------|\n")
		for _, p := range pending {
			fmt.Fprintf(&sb, "| `%s` | %s | `%s` | %s | %d | %s |\n",
				p.ID, p.FromWorkspace, p.Endpoint, p.ChangeType,
				len(p.AffectedCallers), p.ProposedAt.Format("2006-01-02 15:04"))
		}
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "## Resolved (%d)\n\n", len(resolved))
	if len(resolved) == 0 {
		sb.WriteString("_none_\n")
		return sb.String()
	}
	sb.WriteString("| ID | Endpoint | Status | Resolved | By | Note |\n|----|----------|--------|----------|----|------|\n")
	for _, p := range resolved {
		fmt.Fprintf(&sb, "| `%s` | `%s` | %s | %s | %s | %s |\n",
			p.ID, p.Endpoint, p.Status,
			p.ResolvedAt.Format("2006-01-02 15:04"),
			p.ResolvedBy, p.Resolution)
	}
	return sb.String()
}

// newProposalID returns a short human-readable ID YYYY-MM-DD-xxxx.
func newProposalID(t time.Time) string {
	suffix := rand.Intn(0xFFFF) //nolint:gosec // G404: non-crypto ID for display
	return fmt.Sprintf("%s-%04x", t.Format("2006-01-02"), suffix)
}
