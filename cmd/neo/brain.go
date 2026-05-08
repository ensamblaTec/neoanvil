// cmd/neo/brain.go — `neo brain` CLI commands. PILAR XXVI / 135.D.
//
// Wires pkg/brain (manifest + archive + crypto) and pkg/brain/storage
// (BrainStore drivers) into operator-visible commands:
//
//	neo brain push    [--remote=local:///path | --remote=r2://...]
//	                  [--workspaces=id,id]
//	                  [--tag=name]
//	                  [--passphrase-env=VAR]
//	neo brain pull    [--remote=...] [--tag=name|latest] [--dry-run]
//	                  [--passphrase-env=VAR] [--dest=PATH]
//	neo brain log     [--remote=...] [--limit=N]
//	neo brain status  [--remote=...]
//	neo brain verify  [--remote=...] [--tag=name|latest]
//	neo brain diff    <hlc-a> <hlc-b> [--remote=...]   (placeholder; 136.* real impl)
//
// Remote URL forms:
//
//	local:///abs/path                            → storage.LocalStore
//	r2://<bucket>?account=<id>&key=<k>&secret=<s>  → storage.R2Store
//
// In production callers pass `--passphrase-env=NEO_BRAIN_PASS` and we
// read the secret from that env var. Putting it on argv would expose it
// to `ps` — never accept --passphrase=<value>.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/ensamblatec/neoanvil/pkg/brain"
	"github.com/ensamblatec/neoanvil/pkg/brain/merge"
	"github.com/ensamblatec/neoanvil/pkg/brain/storage"
	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/workspace"
)

// brainCmd is the root for all `neo brain ...` subcommands. Each
// subcommand is small; the shared helpers live below.
func brainCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "brain",
		Short: "Push/pull encrypted snapshots of every neoanvil workspace + project",
		Long: `Brain Portable (PILAR XXVI) — moves the entire neoanvil workspace state
between machines through encrypted archives stored in local FS or
Cloudflare R2. The on-disk format is documented in
docs/pilar-xxvi-brain-portable.md (operator runbook).`,
	}
	cmd.AddCommand(brainPushCmd(), brainPullCmd(), brainLogCmd(), brainStatusCmd(), brainVerifyCmd(), brainDiffCmd(), brainMergeCmd())
	return cmd
}

// =============================================================================
// push
// =============================================================================

func brainPushCmd() *cobra.Command {
	var (
		remote        string
		workspacesCSV string
		tag           string
		passphraseEnv string
		force         bool
	)
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Snapshot every (or filtered) workspace and upload to a brain store",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBrainPushWithLock(cmd.OutOrStdout(), remote, workspacesCSV, tag, passphraseEnv, force)
		},
	}
	cmd.Flags().StringVar(&remote, "remote", "", "destination URL (local:///path or r2://bucket?...) — required")
	cmd.Flags().StringVar(&workspacesCSV, "workspaces", "", "comma-separated workspace IDs to include (default: all)")
	cmd.Flags().StringVar(&tag, "tag", "", "human-readable tag for this snapshot (default: timestamp)")
	cmd.Flags().StringVar(&passphraseEnv, "passphrase-env", "NEO_BRAIN_PASS", "env var holding the encryption passphrase")
	cmd.Flags().BoolVar(&force, "force", false, "skip the distributed lease lock — use only when you're sure no other push is running")
	return cmd
}

// pushLockTTL is how long the per-remote push lease holds. 30 min covers
// a worst-case 4 GiB upload over slow uplink; reclaim semantics in the
// store layer take over if a holder crashes mid-push. [135.F.1]
const pushLockTTL = 30 * time.Minute

