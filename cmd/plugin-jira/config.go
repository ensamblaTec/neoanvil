package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/auth"
	"github.com/ensamblatec/neoanvil/pkg/jira"
)

// ── Schema version ──────────────────────────────────────────────────────────

const pluginConfigVersion = 2

// ── Top-level config ────────────────────────────────────────────────────────

type PluginConfig struct {
	Version          int                    `json:"version"`
	ActiveProject    string                 `json:"active_project"`
	APIKeys          map[string]*APIKey     `json:"api_keys"`
	WorkspaceMapping map[string]string      `json:"workspace_mapping"`
	Audit            AuditConfig            `json:"audit"`
	TemplateLibrary  map[string]*Template   `json:"template_library"`
	Projects         map[string]*ProjectCfg `json:"projects"`
}

// ── API Keys ────────────────────────────────────────────────────────────────

type APIKey struct {
	Domain    string     `json:"domain"`
	Email     string     `json:"email"`
	Auth      AuthConfig `json:"auth"`
	RateLimit RateLimit  `json:"rate_limit"`
}

type AuthConfig struct {
	Type              string  `json:"type"` // PAT | OAUTH2_3LO
	Token             *string `json:"token"`
	TokenRef          *string `json:"token_ref"` // env:VAR | vault:path | file:path
	RotationDays      int     `json:"rotation_period_days"`
	CreatedAt         *string `json:"created_at"`
}

type RateLimit struct {
	MaxPerMinute    int    `json:"max_requests_per_minute"`
	Concurrency     int    `json:"concurrency"`
	RetryOn429      bool   `json:"retry_on_429"`
	BackoffStrategy string `json:"backoff_strategy"` // exponential | linear
}

// ── Templates ───────────────────────────────────────────────────────────────

type Template struct {
	Version string `json:"version"`
	Body    string `json:"body"`
}

// ── Audit ───────────────────────────────────────────────────────────────────

type AuditConfig struct {
	LogPath   string   `json:"log_path"`
	Rotation  string   `json:"rotation"`
	HashChain bool     `json:"hash_chain"`
	Include   []string `json:"include"`
}

// ── Project ─────────────────────────────────────────────────────────────────

type ProjectCfg struct {
	APIKeyRef   string            `json:"api_key"`
	ProjectKey  string            `json:"project_key"`
	ProjectName string            `json:"project_name"`
	Board       BoardCfg          `json:"board"`
	IssueTypes  map[string]*IssueTypeCfg `json:"issue_types"`
	CustomFields map[string]string `json:"custom_fields"`
	Naming      NamingCfg         `json:"naming"`
	Transitions TransitionCfg     `json:"transitions"`
	Templates   map[string]string `json:"templates"` // issue_type → template_library key
	DocPack     DocPackCfg        `json:"doc_pack"`
	Hooks       HooksCfg          `json:"hooks"`
	Priorities  PriorityCfg       `json:"priorities"`
	Assignee    AssigneeCfg       `json:"default_assignee"`
	Operational OperationalCfg    `json:"operational"`
}

// ── Board ───────────────────────────────────────────────────────────────────

type BoardCfg struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Style   string     `json:"style"` // scrum | kanban
	Sprint  SprintCfg  `json:"sprint"`
	Columns []ColumnCfg `json:"columns"`
}

type SprintCfg struct {
	DurationDays  int    `json:"duration_days"`
	StartDay      string `json:"start_day"`
	Naming        string `json:"naming"`
	AutoCreateNew bool   `json:"auto_create_new"`
}

type ColumnCfg struct {
	Name     string   `json:"name"`
	Statuses []string `json:"statuses"`
}

// ── Issue Types ─────────────────────────────────────────────────────────────

type IssueTypeCfg struct {
	Workflow []string       `json:"workflow"`
	Fields   IssueFieldsCfg `json:"fields"`
}

type IssueFieldsCfg struct {
	StoryPoints    bool `json:"story_points"`
	Sprint         bool `json:"sprint"`
	DatesRequired  bool `json:"dates_required"`
	ParentRequired bool `json:"parent_required"`
}

// ── Naming ──────────────────────────────────────────────────────────────────

type NamingCfg struct {
	Pattern         string   `json:"pattern"`
	Categories      []string `json:"categories"`
	Scopes          []string `json:"scopes"`
	LabelsUppercase bool     `json:"labels_uppercase"`
}

