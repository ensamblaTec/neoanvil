// pkg/federation/shared_debt.go — SHARED_DEBT.md helpers for Project Federation. [316.A]
//
// AppendMissingContract logs an endpoint not found by CONTRACT_QUERY to the
// project-level .neo-project/SHARED_DEBT.md file. ParseSharedDebt reads it back.
// FindNeoProjectDir walks up the directory tree to find .neo-project/.
package federation

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const sharedDebtFile = "SHARED_DEBT.md"

// MissingContract is a parsed row from SHARED_DEBT.md Missing Contracts table.
type MissingContract struct {
	Endpoint  string
	Caller    string
	Workspace string
	Date      string
	Status    string
}

// FindNeoProjectDir walks up from startDir (up to 5 levels) to find .neo-project/.
// Returns the .neo-project/ path and true on success, empty string and false otherwise.
func FindNeoProjectDir(startDir string) (string, bool) {
	dir := startDir
	for range 5 {
		candidate := filepath.Join(dir, ".neo-project")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

// missingContractsSection is the header under which auto-append rows live.
// Must match the section already used in hand-maintained SHARED_DEBT.md files.
const missingContractsSection = "## Contratos de frontera bajo revisión"

// tableHeaderLine is the Markdown table header prepended on first auto-append.
const tableHeaderLine = "| Endpoint | Requested by | Workspace | Date | Status |"
const tableSeparatorLine = "|----------|-------------|-----------|------|--------|"

// footerPrefix is the prefix used to detect the "_Última actualización: ..._"
// line that must ALWAYS remain as the last non-empty content.
const footerPrefix = "_Última actualización:"

// AppendMissingContract inserts a missing-contract row inside the
// "Contratos de frontera bajo revisión" section of .neo-project/SHARED_DEBT.md.
// The row is placed BEFORE any trailing "_Última actualización: ...—" footer
// line so section order is preserved. If the section does not exist yet, it is
// created with a table header. If the footer line does not exist, one is added.
// [Épica 330.H — fix for auto-append going after footer]
func AppendMissingContract(projDir, endpoint, caller, workspace string) error {
	path := filepath.Join(projDir, sharedDebtFile)
	now := time.Now().UTC().Format("2006-01-02")
	row := fmt.Sprintf("| %s | %s | %s | %s | ⏳ pending |",
		sanitizeTableCell(endpoint), sanitizeTableCell(caller), sanitizeTableCell(workspace), now)

	// First-time creation — seed minimal structure.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		body := fmt.Sprintf(
			"# Shared Technical Debt\n\n%s\n\n%s\n%s\n%s\n\n---\n\n_Última actualización: %s_\n",
			missingContractsSection, tableHeaderLine, tableSeparatorLine, row, now,
		)
		return os.WriteFile(path, []byte(body), 0o644) //nolint:gosec // G306-SHARED-SOCKET: world-readable debt ledger shared between all project workspaces
	}

	data, err := os.ReadFile(path) //nolint:gosec // G304-DIR-WALK: path under process control
	if err != nil {
		return fmt.Errorf("shared_debt read: %w", err)
	}
	updated, err := insertContractRow(string(data), row, now)
	if err != nil {
		return fmt.Errorf("shared_debt rewrite: %w", err)
	}
	return os.WriteFile(path, []byte(updated), 0o644) //nolint:gosec // G306-SHARED-SOCKET: idem above
}

// sanitizeTableCell escapes pipe characters and strips newlines to prevent
// markdown table corruption when user-supplied values contain those characters.
func sanitizeTableCell(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "|", `\|`)
}

func findSectionAndFooter(lines []string) (sectionIdx, footerIdx int) {
	sectionIdx, footerIdx = -1, -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if sectionIdx == -1 && trimmed == missingContractsSection {
			sectionIdx = i
		}
		if strings.HasPrefix(trimmed, footerPrefix) {
			footerIdx = i
		}
	}
	return sectionIdx, footerIdx
}

func buildSectionMissingBlock(row string) []string {
	return []string{missingContractsSection, "", tableHeaderLine, tableSeparatorLine, row, "", "---", ""}
}

func findSectionEnd(lines []string, sectionIdx int) int {
	for i := sectionIdx + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, footerPrefix) {
			return i
		}
	}
	return len(lines)
}

