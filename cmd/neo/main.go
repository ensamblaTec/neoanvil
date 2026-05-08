// cmd/neo — SRE CLI for the NeoAnvil orchestrator.
// [SRE-33] Provides direct operator control: status, audit, recall, rem, chaos, doctor.
// [SRE-34] Adds: workspace, diagnose, heal, debt-export commands.
// Communicates with the running neoanvil daemon via its HTTP API (dashboard port).
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	bolt "go.etcd.io/bbolt"

	"github.com/ensamblatec/neoanvil/pkg/auth"
	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

const (
	version     = "6.2.0"
	defaultPort = 8087
)

// daemonInfo mirrors the JSON written to .neo/daemon.pid by the neoanvil server.
// [SRE-33.1.2]
type daemonInfo struct {
	PID     int    `json:"pid"`
	Port    int    `json:"port"`
	Mode    string `json:"mode"`
	Started string `json:"started"`
}

func main() {
	root := &cobra.Command{
		Use:     "neo",
		Short:   "NeoAnvil SRE CLI — direct operator interface to the NeoAnvil daemon",
		Version: version,
	}
	root.AddCommand(
		statusCmd(),
		auditCmd(),
		recallCmd(),
		remCmd(),
		chaosCmd(),
		doctorCmd(),
		workspaceCmd(),  // [SRE-34.1.2]
		diagnoseCmd(),   // [SRE-34.2.2]
		healCmd(),       // [SRE-34.2.2]
		debtExportCmd(), // [SRE-34.3.3]
		queryCmd(),      // [SRE-47] Sovereign Voice NLP CLI
		bootstrapCmd(),  // [SRE-59.1] Pack consciousness into portable archive
		merkleCmd(),     // [SRE-59.2] Brain sync fingerprint via Merkle root
		hotreloadCmd(),  // [SRE-59.3] Trigger hot-rebuild of neo-mcp binary
		askCmd(),        // [SRE-95.B.1] Voice of the Leviathan — natural language CLI
		chatCmd(),       // [SRE-95.B.2] Interactive REPL
		evolveCmd(),       // [SRE-93.B] Darwin Engine — genetic code evolution CLI
		initProjectCmd(), // [PILAR-XXXI] Project federation: detect monorepo, create .neo-project/
		loginCmd(),       // [PILAR-XXXIII] Store API key/TenantID in ~/.neo/credentials.json
		spaceCmd(),       // [PILAR-XXIII / 124.8] Active space/board per provider
		jiraIDCmd(),      // [134.B.4] Resolve master_plan epic ID → Jira MCPI-N
		brainCmd(),       // [PILAR-XXVI / 135.D] Brain Portable: push/pull encrypted snapshots
	)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// findWorkspace walks up from cwd to find the directory containing neo.yaml.
func findWorkspace() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "neo.yaml")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

// readDaemonInfo reads .neo/daemon.pid to discover the running daemon's HTTP port.
// [SRE-33.1.2]
func readDaemonInfo(workspace string) (*daemonInfo, error) {
	pidPath := filepath.Join(workspace, ".neo", "daemon.pid")
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return nil, fmt.Errorf("daemon not running (no .neo/daemon.pid): %w", err)
	}
	var info daemonInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("corrupt daemon.pid: %w", err)
	}
	if info.Port == 0 {
		info.Port = defaultPort
	}
	return &info, nil
}

func apiURL(port int, path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
}

// [SRE-110.E] CLI helpers always target the loopback daemon — use
// SafeInternalHTTPClient (loopback-only allowlist).
func doGet(url string) ([]byte, error) {
	resp, err := sre.SafeInternalHTTPClient(10).Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func doPost(url string, body any) ([]byte, error) {
	payload, _ := json.Marshal(body)
	resp, err := sre.SafeInternalHTTPClient(30).Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// -- neo status -----------------------------------------------------------

// statusCmd implements `neo status [--rag]`. [SRE-33.1.3 / SRE-35.3.3]
func statusCmd() *cobra.Command {
	var showRAG bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show daemon vitals: version, watts, mode, bouncer state, pending tasks",
		RunE: func(cmd *cobra.Command, args []string) error {
			ws := findWorkspace()
			info, err := readDaemonInfo(ws)
			if err != nil {
				return err
			}
			if showRAG {
				return printRAGStatus(info.Port)
			}
			body, err := doGet(apiURL(info.Port, "/api/status"))
			if err != nil {
				return fmt.Errorf("daemon unreachable at port %d: %w", info.Port, err)
			}
			var st map[string]any
			if err := json.Unmarshal(body, &st); err != nil {
				return err
			}
			stabilizing, _ := st["stabilizing"].(bool)
			stabStr := green("OK")
			if stabilizing {
				stabStr = red("STABILIZING (RAPL>60W)")
			}
			fmt.Printf("NeoAnvil v%v  [%s]\n", st["version"], bold(fmt.Sprintf("%v", st["mode"])))
			fmt.Printf("  Watts     : %.2fW\n", toFloat(st["watts"]))
			fmt.Printf("  Bouncer   : %s\n", stabStr)
			fmt.Printf("  Pending   : %v tasks\n", st["pending"])
			fmt.Printf("  PID       : %d  (started %s)\n", info.PID, info.Started)
			return nil
		},
	}
	cmd.Flags().BoolVar(&showRAG, "rag", false, "Show HNSW graph health, FPI hit-rate, and cognitive drift")
	return cmd
}

// printRAGStatus fetches /api/v1/metrics/rag and renders it. [SRE-35.3.3]
func printRAGStatus(port int) error {
	body, err := doGet(apiURL(port, "/api/v1/metrics/rag"))
	if err != nil {
		return fmt.Errorf("daemon unreachable: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return err
	}
	if e, ok := m["error"].(string); ok {
		return fmt.Errorf("rag metrics: %s", e)
	}

	fmt.Printf("\n%s RAG Memory Health\n\n", bold("▶"))

	// Graph stats
	if gs, ok := m["graph_stats"].(map[string]any); ok {
		fmt.Printf("  Nodes        : %.0f\n", toFloat(gs["total_nodes"]))
		fmt.Printf("  Edges        : %.0f\n", toFloat(gs["total_edges"]))
		fmt.Printf("  Avg edges/node: %.2f\n", toFloat(gs["avg_edges_per_node"]))
		memMB := toFloat(gs["memory_size_bytes"]) / (1024 * 1024)
		diskMB := toFloat(gs["disk_size_bytes"]) / (1024 * 1024)
		fmt.Printf("  RAM          : %.2f MB\n", memMB)
		fmt.Printf("  Disk         : %.2f MB\n", diskMB)
		if warns, ok := gs["workspace_capacity_warnings"].([]any); ok && len(warns) > 0 {
			fmt.Printf("  %s Capacity warning: workspaces near limit: %v\n", yellow("!"), warns)
		}
	}

	// FPI
	if fpi, ok := m["fpi"].(map[string]any); ok {
		hitRate := toFloat(fpi["hit_rate"]) * 100
		hrStr := green(fmt.Sprintf("%.1f%%", hitRate))
		if hitRate < 50 {
			hrStr = red(fmt.Sprintf("%.1f%%", hitRate))
		} else if hitRate < 70 {
			hrStr = yellow(fmt.Sprintf("%.1f%%", hitRate))
		}
		fmt.Printf("\n  Flashback Hit-Rate : %s  (hits=%.0f misses=%.0f)\n",
			hrStr, toFloat(fpi["hits"]), toFloat(fpi["misses"]))
	}

	// Drift
	if d, ok := m["drift"].(map[string]any); ok {
		avgDist := toFloat(d["avg_distance"])
		drifting, _ := d["drifting"].(bool)
		driftStr := green(fmt.Sprintf("%.3f", avgDist))
		if drifting {
			driftStr = red(fmt.Sprintf("%.3f  ⚠ COGNITIVE DRIFT DETECTED", avgDist))
		}
		fmt.Printf("  Avg query dist     : %s  (threshold=%.2f samples=%.0f)\n",
			driftStr, toFloat(d["threshold"]), toFloat(d["samples"]))
	}

	fmt.Println()
	return nil
}

// -- neo audit ------------------------------------------------------------

// auditCmd implements `neo audit <path>`. [SRE-33.2.1]
func auditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "audit <path>",
		Short: "AST audit: CC>15, infinite loops, shadow vars (SRE colors)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ws := findWorkspace()
			info, err := readDaemonInfo(ws)
			if err != nil {
				return err
			}
			abs, _ := filepath.Abs(args[0])
			body, err := doPost(apiURL(info.Port, "/api/audit"), map[string]string{"path": abs})
			if err != nil {
				return fmt.Errorf("daemon unreachable: %w", err)
			}
			var resp map[string]string
			if err := json.Unmarshal(body, &resp); err != nil {
				return err
			}
			if e, ok := resp["error"]; ok {
				return fmt.Errorf("audit error: %s", e)
			}
			report := resp["report"]
			if strings.TrimSpace(report) == "" {
				fmt.Println(green("✅ No issues found"))
				return nil
			}
			for line := range strings.SplitSeq(report, "\n") {
				switch {
				case strings.Contains(line, "❌") || strings.Contains(line, "CC>"):
					fmt.Println(red(line))
				case strings.Contains(line, "✅") || strings.Contains(line, "OK"):
					fmt.Println(green(line))
				default:
					fmt.Println(line)
				}
			}
			return nil
		},
	}
}

// -- neo recall -----------------------------------------------------------