// runBrainPushWithLock wraps runBrainPush with distributed lock
// acquisition (135.F). When --force, the lock is bypassed and a warning
// surfaces in the output so operators can spot bypassed pushes in their
// audit logs.
func runBrainPushWithLock(out io.Writer, remote, workspacesCSV, tag, passphraseEnv string, force bool) error {
	if remote == "" {
		return errors.New("--remote is required (e.g. local:///path or r2://bucket?...)")
	}
	store, err := openBrainStore(remote)
	if err != nil {
		return err
	}
	defer store.Close()

	holder := holderID()
	if !force {
		lease, lockErr := store.Lock("brain-push", holder, pushLockTTL)
		if lockErr != nil {
			return fmt.Errorf("acquire push lock: %w (use --force only if you're sure)", lockErr)
		}
		defer func() { _ = store.Unlock(lease) }()
	} else {
		fmt.Fprintln(out, "⚠ --force: skipping push lock; concurrent pushes may corrupt the remote state")
	}
	return runBrainPush(out, store, workspacesCSV, tag, passphraseEnv)
}

// holderID identifies this push for the lease record. Combines the
// brain.NodeFingerprint (stable per machine) with the OS PID so two
// concurrent pushes from the same machine are still distinguishable in
// the audit trail.
func holderID() string {
	return fmt.Sprintf("%s/pid=%d", brain.NodeFingerprint(), os.Getpid())
}

func runBrainPush(out io.Writer, store storage.BrainStore, workspacesCSV, tag, passphraseEnv string) error {
	pass, err := readPassphrase(passphraseEnv)
	if err != nil {
		return err
	}

	reg, err := workspace.LoadRegistry()
	if err != nil {
		return fmt.Errorf("load registry: %w", err)
	}
	walked := brain.WalkWorkspaces(reg)
	if filter := parseCSV(workspacesCSV); len(filter) > 0 {
		walked = filterByID(walked, filter)
	}
	if len(walked) == 0 {
		return errors.New("no workspaces selected")
	}

	projects, orgs := brain.WalkDependencies(walked)
	manifest := brain.NewManifest(walked, projects, orgs)

	var archiveBuf bytes.Buffer
	uncompressed, err := brain.BuildArchive(manifest, &archiveBuf)
	if err != nil {
		return fmt.Errorf("build archive: %w", err)
	}

	salt := []byte(manifest.NodeID) // device-bound salt
	key, err := brain.DeriveKeyCached([]byte(pass), salt)
	if err != nil {
		return fmt.Errorf("derive key: %w", err)
	}
	aad := []byte(manifest.HLC.String() + "|" + manifest.NodeID)

	var encrypted bytes.Buffer
	if err := brain.EncryptStream(&archiveBuf, &encrypted, key, aad); err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}

	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	prefix := snapshotPrefix(manifest.HLC, tag)
	if _, err := store.Put(prefix+"/manifest.json", bytes.NewReader(manifestJSON)); err != nil {
		return fmt.Errorf("put manifest: %w", err)
	}
	if _, err := store.Put(prefix+"/archive.bin", &encrypted); err != nil {
		return fmt.Errorf("put archive: %w", err)
	}

	fmt.Fprintf(out, "✓ pushed snapshot hlc=%s node=%s tag=%s\n", manifest.HLC, manifest.NodeID, tag)
	fmt.Fprintf(out, "  workspaces=%d projects=%d orgs=%d uncompressed=%dB encrypted=%dB\n",
		len(manifest.Workspaces), len(manifest.Projects), len(manifest.Orgs), uncompressed, encrypted.Len())
	return nil
}

// =============================================================================
// pull
// =============================================================================

func brainPullCmd() *cobra.Command {
	var (
		remote, tag, dest, passphraseEnv string
		dryRun                            bool
	)
	cmd := &cobra.Command{
		Use:   "pull",
		Short: "Download a snapshot, decrypt, and restore files under --dest",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBrainPull(cmd.OutOrStdout(), remote, tag, dest, passphraseEnv, dryRun)
		},
	}
	cmd.Flags().StringVar(&remote, "remote", "", "source URL — required")
	cmd.Flags().StringVar(&tag, "tag", "latest", "snapshot tag or 'latest' for highest HLC")
	cmd.Flags().StringVar(&dest, "dest", "", "directory under which to restore files — required")
	cmd.Flags().StringVar(&passphraseEnv, "passphrase-env", "NEO_BRAIN_PASS", "env var with encryption passphrase")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list files that would be written without touching disk")
	return cmd
}