func scanTableRows(lines []string, sectionIdx, sectionEnd int) (lastTableRow int, hasHeader bool) {
	lastTableRow = -1
	for i := sectionIdx + 1; i < sectionEnd; i++ {
		trimmed := strings.TrimSpace(lines[i])
		switch {
		case trimmed == tableHeaderLine || strings.HasPrefix(trimmed, "| Endpoint"):
			hasHeader = true
		case strings.HasPrefix(trimmed, "|---"):
			// separator — skip
		case strings.HasPrefix(trimmed, "|"):
			lastTableRow = i
		}
	}
	return lastTableRow, hasHeader
}

func computeInsertPoint(lines []string, sectionIdx, sectionEnd, lastTableRow int, hasHeader bool, row string) (insertAt int, toInsert []string) {
	if lastTableRow != -1 {
		return lastTableRow + 1, []string{row}
	}
	if hasHeader {
		for i := sectionIdx + 1; i < sectionEnd; i++ {
			if strings.HasPrefix(strings.TrimSpace(lines[i]), "|---") {
				return i + 1, []string{row}
			}
		}
	}
	return sectionIdx + 1, []string{"", tableHeaderLine, tableSeparatorLine, row}
}

// insertContractRow takes the full SHARED_DEBT.md content and returns a version
// with `row` inserted inside the Contratos-de-frontera-bajo-revisión section.
// It preserves — and updates — the "_Última actualización_" footer line.
func insertContractRow(content, row, today string) (string, error) {
	lines := strings.Split(content, "\n")
	sectionIdx, footerIdx := findSectionAndFooter(lines)

	if sectionIdx == -1 {
		insertAt := len(lines)
		if footerIdx != -1 {
			insertAt = footerIdx
		}
		result := append([]string{}, lines[:insertAt]...)
		result = append(result, buildSectionMissingBlock(row)...)
		result = append(result, lines[insertAt:]...)
		return ensureFooter(result, today), nil
	}

	sectionEnd := findSectionEnd(lines, sectionIdx)
	lastTableRow, hasHeader := scanTableRows(lines, sectionIdx, sectionEnd)
	insertAt, toInsert := computeInsertPoint(lines, sectionIdx, sectionEnd, lastTableRow, hasHeader, row)

	result := append([]string{}, lines[:insertAt]...)
	result = append(result, toInsert...)
	result = append(result, lines[insertAt:]...)
	return ensureFooter(result, today), nil
}

// ensureFooter guarantees the last non-empty line of the document is the
// "_Última actualización: <today>_" footer. Updates it in place if present,
// appends it otherwise.
func ensureFooter(lines []string, today string) string {
	newFooter := fmt.Sprintf("%s %s_", footerPrefix, today)
	// Find last non-empty line index.
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	if last >= 0 && strings.HasPrefix(strings.TrimSpace(lines[last]), footerPrefix) {
		lines[last] = newFooter
	} else {
		// Append a separator + footer (only if last non-empty isn't already a ---).
		if last < 0 || strings.TrimSpace(lines[last]) != "---" {
			lines = append(lines, "", "---", "", newFooter, "")
		} else {
			lines = append(lines, "", newFooter, "")
		}
	}
	return strings.Join(lines, "\n")
}

// ParseSharedDebt reads .neo-project/SHARED_DEBT.md and returns the parsed rows.
// Returns nil, nil when the file does not exist yet.
func ParseSharedDebt(projDir string) ([]MissingContract, error) {
	path := filepath.Join(projDir, sharedDebtFile)
	data, err := os.ReadFile(path) //nolint:gosec // G304-DIR-WALK: path is projDir/.neo-project/SHARED_DEBT.md under process control
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var result []MissingContract
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, "| ") {
			continue
		}
		if strings.HasPrefix(line, "| Endpoint") || strings.HasPrefix(line, "|---") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 7 {
			continue
		}
		result = append(result, MissingContract{
			Endpoint:  strings.TrimSpace(parts[1]),
			Caller:    strings.TrimSpace(parts[2]),
			Workspace: strings.TrimSpace(parts[3]),
			Date:      strings.TrimSpace(parts[4]),
			Status:    strings.TrimSpace(parts[5]),
		})
	}
	return result, nil
}