// recallCmd implements `neo recall "<query>"`. [SRE-33.2.2]
func recallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "recall <query>",
		Short: "Search the Memex HNSW for relevant learned patterns",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ws := findWorkspace()
			info, err := readDaemonInfo(ws)
			if err != nil {
				return err
			}
			body, err := doPost(apiURL(info.Port, "/api/recall"), map[string]string{"query": args[0]})
			if err != nil {
				return fmt.Errorf("daemon unreachable: %w", err)
			}
			var resp struct {
				Results []string `json:"results"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				return err
			}
			if len(resp.Results) == 0 {
				fmt.Println("No memories found.")
				return nil
			}
			for i, r := range resp.Results {
				fmt.Printf("%s[%d]%s %s\n", cyan, i+1, reset, r)
			}
			return nil
		},
	}
}

// -- neo rem --------------------------------------------------------------

// remCmd implements `neo rem`. [SRE-33.2.3]
func remCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rem",
		Short: "Force REM consolidation: flush Memex buffer → HNSW long-term memory",
		RunE: func(cmd *cobra.Command, args []string) error {
			ws := findWorkspace()
			info, err := readDaemonInfo(ws)
			if err != nil {
				return err
			}
			body, err := doPost(apiURL(info.Port, "/api/rem"), map[string]string{})
			if err != nil {
				return fmt.Errorf("daemon unreachable: %w", err)
			}
			var resp map[string]string
			if err := json.Unmarshal(body, &resp); err != nil {
				return err
			}
			if e, ok := resp["error"]; ok {
				return fmt.Errorf("REM error: %s", e)
			}
			fmt.Println(green("✅ " + resp["status"]))
			return nil
		},
	}
}

// -- neo chaos ------------------------------------------------------------

// chaosCmd implements `neo chaos --drill <name>`. [SRE-33.3.1]
func chaosCmd() *cobra.Command {
	var drill string
	cmd := &cobra.Command{
		Use:   "chaos",
		Short: "Trigger a chaos drill via the Operator HUD",
		RunE: func(cmd *cobra.Command, args []string) error {
			ws := findWorkspace()
			info, err := readDaemonInfo(ws)
			if err != nil {
				return err
			}
			fmt.Printf("Firing chaos drill [%s%s%s] → port %d ...\n", bold(""), drill, reset, info.Port)
			body, err := doPost(apiURL(info.Port, "/chaos"), map[string]string{"drill": drill})
			if err != nil {
				return fmt.Errorf("daemon unreachable: %w", err)
			}
			var resp map[string]string
			if err := json.Unmarshal(body, &resp); err != nil {
				return err
			}
			fmt.Println(resp["report"])
			return nil
		},
	}
	cmd.Flags().StringVar(&drill, "drill", "default", "Named drill to execute (e.g. 'network', 'disk')")
	return cmd
}

// -- neo doctor -----------------------------------------------------------

// doctorCmd implements `neo doctor`. [SRE-33.3.2]
// Verifies BoltDB integrity and daemon reachability.
func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Self-diagnosis: BoltDB integrity check + WAL health + daemon ping",
		RunE: func(cmd *cobra.Command, args []string) error {
			ws := findWorkspace()
			dbDir := filepath.Join(ws, ".neo", "db")
			entries, err := os.ReadDir(dbDir)
			if err != nil {
				return fmt.Errorf("cannot read %s: %w", dbDir, err)
			}

			allOK := true
			fmt.Printf("\n%s BoltDB integrity check (%s)\n\n", bold("▶"), dbDir)
			for _, e := range entries {
				if filepath.Ext(e.Name()) != ".db" {
					continue
				}
				dbPath := filepath.Join(dbDir, e.Name())
				db, openErr := bolt.Open(dbPath, 0400, &bolt.Options{
					ReadOnly: true,
					Timeout:  2 * time.Second,
				})
				if openErr != nil {
					fmt.Printf("  %s  %-40s  %s\n", red("✘"), e.Name(), openErr.Error())
					allOK = false
					continue
				}
				// Walk all buckets to catch page-level corruption.
				verifyErr := db.View(func(tx *bolt.Tx) error {
					return tx.ForEach(func(name []byte, b *bolt.Bucket) error {
						c := b.Cursor()
						for k, _ := c.First(); k != nil; k, _ = c.Next() {
						}
						return nil
					})
				})
				_ = db.Close()
				if verifyErr != nil {
					fmt.Printf("  %s  %-40s  %s\n", red("✘"), e.Name(), verifyErr.Error())
					allOK = false
				} else {
					fmt.Printf("  %s  %-40s  OK\n", green("✔"), e.Name())
				}
			}

			// Daemon reachability check.
			fmt.Printf("\n%s Daemon check\n\n", bold("▶"))
			info, pidErr := readDaemonInfo(ws)
			if pidErr != nil {
				fmt.Printf("  %s  daemon.pid    not found — daemon offline\n", yellow("?"))
			} else {
				_, pingErr := doGet(apiURL(info.Port, "/api/status"))
				if pingErr != nil {
					fmt.Printf("  %s  daemon        port %d unreachable\n", red("✘"), info.Port)
					allOK = false
				} else {
					fmt.Printf("  %s  daemon        port %d  mode=%s  pid=%d\n",
						green("✔"), info.Port, info.Mode, info.PID)
				}
			}

			// [363.A] Federation integrity checks.
			if !checkFederationIntegrity(ws) {
				allOK = false
			}

			fmt.Println()
			if allOK {
				fmt.Println(green("✅  All systems healthy"))
			} else {
				fmt.Println(red("❌  Issues detected — run `neoanvil` logs for details"))
				os.Exit(1)
			}
			return nil
		},
	}
}

// checkFederationIntegrity runs two federation-level checks:
//  1. Orphan workspaces: entries in ~/.neo/workspaces.json whose path doesn't
//     exist on disk (stale after dir deletion).
//  2. Invalid member_workspaces: .neo-project/neo.yaml references paths that
//     don't resolve to an existing workspace directory.
//
// Prints a row per check. Returns true iff both pass. [363.A]
func checkFederationIntegrity(workspace string) bool {
	fmt.Printf("\n%s Federation integrity\n\n", bold("▶"))
	ok := true

	// Check 1: orphan workspaces in registry.
	regPath := workspaceRegistryPath()
	if regData, err := os.ReadFile(regPath); err == nil { //nolint:gosec // G304-CLI-CONSENT: user-owned registry
		var reg struct {
			Workspaces []struct {
				ID   string `json:"id"`
				Path string `json:"path"`
			} `json:"workspaces"`
		}
		if json.Unmarshal(regData, &reg) == nil {
			orphans := 0
			for _, w := range reg.Workspaces {
				if _, err := os.Stat(w.Path); err != nil {
					fmt.Printf("  %s  orphan workspace  id=%s  path=%s (not found on disk)\n",
						red("✘"), w.ID, w.Path)
					orphans++
				}
			}
			if orphans == 0 {
				fmt.Printf("  %s  workspace registry: %d entries, all paths resolve\n",
					green("✔"), len(reg.Workspaces))
			} else {
				ok = false
				fmt.Printf("  %s  %d orphan(s) — remove stale entries from %s\n",
					yellow("!"), orphans, regPath)
			}
		}
	}

	// Check 2: member_workspaces in nearest .neo-project/neo.yaml.
	projDir := findProjectDir(workspace)
	if projDir == "" {
		fmt.Printf("  %s  no .neo-project found in walk-up — workspace-only mode\n", yellow("?"))
		return ok
	}
	projCfg, err := os.ReadFile(filepath.Join(projDir, "neo.yaml")) //nolint:gosec // G304-CLI-CONSENT
	if err != nil {
		return ok
	}
	// Parse member_workspaces: "  - /path\n  - path2\n" — simple scan.
	invalidMembers := 0
	parent := filepath.Dir(projDir) // project root, parent of .neo-project/
	for _, line := range strings.Split(string(projCfg), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		member := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
		member = strings.Trim(member, `"'`)
		if member == "" {
			continue
		}
		path := member
		if !filepath.IsAbs(member) {
			path = filepath.Join(parent, member)
		}
		if _, err := os.Stat(path); err != nil {
			fmt.Printf("  %s  member_workspaces  %q not found on disk\n", red("✘"), member)
			invalidMembers++
		}
	}
	if invalidMembers == 0 {
		fmt.Printf("  %s  .neo-project/neo.yaml member_workspaces: all paths resolve\n", green("✔"))
	} else {
		ok = false
	}
	return ok
}

// findProjectDir walks up from workspace up to 5 levels looking for .neo-project/.
func findProjectDir(workspace string) string {
	dir := workspace
	for range 5 {
		candidate := filepath.Join(dir, ".neo-project")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// -- terminal color helpers (zero external deps) --------------------------

const (
	reset  = "\033[0m"
	boldOn = "\033[1m"
	redOn  = "\033[31m"
	greenOn = "\033[32m"
	yellowOn = "\033[33m"
	cyan   = "\033[36m"
)

func red(s string) string    { return redOn + s + reset }
func green(s string) string  { return greenOn + s + reset }
func yellow(s string) string { return yellowOn + s + reset }
func bold(s string) string   { return boldOn + s + reset }

func toFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// ============================================================================
// [SRE-34.1.2] neo workspace — multi-workspace registry management
// ============================================================================

// workspaceEntry mirrors pkg/workspace.WorkspaceEntry for the CLI (no import needed).
type workspaceEntry struct {
	ID           string    `json:"id"`
	Path         string    `json:"path"`
	Name         string    `json:"name"`
	Type         string    `json:"type"`
	DominantLang string    `json:"dominant_lang"`
	Health       string    `json:"health"`
	AddedAt      time.Time `json:"added_at"`
}

type workspaceRegistry struct {
	Workspaces []workspaceEntry `json:"workspaces"`
	ActiveID   string           `json:"active_id"`
}

func globalNeoDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".neo")
}

func workspaceRegistryPath() string {
	return filepath.Join(globalNeoDir(), "workspaces.json")
}

func loadWorkspaceRegistry() (*workspaceRegistry, error) {
	data, err := os.ReadFile(workspaceRegistryPath())
	if os.IsNotExist(err) {
		return &workspaceRegistry{}, nil
	}
	if err != nil {
		return nil, err
	}
	var r workspaceRegistry
	return &r, json.Unmarshal(data, &r)
}

func activeEntry(r *workspaceRegistry) *workspaceEntry {
	for i := range r.Workspaces {
		if r.Workspaces[i].ID == r.ActiveID {
			return &r.Workspaces[i]
		}
	}
	if len(r.Workspaces) > 0 {
		return &r.Workspaces[0]
	}
	return nil
}

func saveWorkspaceRegistry(r *workspaceRegistry) error {
	if err := os.MkdirAll(globalNeoDir(), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := workspaceRegistryPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, workspaceRegistryPath())
}

// workspaceCmd implements `neo workspace <add|list|select|migrate>`. [SRE-34.1.2]
func workspaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workspace",
		Short: "Manage multi-workspace registry (add, list, select, migrate, export, import, db-audit, db-json-export, db-json-import)",
	}
	cmd.AddCommand(
		workspaceAddCmd(),
		workspaceListCmd(),
		workspaceSelectCmd(),
		workspaceMigrateCmd(),
		workspaceExportCmd(),
		workspaceImportCmd(),
		workspaceDBauditCmd(),
		workspaceDBJsonExportCmd(),
		workspaceDBJsonImportCmd(),
		canonicalIDCmd(), // [PILAR-XXVI / 135.G] Cross-machine workspace identity
	)
	return cmd
}

func workspaceAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add [path]",
		Short: "Register a project path as a workspace",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "."
			if len(args) > 0 {
				path = args[0]
			}
			abs, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			if _, err := os.Stat(abs); err != nil {
				return fmt.Errorf("path %q not accessible: %w", abs, err)
			}
			r, err := loadWorkspaceRegistry()
			if err != nil {
				return err
			}
			// Dedup check
			for _, e := range r.Workspaces {
				if e.Path == abs {
					fmt.Printf("%s Workspace already registered: %s [%s]\n", yellow("!"), e.Name, e.ID)
					return nil
				}
			}
			name := filepath.Base(abs)
			id := fmt.Sprintf("%s-%d", slugifyName(name), time.Now().UnixMilli()%100000)
			entry := workspaceEntry{
				ID:           id,
				Path:         abs,
				Name:         name,
				DominantLang: detectLang(abs),
				Health:       "unknown",
				AddedAt:      time.Now(),
			}
			r.Workspaces = append(r.Workspaces, entry)
			if err := saveWorkspaceRegistry(r); err != nil {
				return err
			}
			fmt.Printf("%s Workspace added: %s (%s) [%s]\n", green("✔"), entry.Name, entry.DominantLang, entry.ID)
			return nil
		},
	}
}

func workspaceListCmd() *cobra.Command {
	var filterType string
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all registered workspaces with health and language",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := loadWorkspaceRegistry()
			if err != nil {
				return err
			}
			if len(r.Workspaces) == 0 {
				fmt.Println("No workspaces registered. Use `neo workspace add <path>`.")
				return nil
			}

			// Apply --type filter.
			entries := r.Workspaces
			if filterType != "" {
				filtered := entries[:0]
				for _, e := range entries {
					wsType := e.Type
					if wsType == "" {
						wsType = "workspace"
					}
					if wsType == filterType {
						filtered = append(filtered, e)
					}
				}
				entries = filtered
				if len(entries) == 0 {
					fmt.Printf("No workspaces with type=%q registered.\n", filterType)
					return nil
				}
			}

			if jsonOutput {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(entries)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "  ID\tNAME\tTYPE\tLANG\tHEALTH\tPATH")
			fmt.Fprintln(w, "  --\t----\t----\t----\t------\t----")
			for _, e := range entries {
				active := "  "
				if e.ID == r.ActiveID {
					active = green("▶ ")
				}
				health := e.Health
				switch health {
				case "ok":
					health = green("ok")
				case "degraded":
					health = red("degraded")
				default:
					health = yellow("unknown")
				}
				wsType := e.Type
				if wsType == "" {
					wsType = "workspace"
				}
				fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\t%s\t%s\n",
					active, e.ID, e.Name, wsType, e.DominantLang, health, e.Path)
				// [278.C] For project entries, show member workspaces indented.
				if wsType == "project" {
					pc, pcErr := config.LoadProjectConfig(e.Path)
					if pcErr == nil && pc != nil {
						for _, m := range pc.MemberWorkspaces {
							fmt.Fprintf(w, "    └ %s\t\t\t\t\t\n", m)
						}
					}
				}
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&filterType, "type", "", "Filter by type: workspace|project")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func workspaceSelectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "select <id-or-name>",
		Short: "Set the active workspace for CLI and Dashboard context",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := loadWorkspaceRegistry()
			if err != nil {
				return err
			}
			target := args[0]
			for _, e := range r.Workspaces {
				if e.ID == target || e.Name == target {
					r.ActiveID = e.ID
					if err := saveWorkspaceRegistry(r); err != nil {
						return err
					}
					fmt.Printf("%s Active workspace: %s (%s)\n", green("✔"), e.Name, e.Path)
					return nil
				}
			}
			return fmt.Errorf("workspace %q not found — run `neo workspace list`", target)
		},
	}
}

// workspaceMigrateCmd implements `neo workspace migrate`.
// Back-fills the Type field for all existing registry entries that lack it. [Épica 281]
func workspaceMigrateCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Back-fill Type field in workspace registry (workspace vs project)",
		Long:  "Scans each registered entry and sets Type='project' when .neo-project/neo.yaml exists, otherwise Type='workspace'. Safe to run multiple times.",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := loadWorkspaceRegistry()
			if err != nil {
				return err
			}
			updated := 0
			for i := range r.Workspaces {
				e := &r.Workspaces[i]
				if e.Type != "" {
					continue // already set
				}
				neoProjectYAML := filepath.Join(e.Path, ".neo-project", "neo.yaml")
				newType := "workspace"
				if _, statErr := os.Stat(neoProjectYAML); statErr == nil {
					newType = "project"
				}
				if dryRun {
					fmt.Printf("[dry-run] would set id=%s name=%s type=%s\n", e.ID, e.Name, newType)
				} else {
					e.Type = newType
				}
				updated++
			}
			if dryRun {
				fmt.Printf("[neo workspace migrate] --dry-run: %d entries would be updated\n", updated)
				return nil
			}
			if updated == 0 {
				fmt.Println("[neo workspace migrate] All entries already have Type set. Nothing to do.")
				return nil
			}
			if err := saveWorkspaceRegistry(r); err != nil {
				return fmt.Errorf("saving registry: %w", err)
			}
			fmt.Printf("[neo workspace migrate] Updated %d entries.\n", updated)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print what would change without writing")
	return cmd
}

// workspaceExportCmd packages workspace DBs + registry into a portable tar.gz. [Épica 281.A]
func workspaceExportCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export workspace registry and databases to a tar.gz archive",
		RunE: func(cmd *cobra.Command, args []string) error {
			home, _ := os.UserHomeDir()
			neoDir := filepath.Join(home, ".neo")

			r, err := loadWorkspaceRegistry()
			if err != nil {
				return err
			}
			active := activeEntry(r)
			if active == nil {
				return fmt.Errorf("no active workspace — run `neo workspace add <path>` first")
			}
			wsNeoDir := filepath.Join(active.Path, ".neo")

			candidates := []string{
				filepath.Join(neoDir, "workspaces.json"),
				filepath.Join(neoDir, "credentials.json"),
				filepath.Join(neoDir, "nexus.yaml"),
				filepath.Join(wsNeoDir, "db", "brain.db"),
				filepath.Join(wsNeoDir, "db", "hnsw.db"),
				filepath.Join(wsNeoDir, "db", "cpg.bin"),
			}

			if output == "" {
				output = fmt.Sprintf("neo-backup-%s.tar.gz", time.Now().Format("20060102-150405"))
			}
			f, err := os.Create(output) //nolint:gosec // G304-CLI-CONSENT: operator-chosen output path
			if err != nil {
				return fmt.Errorf("create archive: %w", err)
			}
			defer f.Close()

			gw := gzip.NewWriter(f)
			defer gw.Close()
			tw := tar.NewWriter(gw)
			defer tw.Close()

			added := 0
			for _, src := range candidates {
				fi, statErr := os.Stat(src)
				if statErr != nil {
					continue // skip missing files silently
				}
				in, openErr := os.Open(src) //nolint:gosec // G304-CLI-CONSENT: controlled path list
				if openErr != nil {
					return fmt.Errorf("open %s: %w", src, openErr)
				}
				hdr := &tar.Header{
					Name:    filepath.Base(src),
					Size:    fi.Size(),
					Mode:    0600,
					ModTime: fi.ModTime(),
				}
				if whErr := tw.WriteHeader(hdr); whErr != nil {
					in.Close()
					return fmt.Errorf("tar header %s: %w", src, whErr)
				}
				if _, cpErr := io.Copy(tw, in); cpErr != nil {
					in.Close()
					return fmt.Errorf("tar copy %s: %w", src, cpErr)
				}
				in.Close()
				added++
				fmt.Printf("  + %s\n", filepath.Base(src))
			}
			fmt.Printf("[neo workspace export] %d files → %s\n", added, output)
			return nil
		},
	}
	cmd.Flags().StringVar(&output, "output", "", "Output archive path (default: neo-backup-<timestamp>.tar.gz)")
	return cmd
}