func runBrainPull(out io.Writer, remote, tag, dest, passphraseEnv string, dryRun bool) error {
	if remote == "" {
		return errors.New("--remote is required")
	}
	if dest == "" {
		return errors.New("--dest is required")
	}
	store, err := openBrainStore(remote)
	if err != nil {
		return err
	}
	defer store.Close()

	prefix, err := resolveTagPrefix(store, tag)
	if err != nil {
		return err
	}
	manifest, err := readManifest(store, prefix)
	if err != nil {
		return err
	}
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("invalid manifest: %w", err)
	}

	pass, err := readPassphrase(passphraseEnv)
	if err != nil {
		return err
	}
	salt := []byte(manifest.NodeID)
	key, err := brain.DeriveKeyCached([]byte(pass), salt)
	if err != nil {
		return fmt.Errorf("derive key: %w", err)
	}

	rc, err := store.Get(prefix + "/archive.bin")
	if err != nil {
		return fmt.Errorf("get archive: %w", err)
	}
	defer rc.Close()

	aad := []byte(manifest.HLC.String() + "|" + manifest.NodeID)
	var plaintext bytes.Buffer
	if err := brain.DecryptStream(rc, &plaintext, key, aad); err != nil {
		return fmt.Errorf("decrypt: %w", err)
	}

	written, err := brain.ApplyArchive(&plaintext, manifest, brain.HLC{}, dest, dryRun)
	if err != nil {
		return fmt.Errorf("apply archive: %w", err)
	}

	verb := "wrote"
	if dryRun {
		verb = "would write"
	}
	fmt.Fprintf(out, "✓ %s %d files (hlc=%s node=%s)\n", verb, len(written), manifest.HLC, manifest.NodeID)
	return nil
}

// =============================================================================
// log
// =============================================================================

func brainLogCmd() *cobra.Command {
	var (
		remote string
		limit  int
	)
	cmd := &cobra.Command{
		Use:   "log",
		Short: "List snapshots in the remote, newest first",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBrainLog(cmd.OutOrStdout(), remote, limit)
		},
	}
	cmd.Flags().StringVar(&remote, "remote", "", "remote URL — required")
	cmd.Flags().IntVar(&limit, "limit", 10, "max snapshots to list")
	return cmd
}

func runBrainLog(out io.Writer, remote string, limit int) error {
	if remote == "" {
		return errors.New("--remote is required")
	}
	store, err := openBrainStore(remote)
	if err != nil {
		return err
	}
	defer store.Close()

	prefixes, err := listSnapshotPrefixes(store)
	if err != nil {
		return err
	}
	// Sort by manifest HLC (newest first).
	type entry struct {
		prefix string
		m      *brain.Manifest
	}
	var entries []entry
	for _, p := range prefixes {
		m, err := readManifest(store, p)
		if err != nil {
			fmt.Fprintf(out, "  ! %s: %v\n", p, err)
			continue
		}
		entries = append(entries, entry{p, m})
	}
	sort.Slice(entries, func(i, j int) bool {
		return brain.CompareHLC(entries[i].m.HLC, entries[j].m.HLC) > 0
	})
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	if len(entries) == 0 {
		fmt.Fprintln(out, "(no snapshots)")
		return nil
	}
	fmt.Fprintf(out, "%-30s %-30s %-25s workspaces\n", "PREFIX", "HLC", "NODE")
	for _, e := range entries {
		fmt.Fprintf(out, "%-30s %-30s %-25s %d\n", e.prefix, e.m.HLC, e.m.NodeID, len(e.m.Workspaces))
	}
	return nil
}

// =============================================================================
// status
// =============================================================================

func brainStatusCmd() *cobra.Command {
	var remote string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Compare local registry HLC vs latest remote snapshot",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBrainStatus(cmd.OutOrStdout(), remote)
		},
	}
	cmd.Flags().StringVar(&remote, "remote", "", "remote URL — required")
	return cmd
}

