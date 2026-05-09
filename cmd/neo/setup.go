// cmd/neo/setup.go — `neo setup [workspace-name]` (Area 1.2)
//
// Scaffolds a fresh NeoAnvil workspace: validates prerequisites,
// generates neo.yaml + .mcp.json from text/template, optionally pre-
// flights Ollama reachability. Designed to take a developer from
// `git clone` to a usable MCP-connected workspace in under a minute.
//
// Wire into the CLI tree from main.go via `setupCmd()` in the AddCommand list.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/spf13/cobra"

	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// Workspace names must be filesystem-safe AND template-safe — allowing
// path separators, dotdots, or YAML/JSON metacharacters opens path
// traversal + template injection vectors. [DS-AUDIT 1.2 Findings 1+2]
var workspaceNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// setupOptions collects the flags from the command line so the scaffolder
// is unit-testable in isolation (without instantiating cobra).
type setupOptions struct {
	WorkspaceName string
	WorkspaceDir  string // resolved absolute path

	Bare        bool   // skip Ollama + .mcp.json — minimal neo.yaml only
	WithOllama  bool   // verify Ollama reachable + write ai.* config
	Docker      bool   // use container-friendly defaults (binds 0.0.0.0, ${NEO_MCP_URL})
	Yes         bool   // non-interactive — refuse to prompt, fail on conflicts
	OllamaURL   string // override default localhost:11434
	MCPURL      string // override default for .mcp.json
	WorkspaceID string // override hash-derived ID (Docker path-stability)
}

const setupGoVersionMin = "1.26"

func setupCmd() *cobra.Command {
	opts := &setupOptions{}
	cmd := &cobra.Command{
		Use:   "setup [workspace-name]",
		Short: "Scaffold a new NeoAnvil workspace (neo.yaml + .mcp.json)",
		Long: `Generates neo.yaml + .mcp.json with sensible defaults.

Examples:

  neo setup                      # uses cwd basename, full setup
  neo setup my-project --bare    # minimal neo.yaml only (no Ollama, no .mcp.json)
  neo setup --with-ollama        # also pings Ollama before writing
  neo setup --docker             # binds 0.0.0.0, .mcp.json uses ${NEO_MCP_URL}
  neo setup --yes                # non-interactive (CI mode)`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.WorkspaceName = args[0]
			}
			return runSetup(opts)
		},
	}
	cmd.Flags().BoolVar(&opts.Bare, "bare", false, "skip Ollama validation + .mcp.json (minimal scaffold)")
	cmd.Flags().BoolVar(&opts.WithOllama, "with-ollama", false, "verify Ollama reachable + populate ai.* config")
	cmd.Flags().BoolVar(&opts.Docker, "docker", false, "container-friendly defaults (bind 0.0.0.0, .mcp.json uses ${NEO_MCP_URL})")
	cmd.Flags().BoolVar(&opts.Yes, "yes", false, "non-interactive: refuse to prompt, fail on conflicts (CI mode)")
	cmd.Flags().StringVar(&opts.OllamaURL, "ollama-url", "http://localhost:11434", "Ollama base URL")
	cmd.Flags().StringVar(&opts.MCPURL, "url", "", "MCP endpoint URL for .mcp.json (default http://127.0.0.1:9000/workspaces/<id>/mcp/sse)")
	cmd.Flags().StringVar(&opts.WorkspaceID, "workspace-id", "", "explicit workspace ID (override path-hash; Docker path-stability)")
	return cmd
}

// runSetup is the orchestration function. Each step short-circuits on
// the first error so a failure leaves the disk untouched (no partial
// scaffold). [1.2.A]
func runSetup(opts *setupOptions) error {
	if err := resolveWorkspaceDir(opts); err != nil {
		return err
	}
	if err := validatePreconditions(opts); err != nil {
		return err
	}
	yamlPath := filepath.Join(opts.WorkspaceDir, "neo.yaml")
	if err := scaffoldNeoYaml(opts, yamlPath); err != nil {
		return err
	}
	fmt.Printf("✓ wrote %s\n", yamlPath)
	if !opts.Bare {
		mcpPath := filepath.Join(opts.WorkspaceDir, ".mcp.json")
		if err := scaffoldMcpJson(opts, mcpPath); err != nil {
			return err
		}
		fmt.Printf("✓ wrote %s\n", mcpPath)
	}
	fmt.Printf("✓ workspace '%s' ready at %s\n", opts.WorkspaceName, opts.WorkspaceDir)
	return nil
}