// workspaceImportCmd restores workspace files from a tar.gz archive. [Épica 281.B]
func workspaceImportCmd() *cobra.Command {
	var input string
	var merge bool
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import workspace registry and databases from a tar.gz archive",
		RunE: func(cmd *cobra.Command, args []string) error {
			if input == "" {
				return fmt.Errorf("--input required")
			}
			home, _ := os.UserHomeDir()
			neoDir := filepath.Join(home, ".neo")

			r, err := loadWorkspaceRegistry()
			if err != nil {
				return err
			}
			active := activeEntry(r)
			if active == nil {
				return fmt.Errorf("no active workspace — run `neo workspace add <path>` first")
			}
			wsDBDir := filepath.Join(active.Path, ".neo", "db")

			destMap := map[string]string{
				"workspaces.json":  filepath.Join(neoDir, "workspaces.json"),
				"credentials.json": filepath.Join(neoDir, "credentials.json"),
				"nexus.yaml":       filepath.Join(neoDir, "nexus.yaml"),
				"brain.db":         filepath.Join(wsDBDir, "brain.db"),
				"hnsw.db":          filepath.Join(wsDBDir, "hnsw.db"),
				"cpg.bin":          filepath.Join(wsDBDir, "cpg.bin"),
			}

			f, err := os.Open(input) //nolint:gosec // G304-CLI-CONSENT: operator-supplied archive path
			if err != nil {
				return fmt.Errorf("open archive: %w", err)
			}
			defer f.Close()

			gr, err := gzip.NewReader(f)
			if err != nil {
				return fmt.Errorf("gzip reader: %w", err)
			}
			defer gr.Close()
			tr := tar.NewReader(gr)

			restored := 0
			for {
				hdr, nextErr := tr.Next()
				if nextErr == io.EOF {
					break
				}
				if nextErr != nil {
					return fmt.Errorf("tar read: %w", nextErr)
				}
				dest, ok := destMap[hdr.Name]
				if !ok {
					continue
				}
				// --merge: skip workspaces.json (keep existing registry)
				if merge && hdr.Name == "workspaces.json" {
					fmt.Printf("  ~ skipping workspaces.json (--merge)\n")
					continue
				}
				if mkErr := os.MkdirAll(filepath.Dir(dest), 0700); mkErr != nil {
					return fmt.Errorf("mkdir %s: %w", filepath.Dir(dest), mkErr)
				}
				out, createErr := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600) //nolint:gosec // G304-CLI-CONSENT: controlled dest path
				if createErr != nil {
					return fmt.Errorf("create %s: %w", dest, createErr)
				}
				if _, cpErr := io.Copy(out, tr); cpErr != nil { //nolint:gosec // G110: archive from trusted operator backup
					out.Close()
					return fmt.Errorf("write %s: %w", dest, cpErr)
				}
				out.Close()
				restored++
				fmt.Printf("  ✓ %s → %s\n", hdr.Name, dest)
			}
			fmt.Printf("[neo workspace import] %d files restored from %s\n", restored, input)
			if merge {
				fmt.Println("  Note: existing workspaces.json kept (--merge mode). Run `neo workspace list` to verify.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&input, "input", "", "Input archive path (required)")
	cmd.Flags().BoolVar(&merge, "merge", false, "Keep existing workspaces.json (merge mode)")
	return cmd
}

// workspaceDBauditCmd inspects the .neo/db/ directory for health issues. [Épica 281.C]
func workspaceDBauditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "db-audit",
		Short: "Inspect .neo/db/ databases for size, modification time, and BoltDB validity",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := loadWorkspaceRegistry()
			if err != nil {
				return err
			}
			active := activeEntry(r)
			if active == nil {
				return fmt.Errorf("no active workspace")
			}
			dbDir := filepath.Join(active.Path, ".neo", "db")

			entries, err := os.ReadDir(dbDir)
			if err != nil {
				return fmt.Errorf("read db dir %s: %w", dbDir, err)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "FILE\tSIZE\tMODIFIED\tSTATUS")
			fmt.Fprintln(w, "----\t----\t--------\t------")

			issues := 0
			for _, de := range entries {
				if de.IsDir() {
					continue
				}
				name := de.Name()
				fullPath := filepath.Join(dbDir, name)
				fi, statErr := de.Info()
				if statErr != nil {
					continue
				}
				sizeStr := formatBytes(fi.Size())
				modStr := fi.ModTime().Format("2006-01-02 15:04")

				status := "ok"
				ext := strings.ToLower(filepath.Ext(name))
				if ext == ".db" {
					db, openErr := bolt.Open(fullPath, 0600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
					if openErr != nil {
						status = "ERROR: " + openErr.Error()
						issues++
					} else {
						_ = db.Close()
					}
				} else if ext == ".bin" {
					if fi.Size() == 0 {
						status = "WARN: empty snapshot"
					}
				}

				// Flag macOS-style paths stored as absolute HFS+
				if strings.Contains(fullPath, "/Volumes/") || strings.Contains(fullPath, "\\") {
					status = "WARN: cross-OS path"
					issues++
				}

				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, sizeStr, modStr, status)
			}
			w.Flush()

			if issues > 0 {
				fmt.Printf("\n[neo workspace db-audit] %d issue(s) found in %s\n", issues, dbDir)
			} else {
				fmt.Printf("\n[neo workspace db-audit] All databases OK in %s\n", dbDir)
			}
			return nil
		},
	}
}