// ── Transitions ─────────────────────────────────────────────────────────────

type TransitionCfg struct {
	Rules                    TransitionRules      `json:"rules"`
	ResolutionCommentReq     bool                 `json:"resolution_comment_required"`
	RequiredFieldsOnTransit  map[string][]string  `json:"required_fields_on_transition"`
}

type TransitionRules struct {
	StoryRequiresEpicInProgress    bool `json:"story_requires_epic_in_progress"`
	EpicDoneRequiresAllChildrenDone bool `json:"epic_done_requires_all_children_done"`
	NoSkipStates                   bool `json:"no_skip_states"`
}

// ── Doc-Pack ────────────────────────────────────────────────────────────────

type DocPackCfg struct {
	RequiredSP  int    `json:"required_sp"`
	ReadmeStyle string `json:"readme_style"`
	Timing      string `json:"timing"` // on_in_progress | on_review | on_done
	AutoAttach  bool   `json:"auto_attach"`
}

// ── Hooks ───────────────────────────────────────────────────────────────────

type HooksCfg struct {
	OnCommit         OnCommitHook         `json:"on_commit"`
	OnTransitionDone OnTransitionDoneHook `json:"on_transition_done"`
}

type OnCommitHook struct {
	AutoDocPack bool   `json:"auto_doc_pack"`
	TicketRegex string `json:"ticket_regex"`
}

type OnTransitionDoneHook struct {
	VerifyChildren  bool `json:"verify_children"`
	RequireDocPack  bool `json:"require_doc_pack"`
}

// ── Priorities & Assignee ───────────────────────────────────────────────────

type PriorityCfg struct {
	Mapping map[string]string `json:"mapping"` // P0→Highest, P1→High, ...
	Default string            `json:"default"`
}

type AssigneeCfg struct {
	Type  string `json:"type"`  // user | unassigned | current_user | project_lead
	Value string `json:"value"`
}

// ── Operational ─────────────────────────────────────────────────────────────

type OperationalCfg struct {
	Enabled            bool    `json:"enabled"`
	LastHealthCheckOK  *string `json:"last_health_check_ok"`
	ErrorCount         int     `json:"error_count"`
}

// ── Config loading ──────────────────────────────────────────────────────────

var defaultConfigPath = filepath.Join(os.Getenv("HOME"), ".neo", "plugins", "jira.json")

func loadPluginConfig(path string) (*PluginConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg PluginConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse jira.json: %w", err)
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("validate jira.json: %w", err)
	}
	return &cfg, nil
}

func validateConfig(cfg *PluginConfig) error {
	if cfg.Version < 1 {
		return errors.New("version must be >= 1")
	}
	if len(cfg.Projects) == 0 {
		return errors.New("at least one project is required")
	}
	if cfg.ActiveProject == "" {
		return errors.New("active_project is required")
	}
	if _, ok := cfg.Projects[cfg.ActiveProject]; !ok {
		return fmt.Errorf("active_project %q not found in projects", cfg.ActiveProject)
	}
	// Normalize workspace_mapping keys to lowercase at load time.
	if cfg.WorkspaceMapping != nil {
		normalized := make(map[string]string, len(cfg.WorkspaceMapping))
		for k, v := range cfg.WorkspaceMapping {
			normalized[strings.TrimSpace(strings.ToLower(k))] = v
		}
		cfg.WorkspaceMapping = normalized
	}

	for name, proj := range cfg.Projects {
		if proj.APIKeyRef == "" {
			return fmt.Errorf("project %q: api_key is required", name)
		}
		if _, ok := cfg.APIKeys[proj.APIKeyRef]; !ok {
			return fmt.Errorf("project %q: api_key %q not found in api_keys", name, proj.APIKeyRef)
		}
		if proj.ProjectKey == "" {
			return fmt.Errorf("project %q: project_key is required", name)
		}
		if len(proj.IssueTypes) == 0 {
			return fmt.Errorf("project %q: at least one issue_type is required", name)
		}
		for itName, it := range proj.IssueTypes {
			if len(it.Workflow) < 2 {
				return fmt.Errorf("project %q: issue_type %q: workflow must have >= 2 statuses", name, itName)
			}
		}
	}
	return nil
}