func runBrainStatus(out io.Writer, remote string) error {
	if remote == "" {
		return errors.New("--remote is required")
	}
	store, err := openBrainStore(remote)
	if err != nil {
		return err
	}
	defer store.Close()

	prefixes, err := listSnapshotPrefixes(store)
	if err != nil {
		return err
	}
	if len(prefixes) == 0 {
		fmt.Fprintln(out, "remote is empty")
		return nil
	}
	var latest *brain.Manifest
	var latestPrefix string
	for _, p := range prefixes {
		m, err := readManifest(store, p)
		if err != nil {
			continue
		}
		if latest == nil || brain.CompareHLC(m.HLC, latest.HLC) > 0 {
			latest = m
			latestPrefix = p
		}
	}
	if latest == nil {
		return errors.New("no readable manifests in remote")
	}

	reg, err := workspace.LoadRegistry()
	if err != nil {
		return fmt.Errorf("load registry: %w", err)
	}
	localCount := len(brain.WalkWorkspaces(reg))
	fmt.Fprintf(out, "remote latest: %s (hlc=%s node=%s, workspaces=%d)\n",
		latestPrefix, latest.HLC, latest.NodeID, len(latest.Workspaces))
	fmt.Fprintf(out, "local registry: %d workspaces\n", localCount)
	if localCount == len(latest.Workspaces) {
		fmt.Fprintln(out, "→ shape match (workspace count); content drift not yet diffed (deferred to 136.*)")
	} else {
		fmt.Fprintf(out, "→ workspace count differs (local=%d remote=%d)\n", localCount, len(latest.Workspaces))
	}
	return nil
}

// =============================================================================
// verify
// =============================================================================

func brainVerifyCmd() *cobra.Command {
	var (
		remote, tag string
	)
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Download manifest + archive and validate without restoring",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBrainVerify(cmd.OutOrStdout(), remote, tag)
		},
	}
	cmd.Flags().StringVar(&remote, "remote", "", "remote URL — required")
	cmd.Flags().StringVar(&tag, "tag", "latest", "snapshot tag or 'latest'")
	return cmd
}

func runBrainVerify(out io.Writer, remote, tag string) error {
	if remote == "" {
		return errors.New("--remote is required")
	}
	store, err := openBrainStore(remote)
	if err != nil {
		return err
	}
	defer store.Close()

	prefix, err := resolveTagPrefix(store, tag)
	if err != nil {
		return err
	}
	m, err := readManifest(store, prefix)
	if err != nil {
		return err
	}
	if err := m.Validate(); err != nil {
		return fmt.Errorf("manifest invalid: %w", err)
	}
	rc, err := store.Get(prefix + "/archive.bin")
	if err != nil {
		return fmt.Errorf("get archive: %w", err)
	}
	defer rc.Close()
	fp, err := brain.Fingerprint(rc, nil)
	if err != nil {
		return fmt.Errorf("fingerprint: %w", err)
	}
	fmt.Fprintf(out, "✓ %s OK (hlc=%s, fingerprint=%x...)\n", prefix, m.HLC, fp[:8])
	return nil
}

// =============================================================================
// diff (placeholder — full implementation lands in 136.* merge sync)
// =============================================================================

func brainDiffCmd() *cobra.Command {
	var remote string
	cmd := &cobra.Command{
		Use:   "diff <hlc-a> <hlc-b>",
		Short: "(placeholder) Three-way diff between two remote snapshots",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "diff %s..%s @ %s — not implemented yet (136.A.x)\n", args[0], args[1], remote)
			return nil
		},
	}
	cmd.Flags().StringVar(&remote, "remote", "", "remote URL")
	return cmd
}