// dbJSONLine is one record in the cross-machine JSONL export. [Épica 369.A]
type dbJSONLine struct {
	DB        string   `json:"db"`
	NS        string   `json:"ns,omitempty"`
	Bucket    string   `json:"bucket,omitempty"`
	Key       string   `json:"key"`
	Content   string   `json:"content,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	Hot       bool     `json:"hot,omitempty"`
	Topic     string   `json:"topic,omitempty"`
	Scope     string   `json:"scope,omitempty"`
	CreatedAt int64    `json:"created_at,omitempty"`
	UpdatedAt int64    `json:"updated_at,omitempty"`
	Timestamp int64    `json:"timestamp,omitempty"`
}

func normalizePaths(s, wsFrom string) string {
	if wsFrom == "" || s == "" {
		return s
	}
	return strings.ReplaceAll(s, wsFrom, "<WORKSPACE>")
}

func remapPaths(s, wsFrom, wsTo string) string {
	if s == "" || wsTo == "" {
		return s
	}
	out := strings.ReplaceAll(s, "<WORKSPACE>", wsTo)
	if wsFrom != "" && wsFrom != wsTo {
		out = strings.ReplaceAll(out, wsFrom, wsTo)
	}
	return out
}

// workspaceDBJsonExportCmd exports knowledge.db and planner.db memex to a JSONL file. [Épica 369.A]
func workspaceDBJsonExportCmd() *cobra.Command {
	var output, dbFlag, wsFrom string
	cmd := &cobra.Command{
		Use:   "db-json-export",
		Short: "Export knowledge.db / planner.db memex to JSONL for cross-machine migration",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := loadWorkspaceRegistry()
			if err != nil {
				return err
			}
			active := activeEntry(r)
			if active == nil {
				return fmt.Errorf("no active workspace — run `neo workspace add <path>` first")
			}
			if wsFrom == "" {
				wsFrom = active.Path
			}
			if output == "" {
				output = fmt.Sprintf("neo-db-export-%s.jsonl", time.Now().Format("20060102-150405"))
			}
			outF, createErr := os.Create(output) //nolint:gosec // G304-CLI-CONSENT: operator-chosen output path
			if createErr != nil {
				return fmt.Errorf("create output: %w", createErr)
			}
			defer outF.Close()
			bw := bufio.NewWriter(outF)
			defer bw.Flush()
			enc := json.NewEncoder(bw)
			if hErr := enc.Encode(map[string]any{
				"version":        1,
				"exported_at":    time.Now().UTC().Format(time.RFC3339),
				"workspace_from": wsFrom,
				"workspace_name": active.Name,
			}); hErr != nil {
				return hErr
			}
			dbDir := filepath.Join(active.Path, ".neo", "db")
			total := 0
			if dbFlag == "knowledge" || dbFlag == "all" {
				n, kErr := exportKnowledgeBoltDB(filepath.Join(dbDir, "knowledge.db"), wsFrom, enc)
				if kErr != nil {
					return fmt.Errorf("knowledge.db: %w", kErr)
				}
				total += n
				fmt.Printf("  knowledge.db: %d entries\n", n)
			}
			if dbFlag == "planner" || dbFlag == "all" {
				n, pErr := exportPlannerBoltDB(filepath.Join(dbDir, "planner.db"), wsFrom, enc)
				if pErr != nil {
					return fmt.Errorf("planner.db: %w", pErr)
				}
				total += n
				fmt.Printf("  planner.db memex: %d entries\n", n)
			}
			fmt.Printf("[neo workspace db-json-export] %d entries → %s\n", total, output)
			return nil
		},
	}
	cmd.Flags().StringVar(&output, "output", "", "Output JSONL path (default: neo-db-export-<timestamp>.jsonl)")
	cmd.Flags().StringVar(&dbFlag, "db", "all", "DB to export: knowledge|planner|all")
	cmd.Flags().StringVar(&wsFrom, "workspace-from", "", "Source workspace path for normalization (default: active workspace)")
	return cmd
}

// workspaceDBJsonImportCmd imports a JSONL dump into knowledge.db and/or planner.db. [Épica 369.A]
// Stop neo-mcp before running — BoltDB requires an exclusive file lock.
func workspaceDBJsonImportCmd() *cobra.Command {
	var input, wsTo string
	var merge, dryRun bool
	cmd := &cobra.Command{
		Use:   "db-json-import",
		Short: "Import JSONL dump into knowledge.db / planner.db (stop neo-mcp first)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if input == "" {
				return fmt.Errorf("--input required")
			}
			r, err := loadWorkspaceRegistry()
			if err != nil {
				return err
			}
			active := activeEntry(r)
			if active == nil {
				return fmt.Errorf("no active workspace")
			}
			if wsTo == "" {
				wsTo = active.Path
			}
			inF, openErr := os.Open(input) //nolint:gosec // G304-CLI-CONSENT: operator-supplied input path
			if openErr != nil {
				return fmt.Errorf("open input: %w", openErr)
			}
			defer inF.Close()
			scanner := bufio.NewScanner(inF)
			scanner.Buffer(make([]byte, 4<<20), 4<<20)
			if !scanner.Scan() {
				return fmt.Errorf("empty export file")
			}
			var header map[string]any
			if hErr := json.Unmarshal(scanner.Bytes(), &header); hErr != nil {
				return fmt.Errorf("invalid header: %w", hErr)
			}
			wsFrom, _ := header["workspace_from"].(string)
			fmt.Printf("  From: %s\n  To:   %s\n", wsFrom, wsTo)
			if dryRun {
				fmt.Println("  Mode: DRY RUN")
			}
			dbDir := filepath.Join(active.Path, ".neo", "db")
			counts := map[string]int{"knowledge": 0, "planner": 0, "skipped": 0, "errors": 0}
			var knowledgeDB, plannerDB *bolt.DB
			defer func() {
				if knowledgeDB != nil {
					_ = knowledgeDB.Close()
				}
				if plannerDB != nil {
					_ = plannerDB.Close()
				}
			}()
			openDB := func(name string) (*bolt.DB, error) {
				p := filepath.Join(dbDir, name+".db")
				return bolt.Open(p, 0600, &bolt.Options{Timeout: 3 * time.Second}) //nolint:gosec // G304-CLI-CONSENT: controlled path under active workspace
			}
			for scanner.Scan() {
				var line dbJSONLine
				if unmarshalErr := json.Unmarshal(scanner.Bytes(), &line); unmarshalErr != nil {
					counts["errors"]++
					continue
				}
				line.Content = remapPaths(line.Content, wsFrom, wsTo)
				line.Topic = remapPaths(line.Topic, wsFrom, wsTo)
				line.Scope = remapPaths(line.Scope, wsFrom, wsTo)
				if dryRun {
					label := line.NS + "/" + line.Key
					if line.DB == "planner" {
						label = line.Bucket + "/" + line.Key
					}
					fmt.Printf("  [dry] %s %s\n", line.DB, label)
					counts[line.DB]++
					continue
				}
				var importErr error
				switch line.DB {
				case "knowledge":
					if knowledgeDB == nil {
						if knowledgeDB, importErr = openDB("knowledge"); importErr != nil {
							return fmt.Errorf("open knowledge.db: %w", importErr)
						}
					}
					importErr = importKnowledgeLine(knowledgeDB, line, merge)
				case "planner":
					if plannerDB == nil {
						if plannerDB, importErr = openDB("planner"); importErr != nil {
							return fmt.Errorf("open planner.db: %w", importErr)
						}
					}
					importErr = importPlannerLine(plannerDB, line, merge)
				default:
					counts["skipped"]++
					continue
				}
				if importErr != nil {
					if importErr.Error() == "skip:exists" {
						counts["skipped"]++
					} else {
						fmt.Printf("  [warn] %s/%s: %v\n", line.NS+line.Bucket, line.Key, importErr)
						counts["errors"]++
					}
				} else {
					counts[line.DB]++
				}
			}
			if scanErr := scanner.Err(); scanErr != nil {
				return fmt.Errorf("read input: %w", scanErr)
			}
			fmt.Printf("[neo workspace db-json-import] knowledge=%d planner=%d skipped=%d errors=%d\n",
				counts["knowledge"], counts["planner"], counts["skipped"], counts["errors"])
			return nil
		},
	}
	cmd.Flags().StringVar(&input, "input", "", "Input JSONL file (required)")
	cmd.Flags().StringVar(&wsTo, "workspace-to", "", "Target workspace path for path remapping (default: active workspace)")
	cmd.Flags().BoolVar(&merge, "merge", true, "Skip entries that already exist in the target DB")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print what would be imported without writing")
	return cmd
}

// exportKnowledgeBoltDB walks knowledge.db buckets and emits JSONL lines. [369.A]
func exportKnowledgeBoltDB(dbPath, wsFrom string, enc *json.Encoder) (int, error) {
	if _, statErr := os.Stat(dbPath); os.IsNotExist(statErr) {
		return 0, nil
	}
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second}) //nolint:gosec // G304-CLI-CONSENT: CLI operator path
	if err != nil {
		return 0, err
	}
	defer db.Close()
	count := 0
	return count, db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, bkt *bolt.Bucket) error {
			bname := string(name)
			if !strings.HasPrefix(bname, "knowledge:") {
				return nil
			}
			ns := strings.TrimPrefix(bname, "knowledge:")
			return bkt.ForEach(func(k, v []byte) error {
				if v == nil {
					return nil
				}
				var raw struct {
					Content   string   `json:"content"`
					Tags      []string `json:"tags"`
					Hot       bool     `json:"hot"`
					CreatedAt int64    `json:"created_at"`
					UpdatedAt int64    `json:"updated_at"`
				}
				if json.Unmarshal(v, &raw) != nil {
					return nil
				}
				count++
				return enc.Encode(dbJSONLine{
					DB: "knowledge", NS: ns, Key: string(k),
					Content: normalizePaths(raw.Content, wsFrom), Tags: raw.Tags, Hot: raw.Hot,
					CreatedAt: raw.CreatedAt, UpdatedAt: raw.UpdatedAt,
				})
			})
		})
	})
}

// exportPlannerBoltDB walks planner.db memex_buffer buckets. [369.A]
func exportPlannerBoltDB(dbPath, wsFrom string, enc *json.Encoder) (int, error) {
	if _, statErr := os.Stat(dbPath); os.IsNotExist(statErr) {
		return 0, nil
	}
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second}) //nolint:gosec // G304-CLI-CONSENT: CLI operator path
	if err != nil {
		return 0, err
	}
	defer db.Close()
	count := 0
	return count, db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, bkt *bolt.Bucket) error {
			bname := string(name)
			if bname != "memex_buffer" && !strings.HasPrefix(bname, "memex_buffer:") {
				return nil
			}
			return bkt.ForEach(func(k, v []byte) error {
				if v == nil {
					return nil
				}
				var raw struct {
					Topic     string `json:"topic"`
					Scope     string `json:"scope"`
					Content   string `json:"content"`
					Timestamp int64  `json:"timestamp"`
				}
				if json.Unmarshal(v, &raw) != nil {
					return nil
				}
				count++
				return enc.Encode(dbJSONLine{
					DB: "planner", Bucket: bname, Key: string(k),
					Topic: normalizePaths(raw.Topic, wsFrom), Scope: normalizePaths(raw.Scope, wsFrom),
					Content: normalizePaths(raw.Content, wsFrom), Timestamp: raw.Timestamp,
				})
			})
		})
	})
}

// importKnowledgeLine upserts one knowledge entry into an open knowledge.db. [369.A]
func importKnowledgeLine(db *bolt.DB, line dbJSONLine, merge bool) error {
	return db.Update(func(tx *bolt.Tx) error {
		bkt, bErr := tx.CreateBucketIfNotExists([]byte("knowledge:" + line.NS))
		if bErr != nil {
			return bErr
		}
		k := []byte(line.Key)
		if merge && bkt.Get(k) != nil {
			return fmt.Errorf("skip:exists")
		}
		now := time.Now().Unix()
		if line.CreatedAt == 0 {
			line.CreatedAt = now
		}
		val, mErr := json.Marshal(map[string]any{
			"key": line.Key, "namespace": line.NS, "content": line.Content,
			"tags": line.Tags, "hot": line.Hot,
			"created_at": line.CreatedAt, "updated_at": now,
		})
		if mErr != nil {
			return mErr
		}
		return bkt.Put(k, val)
	})
}

// importPlannerLine upserts one memex entry into an open planner.db. [369.A]
func importPlannerLine(db *bolt.DB, line dbJSONLine, merge bool) error {
	bucketName := line.Bucket
	if bucketName == "" {
		bucketName = "memex_buffer"
	}
	return db.Update(func(tx *bolt.Tx) error {
		bkt, bErr := tx.CreateBucketIfNotExists([]byte(bucketName))
		if bErr != nil {
			return bErr
		}
		k := []byte(line.Key)
		if merge && bkt.Get(k) != nil {
			return fmt.Errorf("skip:exists")
		}
		val, mErr := json.Marshal(map[string]any{
			"id": line.Key, "topic": line.Topic,
			"scope": line.Scope, "content": line.Content, "timestamp": line.Timestamp,
		})
		if mErr != nil {
			return mErr
		}
		return bkt.Put(k, val)
	})
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func slugifyName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}

func detectLang(root string) string {
	counts := map[string]int{}
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		if d.IsDir() {
			n := d.Name()
			if n == "node_modules" || n == "vendor" || n == ".git" || n == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".go":
			counts["go"]++
		case ".ts", ".tsx":
			counts["typescript"]++
		case ".js", ".jsx":
			counts["javascript"]++
		case ".py":
			counts["python"]++
		case ".rs":
			counts["rust"]++
		}
		return nil
	})
	if walkErr != nil {
		log.Printf("[NEO-CLI-WARN] detectLang: walk %s failed: %v", root, walkErr)
	}
	best, max := "unknown", 0
	for lang, n := range counts {
		if n > max {
			best, max = lang, n
		}
	}
	return best
}

// ============================================================================
// [SRE-34.2.2] neo diagnose / neo heal
// ============================================================================

// diagnoseCmd implements `neo diagnose <file>`. [SRE-34.2.2]
// Runs LOCAL + OLLAMA inference layers and prints the result.
func diagnoseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diagnose <file>",
		Short: "Run LOCAL+OLLAMA inference to identify bugs in a file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ws := findWorkspace()
			info, err := readDaemonInfo(ws)
			if err != nil {
				return err
			}
			abs, _ := filepath.Abs(args[0])
			body, err := doPost(apiURL(info.Port, "/api/diagnose"), map[string]string{"path": abs})
			if err != nil {
				return fmt.Errorf("daemon unreachable: %w", err)
			}
			var resp struct {
				Level      string  `json:"level"`
				Confidence float64 `json:"confidence"`
				Risk       string  `json:"risk"`
				Summary    string  `json:"summary"`
				Suggestion string  `json:"suggestion"`
				Tokens     int     `json:"tokens"`
				Error      string  `json:"error"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				return err
			}
			if resp.Error != "" {
				return fmt.Errorf("diagnose error: %s", resp.Error)
			}
			riskColor := green(resp.Risk)
			if resp.Risk == "HIGH" || resp.Risk == "CRITICAL" {
				riskColor = red(resp.Risk)
			} else if resp.Risk == "MEDIUM" {
				riskColor = yellow(resp.Risk)
			}
			fmt.Printf("\n%s Inference result [%s — %.0f%% confidence] Risk: %s\n\n",
				bold("▶"), resp.Level, resp.Confidence*100, riskColor)
			fmt.Printf("  Summary:    %s\n", resp.Summary)
			if resp.Suggestion != "" {
				fmt.Printf("  Suggestion: %s\n", resp.Suggestion)
			}
			if resp.Tokens > 0 {
				fmt.Printf("  Tokens:     %d (CLOUD tier)\n", resp.Tokens)
			}
			fmt.Println()
			return nil
		},
	}
}