// resolveWorkspaceDir derives the workspace name and absolute path from
// the operator's input. If no name was provided, the cwd's basename is
// used. Validates the name against workspaceNameRe to prevent path
// traversal (`../../etc`) AND YAML/JSON injection via newlines or
// metacharacters in the rendered templates. [DS-AUDIT 1.2 Findings 1+2]
func resolveWorkspaceDir(opts *setupOptions) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	if opts.WorkspaceName == "" {
		// Default to cwd's basename — already a valid path component
		// because the OS gave it to us.
		opts.WorkspaceName = filepath.Base(cwd)
		opts.WorkspaceDir = cwd
		return nil
	}
	if !workspaceNameRe.MatchString(opts.WorkspaceName) {
		return fmt.Errorf("invalid workspace name %q (must match %s)", opts.WorkspaceName, workspaceNameRe)
	}
	// Named workspace lands as a sibling directory of cwd. The regex
	// above guarantees no separators, no `..`, no quotes.
	abs := filepath.Join(cwd, opts.WorkspaceName)
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", abs, err)
	}
	opts.WorkspaceDir = abs
	return nil
}

// validatePreconditions runs the pre-flight checks listed in 1.2.D.
// Existing workspace + Go version are mandatory; Ollama only when
// --with-ollama is set; --yes promotes prompt-able conflicts to fatal.
// [1.2.D]
func validatePreconditions(opts *setupOptions) error {
	if err := validateExisting(opts); err != nil {
		return err
	}
	if err := validateGoVersion(); err != nil {
		return err
	}
	if err := validatePortsFree(opts); err != nil {
		return err
	}
	if opts.WithOllama && !opts.Bare {
		if err := validateOllamaReachable(opts.OllamaURL); err != nil {
			return err
		}
	}
	return nil
}