// resolveToken resolves the effective token for an API key.
// Priority: auth.token (inline) > auth.token_ref (env:VAR | file:path) > error.
func resolveToken(key *APIKey) (string, error) {
	if key.Auth.Token != nil && *key.Auth.Token != "" {
		return *key.Auth.Token, nil
	}
	if key.Auth.TokenRef != nil && *key.Auth.TokenRef != "" {
		ref := *key.Auth.TokenRef
		switch {
		case strings.HasPrefix(ref, "env:"):
			v := os.Getenv(strings.TrimPrefix(ref, "env:"))
			if v == "" {
				return "", fmt.Errorf("token_ref %q: environment variable is empty or not set", ref)
			}
			return v, nil
		case strings.HasPrefix(ref, "file:"):
			data, err := os.ReadFile(strings.TrimPrefix(ref, "file:"))
			if err != nil {
				return "", fmt.Errorf("token_ref %q: %w", ref, err)
			}
			return strings.TrimSpace(string(data)), nil
		default:
			return "", fmt.Errorf("token_ref %q: unsupported scheme (use env: or file:)", ref)
		}
	}
	return "", errors.New("no token or token_ref configured")
}

// ── Workspace resolver ──────────────────────────────────────────────────────

// resolveProject maps a workspace ID to a project config.
// Falls back to "default" mapping, then to active_project.
func (cfg *PluginConfig) resolveProject(workspaceID string) (*ProjectCfg, string, error) {
	wsID := strings.TrimSpace(strings.ToLower(workspaceID))

	if wsID != "" {
		if projName, ok := cfg.WorkspaceMapping[wsID]; ok {
			if proj, ok := cfg.Projects[projName]; ok {
				return proj, projName, nil
			}
			return nil, "", fmt.Errorf("workspace_mapping %q points to unknown project %q", wsID, projName)
		}
	}
	if projName, ok := cfg.WorkspaceMapping["default"]; ok {
		if proj, ok := cfg.Projects[projName]; ok {
			return proj, projName, nil
		}
	}
	if proj, ok := cfg.Projects[cfg.ActiveProject]; ok {
		return proj, cfg.ActiveProject, nil
	}
	return nil, "", fmt.Errorf("cannot resolve workspace %q: no mapping, no default, no active_project", workspaceID)
}

// ── Dual-boot: legacy fallback ──────────────────────────────────────────────

// buildStateFromLegacy creates a single-tenant state from env vars (current contract).
// Used when jira.json does not exist.
func buildStateFromLegacy() (*state, error) {
	token := os.Getenv("JIRA_TOKEN")
	email := os.Getenv("JIRA_EMAIL")
	domain := os.Getenv("JIRA_DOMAIN")
	if token == "" || email == "" || domain == "" {
		return nil, errors.New("JIRA_TOKEN, JIRA_EMAIL and JIRA_DOMAIN are required (legacy mode)")
	}
	c, err := jira.NewClient(jira.Config{
		Domain:  domain,
		Email:   email,
		Token:   token,
		BaseURL: os.Getenv("JIRA_BASE_URL"), // Area 3.2.A: integration test override
	})
	if err != nil {
		return nil, fmt.Errorf("build legacy client: %w", err)
	}
	auditPath := pluginAuditPath()
	logger, err := auth.OpenAuditLog(auditPath)
	if err != nil {
		return nil, fmt.Errorf("open audit log %s: %w", auditPath, err)
	}
	return &state{
		client:      c,
		activeSpace: os.Getenv("JIRA_ACTIVE_SPACE"),
		activeBoard: os.Getenv("JIRA_ACTIVE_BOARD"),
		audit:       logger,
	}, nil
}