// healCmd implements `neo heal --mode [auto|manual]`. [SRE-34.2.2]
func healCmd() *cobra.Command {
	var mode string
	cmd := &cobra.Command{
		Use:   "heal",
		Short: "Attempt auto-fix via inference gateway (--mode auto|manual)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ws := findWorkspace()
			info, err := readDaemonInfo(ws)
			if err != nil {
				return err
			}
			body, err := doPost(apiURL(info.Port, "/api/heal"), map[string]string{"mode": mode})
			if err != nil {
				return fmt.Errorf("daemon unreachable: %w", err)
			}
			var resp map[string]string
			if err := json.Unmarshal(body, &resp); err != nil {
				return err
			}
			if e, ok := resp["error"]; ok {
				return fmt.Errorf("heal error: %s", e)
			}
			if mode == "auto" {
				fmt.Printf("%s [AUTO-APPROVED] heal cycle started\n", green("✔"))
			} else {
				fmt.Printf("%s Heal plan generated — review and apply manually:\n\n%s\n",
					bold("▶"), resp["plan"])
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "manual", "Heal mode: 'auto' (Watchdog-supervised) or 'manual' (review first)")

	// [SRE-51.3] --from-backlog: consume surrender debt file using Cloud budget.
	cmd.AddCommand(healFromBacklogCmd())
	return cmd
}

// healFromBacklogCmd implements `neo heal --from-backlog`. [SRE-51.3]
// Reads the debt backlog file and sends each SURRENDER entry to the /api/diagnose
// endpoint using the CLOUD inference tier.
func healFromBacklogCmd() *cobra.Command {
	var backlogPath string
	return &cobra.Command{
		Use:   "from-backlog",
		Short: "[SRE-51] Process surrender entries in debt backlog using Cloud inference budget",
		RunE: func(cmd *cobra.Command, args []string) error {
			ws := findWorkspace()
			if backlogPath == "" {
				cfgPath := filepath.Join(ws, "neo.yaml")
				if data, err := os.ReadFile(cfgPath); err == nil {
					// Simple extraction — avoid importing config pkg.
					for line := range strings.SplitSeq(string(data), "\n") {
						line = strings.TrimSpace(line)
						if v, ok := strings.CutPrefix(line, "debt_file:"); ok {
							backlogPath = strings.Trim(v, ` "`)
						}
					}
				}
			}
			if backlogPath == "" {
				backlogPath = filepath.Join(ws, "technical_debt_backlog.md")
			}

			data, err := os.ReadFile(backlogPath)
			if err != nil {
				return fmt.Errorf("cannot read debt backlog %s: %w", backlogPath, err)
			}

			info, err := readDaemonInfo(ws)
			if err != nil {
				return fmt.Errorf("daemon unreachable: %w", err)
			}

			var healed, skipped int
			for line := range strings.SplitSeq(string(data), "\n") {
				if !strings.Contains(line, "**SURRENDER**") {
					continue
				}
				// Extract mutation path from: - [ ] **SURRENDER** `path` @ timestamp
				path := extractBacktickContent(line)
				if path == "" {
					skipped++
					continue
				}
				body, healErr := doPost(apiURL(info.Port, "/api/diagnose"),
					map[string]string{"path": path})
				if healErr != nil {
					fmt.Printf("  ✗ %s: %v\n", path, healErr)
					skipped++
					continue
				}
				var result map[string]any
				if jsonErr := json.Unmarshal(body, &result); jsonErr == nil {
					level, _ := result["level"]
					fmt.Printf("  ✔ %s → level=%v\n", path, level)
				}
				healed++
			}
			fmt.Printf("\n%s Bulk Heal: %d healed, %d skipped (from %s)\n",
				bold("▶"), healed, skipped, backlogPath)
			return nil
		},
	}
}

func extractBacktickContent(s string) string {
	start := strings.Index(s, "`")
	if start < 0 {
		return ""
	}
	end := strings.Index(s[start+1:], "`")
	if end < 0 {
		return ""
	}
	return s[start+1 : start+1+end]
}

// ============================================================================
// [SRE-34.3.3] neo debt-export — English technical debt consolidation
// ============================================================================

// debtExportCmd implements `neo debt-export`. [SRE-34.3.3]
// Scans all registered workspaces for .neo/master_plan.md and technical_debt.md,
// consolidates uncompleted tasks, and writes to ~/.neo/technical_debt_backlog.md.
func debtExportCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "debt-export",
		Short: "Consolidate technical debt from all workspaces → technical_debt_backlog.md",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := loadWorkspaceRegistry()
			if err != nil {
				return err
			}
			if len(r.Workspaces) == 0 {
				fmt.Println("No workspaces registered.")
				return nil
			}

			var sb strings.Builder
			sb.WriteString("# Technical Debt Backlog (consolidated)\n")
			sb.WriteString("<!-- Generated by `neo debt-export` — do not edit manually -->\n")
			fmt.Fprintf(&sb, "<!-- Generated: %s -->\n\n", time.Now().UTC().Format(time.RFC3339))

			totalItems := 0
			for _, ws := range r.Workspaces {
				items := collectDebt(ws.Path)
				if len(items) == 0 {
					continue
				}
				fmt.Fprintf(&sb, "## %s (%s)\n\n", ws.Name, ws.DominantLang)
				for _, item := range items {
					fmt.Fprintf(&sb, "- [ ] %s\n", item)
					totalItems++
				}
				sb.WriteString("\n")
			}

			if totalItems == 0 {
				fmt.Println(green("✅ No pending debt found across registered workspaces."))
				return nil
			}

			if out == "" {
				out = filepath.Join(globalNeoDir(), "technical_debt_backlog.md")
			}
			if err := os.MkdirAll(filepath.Dir(out), 0700); err != nil {
				return err
			}
			if err := os.WriteFile(out, []byte(sb.String()), 0644); err != nil {
				return err
			}
			fmt.Printf("%s Exported %d debt items → %s\n", green("✔"), totalItems, out)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "Output path (default: ~/.neo/technical_debt_backlog.md)")
	return cmd
}

// queryCmd implements `neo query "<text>"` — routes natural language to the inference gateway. [SRE-47]
func queryCmd() *cobra.Command {
	var thermal bool
	cmd := &cobra.Command{
		Use:   "query <text>",
		Short: "Send a natural language query to the local inference gateway (Sovereign Voice)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.Join(args, " ")
			workspace := findWorkspace()

			// [SRE-47.2] Special thermal query: returns KineticSensor snapshot via daemon API.
			lq := strings.ToLower(query)
			if thermal || strings.Contains(lq, "térm") || strings.Contains(lq, "therm") ||
				strings.Contains(lq, "temperatura") || strings.Contains(lq, "calor") {
				info, err := readDaemonInfo(workspace)
				if err != nil {
					return fmt.Errorf("daemon unreachable: %w", err)
				}
				body, err := doGet(apiURL(info.Port, "/api/status"))
				if err != nil {
					return fmt.Errorf("status query failed: %w", err)
				}
				var status map[string]any
				if jsonErr := json.Unmarshal(body, &status); jsonErr != nil {
					fmt.Println(string(body))
					return nil
				}
				fmt.Printf("🌡️  Thermal Query: %s\n", query)
				if watts, ok := status["watts"]; ok {
					fmt.Printf("   RAPL: %.1f W\n", watts)
				}
				if goroutines, ok := status["goroutines"]; ok {
					fmt.Printf("   Goroutines: %.0f\n", goroutines)
				}
				if mode, ok := status["mode"]; ok {
					fmt.Printf("   Mode: %s\n", mode)
				}
				return nil
			}

			// General query: route to inference gateway via /api/diagnose.
			info, err := readDaemonInfo(workspace)
			if err != nil {
				return fmt.Errorf("daemon unreachable: %w", err)
			}
			reqBody, _ := json.Marshal(map[string]string{"path": query})
			// [SRE-110.E] daemon API on loopback — SafeInternalHTTPClient.
			client := sre.SafeInternalHTTPClient(30)
			resp, err := client.Post(apiURL(info.Port, "/api/diagnose"), "application/json",
				strings.NewReader(string(reqBody)))
			if err != nil {
				return fmt.Errorf("query failed: %w", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			var result map[string]any
			if jsonErr := json.Unmarshal(body, &result); jsonErr != nil {
				fmt.Println(string(body))
				return nil
			}
			fmt.Printf("🗣️  Query: %s\n\n", query)
			if level, ok := result["level"]; ok {
				fmt.Printf("   Inference Level: %v\n", level)
			}
			if confidence, ok := result["confidence"]; ok {
				fmt.Printf("   Confidence: %v\n", confidence)
			}
			if response, ok := result["response"]; ok {
				fmt.Printf("   Response: %v\n", response)
			} else {
				fmt.Println(string(body))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&thermal, "thermal", false, "Force thermal/power query mode")
	return cmd
}

// ── Épica 59: Self-Replicator ────────────────────────────────────────────────

// bootstrapCmd packages the neoanvil consciousness (brain DB + config) into a
// portable tar.gz archive for migration or backup. [SRE-59.1]
func bootstrapCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bootstrap",
		Short: "Package NeoAnvil brain + config into a portable archive",
		RunE: func(cmd *cobra.Command, args []string) error {
			ws := findWorkspace()
			home, _ := os.UserHomeDir()
			stamp := time.Now().Format("20060102-150405")
			outPath := filepath.Join(ws, fmt.Sprintf("neo-bootstrap-%s.tar.gz", stamp))

			sources := []struct{ src, archiveName string }{
				{filepath.Join(home, ".neo", "workspaces.json"), "workspaces.json"},
				{filepath.Join(ws, ".neo", "db", "brain.db"), "brain.db"},
				{filepath.Join(ws, ".neo", "db", "hnsw.db"), "hnsw.db"},
				{filepath.Join(ws, "neo.yaml"), "neo.yaml"},
				{filepath.Join(ws, ".neo", "master_plan.md"), "master_plan.md"},
			}

			f, err := os.Create(outPath) //nolint:gosec // G304-CLI-CONSENT
			if err != nil {
				return fmt.Errorf("cannot create archive: %w", err)
			}
			defer f.Close() //nolint:errcheck

			gzw := gzip.NewWriter(f)
			defer gzw.Close() //nolint:errcheck
			tw := tar.NewWriter(gzw)
			defer tw.Close() //nolint:errcheck

			packed := 0
			for _, s := range sources {
				data, readErr := os.ReadFile(s.src) //nolint:gosec // G304-CLI-CONSENT
				if readErr != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "⚠  skipping %s: %v\n", s.archiveName, readErr)
					continue
				}
				hdr := &tar.Header{
					Name:    s.archiveName,
					Mode:    0600,
					Size:    int64(len(data)),
					ModTime: time.Now(),
				}
				if err := tw.WriteHeader(hdr); err != nil {
					return err
				}
				if _, err := tw.Write(data); err != nil {
					return err
				}
				packed++
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✅ Bootstrap archive created: %s (%d files)\n", outPath, packed)
			return nil
		},
	}
}

// merkleCmd queries the daemon's /api/v1/brain/merkle endpoint and prints
// the Merkle root for sync verification. [SRE-59.2]
func merkleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "merkle",
		Short: "Print the Merkle root of the HNSW brain graph",
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := readDaemonInfo(findWorkspace())
			if err != nil {
				return err
			}
			url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/brain/merkle", info.Port)
			resp, err := http.Get(url) //nolint:gosec,noctx
			if err != nil {
				return fmt.Errorf("daemon unreachable: %w", err)
			}
			defer resp.Body.Close() //nolint:errcheck
			var result map[string]any
			if decErr := json.NewDecoder(resp.Body).Decode(&result); decErr != nil {
				return decErr
			}
			root, _ := result["merkle_root"].(string)
			at, _ := result["at"].(string)
			if root == "" {
				fmt.Fprintln(cmd.OutOrStdout(), "Brain graph is empty — no Merkle root available.")
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Brain Merkle Root: %s\nAt: %s\n", root, at)
			return nil
		},
	}
}