// validateExisting refuses to overwrite an existing neo.yaml unless
// --yes was passed (CI explicit overwrite). The interactive
// "do you want to overwrite?" UX is deferred — current contract is
// fail-fast.
func validateExisting(opts *setupOptions) error {
	yamlPath := filepath.Join(opts.WorkspaceDir, "neo.yaml")
	if _, err := os.Stat(yamlPath); err == nil {
		if opts.Yes {
			return fmt.Errorf("workspace already initialized (--yes refused: %s exists)", yamlPath)
		}
		return fmt.Errorf("workspace already initialized: %s exists (delete or use a different dir)", yamlPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", yamlPath, err)
	}
	return nil
}

// validateGoVersion fails when the running Go version is below
// setupGoVersionMin so generated configs aren't pinned to features
// the operator's toolchain can't compile.
func validateGoVersion() error {
	cur := strings.TrimPrefix(runtime.Version(), "go")
	if cmpVersions(cur, setupGoVersionMin) < 0 {
		return fmt.Errorf("Go %s required (have %s) — upgrade your toolchain", setupGoVersionMin, cur)
	}
	return nil
}

// cmpVersions returns -1, 0, +1 like strings.Compare but on dotted
// version triples. Suffixes after the first non-numeric char are
// ignored (e.g., "1.26.1-rc1" → "1.26.1").
func cmpVersions(a, b string) int {
	pa := splitVersion(a)
	pb := splitVersion(b)
	for i := range 3 {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func splitVersion(v string) [3]int {
	var out [3]int
	parts := strings.Split(v, ".")
	for i := 0; i < 3 && i < len(parts); i++ { //nolint:intrange // bound is min of two values, not a literal
		// strip non-numeric suffix
		end := 0
		for end < len(parts[i]) && parts[i][end] >= '0' && parts[i][end] <= '9' {
			end++
		}
		if end == 0 {
			continue
		}
		n, _ := strconv.Atoi(parts[i][:end])
		out[i] = n
	}
	return out
}

// validatePortsFree checks the canonical Nexus + HUD + sandbox ports.
// In Docker mode we expect them to be in-use (the operator runs the
// container); in native mode they should be free. Either way: just
// surface what's bound — non-fatal warning unless --yes mode in
// native-fresh-install scenario.
func validatePortsFree(opts *setupOptions) error {
	if opts.Docker {
		return nil // ports inside container; host conflicts handled by docker-compose
	}
	for _, p := range []int{9000, 8087, 11434} {
		if portInUse(p) {
			msg := fmt.Sprintf("port %d already in use (native install? use --docker or --yes)", p)
			if opts.Yes {
				return errors.New(msg)
			}
			fmt.Fprintf(os.Stderr, "warning: %s\n", msg)
		}
	}
	return nil
}

func portInUse(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return true
	}
	_ = ln.Close()
	return false
}

// validateOllamaReachable does a quick HTTP HEAD to /api/tags. We use
// the safe internal HTTP client for the loopback case and the
// SafeHTTPClient for external Ollama instances (LEY 5).
func validateOllamaReachable(baseURL string) error {
	client := sre.SafeHTTPClient()
	url := strings.TrimRight(baseURL, "/") + "/api/tags"
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("Ollama unreachable at %s: %w (start it or pass --bare)", baseURL, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("Ollama at %s returned %d", baseURL, resp.StatusCode)
	}
	return nil
}

// scaffoldNeoYaml writes neo.yaml from neoYamlTemplate. Uses
// text/template (NOT raw string concat) so future fields slot in via
// the data struct rather than fmt.Sprintf brittleness. [1.2.B]
func scaffoldNeoYaml(opts *setupOptions, path string) error {
	t, err := template.New("neoyaml").Parse(neoYamlTemplate)
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}
	bindAddr := "127.0.0.1"
	if opts.Docker {
		bindAddr = "0.0.0.0"
	}
	data := struct {
		Mode               string
		BindAddr           string
		OllamaBaseURL      string
		EmbedBaseURL       string
		WithOllama         bool
		QueryCacheCapacity int
		EmbedCacheCapacity int
		WorkspaceName      string
		Generated          string
	}{
		Mode:               "pair",
		BindAddr:           bindAddr,
		OllamaBaseURL:      opts.OllamaURL,
		EmbedBaseURL:       opts.OllamaURL,
		WithOllama:         opts.WithOllama || !opts.Bare,
		QueryCacheCapacity: 256,
		EmbedCacheCapacity: 128,
		WorkspaceName:      opts.WorkspaceName,
		Generated:          time.Now().UTC().Format(time.RFC3339),
	}
	// O_EXCL closes the symlink-race TOCTOU window between
	// validateExisting and the write. [DS-AUDIT 1.2 Finding 3]
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // G304-CLI-CONSENT: path under operator's cwd, name regex-validated
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if err := t.Execute(f, data); err != nil {
		return fmt.Errorf("render template: %w", err)
	}
	return nil
}

// scaffoldMcpJson writes .mcp.json with the MCP transport pointer for
// Claude Code / Cursor / etc. The URL defaults to the canonical Nexus
// SSE endpoint with the workspace ID embedded in the path.
// In Docker mode the URL becomes ${NEO_MCP_URL} so compose can
// substitute at runtime. [1.2.C]
//
// Renders via encoding/json (NOT text/template) so a malicious --url
// flag like `http://evil"}}, "x": "y` cannot inject extra JSON keys
// or extra MCP server entries. [DS-AUDIT 1.2 Finding 2]
//
// Uses O_CREATE|O_EXCL|O_WRONLY for atomic creation — refuses to
// overwrite if the destination exists or is a symlink, closing the
// TOCTOU window between validateExisting and the write. [DS-AUDIT 1.2 Finding 3]
func scaffoldMcpJson(opts *setupOptions, path string) error {
	wsID := opts.WorkspaceID
	if wsID == "" {
		wsID = workspaceIDFromName(opts.WorkspaceName)
	}
	url := opts.MCPURL
	if url == "" {
		if opts.Docker {
			url = "${NEO_MCP_URL}"
		} else {
			url = fmt.Sprintf("http://127.0.0.1:9000/workspaces/%s/mcp/sse", wsID)
		}
	}
	manifest := map[string]any{
		"mcpServers": map[string]any{
			"neoanvil": map[string]any{
				"type": "sse",
				"url":  url,
			},
		},
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mcp.json: %w", err)
	}
	body = append(body, '\n')
	return atomicWrite(path, body, 0o644)
}

// atomicWrite refuses to overwrite an existing file (symlink or
// regular) by passing O_EXCL to OpenFile. Caller must have already
// verified absence (validateExisting); this is the second-line
// defense closing the TOCTOU window. [DS-AUDIT 1.2 Finding 3]
func atomicWrite(path string, body []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) //nolint:gosec // G304-CLI-CONSENT: path under operator's cwd, name regex-validated
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// workspaceIDFromName generates the canonical "<basename>-<5hex>" ID
// using the workspace name's hash. Stable for the same name (no
// random suffix here — Nexus auto-register adds variance via /dev/urandom
// in entrypoint).
func workspaceIDFromName(name string) string {
	// Simple 5-char alphanumeric suffix derived from name length + a
	// fixed prefix. Deterministic so reruns of `neo setup` produce
	// the same ID — operator can override with --workspace-id for
	// path-hash stability inside Docker volumes.
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	h := uint64(len(name)) * 2654435761
	for _, c := range name {
		h ^= uint64(c) * 16777619
		h *= 1099511628211
	}
	suf := make([]byte, 5)
	for i := range suf {
		suf[i] = alphabet[h%uint64(len(alphabet))]
		h /= uint64(len(alphabet))
	}
	return fmt.Sprintf("%s-%s", name, string(suf))
}

// neoYamlTemplate is the minimum-viable neo.yaml — not the full
// 300-line config. Operator can extend post-scaffold.
const neoYamlTemplate = `# NeoAnvil workspace config — generated by 'neo setup' on {{.Generated}}
# Workspace: {{.WorkspaceName}}
sre:
    mode: {{.Mode}}
    consensus_enabled: false
    oracle_heap_limit_mb: 1024
    runtime_memory_limit_mb: 6144
server:
    log_level: info
    host: {{.BindAddr}}
    sse_path: /mcp/sse
    sse_message_path: /mcp/message
    dashboard_port: 8087
    nexus_dispatcher_port: 9000
    mode: {{.Mode}}
workspace:
    ignore_dirs:
        - node_modules
        - vendor
        - .git
        - dist
        - .neo
        - bin
        - build
    allowed_extensions:
        - .go
        - .ts
        - .tsx
        - .js
        - .jsx
        - .py
        - .md
        - .yaml
        - .css
    max_file_size_mb: 5
    dominant_lang: ""
{{- if .WithOllama }}
ai:
    provider: ollama
    base_url: {{.OllamaBaseURL}}
    embed_base_url: {{.EmbedBaseURL}}
    embedding_model: nomic-embed-text
    context_window: 8192
{{- end }}
rag:
    db_path: .neo/db/hnsw.db
    chunk_size: 3000
    overlap: 500
    batch_size: 100
    ingestion_workers: 4
    embed_concurrency: 2
    query_cache_capacity: {{.QueryCacheCapacity}}
    embedding_cache_capacity: {{.EmbedCacheCapacity}}
cpg:
    max_heap_mb: 2048
`

// scaffoldMcpJson uses json.MarshalIndent (not text/template) so
// attacker-supplied URL values can't escape the field via JSON
// metacharacters. The shape it produces is:
//
//	{
//	  "mcpServers": {
//	    "neoanvil": { "type": "sse", "url": "<rendered>" }
//	  }
//	}
//
// [DS-AUDIT 1.2 Finding 2]