// buildStateFromConfig creates state from the multi-tenant jira.json.
// For now it resolves the active project and creates a single client.
// Epic 2 will add per-tenant client pool.
func buildStateFromConfig(cfg *PluginConfig) (*state, error) {
	proj, _, err := cfg.resolveProject("")
	if err != nil {
		return nil, fmt.Errorf("resolve active project: %w", err)
	}
	key, ok := cfg.APIKeys[proj.APIKeyRef]
	if !ok {
		return nil, fmt.Errorf("api_key %q not found", proj.APIKeyRef)
	}
	token, err := resolveToken(key)
	if err != nil {
		return nil, fmt.Errorf("resolve token for %q: %w", proj.APIKeyRef, err)
	}
	c, err := jira.NewClient(jira.Config{
		Domain:  key.Domain,
		Email:   key.Email,
		Token:   token,
		BaseURL: os.Getenv("JIRA_BASE_URL"), // Area 3.2.A: integration test override
	})
	if err != nil {
		return nil, fmt.Errorf("build client: %w", err)
	}
	auditPath := pluginAuditPath()
	if cfg.Audit.LogPath != "" {
		auditPath = os.ExpandEnv(strings.ReplaceAll(cfg.Audit.LogPath, "~", os.Getenv("HOME")))
	}
	logger, err := auth.OpenAuditLog(auditPath)
	if err != nil {
		return nil, fmt.Errorf("open audit log %s: %w", auditPath, err)
	}
	return &state{
		client:      c,
		activeSpace: proj.ProjectKey,
		activeBoard: proj.Board.ID,
		audit:       logger,
		pluginCfg:   cfg,
		pool:        newClientPool(),
		ctx:         context.Background(),
	}, nil
}

// ── Migration ───────────────────────────────────────────────────────────────

// legacyJiraEntry is the projection of a single jira-provider row in
// the host's credentials.json that's relevant for migration.
type legacyJiraEntry struct {
	Token    string
	Email    string
	Domain   string
	TenantID string
}