// hotreloadCmd triggers a background rebuild of the neo-mcp binary via the
// daemon's /api/v1/hotreload endpoint. [SRE-59.3]
func hotreloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "hotreload",
		Short: "Rebuild neo-mcp binary in the background (apply with restart)",
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := readDaemonInfo(findWorkspace())
			if err != nil {
				return err
			}
			url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/hotreload", info.Port)
			resp, err := http.Post(url, "application/json", bytes.NewBufferString("{}")) //nolint:gosec,noctx
			if err != nil {
				return fmt.Errorf("daemon unreachable: %w", err)
			}
			defer resp.Body.Close() //nolint:errcheck
			var result map[string]any
			if decErr := json.NewDecoder(resp.Body).Decode(&result); decErr != nil {
				return decErr
			}
			status, _ := result["status"].(string)
			message, _ := result["message"].(string)
			binary, _ := result["binary"].(string)
			switch status {
			case "ready":
				fmt.Fprintf(cmd.OutOrStdout(), "✅ Build ready: %s\n%s\n", binary, message)
			case "build_failed":
				errMsg, _ := result["error"].(string)
				output, _ := result["output"].(string)
				fmt.Fprintf(cmd.OutOrStdout(), "❌ Build failed: %s\n%s\n", errMsg, output)
			default:
				fmt.Fprintf(cmd.OutOrStdout(), "Response: %v\n", result)
			}
			return nil
		},
	}
}

// collectDebt reads .neo/master_plan.md and .neo/technical_debt.md from workspacePath
// and returns uncompleted task lines (lines with "- [ ]").
func collectDebt(workspacePath string) []string {
	var items []string
	candidates := []string{
		filepath.Join(workspacePath, ".neo", "master_plan.md"),
		filepath.Join(workspacePath, ".neo", "technical_debt.md"),
		filepath.Join(workspacePath, "TECHNICAL_DEBT.md"),
	}
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if task, ok := strings.CutPrefix(trimmed, "- [ ]"); ok {
				task = strings.TrimSpace(task)
				if task != "" {
					items = append(items, task)
				}
			}
		}
	}
	return items
}

// =============================================================================
// PILAR-XXXI: neo init --project — monorepo project federation
// =============================================================================

// initProjectCmd implements `neo init --project [--name <name>] [--dry-run]`.
// Walks the current directory for nested neo.yaml files, detects dominant
// languages, and writes .neo-project/neo.yaml. [Épicas 259.A-C, 269]
func initProjectCmd() *cobra.Command {
	var projectName string
	var dryRun bool
	var lang string
	var dir string
	var members []string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a project federation (.neo-project/neo.yaml)",
		Long:  "Scan the current directory for workspaces and create a project config that federates them for unified BRIEFING and BLAST_RADIUS.",
		RunE: func(cmd *cobra.Command, args []string) error {
			root := dir
			if root == "" {
				var err error
				root, err = os.Getwd()
				if err != nil {
					return err
				}
			}
			root, err := filepath.Abs(root)
			if err != nil {
				return fmt.Errorf("resolving --dir: %w", err)
			}

			var memberPaths []string
			if len(members) > 0 {
				// explicit --members: validate each path exists
				for _, m := range members {
					abs := m
					if !filepath.IsAbs(m) {
						abs = filepath.Join(root, m)
					}
					if _, statErr := os.Stat(abs); statErr != nil {
						return fmt.Errorf("--members path %q not found: %w", m, statErr)
					}
					rel, _ := filepath.Rel(root, abs)
					memberPaths = append(memberPaths, filepath.ToSlash(rel))
				}
			} else {
				workspaces, wsErr := detectWorkspaces(root)
				if wsErr != nil {
					return wsErr
				}
				if len(workspaces) == 0 {
					fmt.Println("[neo init] No neo.yaml workspaces found under current directory (depth ≤ 3).")
					return nil
				}

				tw := tabwriter.NewWriter(os.Stdout, 2, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "[neo init --project] Detected workspaces:")
				sort.Slice(workspaces, func(i, j int) bool { return workspaces[i].Path < workspaces[j].Path })
				for _, ws := range workspaces {
					fmt.Fprintf(tw, "  %s\t(dominant: %s,\t%d source files)\n", ws.Name, ws.Lang, ws.FileCount)
				}
				_ = tw.Flush()

				for _, ws := range workspaces {
					rel, _ := filepath.Rel(root, ws.Path)
					memberPaths = append(memberPaths, filepath.ToSlash(rel))
				}
			}

			if projectName == "" {
				projectName = filepath.Base(root)
			}
			pc := &config.ProjectConfig{
				ProjectName:      projectName,
				MemberWorkspaces: memberPaths,
				DominantLang:     lang,
			}

			if dryRun {
				fmt.Printf("[neo init] --dry-run: would create %s\n", filepath.Join(root, ".neo-project", "neo.yaml"))
				fmt.Printf("  project_name: %s\n  dominant_lang: %s\n  member_workspaces: %v\n", pc.ProjectName, pc.DominantLang, pc.MemberWorkspaces)
				return nil
			}

			if err := config.WriteProjectConfig(root, pc); err != nil {
				return fmt.Errorf("writing project config: %w", err)
			}
			yamlPath := filepath.Join(root, ".neo-project", "neo.yaml")
			fmt.Printf("[neo init] Created %s\n\n", yamlPath)
			if raw, readErr := os.ReadFile(yamlPath); readErr == nil { //nolint:gosec // G304-CLI-CONSENT: operator-chosen path
				fmt.Printf("```yaml\n%s```\n\n", raw)
			}
			// [363.A] Seed SHARED_DEBT.md + knowledge/ directory so downstream
			// tools (neo_debt scope:"project", filesync watcher) see valid
			// skeleton on first boot. Idempotent — skips existing files.
			if err := seedProjectArtifacts(root, projectName); err != nil {
				fmt.Printf("[neo init-WARN] could not seed project artifacts: %v\n", err)
			}
			fmt.Printf("Next step: run `neo workspace list` to verify Type=\"project\" detection.\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&projectName, "name", "", "Project name (default: current directory name)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print what would be done without writing files")
	cmd.Flags().StringVar(&lang, "lang", "", "Dominant language (go|python|rust|typescript; auto-detected if omitted)")
	cmd.Flags().StringVar(&dir, "dir", "", "Root directory to initialize (default: current directory)")
	cmd.Flags().StringSliceVar(&members, "members", nil, "Explicit member workspace paths (comma-separated or repeated flag; skips auto-detection)")
	return cmd
}

// seedProjectArtifacts creates the baseline files that federation tools expect
// to exist under .neo-project/: SHARED_DEBT.md (empty registry) + knowledge/.gitkeep
// (so the filesync watcher sees a valid dir to walk). Idempotent — never
// overwrites existing content. [363.A]
func seedProjectArtifacts(root, projectName string) error {
	projDir := filepath.Join(root, ".neo-project")
	debtPath := filepath.Join(projDir, "SHARED_DEBT.md")
	if _, err := os.Stat(debtPath); os.IsNotExist(err) {
		header := fmt.Sprintf("# SHARED_DEBT — %s\n\n"+
			"> Deuda técnica cross-workspace detectada por el sistema.\n"+
			"> Managed by neo_debt(scope:\"project\") + federation.AppendMissingContract.\n\n"+
			"## P0 — Blocker (rompe la frontera)\n\n_none_\n\n"+
			"## P1 — Alto (impide release)\n\n_none_\n\n"+
			"## P2 — Medio (afecta DX)\n\n_none_\n\n"+
			"## P3 — Bajo (observación)\n\n_none_\n\n"+
			"## Contratos de frontera bajo revisión\n\n_none_\n",
			projectName)
		if werr := os.WriteFile(debtPath, []byte(header), 0o640); werr != nil {
			return fmt.Errorf("write SHARED_DEBT.md: %w", werr)
		}
		fmt.Printf("[neo init] Seeded %s\n", debtPath)
	}
	knowDir := filepath.Join(projDir, "knowledge")
	if err := os.MkdirAll(knowDir, 0o750); err != nil {
		return fmt.Errorf("mkdir knowledge/: %w", err)
	}
	gitkeep := filepath.Join(knowDir, ".gitkeep")
	if _, err := os.Stat(gitkeep); os.IsNotExist(err) {
		_ = os.WriteFile(gitkeep, []byte(""), 0o640)
	}
	return nil
}

// workspaceInfo holds basic info about a detected workspace.
type workspaceInfo struct {
	Name      string
	Path      string
	Lang      string
	FileCount int
}

// detectWorkspaces walks up to 3 levels deep looking for directories containing neo.yaml.
func detectWorkspaces(root string) ([]workspaceInfo, error) {
	var results []workspaceInfo
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		// Limit depth to 3.
		rel, _ := filepath.Rel(root, path)
		if strings.Count(rel, string(os.PathSeparator)) > 3 {
			return filepath.SkipDir
		}
		// Skip hidden and common non-source dirs.
		name := d.Name()
		if strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" {
			return filepath.SkipDir
		}
		// Check for neo.yaml.
		if _, statErr := os.Stat(filepath.Join(path, "neo.yaml")); statErr != nil {
			return nil
		}
		if path == root {
			return nil // skip root itself
		}
		lang, count := detectDirLang(path)
		results = append(results, workspaceInfo{
			Name:      filepath.Base(path),
			Path:      path,
			Lang:      lang,
			FileCount: count,
		})
		return nil
	})
	return results, err
}

// detectDirLang counts source files and returns the dominant language + count.
func detectDirLang(dir string) (string, int) {
	counts := map[string]int{"go": 0, "python": 0, "typescript": 0, "rust": 0}
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".go":
			counts["go"]++
		case ".py":
			counts["python"]++
		case ".ts", ".tsx", ".js", ".jsx":
			counts["typescript"]++
		case ".rs":
			counts["rust"]++
		}
		return nil
	})
	if walkErr != nil {
		log.Printf("[NEO-CLI-WARN] detectDirLang: walk %s failed: %v", dir, walkErr)
	}
	dominant, max := "go", 0
	for lang, n := range counts {
		if n > max {
			dominant, max = lang, n
		}
	}
	total := counts["go"] + counts["python"] + counts["typescript"] + counts["rust"]
	return dominant, total
}