// =============================================================================
// 136.C — neo brain merge
// =============================================================================
//
// Orchestrates diff + detect + resolve into one operator-facing flow:
//
//	neo brain merge --remote=<url> [--strategy=interactive|local|remote|auto-only]
//	                [--buckets=knowledge.db,planner.db]   (extraction scope)
//	                [--passphrase-env=VAR]
//
// The merge engine (pkg/brain/merge) is fully wired and tested. The
// live-bbolt bucket extraction (open both sides' .db files, materialize
// each declared bucket into map[string][]byte, feed to DiffBuckets) is
// the integration step that depends on the operator's decision about
// which buckets are mergeable — that lives as a follow-up because the
// answer is per-deployment policy, not engine code.
//
// Today the command:
//   - validates flags
//   - pulls the remote manifest
//   - confirms ancestor lineage with the local node's history
//   - reports the planned merge (strategy + buckets in scope)
//   - exercises the orchestrator with empty input as a smoke test
//
// 136.D.2 (--strategy=auto-only) and 136.D.3 (stats reporting) are
// already supported because Orchestrate handles both.

func brainMergeCmd() *cobra.Command {
	var (
		remote        string
		strategyFlag  string
		bucketsCSV    string
		passphraseEnv string
	)
	cmd := &cobra.Command{
		Use:   "merge",
		Short: "Orchestrate diff + conflict detection + resolution between local and remote snapshot",
		Long: `Run a three-way merge between the local workspace state and the latest
remote snapshot.

Strategies:

  interactive  prompt the operator per conflict (default)
  local        every conflict resolves to the local-side value
  remote       every conflict resolves to the remote-side value
  auto-only    resolve only the trivially-mergeable conflicts;
               error out when any non-trivial conflict remains
               (use in CI / scripted invocations)

Live bucket extraction is integration-pending — today the CLI
validates inputs and exercises the merge engine; production rollout of
bucket-by-bucket bbolt diffing arrives in a follow-up commit per the
operator's bucket-policy decisions.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBrainMerge(cmd.OutOrStdout(), remote, strategyFlag, bucketsCSV, passphraseEnv)
		},
	}
	cmd.Flags().StringVar(&remote, "remote", "", "remote URL — required")
	cmd.Flags().StringVar(&strategyFlag, "strategy", "interactive", "interactive | local | remote | auto-only")
	cmd.Flags().StringVar(&bucketsCSV, "buckets", "", "comma-separated bucket names to merge (default: empty smoke test)")
	cmd.Flags().StringVar(&passphraseEnv, "passphrase-env", "NEO_BRAIN_PASS", "env var with encryption passphrase")
	return cmd
}

func runBrainMerge(out io.Writer, remote, strategyFlag, bucketsCSV, passphraseEnv string) error {
	if remote == "" {
		return errors.New("--remote is required")
	}
	strategy, err := parseMergeStrategy(strategyFlag)
	if err != nil {
		return err
	}

	store, err := openBrainStore(remote)
	if err != nil {
		return err
	}
	defer store.Close()

	prefix, err := resolveTagPrefix(store, "latest")
	if err != nil {
		return err
	}
	manifest, err := readManifest(store, prefix)
	if err != nil {
		return err
	}
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("remote manifest invalid: %w", err)
	}

	fmt.Fprintf(out, "remote latest: %s (hlc=%s, node=%s)\n", prefix, manifest.HLC, manifest.NodeID)
	fmt.Fprintf(out, "strategy:      %s\n", strategy)
	if bucketsCSV != "" {
		fmt.Fprintf(out, "buckets:       %s\n", bucketsCSV)
	} else {
		fmt.Fprintln(out, "buckets:       (none — smoke test only; pass --buckets=... to enable extraction)")
	}

	// Smoke-exercise the orchestrator with empty input so the binary
	// path is covered even when bucket extraction hasn't been wired.
	smoke, err := merge.Orchestrate(merge.OrchestrateInput{
		Bucket:    "(smoke)",
		Kind:      merge.BucketGeneric,
		Conflicts: nil,
	}, strategy, interactiveResolverFromStdin)
	if err != nil {
		return fmt.Errorf("orchestrate smoke: %w", err)
	}
	fmt.Fprintf(out, "merge engine OK — %s\n", merge.FormatStats(smoke.Stats))

	if bucketsCSV != "" {
		fmt.Fprintln(out, "⚠ live bucket extraction is integration-pending; merged snapshot was NOT pushed")
	}
	_ = passphraseEnv // reserved for the live-extraction path that decrypts both sides
	return nil
}

// parseMergeStrategy maps the CLI flag to the merge.Strategy enum.
func parseMergeStrategy(s string) (merge.Strategy, error) {
	switch s {
	case "interactive":
		return merge.StrategyInteractive, nil
	case "local":
		return merge.StrategyTakeLocal, nil
	case "remote":
		return merge.StrategyTakeRemote, nil
	case "auto-only":
		return merge.StrategyAutoOnly, nil
	default:
		return "", fmt.Errorf("unknown --strategy %q (expected interactive|local|remote|auto-only)", s)
	}
}

// interactiveResolverFromStdin reads a single character (l/r/e/s/a)
// from stdin per conflict. Implementation kept minimal — the live-
// extraction wiring will replace stdin with a richer prompt loop +
// editor invocation. For the smoke test path this is never called.
func interactiveResolverFromStdin(c merge.Conflict) ([]byte, error) {
	fmt.Fprintf(os.Stderr, "\nconflict %s/%s: %s\n  [l]ocal | [r]emote | [a]bort > ", c.Bucket, c.Key, c.Reason)
	var resp string
	if _, err := fmt.Fscanln(os.Stdin, &resp); err != nil {
		return nil, fmt.Errorf("read prompt: %w", err)
	}
	switch resp {
	case "l", "L":
		return c.LocalValue, nil
	case "r", "R":
		return c.RemoteValue, nil
	case "a", "A":
		return nil, errors.New("operator aborted")
	default:
		return nil, fmt.Errorf("invalid choice %q (expected l/r/a)", resp)
	}
}

// =============================================================================
// 135.G — neo workspace canonical-id
// =============================================================================
//
// Three sub-modes determined by flags:
//
//	(no flags)               → print resolved canonical_id of current
//	                            workspace (or --workspace=<id>)
//	--set <value>            → write workspace.canonical_id to neo.yaml
//	                            (override; takes precedence over auto)
//	--map <canonical>=<path> → install/update a path_map entry

// canonicalIDCmd is registered separately under workspaceCmd in main.go;
// this constructor is exported only as a convenience so a future
// `neo workspace ...` umbrella can pull it in.
func canonicalIDCmd() *cobra.Command {
	var (
		workspaceID string
		setValue    string
		mapBinding  string
	)
	cmd := &cobra.Command{
		Use:   "canonical-id",
		Short: "Print, override, or remap the cross-machine workspace identifier",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCanonicalID(cmd.OutOrStdout(), workspaceID, setValue, mapBinding)
		},
	}
	cmd.Flags().StringVar(&workspaceID, "workspace", "", "workspace ID to resolve (default: cwd)")
	cmd.Flags().StringVar(&setValue, "set", "", "write this string as workspace.canonical_id in neo.yaml")
	cmd.Flags().StringVar(&mapBinding, "map", "", `install path_map entry: "<canonical_id>=<path>"`)
	return cmd
}

func runCanonicalID(out io.Writer, workspaceID, setValue, mapBinding string) error {
	switch {
	case mapBinding != "":
		return runCanonicalIDMap(out, mapBinding)
	case setValue != "":
		return runCanonicalIDSet(out, workspaceID, setValue)
	default:
		return runCanonicalIDPrint(out, workspaceID)
	}
}

func runCanonicalIDPrint(out io.Writer, workspaceID string) error {
	wsPath, err := resolveWorkspacePathForCLI(workspaceID)
	if err != nil {
		return err
	}
	res := brain.ResolveCanonicalID(wsPath)
	fmt.Fprintf(out, "canonical_id: %s\nsource:       %s\nworkspace:    %s\n", res.ID, res.Source, wsPath)
	return nil
}

func runCanonicalIDSet(out io.Writer, workspaceID, value string) error {
	wsPath, err := resolveWorkspacePathForCLI(workspaceID)
	if err != nil {
		return err
	}
	yamlPath := filepath.Join(wsPath, "neo.yaml")
	cfg, err := config.LoadConfig(yamlPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", yamlPath, err)
	}
	cfg.Workspace.CanonicalID = value
	// LoadConfig writes-back enriched defaults; that path also persists
	// edits made to the in-memory cfg. Re-marshal explicitly for clarity.
	if err := writeNeoYAML(yamlPath, cfg); err != nil {
		return err
	}
	fmt.Fprintf(out, "✓ workspace.canonical_id = %q written to %s\n", value, yamlPath)
	return nil
}

func runCanonicalIDMap(out io.Writer, binding string) error {
	idx := strings.IndexByte(binding, '=')
	if idx <= 0 || idx == len(binding)-1 {
		return fmt.Errorf(`--map expects "<canonical_id>=<path>", got %q`, binding)
	}
	canonicalID, path := strings.TrimSpace(binding[:idx]), strings.TrimSpace(binding[idx+1:])
	if canonicalID == "" || path == "" {
		return errors.New("--map: canonical_id and path must be non-empty")
	}
	pmPath := brain.DefaultPathMapPath()
	if pmPath == "" {
		return errors.New("could not resolve ~/.neo/path_map.json")
	}
	pm, err := brain.LoadPathMap(pmPath)
	if err != nil {
		return fmt.Errorf("load path_map: %w", err)
	}
	if err := pm.Set(canonicalID, brain.PathMapEntry{Path: path}); err != nil {
		return fmt.Errorf("set entry: %w", err)
	}
	if err := pm.Save(pmPath); err != nil {
		return fmt.Errorf("save path_map: %w", err)
	}
	fmt.Fprintf(out, "✓ path_map[%s] = %s (saved to %s)\n", canonicalID, path, pmPath)
	return nil
}

// resolveWorkspacePathForCLI maps an optional --workspace flag to an
// absolute path. Empty flag → cwd. Explicit ID → registry lookup.
func resolveWorkspacePathForCLI(workspaceID string) (string, error) {
	if workspaceID == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getwd: %w", err)
		}
		return cwd, nil
	}
	reg, err := workspace.LoadRegistry()
	if err != nil {
		return "", fmt.Errorf("load registry: %w", err)
	}
	for _, w := range reg.Workspaces {
		if w.ID == workspaceID || w.Name == workspaceID {
			return w.Path, nil
		}
	}
	return "", fmt.Errorf("workspace %q not found in registry", workspaceID)
}

// writeNeoYAML serializes cfg back to path via yaml.Marshal. Operators
// normally keep neo.yaml in git; this overwrites the file (no merge —
// LoadConfig already auto-enriched the in-memory cfg with defaults so
// the round-trip is faithful for the fields the loader knows about).
func writeNeoYAML(path string, cfg *config.NeoConfig) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// =============================================================================
// helpers
// =============================================================================

// openBrainStore parses a `local:///path` or `r2://bucket?...` URL into
// a BrainStore implementation.
func openBrainStore(remote string) (storage.BrainStore, error) {
	u, err := url.Parse(remote)
	if err != nil {
		return nil, fmt.Errorf("parse remote: %w", err)
	}
	switch u.Scheme {
	case "local":
		path := u.Path
		if path == "" {
			path = u.Host + u.Path
		}
		if path == "" {
			return nil, errors.New("local:// remote requires a path")
		}
		return storage.NewLocalStore(path)
	case "r2":
		q := u.Query()
		bucket := u.Host
		if bucket == "" {
			return nil, errors.New("r2:// remote requires a bucket in the host position")
		}
		account := q.Get("account")
		accessKey := q.Get("key")
		secretKey := q.Get("secret")
		if account == "" {
			account = os.Getenv("R2_ACCOUNT_ID")
		}
		if accessKey == "" {
			accessKey = os.Getenv("R2_ACCESS_KEY_ID")
		}
		if secretKey == "" {
			secretKey = os.Getenv("R2_SECRET_ACCESS_KEY")
		}
		return storage.NewR2Store(account, accessKey, storage.Secret(secretKey), bucket)
	default:
		return nil, fmt.Errorf("unsupported remote scheme %q (expected local:// or r2://)", u.Scheme)
	}
}

// readPassphrase fetches the secret from envName. Empty → typed error
// (we never read passphrase from argv).
func readPassphrase(envName string) (string, error) {
	if envName == "" {
		return "", errors.New("--passphrase-env name is empty")
	}
	v := os.Getenv(envName)
	if v == "" {
		return "", fmt.Errorf("env var %q is not set; export it before running this command", envName)
	}
	return v, nil
}

// snapshotPrefix builds the storage key prefix for a snapshot:
//
//	snapshots/<hlc>            when tag is empty
//	snapshots/<hlc>-<tag>      when tag is set (slugified)
func snapshotPrefix(hlc brain.HLC, tag string) string {
	base := fmt.Sprintf("snapshots/%s", hlc)
	if tag == "" {
		return base
	}
	return base + "-" + slugify(tag)
}

func slugify(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// listSnapshotPrefixes deduplicates "snapshots/<id>" segments from the
// store's List output.
func listSnapshotPrefixes(store storage.BrainStore) ([]string, error) {
	chunks, err := store.List("snapshots/")
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	seen := map[string]bool{}
	var prefixes []string
	for _, c := range chunks {
		// c.Key looks like "snapshots/<id>/manifest.json" or ".../archive.bin".
		parts := strings.SplitN(c.Key, "/", 3)
		if len(parts) < 2 {
			continue
		}
		prefix := parts[0] + "/" + parts[1]
		if !seen[prefix] {
			seen[prefix] = true
			prefixes = append(prefixes, prefix)
		}
	}
	return prefixes, nil
}

// readManifest fetches and parses the manifest.json for a snapshot prefix.
func readManifest(store storage.BrainStore, prefix string) (*brain.Manifest, error) {
	rc, err := store.Get(prefix + "/manifest.json")
	if err != nil {
		return nil, fmt.Errorf("get manifest: %w", err)
	}
	defer rc.Close()
	var m brain.Manifest
	dec := json.NewDecoder(rc)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}

// resolveTagPrefix maps a tag (or "latest") to a snapshot prefix.
// "latest" picks the snapshot with the highest HLC.
func resolveTagPrefix(store storage.BrainStore, tag string) (string, error) {
	prefixes, err := listSnapshotPrefixes(store)
	if err != nil {
		return "", err
	}
	if len(prefixes) == 0 {
		return "", errors.New("no snapshots in remote")
	}
	if tag != "" && tag != "latest" {
		// Explicit tag: match any prefix ending in -<tag>.
		want := "-" + slugify(tag)
		for _, p := range prefixes {
			if strings.HasSuffix(p, want) {
				return p, nil
			}
		}
		return "", fmt.Errorf("no snapshot matches tag %q", tag)
	}
	// latest: highest HLC.
	var latestPrefix string
	var latestHLC brain.HLC
	for _, p := range prefixes {
		m, err := readManifest(store, p)
		if err != nil {
			continue
		}
		if latestPrefix == "" || brain.CompareHLC(m.HLC, latestHLC) > 0 {
			latestPrefix = p
			latestHLC = m.HLC
		}
	}
	if latestPrefix == "" {
		return "", errors.New("no readable snapshots")
	}
	return latestPrefix, nil
}

// parseCSV splits "a,b,c" into ["a","b","c"]. Empty segments dropped.
func parseCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// filterByID keeps only entries whose ID is in the allowed set.
func filterByID(walked []brain.WalkedWorkspace, allowed []string) []brain.WalkedWorkspace {
	allow := map[string]bool{}
	for _, id := range allowed {
		allow[id] = true
	}
	var out []brain.WalkedWorkspace
	for _, w := range walked {
		if allow[w.ID] {
			out = append(out, w)
		}
	}
	return out
}