// readJiraCredEntry parses ~/.neo/credentials.json and returns the
// jira-provider entry if present, plus the source path and raw bytes
// (needed by the migration backup step). [CC refactor — split out of
// migrateToPluginConfig]
func readJiraCredEntry(home string) (entry *legacyJiraEntry, credPath string, raw []byte, err error) {
	credPath = filepath.Join(home, ".neo", "credentials.json")
	raw, err = os.ReadFile(credPath)
	if err != nil {
		return nil, credPath, nil, fmt.Errorf("read credentials.json: %w", err)
	}
	var creds struct {
		Entries []struct {
			Provider string `json:"provider"`
			Token    string `json:"token"`
			Email    string `json:"email"`
			Domain   string `json:"domain"`
			TenantID string `json:"tenant_id"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &creds); err != nil {
		return nil, credPath, raw, fmt.Errorf("parse credentials.json: %w", err)
	}
	for _, e := range creds.Entries {
		if e.Provider == "jira" {
			return &legacyJiraEntry{Token: e.Token, Email: e.Email, Domain: e.Domain, TenantID: e.TenantID}, credPath, raw, nil
		}
	}
	return nil, credPath, raw, errors.New("no jira entry in credentials.json")
}

// resolveLegacyContextEnv resolves the active jira space + board.
// Tries env vars first (JIRA_ACTIVE_SPACE / JIRA_ACTIVE_BOARD), then
// falls back to reading ~/.neo/contexts.json. Returns "UNKNOWN" for
// space when nothing resolves so the migrated config is still valid.
// [CC refactor]
func resolveLegacyContextEnv(home string) (space, board string) {
	space = os.Getenv("JIRA_ACTIVE_SPACE")
	board = os.Getenv("JIRA_ACTIVE_BOARD")
	if space != "" && board != "" {
		return space, board
	}
	ctxPath := filepath.Join(home, ".neo", "contexts.json")
	ctxData, err := os.ReadFile(ctxPath)
	if err != nil {
		if space == "" {
			space = "UNKNOWN"
		}
		return space, board
	}
	var ctxFile struct {
		Contexts []struct {
			Provider string `json:"provider"`
			SpaceID  string `json:"space_id"`
			BoardID  string `json:"board_id"`
		} `json:"contexts"`
	}
	if json.Unmarshal(ctxData, &ctxFile) == nil {
		for _, c := range ctxFile.Contexts {
			if c.Provider != "jira" {
				continue
			}
			if space == "" {
				space = c.SpaceID
			}
			if board == "" {
				board = c.BoardID
			}
			break
		}
	}
	if space == "" {
		space = "UNKNOWN"
	}
	return space, board
}

// migrateToPluginConfig detects legacy credentials+contexts and generates jira.json.
// Creates a backup of the legacy files before writing.
func migrateToPluginConfig(destPath string) (*PluginConfig, error) {
	home := os.Getenv("HOME")
	jiraEntry, credPath, credData, err := readJiraCredEntry(home)
	if err != nil {
		return nil, err
	}
	keyName := "default"
	if jiraEntry.TenantID != "" {
		keyName = jiraEntry.TenantID
	}
	space, boardID := resolveLegacyContextEnv(home)

	cfg := &PluginConfig{
		Version:       pluginConfigVersion,
		ActiveProject: keyName,
		APIKeys: map[string]*APIKey{
			keyName: {
				Domain: jiraEntry.Domain,
				Email:  jiraEntry.Email,
				Auth: AuthConfig{
					Type:  "PAT",
					Token: &jiraEntry.Token,
				},
				RateLimit: RateLimit{
					MaxPerMinute:    300,
					Concurrency:     5,
					RetryOn429:      true,
					BackoffStrategy: "exponential",
				},
			},
		},
		WorkspaceMapping: map[string]string{
			"default": keyName,
		},
		Audit: AuditConfig{
			LogPath:   "~/.neo/audit-jira.log",
			Rotation:  "daily",
			HashChain: true,
			Include:   []string{"transition", "create_issue", "assignee_change", "field_update", "doc_pack"},
		},
		TemplateLibrary: map[string]*Template{},
		Projects: map[string]*ProjectCfg{
			keyName: {
				APIKeyRef:   keyName,
				ProjectKey:  space,
				ProjectName: space,
				Board: BoardCfg{
					ID:    boardID,
					Style: "scrum",
				},
				IssueTypes: map[string]*IssueTypeCfg{
					"epic":  {Workflow: []string{"Backlog", "In Progress", "Done"}},
					"story": {Workflow: []string{"Backlog", "Selected for Development", "In Progress", "REVIEW", "READY TO DEPLOY", "Done"}},
					"bug":   {Workflow: []string{"Backlog", "In Progress", "REVIEW", "Done"}},
					"task":  {Workflow: []string{"Backlog", "In Progress", "Done"}},
				},
				CustomFields: map[string]string{
					"story_points":     "customfield_10016",
					"story_points_alt": "customfield_10038",
				},
				Naming: NamingCfg{
					Pattern:         "[{category}][{scope}] {text}",
					Categories:      []string{"FEATURE", "BUG", "FIX", "ARCHITECTURE", "CHORE", "DOCS"},
					Scopes:          []string{"ENGINE", "UI", "API", "DB", "INFRA", "SHARED"},
					LabelsUppercase: true,
				},
				Transitions: TransitionCfg{
					Rules: TransitionRules{NoSkipStates: true},
					ResolutionCommentReq: true,
				},
				Templates:   map[string]string{},
				DocPack:     DocPackCfg{RequiredSP: 3, ReadmeStyle: "non-technical", Timing: "on_in_progress", AutoAttach: true},
				Priorities:  PriorityCfg{Mapping: map[string]string{"P0": "Highest", "P1": "High", "P2": "Medium", "P3": "Low", "P4": "Lowest"}, Default: "P2"},
				Assignee:    AssigneeCfg{Type: "user", Value: jiraEntry.Email},
				Operational: OperationalCfg{Enabled: true},
			},
		},
	}

	// Backup legacy files
	ts := time.Now().Format("20060102T150405")
	backupCred := credPath + ".legacy.backup." + ts
	if err := os.WriteFile(backupCred, credData, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "plugin-jira: warning: backup credentials.json failed: %v\n", err)
	}

	// Write jira.json
	if err := os.MkdirAll(filepath.Dir(destPath), 0700); err != nil {
		return nil, fmt.Errorf("create plugins dir: %w", err)
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(destPath, out, 0600); err != nil {
		return nil, fmt.Errorf("write jira.json: %w", err)
	}
	fmt.Fprintf(os.Stderr, "plugin-jira: migrated legacy config → %s (backup: %s)\n", destPath, backupCred)
	return cfg, nil
}

// ── Config holder with atomic swap ──────────────────────────────────────────

type configHolder struct {
	mu  sync.RWMutex
	cfg *PluginConfig
}

func (h *configHolder) get() *PluginConfig {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

func (h *configHolder) set(cfg *PluginConfig) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg = cfg
}
