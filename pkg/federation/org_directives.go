package federation

// org_directives.go — `.neo-org/DIRECTIVES.md` storage for org-scoped
// architectural directives. [PILAR LXVII / 356.B]
//
// Mirror del patrón de neo-synced-directives.md (workspace-level) pero para
// directivas que aplican a TODOS los projects del org. 355.B ya auto-sync'a
// `.neo-org/knowledge/directives/*.md` a cada workspace; este archivo es el
// compendio editable desde `neo_memory(action:"learn", scope:"org")`.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// OrgDirective representa una regla arquitectónica con scope org. Se
// persiste con ID monotónico 1-based (consistente con la tool legacy
// neo_learn_directive que numera 1..N). [356.B]
type OrgDirective struct {
	ID         int       `json:"id"`
	Text       string    `json:"text"`
	CreatedAt  time.Time `json:"created_at"`
	Deprecated bool      `json:"deprecated,omitempty"`
	Supersedes []int     `json:"supersedes,omitempty"`
}

var (
	// ErrOrgDirectiveNotFound se retorna cuando el ID no existe.
	ErrOrgDirectiveNotFound = errors.New("org_directives: id not found")
	// ErrOrgDirectiveAlreadyDeprecated para double-delete.
	ErrOrgDirectiveAlreadyDeprecated = errors.New("org_directives: already deprecated")
)

const orgDirectivesFile = "DIRECTIVES.md"
const odrJSONOpen = "<!-- neo-org-directives-v1\n"
const odrJSONClose = "\n-->\n"

var odrJSONBlock = regexp.MustCompile(`(?s)<!-- neo-org-directives-v1\n(.*?)\n-->`)

var odrMu sync.Mutex

// AppendOrgDirective registra una nueva directiva en `.neo-org/DIRECTIVES.md`.
// ID auto-asignado = len(existing)+1. Con `supersedes: [3, 7]` auto-depreca
// los IDs indicados. [356.B]
func AppendOrgDirective(orgDir, text string, supersedes []int) (OrgDirective, error) {
	odrMu.Lock()
	defer odrMu.Unlock()
	if orgDir == "" {
		return OrgDirective{}, errors.New("org_directives: empty orgDir")
	}
	if text == "" {
		return OrgDirective{}, errors.New("org_directives: text required")
	}
	path := filepath.Join(orgDir, orgDirectivesFile)
	all, err := loadOrgDirectives(path)
	if err != nil {
		return OrgDirective{}, err
	}
	newDir := OrgDirective{
		ID:         len(all) + 1,
		Text:       text,
		CreatedAt:  time.Now().UTC(),
		Supersedes: supersedes,
	}
	all = append(all, newDir)
	// Auto-deprecate superseded IDs.
	for _, sid := range supersedes {
		if sid >= 1 && sid <= len(all) {
			all[sid-1].Deprecated = true
		}
	}
	if err := writeOrgDirectives(path, all); err != nil {
		return OrgDirective{}, err
	}
	return newDir, nil
}

// ListOrgDirectives retorna todas las directivas (activas + deprecated).
// Missing file → (nil, nil). [356.B]
func ListOrgDirectives(orgDir string) ([]OrgDirective, error) {
	odrMu.Lock()
	defer odrMu.Unlock()
	if orgDir == "" {
		return nil, nil
	}
	return loadOrgDirectives(filepath.Join(orgDir, orgDirectivesFile))
}

// DeprecateOrgDirective marca un ID como deprecated (soft-delete — la
// entry permanece para trazabilidad histórica). [356.B]
func DeprecateOrgDirective(orgDir string, id int) error {
	odrMu.Lock()
	defer odrMu.Unlock()
	if orgDir == "" || id < 1 {
		return errors.New("org_directives: invalid args")
	}
	path := filepath.Join(orgDir, orgDirectivesFile)
	all, err := loadOrgDirectives(path)
	if err != nil {
		return err
	}
	if id > len(all) {
		return ErrOrgDirectiveNotFound
	}
	if all[id-1].Deprecated {
		return ErrOrgDirectiveAlreadyDeprecated
	}
	all[id-1].Deprecated = true
	return writeOrgDirectives(path, all)
}

// UpdateOrgDirective replaces the text of an existing directive. [356.B]
func UpdateOrgDirective(orgDir string, id int, newText string) error {
	odrMu.Lock()
	defer odrMu.Unlock()
	if orgDir == "" || id < 1 || newText == "" {
		return errors.New("org_directives: invalid args")
	}
	path := filepath.Join(orgDir, orgDirectivesFile)
	all, err := loadOrgDirectives(path)
	if err != nil {
		return err
	}
	if id > len(all) {
		return ErrOrgDirectiveNotFound
	}
	all[id-1].Text = newText
	return writeOrgDirectives(path, all)
}

func loadOrgDirectives(path string) ([]OrgDirective, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304-WORKSPACE-CANON: orgDir under operator control
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("org_directives: read: %w", err)
	}
	m := odrJSONBlock.FindSubmatch(data)
	if m == nil {
		return nil, nil
	}
	var payload struct {
		Directives []OrgDirective `json:"directives"`
	}
	if err := json.Unmarshal(m[1], &payload); err != nil {
		return nil, fmt.Errorf("org_directives: parse: %w", err)
	}
	return payload.Directives, nil
}

func writeOrgDirectives(path string, all []OrgDirective) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("org_directives: mkdir: %w", err)
	}
	payload := struct {
		Directives []OrgDirective `json:"directives"`
	}{Directives: all}
	jsonBytes, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("org_directives: marshal: %w", err)
	}
	var sb strings.Builder
	sb.WriteString("# Org-Level Architectural Directives\n\n")
	sb.WriteString("> Reglas que aplican a TODOS los projects del org. Gestionadas via\n")
	sb.WriteString("> `neo_memory(action:\"learn\", scope:\"org\", directive:\"...\")`.\n")
	sb.WriteString("> `.claude/rules/` de cada workspace importa estas via el auto-sync de 355.B.\n\n")
	sb.WriteString(odrJSONOpen)
	sb.Write(jsonBytes)
	sb.WriteString(odrJSONClose)
	sb.WriteString("\n")
	sb.WriteString(renderOrgDirectivesList(all))

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o600); err != nil { //nolint:gosec // G306 0o600 tight
		return fmt.Errorf("org_directives: write tmp: %w", err)
	}
	return os.Rename(tmp, path)
}

func renderOrgDirectivesList(all []OrgDirective) string {
	var sb strings.Builder
	active := 0
	for _, d := range all {
		if !d.Deprecated {
			active++
		}
	}
	fmt.Fprintf(&sb, "## Active (%d / %d total)\n\n", active, len(all))
	if len(all) == 0 {
		sb.WriteString("_none_\n")
		return sb.String()
	}
	for _, d := range all {
		prefix := ""
		if d.Deprecated {
			prefix = "~~OBSOLETO~~ "
		}
		fmt.Fprintf(&sb, "%d. %s%s", d.ID, prefix, d.Text)
		if len(d.Supersedes) > 0 {
			fmt.Fprintf(&sb, " _(supersedes: %v)_", d.Supersedes)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}