// =============================================================================
// PILAR-XXXIII: neo login — store API key in ~/.neo/credentials.json
// =============================================================================

// loginCmd implements `neo login [--token TOKEN] [--tenant TENANT] [--provider PROVIDER]`.
// Stores credentials in ~/.neo/credentials.json with 0600 permissions. [Épica 264.C]
func loginCmd() *cobra.Command {
	var (
		token, tenantID, provider string
		credType, email, domain   string
		refreshToken, expiresAt   string
	)

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store credentials in ~/.neo/credentials.json",
		Long: `Persist credentials for a provider (api_token or oauth2). Stored with 0600 permissions.

Examples:
  neo login --provider jira --token xyz --email me@acme.com --domain acme.atlassian.net
  neo login --provider github --token ghp_xxx --type api_token
  neo login --provider neo-cloud --token x --tenant t1
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if provider == "" {
				provider = "neo-cloud"
			}
			if credType == "" {
				credType = auth.CredTypeAPIToken
			}

			// Interactive prompt when --token not provided.
			if token == "" {
				fmt.Print("Enter token: ")
				scanner := bufio.NewScanner(os.Stdin)
				if scanner.Scan() {
					token = strings.TrimSpace(scanner.Text())
				}
				if token == "" {
					return fmt.Errorf("token is required")
				}
			}
			if tenantID == "" && !cmd.Flags().Changed("tenant") {
				fmt.Print("Enter tenant ID (optional, press Enter to skip): ")
				scanner := bufio.NewScanner(os.Stdin)
				if scanner.Scan() {
					tenantID = strings.TrimSpace(scanner.Text())
				}
			}

			entry := auth.CredEntry{
				Provider:     provider,
				Type:         credType,
				Token:        token,
				RefreshToken: refreshToken,
				Email:        email,
				Domain:       domain,
				TenantID:     tenantID,
				ExpiresAt:    expiresAt,
			}

			// Validate via Provider registry when a Provider is registered
			// for this Type. Unknown types skip validation (extension-friendly).
			registry := auth.DefaultProviderRegistry()
			if _, ok := registry.Get(entry.Type); ok {
				if err := registry.Validate(&entry); err != nil {
					return fmt.Errorf("validation failed: %w", err)
				}
			}

			credsPath := auth.DefaultCredentialsPath()
			creds, err := auth.Load(credsPath)
			if err != nil {
				creds = &auth.Credentials{Version: 1}
			}
			creds.Add(entry)
			if err := auth.Save(creds, credsPath); err != nil {
				return fmt.Errorf("saving credentials: %w", err)
			}

			// Append to audit log — best effort; surface error but don't
			// roll back the credentials write (the operator already typed
			// the secret; failing here would be the worst UX).
			if err := appendLoginAuditEntry(provider, credType, tenantID); err != nil {
				fmt.Fprintf(os.Stderr, "warning: audit log append failed: %v\n", err)
			}

			fmt.Printf("Credentials saved to %s (type=%s)\n", credsPath, credType)
			if tenantID != "" {
				fmt.Printf("Tenant: %s\n", tenantID)
			}
			if email != "" || domain != "" {
				fmt.Printf("Identity: %s @ %s\n", email, domain)
			}
			if expiresAt != "" {
				fmt.Printf("Expires: %s\n", expiresAt)
			}
			fmt.Println("Run `neo status` to verify.")
			return nil
		},
	}
	cmd.Flags().StringVar(&token, "token", "", "API token")
	cmd.Flags().StringVar(&tenantID, "tenant", "", "Tenant ID (optional)")
	cmd.Flags().StringVar(&provider, "provider", "neo-cloud", "Provider name (default: neo-cloud)")
	cmd.Flags().StringVar(&credType, "type", "", "Credential type: api_token (default) or oauth2")
	cmd.Flags().StringVar(&email, "email", "", "Email (for Atlassian, GitHub Enterprise, etc.)")
	cmd.Flags().StringVar(&domain, "domain", "", "Domain (e.g. acme.atlassian.net)")
	cmd.Flags().StringVar(&refreshToken, "refresh-token", "", "OAuth2 refresh token (for type=oauth2)")
	cmd.Flags().StringVar(&expiresAt, "expires", "", "Expiration RFC3339 (e.g. 2027-04-28T00:00:00Z)")
	return cmd
}

// spaceCmd manages the active workspace/space/board per provider —
// PILAR XXIII / Épica 124.8. Persists to ~/.neo/contexts.json.
//
//	neo space use --provider jira --id ENG --name "Engineering" --board 15 --board-name "Sprint"
//	neo space list [--provider jira]
//	neo space current
//	neo space remove --provider jira --id ENG
func spaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "space",
		Short: "Manage active space/board per provider (Jira project, GitHub repo, etc.)",
	}
	cmd.AddCommand(spaceUseCmd(), spaceListCmd(), spaceCurrentCmd(), spaceRemoveCmd())
	return cmd
}

func spaceUseCmd() *cobra.Command {
	var provider, spaceID, spaceName, boardID, boardName string
	c := &cobra.Command{
		Use:   "use",
		Short: "Register and activate a space (and optional board) for a provider",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(provider) == "" || strings.TrimSpace(spaceID) == "" {
				return fmt.Errorf("--provider and --id are required")
			}
			path := auth.DefaultContextsPath()
			store, err := auth.LoadContexts(path)
			if err != nil {
				return fmt.Errorf("load contexts: %w", err)
			}
			store.Set(auth.Space{
				Provider:  provider,
				SpaceID:   spaceID,
				SpaceName: spaceName,
				BoardID:   boardID,
				BoardName: boardName,
			})
			if err := store.Use(provider, spaceID); err != nil {
				return err
			}
			if err := auth.SaveContexts(store, path); err != nil {
				return fmt.Errorf("save contexts: %w", err)
			}
			fmt.Printf("Active space for %q: %s", provider, spaceID)
			if spaceName != "" {
				fmt.Printf(" (%s)", spaceName)
			}
			if boardID != "" {
				fmt.Printf(" — board %s", boardID)
				if boardName != "" {
					fmt.Printf(" (%s)", boardName)
				}
			}
			fmt.Println()
			return nil
		},
	}
	c.Flags().StringVar(&provider, "provider", "", "Provider name (jira, github, ...)")
	c.Flags().StringVar(&spaceID, "id", "", "Space ID (project key, repo, etc.)")
	c.Flags().StringVar(&spaceName, "name", "", "Human-friendly space name (optional)")
	c.Flags().StringVar(&boardID, "board", "", "Board ID (optional — Jira/Linear/GitHub Projects)")
	c.Flags().StringVar(&boardName, "board-name", "", "Human-friendly board name (optional)")
	return c
}

func spaceListCmd() *cobra.Command {
	var provider string
	c := &cobra.Command{
		Use:   "list",
		Short: "List registered spaces (optionally filtered by provider)",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := auth.LoadContexts(auth.DefaultContextsPath())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			defer tw.Flush()
			fmt.Fprintln(tw, "ACTIVE\tPROVIDER\tSPACE\tNAME\tBOARD\tBOARD NAME")
			for _, sp := range store.Contexts {
				if provider != "" && sp.Provider != provider {
					continue
				}
				marker := ""
				if active := store.ActiveSpace(sp.Provider); active != nil && active.SpaceID == sp.SpaceID {
					marker = "*"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					marker, sp.Provider, sp.SpaceID, sp.SpaceName, sp.BoardID, sp.BoardName)
			}
			return nil
		},
	}
	c.Flags().StringVar(&provider, "provider", "", "Filter by provider")
	return c
}

func spaceCurrentCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "current",
		Short: "Show the currently active space for each provider",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := auth.LoadContexts(auth.DefaultContextsPath())
			if err != nil {
				return err
			}
			if len(store.Active) == 0 {
				fmt.Println("No active space. Run `neo space use --provider X --id Y` to set one.")
				return nil
			}
			for provider, spaceID := range store.Active {
				active := store.ActiveSpace(provider)
				if active == nil {
					fmt.Printf("%s: %s (orphan — space removed)\n", provider, spaceID)
					continue
				}
				suffix := ""
				if active.SpaceName != "" {
					suffix += fmt.Sprintf(" — %s", active.SpaceName)
				}
				if active.BoardID != "" {
					suffix += fmt.Sprintf(" / board %s", active.BoardID)
					if active.BoardName != "" {
						suffix += fmt.Sprintf(" (%s)", active.BoardName)
					}
				}
				fmt.Printf("%s: %s%s\n", provider, active.SpaceID, suffix)
			}
			return nil
		},
	}
}

func spaceRemoveCmd() *cobra.Command {
	var provider, spaceID string
	c := &cobra.Command{
		Use:   "remove",
		Short: "Remove a registered space (clears active marker if it pointed there)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(provider) == "" || strings.TrimSpace(spaceID) == "" {
				return fmt.Errorf("--provider and --id are required")
			}
			path := auth.DefaultContextsPath()
			store, err := auth.LoadContexts(path)
			if err != nil {
				return err
			}
			if !store.Remove(provider, spaceID) {
				return fmt.Errorf("space %q for provider %q not found", spaceID, provider)
			}
			if err := auth.SaveContexts(store, path); err != nil {
				return err
			}
			fmt.Printf("Removed space %q from provider %q.\n", spaceID, provider)
			return nil
		},
	}
	c.Flags().StringVar(&provider, "provider", "", "Provider name")
	c.Flags().StringVar(&spaceID, "id", "", "Space ID to remove")
	return c
}

// appendLoginAuditEntry writes one credential_added entry to the audit log.
// Best-effort: errors are propagated to the caller for surface-level warning,
// not for rollback. PILAR XXIII / Épica 124.6.
func appendLoginAuditEntry(provider, credType, tenantID string) error {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return fmt.Errorf("resolve home: %w", err)
	}
	logPath := filepath.Join(home, ".neo", "audit.log")
	logger, err := auth.OpenAuditLog(logPath)
	if err != nil {
		return err
	}
	defer logger.Close()
	_, err = logger.Append(auth.Event{
		Kind:     "credential_added",
		Actor:    "neo-cli/login",
		Provider: provider,
		TenantID: tenantID,
		Details:  map[string]any{"type": credType},
	})
	return err
}
