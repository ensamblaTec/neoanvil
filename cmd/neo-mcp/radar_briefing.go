package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"sort"

	"github.com/ensamblatec/neoanvil/pkg/astx"
	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/federation"
	"github.com/ensamblatec/neoanvil/pkg/hardware"
	"github.com/ensamblatec/neoanvil/pkg/incidents"
	"github.com/ensamblatec/neoanvil/pkg/knowledge"
	"github.com/ensamblatec/neoanvil/pkg/rag"
	"github.com/ensamblatec/neoanvil/pkg/sre"
	"github.com/ensamblatec/neoanvil/pkg/state"
)

func (t *RadarTool) handleReadMasterPlan(_ context.Context, args map[string]any) (any, error) {
	filterOpen, _ := args["filter_open"].(bool)
	scope, _ := args["scope"].(string)

	var data string
	var err error
	if filterOpen {
		data, err = state.ReadOpenTasks(t.workspace)
	} else {
		data, err = state.ReadActivePhase(t.workspace)
	}
	if err != nil {
		return nil, err
	}

	// [332.B] scope:"project" merges shared epics from KnowledgeStore.
	if scope == "project" {
		data += masterPlanProjectEpics(t.knowledgeStore, filterOpen)
	}

	return mcpText(data), nil
}

// masterPlanProjectEpics renders a Markdown table of shared epics from
// KnowledgeStore, annotated with [proj]. [332.B]
func masterPlanProjectEpics(ks *knowledge.KnowledgeStore, filterOpen bool) string {
	if ks == nil {
		return "\n\n> ℹ️ scope:project — KnowledgeStore not available (not in project mode)\n"
	}
	epics, err := ks.ListEpicsByStatus("")
	if err != nil {
		return fmt.Sprintf("\n\n> ⚠️ scope:project — epics fetch error: %v\n", err)
	}
	if filterOpen {
		filtered := epics[:0]
		for _, e := range epics {
			if e.EpicStatus == knowledge.EpicStatusOpen || e.EpicStatus == knowledge.EpicStatusInProgress {
				filtered = append(filtered, e)
			}
		}
		epics = filtered
	}
	if len(epics) == 0 {
		return "\n\n## Project Epics [proj]\n\n_No shared epics found._\n"
	}
	// Sort: P0→P1→P2→P3→unset, then in_progress→open→blocked→done, then by key.
	sort.Slice(epics, func(i, j int) bool {
		pi, pj := epicPriorityRank(epics[i].Priority), epicPriorityRank(epics[j].Priority)
		if pi != pj {
			return pi < pj
		}
		si, sj := epicStatusRank(epics[i].EpicStatus), epicStatusRank(epics[j].EpicStatus)
		if si != sj {
			return si < sj
		}
		return epics[i].Key < epics[j].Key
	})
	var sb strings.Builder
	sb.WriteString("\n\n## Project Epics [proj]\n\n")
	sb.WriteString("| Key | Title | Status | Priority | Owner |\n")
	sb.WriteString("|-----|-------|--------|----------|-------|\n")
	for _, e := range epics {
		st := e.EpicStatus
		if st == "" {
			st = knowledge.EpicStatusOpen
		}
		fmt.Fprintf(&sb, "| `%s` | %s | %s | %s | %s |\n",
			e.Key, e.EpicTitle, st, e.Priority, e.EpicOwner)
	}
	return sb.String()
}

func epicPriorityRank(p string) int {
	switch p {
	case knowledge.EpicPriorityP0:
		return 0
	case knowledge.EpicPriorityP1:
		return 1
	case knowledge.EpicPriorityP2:
		return 2
	case knowledge.EpicPriorityP3:
		return 3
	default:
		return 4
	}
}

func epicStatusRank(s string) int {
	switch s {
	case knowledge.EpicStatusInProgress:
		return 0
	case knowledge.EpicStatusOpen:
		return 1
	case knowledge.EpicStatusBlocked:
		return 2
	default:
		return 3
	}
}

func (t *RadarTool) handleSemanticAST(_ context.Context, args map[string]any) (any, error) {
	target, _ := args["target"].(string)
	if target == "" {
		return nil, fmt.Errorf("target is required for SEMANTIC_AST")
	}
	data, err := os.ReadFile(target) //nolint:gosec // G304-WORKSPACE-CANON
	if err != nil {
		return nil, err
	}
	chunks := astx.SemanticChunk(context.Background(), data, filepath.Ext(target))
	return mcpText(fmt.Sprintf("Found %d semantic chunks in %s", len(chunks), target)), nil
}

func (t *RadarTool) handleReadSlice(_ context.Context, args map[string]any) (any, error) {
	target, _ := args["target"].(string)
	if target == "" {
		return nil, fmt.Errorf("target (filename) is required for READ_SLICE")
	}
	startLine := 1
	if sl, ok := args["start_line"].(float64); ok && sl > 0 {
		startLine = int(sl)
	}
	limit := 100
	if lFloat, ok := args["limit"].(float64); ok && lFloat > 0 {
		limit = int(lFloat)
	}

	// [SRE-60.1] Direct file read — no intermediate map, no fmt.Sprintf("%v") fallback.
	// Resolves path relative to workspace when not absolute.
	absPath := target
	if !filepath.IsAbs(target) {
		absPath = filepath.Join(t.workspace, target)
	}
	// [LARGE-PROJECT/A] Try hot-files cache first; mtime+size invalidation
	// inside Get guarantees we never serve stale content. Miss path falls
	// through to os.ReadFile + Put for next time.
	var data []byte
	if t.hotFiles != nil {
		if cached, ok := t.hotFiles.Get(absPath); ok {
			data = cached
		}
	}
	if data == nil {
		var err error
		data, err = os.ReadFile(absPath) //nolint:gosec // G304-WORKSPACE-CANON
		if err != nil {
			return nil, fmt.Errorf("READ_SLICE: cannot read %s: %w", target, err)
		}
		if t.hotFiles != nil {
			t.hotFiles.Put(absPath, data)
		}
	}

	allLines := strings.Split(string(data), "\n")
	total := len(allLines)

	start := max(startLine-1, 0) // convert to 0-based index
	if start >= total {
		return mcpText(fmt.Sprintf("// %s — start_line %d exceeds file length (%d lines)", target, startLine, total)), nil
	}
	end := min(start+limit, total)

	header := fmt.Sprintf("// %s — lines %d-%d of %d\n", target, start+1, end, total)
	content := header + strings.Join(allLines[start:end], "\n")

	// [372.A+B] Advisory when READ_SLICE used without prior COMPILE_AUDIT.
	if !t.cfg.SRE.ReadSliceAdvisoryOff {
		t.sessionIntentMu.Lock()
		compileCount := t.sessionIntentCounts["COMPILE_AUDIT"]
		t.sessionIntentMu.Unlock()
		if compileCount == 0 {
			tokensConsumed := len(content) / 4
			content += fmt.Sprintf("\n\n---\n💡 FILE_EXTRACT(target:%q, query:<symbol>) extracts only relevant context (~75%% fewer tokens). Run COMPILE_AUDIT first for symbol_map.\n📊 ~%d tokens consumed. FILE_EXTRACT for a symbol ≈ %d tokens.", target, tokensConsumed, tokensConsumed/4)
		}
	}

	return mcpText(content), nil
}

// briefingData holds all gathered state for a BRIEFING response. [SRE-118.A]
type briefingData struct {
	serverMode      string
	phaseErr        error
	activePhase     string
	planOpen        int
	planClosed      int
	openTaskLines   []string
	boltPending     int
	heapMB          float64
	gcRuns          uint32
	recvBytes       int64
	sentBytes       int64
	sessionMuts     []string
	ragCoverage     float64 // percentage 0-100
	binAgeStr       string
	phaseName       string
	compactLine     string
	hasDigest            bool    // true when PROJECT_DIGEST was run this session [148.D]
	digestAgeHours       float64 // hours since last PROJECT_DIGEST run [148.D]
	incidentCount        int     // total INC-*.md files [154.A]
	lastIncidentAgeHours float64 // hours since most-recent INC file mtime [154.A]
	lastIncidentPath     string  // path of most-recent INC file [154.A]
	criticalIncidents24h int     // INC files in last 24h with CRITICAL severity [154.B]
	criticalIncidentName string  // name of most recent critical INC in last 24h [154.B]
	resumeWarning        bool   // true when agent worked without prior BRIEFING this boot [156.C]
	// [Épica 176/180] Cache telemetry — both LRU layers surfaced in BRIEFING
	// so the operator sees at a glance whether the caches are pulling their
	// weight. cacheX = QueryCache (SEMANTIC_CODE node IDs).
	// textCacheX = TextCache (BLAST_RADIUS full markdown).
	cacheHits     uint64
	cacheMisses   uint64
	cacheEvicts   uint64
	cacheSize     int
	cacheCapacity int
	textCacheHits   uint64
	textCacheMisses uint64
	textCacheEvicts uint64 // [189] used for eviction-rate warning
	textCacheSize   int
	embCacheHits    uint64 // [205]
	embCacheMisses  uint64
	embCacheSize    int
	binaryStaleAlert     bool   // true when bin/neo-mcp is older than most recent commit [162.A]
	staleMinutes         int    // minutes of binary_stale_vs_HEAD [162.A]
	memorySchemaStale    bool   // true when tool_memory.go is newer than bin/neo-mcp [PILAR XXXIX]
	knowledgeHot         int      // [295.D] hot entries in KnowledgeStore HotCache
	knowledgeTotal       int      // [295.D] total entries in KnowledgeStore HotCache
	staleContractKeys    []string // [298.C] contract entries updated after session start
	// [331.B] Inbox presence surfacing — nonzero when current workspace has
	// unread messages addressed to it in namespace "inbox".
	unreadInboxCount     int      // how many entries with ReadAt==0 target this workspace
	unreadInboxSenders   []string // up to 3 distinct From values, for the compact line hint
	incTotal             int    // total INC-*.md files in workspace [158.B]
	incIndexed           int64  // incidents indexed this session [158.B]
	knowledgeConflicts   int    // [342.A] unresolved LWW conflict entries in NSConflicts
	// [Épica 236] CPG OOM observability — complements 229.4's hot-reloadable
	// OOM guard by surfacing current vs limit. SEMANTIC NOTE 2026-05-15: the
	// "Heap" value here is PROCESS-WIDE HeapAlloc (HNSW + memex + SharedMem
	// + dep-graph + everything else), NOT CPG-only allocation. The label
	// "CPG:" in the BRIEFING line is historical — see Manager.CurrentHeapMB
	// for the source. Pairing high process heap with cpg.max_heap_mb limit
	// trips the OOM guard; raising the limit raises the WHOLE-PROCESS
	// ceiling, not the CPG-subset.
	cpgHeapMB  int // PROCESS heap, not CPG-only
	cpgLimitMB int // process OOM ceiling (named cpg.max_heap_mb in yaml)
	// [Épica 263.D] Boot mode: "fast" (snapshot) or "cold" (SSA rebuild).
	cpgBootMode string
	// [ÉPICA 149.E] HNSW boot mode mirror — "fast" when LoadHNSWSnapshot
	// succeeded, "cold" when wal.LoadGraph rebuild ran. Default "cold"
	// (the historical path; safer assumption when the field is unset).
	hnswBootMode string
	// [Épica 260.C] Project federation compact fields.
	projectName       string
	projectWorkspaces int
	projectRunning    int
	// [354.Z-redesign] tier:"project" ownership role for this workspace:
	//   "leader"    — this workspace is coordinator_workspace (owns shared.db flock)
	//   "proxy:X"   — non-coord, proxies tier:"project" to X (coord basename)
	//   "legacy"    — project config has no coordinator_workspace (nondeterministic — first-to-boot wins)
	//   ""          — no project config, segment suppressed
	projectTierRole string
	// [Épica 265.C] Tenant identifier from credentials.json — empty when not configured.
	tenantID string
	// [287.F] Shared HNSW tier doc count — 0 when not in project mode.
	sharedMemCount int
	// [276.A] Ollama service health from Nexus /api/v1/services — nil when Nexus unreachable.
	ollamaServices []ollamaServiceStatus
	// [311.B] Top token consumer for compact line — empty when no data yet.
	topTokenStr string
	// [316.C] Pending contract gaps from .neo-project/SHARED_DEBT.md — 0 when absent.
	contractGapCount int
	// [335.A] Peer session mutations from sibling workspaces mirrored via Nexus.
	peerMuts map[string][]string // peerWSID → []file
	// [337.A] Peer presence from Nexus /api/v1/presence — peer workspaces active in last 2min.
	peerPresence string // formatted compact segment, e.g. "peers: 1 active (ws-name, last: 12s ago)"
	// [PILAR-XXIII / 126.4] Subprocess plugin pool summary from Nexus
	// /api/v1/plugins. Empty when disabled or unreachable. Format:
	// "plugins: 2 active (jira, github)" or "plugins: 1/2 errored" on partial.
	pluginsSegment string
	// [352.A] Nexus-level debt affecting this workspace, from
	// /internal/nexus/debt/affecting. Empty when Nexus unreachable, debt
	// registry disabled, or no open events target this workspace.
	nexusDebtEvents []nexusDebtBriefEntry
	// [GPU-AWARE] Live GPU snapshot from hardware.Detect() — Available=false when no NVIDIA GPU.
	gpuAvailable  bool
	gpuName       string
	gpuVRAMUsedMB int64 // total - free
	gpuVRAMTotMB  int64
	gpuUtilPct    int
	gpuTempC      int
	// [362.A] Delegate task orphan count — tasks stuck in_progress past TaskOrphanTimeoutMin.
	orphanedTaskCount int
	// [127.1-127.3] Session context fields — populated by populateGit/Tooling/RecentEpics.
	gitState     string // "git: <branch> ↔ origin (<ahead>/<behind>) <clean|N changes>"
	toolingState string // "hooks: post-commit:✓|✗ · style: <name> · skills: N (A auto, T task)"
	recentEpics  string // "last_epics: N, M, K"
	// [132.F.7] Daemon backend compact segment — empty when queue empty and mode != daemon.
	// Format: "Q/N tasks · backend:deepseek|claude|auto"
	daemonBackend string
	// [374.D] Directive inflation alert — populated from WAL.GetDirectives count.
	directiveCount     int
	directiveMax       int
	dsPreCertifyMode   string // [371.F] "auto"|"manual"|"off"
	dsHotPathFileCount int    // [371.F] number of hot-path files certified this session
	readSliceCount     int    // [372.D] READ_SLICE calls this session
	fileExtractCount   int    // [372.D] FILE_EXTRACT calls this session
	semanticCodeCount  int    // [373.A] SEMANTIC_CODE calls this session
	graphWalkCount     int    // [373.A] GRAPH_WALK calls this session
	blastRadiusCount   int    // [373.A] BLAST_RADIUS calls this session
	nudgeShown         map[string]bool // [373.C] dedup nudges per session
	// [138.E.4] Top trust scores by tier (≠ default L0/prior) for the
	// compact line. Populated by populateTrustHighlights when there's
	// real evidence to surface. Limited to top 3 by LowerBound DESC.
	trustHighlights []TrustStatusEntry
}

// nexusDebtBriefEntry is the subset of NexusDebtEvent needed by BRIEFING
// rendering — avoids pulling all JSON fields when we only show P0 badges. [352.A]
type nexusDebtBriefEntry struct {
	ID          string
	Priority    string
	Title       string
	Detected    time.Time
	Recommended string
}

// ollamaServiceStatus mirrors the relevant fields from Nexus /api/v1/services. [276.A]
type ollamaServiceStatus struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Port    int    `json:"port"`
	Enabled bool   `json:"enabled"`
}

// briefingSnapshot captures key diffable metrics from a BRIEFING call. [315.A]
type briefingSnapshot struct {
	planOpen     int
	planClosed   int
	heapMB       float64
	recvBytes    int64
	sentBytes    int64
	ragCoverage  float64
	binaryStale  bool
	staleMinutes int
	incTotal     int
	incIndexed   int64
	cpgHeapMB    int
}

func snapshotFromData(d briefingData) *briefingSnapshot {
	return &briefingSnapshot{
		planOpen:     d.planOpen,
		planClosed:   d.planClosed,
		heapMB:       d.heapMB,
		recvBytes:    d.recvBytes,
		sentBytes:    d.sentBytes,
		ragCoverage:  d.ragCoverage,
		binaryStale:  d.binaryStaleAlert,
		staleMinutes: d.staleMinutes,
		incTotal:     d.incTotal,
		incIndexed:   d.incIndexed,
		cpgHeapMB:    d.cpgHeapMB,
	}
}

func absDiffF64(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

// diffBriefingSnapshots returns a compact markdown string of fields that changed. [315.B]
func diffBriefingSnapshots(prev, cur *briefingSnapshot) string {
	var lines []string
	if prev.planOpen != cur.planOpen || prev.planClosed != cur.planClosed {
		lines = append(lines, fmt.Sprintf("Tasks: Open %d→%d | Closed %d→%d",
			prev.planOpen, cur.planOpen, prev.planClosed, cur.planClosed))
	}
	if absDiffF64(cur.heapMB, prev.heapMB) >= 1 {
		lines = append(lines, fmt.Sprintf("RAM: %.0f→%.0fMB", prev.heapMB, cur.heapMB))
	}
	recvDelta := cur.recvBytes - prev.recvBytes
	sentDelta := cur.sentBytes - prev.sentBytes
	if recvDelta > 0 || sentDelta > 0 {
		lines = append(lines, fmt.Sprintf("IO: +%dKB recv / +%dKB sent", recvDelta/1024, sentDelta/1024))
	}
	if absDiffF64(cur.ragCoverage, prev.ragCoverage) >= 0.5 {
		lines = append(lines, fmt.Sprintf("RAG: %.0f%%→%.0f%%", prev.ragCoverage, cur.ragCoverage))
	}
	if cur.binaryStale != prev.binaryStale || cur.staleMinutes != prev.staleMinutes {
		if cur.binaryStale {
			lines = append(lines, fmt.Sprintf("⚠️ BINARY_STALE:%dm", cur.staleMinutes))
		} else {
			lines = append(lines, "BINARY_STALE: cleared")
		}
	}
	if cur.incTotal != prev.incTotal || cur.incIndexed != prev.incIndexed {
		lines = append(lines, fmt.Sprintf("INC: %d/%d indexed", cur.incIndexed, cur.incTotal))
	}
	if cur.cpgHeapMB != prev.cpgHeapMB {
		lines = append(lines, fmt.Sprintf("CPG: %d→%dMB", prev.cpgHeapMB, cur.cpgHeapMB))
	}
	if len(lines) == 0 {
		return "_No changes since last BRIEFING._"
	}
	return strings.Join(lines, "\n")
}

// parsePlanCounts reads master_plan.md and returns open/closed counts and open task lines. [SRE-118.A]
func parsePlanCounts(planPath string) (open, closed int, openLines []string) {
	planData, err := os.ReadFile(planPath) //nolint:gosec // G304-WORKSPACE-CANON
	if err != nil {
		return 0, 0, nil
	}
	for planLine := range strings.SplitSeq(string(planData), "\n") {
		stripped := strings.TrimLeft(planLine, " \t")
		if strings.HasPrefix(stripped, "- [ ]") {
			open++
			trimmed := strings.TrimSpace(planLine)
			if len(trimmed) > 90 {
				trimmed = trimmed[:90] + "…"
			}
			openLines = append(openLines, trimmed)
		} else if strings.HasPrefix(stripped, "- [x]") || strings.HasPrefix(stripped, "- [X]") {
			closed++
		}
	}
	return open, closed, openLines
}

// gatherIncidentMetrics scans .neo/incidents/ and populates incident fields on d. [154.A-B]
func gatherIncidentMetrics(workspace string, d *briefingData) {
	incDir := filepath.Join(workspace, ".neo", "incidents")
	entries, err := os.ReadDir(incDir)
	if err != nil {
		return
	}
	now := time.Now()
	var latestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "INC-") || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		d.incidentCount++
		info, infoErr := e.Info()
		if infoErr != nil {
			continue
		}
		mtime := info.ModTime()
		if mtime.After(latestTime) {
			latestTime = mtime
			d.lastIncidentPath = filepath.Join(incDir, e.Name())
		}
		// [154.B] Check severity for incidents within last 24h.
		if now.Sub(mtime) <= 24*time.Hour {
			content, readErr := os.ReadFile(filepath.Join(incDir, e.Name())) //nolint:gosec // G304-DIR-WALK
			if readErr != nil {
				continue
			}
			if bytes.Contains(content, []byte("**Severity:** CRITICAL")) {
				d.criticalIncidents24h++
				if d.criticalIncidentName == "" {
					d.criticalIncidentName = strings.TrimSuffix(e.Name(), ".md")
				}
			}
		}
	}
	if !latestTime.IsZero() {
		d.lastIncidentAgeHours = now.Sub(latestTime).Hours()
	}
}

// gatherBriefingData collects all runtime state needed for a BRIEFING response. [SRE-118.A]
func extractPhaseName(activePhase string) string {
	for line := range strings.SplitSeq(activePhase, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimLeft(line, "#🌟 ")
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return "unknown"
}

func extractNextTask(openTaskLines []string) string {
	if len(openTaskLines) == 0 {
		return ""
	}
	line := openTaskLines[0]
	_, after, ok := strings.Cut(line, "Task ")
	if !ok {
		return ""
	}
	end := strings.IndexAny(after, " (")
	if end < 0 {
		end = len(after)
	}
	return "Task " + after[:end]
}

// renderCacheSegments returns the trailing " | Qcache..." tail with
// optional Tcache/Ecache blocks and eviction-rate warnings. Each layer
// is suppressed when never exercised this session (avoids noise on
// cold starts). [Épica 228]
func renderCacheSegments(d *briefingData) string {
	var sb strings.Builder
	if d.cacheHits+d.cacheMisses > 0 {
		ratio := float64(d.cacheHits) * 100.0 / float64(d.cacheHits+d.cacheMisses)
		fmt.Fprintf(&sb, " | Qcache: %.0f%% (%dH/%dM, sz=%d/%d)",
			ratio, d.cacheHits, d.cacheMisses, d.cacheSize, d.cacheCapacity)
	}
	if d.textCacheHits+d.textCacheMisses > 0 {
		ratio := float64(d.textCacheHits) * 100.0 / float64(d.textCacheHits+d.textCacheMisses)
		fmt.Fprintf(&sb, " | Tcache: %.0f%% (%dH/%dM, sz=%d)",
			ratio, d.textCacheHits, d.textCacheMisses, d.textCacheSize)
	}
	// [Épica 205] Embedding cache segment — operator sees the ~30ms
	// Ollama-skip savings live.
	if d.embCacheHits+d.embCacheMisses > 0 {
		ratio := float64(d.embCacheHits) * 100.0 / float64(d.embCacheHits+d.embCacheMisses)
		fmt.Fprintf(&sb, " | Ecache: %.0f%% (%dH/%dM, sz=%d)",
			ratio, d.embCacheHits, d.embCacheMisses, d.embCacheSize)
	}
	// [Épica 189] Eviction-rate alert — threshold 0.30 means ~30% of
	// requests cause an eviction, the point where LRU starts thrashing.
	if d.cacheEvicts > 0 && d.cacheHits+d.cacheMisses > 20 {
		if er := float64(d.cacheEvicts) / float64(d.cacheHits+d.cacheMisses); er > 0.30 {
			fmt.Fprintf(&sb, " ⚠️ Qcache evict_rate=%.0f%% — consider raising query_cache_capacity", er*100)
		}
	}
	if d.textCacheEvicts > 0 && d.textCacheHits+d.textCacheMisses > 20 {
		if er := float64(d.textCacheEvicts) / float64(d.textCacheHits+d.textCacheMisses); er > 0.30 {
			fmt.Fprintf(&sb, " ⚠️ Tcache evict_rate=%.0f%% — consider raising query_cache_capacity", er*100)
		}
	}
	// [Épica 236] CPG heap-vs-limit segment. Always shown when the limit is
	// configured so the operator sees the headroom before the guard trips.
	// Warning marker when heap > 80% of limit.
	if d.cpgLimitMB > 0 {
		pct := d.cpgHeapMB * 100 / d.cpgLimitMB
		warn := ""
		if pct >= 80 {
			warn = " ⚠️"
		}
		// [2026-05-15] Label kept as "CPG:" for back-compat with operator
		// mental models + TUI parsers, but the value is process-wide heap
		// (see cpgHeapMB declaration). Raising cpg.max_heap_mb in neo.yaml
		// raises the WHOLE-PROCESS OOM threshold, not CPG-only headroom.
		fmt.Fprintf(&sb, " | CPG: %d/%dMB (%d%%)%s", d.cpgHeapMB, d.cpgLimitMB, pct, warn)
	}
	// [ÉPICA 149.E] HNSW boot mode mirror. Append after CPG so the two
	// fast-boot subsystems sit next to each other. fast = snapshot hit;
	// cold = WAL rebuild path (the 6-min case this épica fixes).
	if d.hnswBootMode != "" {
		fmt.Fprintf(&sb, " | HNSW:%s", d.hnswBootMode)
	}
	return sb.String()
}

func buildBriefingCompactLine(d *briefingData) {
	if d.phaseErr == nil {
		d.phaseName = extractPhaseName(d.activePhase)
	}
	totalKB := float64(d.recvBytes+d.sentBytes) / 1024
	stalePrefix, resumePrefix, binAgeSuffix := compactBriefingAlerts(d)
	nexusDebtPrefix := compactNexusDebtPrefix(d) // [352.A]
	ragWarn, incWarn, mutsStr, incIdxStr := compactBriefingStatusFields(d)
	cacheStr := renderCacheSegments(d)
	extraFields := compactBriefingExtraFields(d)
	ollamaStr := compactOllamaSegment(d.ollamaServices)
	inboxStr := compactInboxSegment(d) // [331.B]
	trustStr := compactTrustSegment(d) // [138.E.4]
	suffix := ragWarn + binAgeSuffix + mutsStr + incIdxStr + incWarn + cacheStr + extraFields + ollamaStr + inboxStr + d.peerPresence + d.pluginsSegment + d.topTokenStr + trustStr
	d.compactLine = fmt.Sprintf("%s%s%sMode: %s | Phase: %s | Open: %d | Closed: %d | Next: %s | RAM: %.0fMB | IO: %.0fKB | RAG: %.0f%%%s",
		stalePrefix, resumePrefix, nexusDebtPrefix, d.serverMode, d.phaseName, d.planOpen, d.planClosed, extractNextTask(d.openTaskLines),
		d.heapMB, totalKB, d.ragCoverage, suffix)
}

// compactNexusDebtPrefix renders the high-visibility Nexus debt badge for the
// compact line when events target this workspace. Format:
// `⚠️ NEXUS-DEBT:N P0:M | ` — omitted entirely when no events. [352.A]
func compactNexusDebtPrefix(d *briefingData) string {
	if len(d.nexusDebtEvents) == 0 {
		return ""
	}
	p0 := 0
	for _, e := range d.nexusDebtEvents {
		if e.Priority == "P0" {
			p0++
		}
	}
	if p0 > 0 {
		return fmt.Sprintf("⚠️ NEXUS-DEBT:%d P0:%d | ", len(d.nexusDebtEvents), p0)
	}
	return fmt.Sprintf("⚠️ NEXUS-DEBT:%d | ", len(d.nexusDebtEvents))
}

// compactBriefingAlerts returns the stale/resume prefix strings and binAge suffix. [319.D]
// [162.B] binAgeSuffix is cleared when binaryStaleAlert is active (already in stalePrefix).
func compactBriefingAlerts(d *briefingData) (stalePrefix, resumePrefix, binAgeSuffix string) {
	binAgeSuffix = d.binAgeStr
	if d.binaryStaleAlert {
		stalePrefix = fmt.Sprintf("⚠️ BINARY_STALE:%dm | ", d.staleMinutes)
		binAgeSuffix = "" // already in prefix, don't double-print
	}
	if d.resumeWarning {
		resumePrefix = "⚠️ RESUME | "
	}
	return
}

// compactBriefingStatusFields returns per-call warning/counter segments. [319.D]
func compactBriefingStatusFields(d *briefingData) (ragWarn, incWarn, mutsStr, incIdxStr string) {
	if d.ragCoverage < 80 {
		ragWarn = fmt.Sprintf(" ⚠️ low_rag_coverage=%.0f%% (fallback_to_grep_recommended)", d.ragCoverage)
	}
	if d.criticalIncidents24h > 0 && d.lastIncidentAgeHours < 4 {
		incWarn = fmt.Sprintf(" | INC:%d⚠️", d.criticalIncidents24h)
	}
	// [163.B] Muts counter when session has certified files.
	if n := len(d.sessionMuts); n > 0 {
		mutsStr = fmt.Sprintf(" | Muts: %d", n)
	}
	// [158.B] INC-IDX counter when incidents directory is non-empty.
	if d.incTotal > 0 {
		incIdxStr = fmt.Sprintf(" | INC-IDX: %d/%d (BM25:%d)", d.incIndexed, d.incTotal, incidents.BM25IndexedCount())
	}
	return
}

// compactBriefingExtraFields returns the concatenated optional project/tenant/knowledge segments. [319.D]
func compactBriefingExtraFields(d *briefingData) string {
	var sb strings.Builder
	// [Épica 260.C] Project federation one-liner.
	if d.projectName != "" {
		fmt.Fprintf(&sb, " | Project: %s (%d ws, %d running)", d.projectName, d.projectWorkspaces, d.projectRunning)
		// [354.Z-redesign] tier ownership badge — helps agent see at a glance
		// whether this workspace owns shared.db (LEADER) or proxies to another
		// (proxy:X). "legacy" = no coordinator configured → nondeterministic.
		if d.projectTierRole != "" {
			fmt.Fprintf(&sb, " | tier:project=%s", d.projectTierRole)
		}
	}
	// [Épica 265.C] Tenant display — silent when not configured.
	if d.tenantID != "" {
		fmt.Fprintf(&sb, " | Tenant: %s", d.tenantID)
	}
	// [PILAR XXXIX] neo_memory schema stale marker.
	if d.memorySchemaStale {
		sb.WriteString(" | mem_schema_stale:true")
	}
	// [295.D] Knowledge Base compact counter.
	if d.knowledgeTotal > 0 {
		fmt.Fprintf(&sb, " | Know: %d/%d", d.knowledgeHot, d.knowledgeTotal)
	}
	// [298.C] Stale contract suffix.
	if n := len(d.staleContractKeys); n > 0 {
		fmt.Fprintf(&sb, " | ⚠️ctr_stale:%d", n)
	}
	// [287.F] Shared memory tier count.
	if d.sharedMemCount > 0 {
		fmt.Fprintf(&sb, " | SharedMem: %d", d.sharedMemCount)
	}
	// [316.C] Contract gap counter.
	if d.contractGapCount > 0 {
		fmt.Fprintf(&sb, " | contract_gaps:%d", d.contractGapCount)
	}
	// [342.A] Unresolved LWW knowledge conflicts.
	if d.knowledgeConflicts > 0 {
		fmt.Fprintf(&sb, " | ⚠️knowledge_conflicts:%d", d.knowledgeConflicts)
	}
	// [362.A] Orphaned delegate task warning — tasks stuck in_progress past timeout.
	if d.orphanedTaskCount > 0 {
		fmt.Fprintf(&sb, " | ⚠️orphaned_tasks:%d", d.orphanedTaskCount)
	}
	// [132.F.7] Daemon backend segment — queue depth + configured backend mode.
	if d.daemonBackend != "" {
		fmt.Fprintf(&sb, " | daemon: %s", d.daemonBackend)
	}
	// [GPU-AWARE] Live GPU segment: "GPU: RTX 3090 · 1.6/24.0GB · 15% · 65°C"
	if d.gpuAvailable {
		usedGB := float64(d.gpuVRAMUsedMB) / 1024
		totGB := float64(d.gpuVRAMTotMB) / 1024
		fmt.Fprintf(&sb, " | GPU: %s · %.1f/%.1fGB · %d%% · %d°C",
			d.gpuName, usedGB, totGB, d.gpuUtilPct, d.gpuTempC)
	}
	appendCompactGovernanceSegments(&sb, d)
	return sb.String()
}

// appendCompactGovernanceSegments adds directive inflation, DS pre-certify,
// and READ_SLICE dominance segments. Extracted to keep compactBriefingExtraFields CC<=15.
func appendCompactGovernanceSegments(sb *strings.Builder, d *briefingData) {
	if d.directiveCount > 0 && d.directiveMax > 0 && d.directiveCount > d.directiveMax*4/5 {
		fmt.Fprintf(sb, " | ⚠️DIRECTIVE_INFLATION:%d/%d", d.directiveCount, d.directiveMax)
	}
	if d.dsPreCertifyMode != "off" && d.dsHotPathFileCount > 0 {
		fmt.Fprintf(sb, " | DS:%d hot-path", d.dsHotPathFileCount)
	}
	if d.readSliceCount > 10 && d.fileExtractCount < 3 {
		fmt.Fprintf(sb, " | ⚠️READ_SLICE:%d vs FILE_EXTRACT:%d", d.readSliceCount, d.fileExtractCount)
	}
}

// compactOllamaSegment formats the Ollama service health as "| Ollama: llm:✓ embed:✓". [319.D]
// [276.A] Shows ✗ for unhealthy/disabled services so the operator notices at a glance.
func compactOllamaSegment(svcs []ollamaServiceStatus) string {
	if len(svcs) == 0 {
		return ""
	}
	var parts []string
	anyUnhealthy := false
	for _, svc := range svcs {
		icon := "✓"
		if svc.Status != "healthy" {
			icon = "✗"
			anyUnhealthy = true
		}
		parts = append(parts, strings.TrimPrefix(svc.Name, "ollama_")+":"+icon)
	}
	prefix := ""
	if anyUnhealthy {
		prefix = "⚠️"
	}
	return fmt.Sprintf(" | Ollama: %s%s", prefix, strings.Join(parts, " "))
}

func gatherBriefingData(t *RadarTool, isFirstBriefing bool) briefingData {
	d := briefingData{}
	d.serverMode = os.Getenv("NEO_SERVER_MODE")
	if d.serverMode == "" {
		d.serverMode = "unknown"
	}
	d.activePhase, d.phaseErr = state.ReadActivePhase(t.workspace)

	// [SRE-BRIEFING-FIX] Authoritative task counts from master_plan.md — BoltDB can diverge.
	planPath := filepath.Join(t.workspace, ".neo", "master_plan.md")
	d.planOpen, d.planClosed, d.openTaskLines = parsePlanCounts(planPath)
	d.boltPending, _ = state.GetPlannerState()

	gatherMemAndCPGMetrics(t, &d)
	gatherSessionAndRAGMetrics(t, &d)

	if !t.lastDigestTime.IsZero() {
		d.hasDigest = true
		d.digestAgeHours = time.Since(t.lastDigestTime).Hours()
	}

	gatherIncidentMetrics(t.workspace, &d)

	// [158.B] INC indexing counters for compact line.
	d.incTotal = incidents.CountIncidentFiles(t.workspace)
	d.incIndexed = incidents.IndexedCount()
	gatherCacheMetrics(t, &d)

	// [156.C] Resume warning: detect agent operating without prior BRIEFING this boot.
	if isFirstBriefing && (len(d.sessionMuts) > 0 || time.Since(serverBootTime) > 2*time.Minute) {
		d.resumeWarning = true
	}

	// [Épica 265.C] Tenant ID from live config.
	if t.cfg != nil {
		d.tenantID = t.cfg.Auth.TenantID
	}

	// [287.F] Shared HNSW tier doc count.
	if t.sharedGraph != nil {
		if n, cntErr := t.sharedGraph.Count(); cntErr == nil {
			d.sharedMemCount = n
		}
	}

	gatherProjectMetrics(t, &d)
	gatherInboxMetrics(t, &d) // [331.B] BoltDB — sequential, no goroutine

	// [MCPI-46] Parallelize the 4 HTTP-bound gathering calls.
	// Each writes to a distinct field of briefingData → no data race.
	// wg.Wait() provides the happens-before for subsequent reads.
	var wg sync.WaitGroup
	var peerSeg, pluginSeg string
	wg.Add(4)
	go func() { defer wg.Done(); gatherOllamaAndContractMetrics(t, &d) }()
	go func() { defer wg.Done(); peerSeg = fetchPresenceSegment(t.workspace, t.cfg) }() // [337.A]
	go func() { defer wg.Done(); pluginSeg = fetchPluginsSegment(t) }()                  // [PILAR-XXIII / 126.4]
	go func() { defer wg.Done(); gatherNexusDebt(t, &d) }()                              // [352.A]
	wg.Wait()
	d.peerPresence = peerSeg
	d.pluginsSegment = pluginSeg

	// [342.A] LWW conflict count — nonzero means two agents wrote the same key concurrently.
	if t.knowledgeStore != nil {
		d.knowledgeConflicts = t.knowledgeStore.CountConflicts()
	}

	// [362.A] Orphaned delegate task count — tasks stuck in_progress past timeout.
	d.orphanedTaskCount = state.CountOrphanedTasks()

	// [374.D] Directive inflation alert.
	if rules, err := t.wal.GetDirectives(); err == nil {
		active := 0
		for _, r := range rules {
			if !strings.Contains(r, "~~OBSOLETO~~") {
				active++
			}
		}
		d.directiveCount = active
		d.directiveMax = t.cfg.SRE.MaxDirectives
	}

	populateSessionIntentCounters(t, &d)

	// [GPU-AWARE] Live GPU snapshot: always call hardware.Detect() so the compact
	// line reflects current utilisation/temperature, not the stale boot snapshot.
	if t.gpuInfo.Available {
		live := hardware.Detect()
		if live.Available {
			d.gpuAvailable = true
			d.gpuName = live.DeviceName
			d.gpuVRAMUsedMB = live.VRAMTotalMB - live.VRAMFreeMB
			d.gpuVRAMTotMB = live.VRAMTotalMB
			d.gpuUtilPct = live.GPUUtilPct
			d.gpuTempC = live.TempC
		}
	}

	// [127.1-127.3] Session context: git state, tooling state, recent closed épicas.
	d.gitState = populateGitState(t.workspace)
	d.toolingState = populateToolingState(t.workspace)
	d.recentEpics = populateRecentEpics(t.workspace)

	// [138.E.4] Top trust patterns with real evidence (tier > L0 OR
	// total_executions > 0). Empty when bucket is fresh — keeps the
	// compact line clean for new workspaces.
	d.trustHighlights = populateTrustHighlights()

	// [132.F.7] Daemon backend segment — extracted to helper to keep
	// gatherBriefingData under CC=15.
	d.daemonBackend = computeDaemonBackendSegment(t.cfg, d.serverMode)

	buildBriefingCompactLine(&d)
	return d
}

// gatherInboxMetrics populates unreadInboxCount + unreadInboxSenders. [331.B]
// Silent no-op when knowledgeStore isn't wired (non-project workspace) or
// when the workspace has no unread messages.
func gatherInboxMetrics(t *RadarTool, d *briefingData) {
	if t.knowledgeStore == nil {
		return
	}
	wsID := resolveWorkspaceID(t.workspace)
	if wsID == "" {
		return
	}
	entries, err := t.knowledgeStore.ListInboxFor(wsID, true /* unreadOnly */)
	if err != nil || len(entries) == 0 {
		return
	}
	d.unreadInboxCount = len(entries)
	// Collect up to 3 distinct senders for the compact hint.
	seen := make(map[string]struct{}, 3)
	for _, e := range entries {
		if e.From == "" {
			continue
		}
		if _, dup := seen[e.From]; dup {
			continue
		}
		seen[e.From] = struct{}{}
		d.unreadInboxSenders = append(d.unreadInboxSenders, e.From)
		if len(d.unreadInboxSenders) == 3 {
			break
		}
	}
}

// compactInboxSegment renders "📬 inbox: N (from: X, Y)" when there are unread
// messages for this workspace. Returns empty string otherwise. [331.B]
func compactInboxSegment(d *briefingData) string {
	if d.unreadInboxCount == 0 {
		return ""
	}
	if len(d.unreadInboxSenders) == 0 {
		return fmt.Sprintf(" | 📬 inbox: %d", d.unreadInboxCount)
	}
	return fmt.Sprintf(" | 📬 inbox: %d (from: %s)", d.unreadInboxCount, strings.Join(d.unreadInboxSenders, ", "))
}

// fetchPresenceSegment queries Nexus /api/v1/presence and returns a compact
// segment listing peer workspaces active in the last 2 minutes. [337.A]
//
// [ÉPICA 148.C] When a peer's Nexus /status entry reports
// status=starting + boot_phase + boot_pct, the segment annotates the
// peer with its boot progress: "strategosia hnsw_load=67%" instead of
// the bare "last: Ns ago". Removes the operator's "is the peer hung?"
// uncertainty during a multi-workspace boot.
func fetchPresenceSegment(workspace string, cfg *config.NeoConfig) string {
	if cfg == nil {
		return ""
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.NexusDispatcherPort)
	url := base + "/api/v1/presence"
	client := sre.SafeInternalHTTPClient(1)
	resp, err := client.Get(url) //nolint:gosec // G107-TRUSTED-CONFIG-URL: nexusBase from operator config
	if err != nil || resp == nil {
		return ""
	}
	defer resp.Body.Close()
	var entries []struct {
		WorkspaceID      string `json:"workspace_id"`
		SessionAgentID   string `json:"session_agent_id"`
		LastActivityUnix int64  `json:"last_activity_unix"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return ""
	}
	bootProgress := fetchPeerBootProgress(base, client) // [148.C] best-effort
	selfBase := filepath.Base(workspace)
	now := time.Now().Unix()
	var parts []string
	for _, e := range entries {
		if filepath.Base(e.WorkspaceID) == selfBase || e.WorkspaceID == workspace {
			continue // skip self
		}
		peerName := filepath.Base(e.WorkspaceID)
		if bp, ok := bootProgress[e.WorkspaceID]; ok && bp.Status == "starting" && bp.Phase != "" {
			parts = append(parts, fmt.Sprintf("%s, %s=%.0f%%", peerName, bp.Phase, bp.Pct*100))
			continue
		}
		ago := now - e.LastActivityUnix
		parts = append(parts, fmt.Sprintf("%s, last: %ds ago", peerName, ago))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf(" | peers: %d active (%s)", len(parts), strings.Join(parts, "; "))
}

// peerBootProgress is the parsed shape of a single Nexus /status entry
// limited to the fields we render in the BRIEFING peer segment. [148.C]
type peerBootProgress struct {
	Status string
	Phase  string
	Pct    float64
}

// fetchPeerBootProgress queries Nexus /status and returns the boot
// progress per workspace ID. Best-effort: any error returns an empty
// map and the caller falls back to the legacy "last: Ns ago" rendering.
// Timeout is bounded by the supplied client (already 1s in caller).
func fetchPeerBootProgress(nexusBase string, client *http.Client) map[string]peerBootProgress {
	out := map[string]peerBootProgress{}
	resp, err := client.Get(nexusBase + "/status") //nolint:gosec // G107-TRUSTED-CONFIG-URL: nexusBase from operator config
	if err != nil || resp == nil {
		return out
	}
	defer resp.Body.Close()
	var entries []struct {
		ID        string  `json:"id"`
		Status    string  `json:"status"`
		BootPhase string  `json:"boot_phase"`
		BootPct   float64 `json:"boot_pct"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return out
	}
	for _, e := range entries {
		out[e.ID] = peerBootProgress{Status: e.Status, Phase: e.BootPhase, Pct: e.BootPct}
	}
	return out
}

// gatherNexusDebt queries Nexus /internal/nexus/debt/affecting?workspace_id=
// for events blocking the current workspace. Timeout 500ms, failure fallthrough
// silently (Nexus may be offline or debt disabled). [352.A]
func gatherNexusDebt(t *RadarTool, d *briefingData) {
	if t.cfg == nil {
		return
	}
	wsID := lookupWorkspaceID(t.workspace)
	if wsID == "" {
		return
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", t.cfg.Server.NexusDispatcherPort)
	url := fmt.Sprintf("%s/internal/nexus/debt/affecting?workspace_id=%s", base, wsID)
	client := sre.SafeInternalHTTPClient(1)
	resp, err := client.Get(url) //nolint:gosec // G107-TRUSTED-CONFIG-URL: nexus base from cfg
	if err != nil || resp == nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return // 404 = debt disabled; treat as no events
	}
	var events []struct {
		ID          string    `json:"id"`
		Priority    string    `json:"priority"`
		Title       string    `json:"title"`
		DetectedAt  time.Time `json:"detected_at"`
		Recommended string    `json:"recommended,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return
	}
	for _, e := range events {
		d.nexusDebtEvents = append(d.nexusDebtEvents, nexusDebtBriefEntry{
			ID:          e.ID,
			Priority:    e.Priority,
			Title:       e.Title,
			Detected:    e.DetectedAt,
			Recommended: e.Recommended,
		})
	}
}

// gatherMemAndCPGMetrics populates RAM, IO, GC, and CPG heap/boot fields. [319.A]
func gatherMemAndCPGMetrics(t *RadarTool, d *briefingData) {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	d.heapMB = float64(memStats.HeapAlloc) / (1024 * 1024)
	d.gcRuns = memStats.NumGC
	d.recvBytes, d.sentBytes = GetIOStats()
	// [Épica 236] CPG heap-vs-limit visibility.
	d.cpgHeapMB = int(memStats.HeapAlloc / (1024 * 1024))
	d.cpgLimitMB = LiveConfig(t.cfg).CPG.MaxHeapMB
	// [Épica 263.D] cpg_boot: "fast" from snapshot, "cold" from SSA rebuild.
	if t.cpgManager != nil && t.cpgManager.BootedFast() {
		d.cpgBootMode = "fast"
	} else {
		d.cpgBootMode = "cold"
	}
	// [ÉPICA 149.E] hnsw_boot mirror — read globalBootProgress (set by
	// bootRAG when LoadHNSWSnapshot succeeds).
	if globalBootProgress.HNSWBootedFast() {
		d.hnswBootMode = "fast"
	} else {
		d.hnswBootMode = "cold"
	}
}

// gatherSessionAndRAGMetrics populates session mutations, RAG coverage, binary stale, and knowledge fields. [319.A]
func gatherSessionAndRAGMetrics(t *RadarTool, d *briefingData) {
	sessionID := briefingSessionID(t.workspace)
	d.sessionMuts, _ = t.wal.GetSessionMutations(sessionID)
	// [335.A] Load peer workspace mutations mirrored via Nexus.
	d.peerMuts, _ = t.wal.GetAllPeerSessionMutations()
	// [SRE-LANG-AWARE-COVERAGE-2026-05-15] Use dominant_lang from cfg so
	// non-Go workspaces (strategosia/Next.js) don't get a misleading 0%
	// RAG coverage just because IndexCoverage's legacy default counts only
	// .go files. cfg.Workspace.DominantLang is populated at workspace
	// registration via DetectDominantLang.
	dominantLang := ""
	if t.cfg != nil {
		dominantLang = t.cfg.Workspace.DominantLang
	}
	d.ragCoverage = rag.IndexCoverageWithLang(t.graph, t.workspace, dominantLang) * 100
	d.binAgeStr = briefingBinaryAge(t.workspace)
	// [162.A] Detect binary stale alert from binAgeStr marker.
	if _, after, ok := strings.Cut(d.binAgeStr, "binary_stale_vs_HEAD="); ok {
		d.binaryStaleAlert = true
		if m, err := strconv.Atoi(strings.TrimSuffix(after, "m")); err == nil {
			d.staleMinutes = m
		}
	}
	// [PILAR XXXIX] neo_memory schema stale.
	d.memorySchemaStale = detectMemorySchemaStale(t.workspace)
	// [295.D] Knowledge Base stats — nil-safe.
	if t.knowledgeStats != nil {
		d.knowledgeHot, d.knowledgeTotal = t.knowledgeStats()
	}
	// [298.C] Stale contract detection.
	if t.knowledgeStaleContracts != nil {
		d.staleContractKeys = t.knowledgeStaleContracts()
	}
}

// gatherCacheMetrics populates query/text/embedding cache telemetry. [319.A]
func gatherCacheMetrics(t *RadarTool, d *briefingData) {
	// [Épica 176] Query cache.
	if t.queryCache != nil {
		d.cacheHits, d.cacheMisses, d.cacheEvicts, d.cacheSize = t.queryCache.Stats()
		if t.cfg != nil {
			d.cacheCapacity = t.cfg.RAG.QueryCacheCapacity
		}
	}
	// [Épica 180/189] Text cache — evicts feed the undersized-cache warning.
	if t.textCache != nil {
		d.textCacheHits, d.textCacheMisses, d.textCacheEvicts, d.textCacheSize = t.textCache.Stats()
	}
	// [Épica 205] Embedding cache.
	if t.embCache != nil {
		d.embCacheHits, d.embCacheMisses, _, d.embCacheSize = t.embCache.Stats()
	}
}

// gatherProjectMetrics populates project federation health (running member count). [319.A]
func gatherProjectMetrics(t *RadarTool, d *briefingData) {
	// [Épica 260.C] Project federation compact telemetry — best-effort, no blocking.
	if t.cfg.Project == nil {
		return
	}
	d.projectName = t.cfg.Project.ProjectName
	d.projectWorkspaces = len(t.cfg.Project.MemberWorkspaces)
	// [354.Z-redesign] Role detection for compact tier display.
	coord := t.cfg.Project.CoordinatorWorkspace
	switch {
	case coord == "":
		d.projectTierRole = "legacy"
	case isCoordinatorWorkspace(t.workspace, t.cfg):
		d.projectTierRole = "leader"
	default:
		d.projectTierRole = "proxy:" + filepath.Base(coord)
	}
	client := sre.SafeInternalHTTPClient(1)
	nexusBase := fmt.Sprintf("http://127.0.0.1:%d", t.cfg.Server.NexusDispatcherPort)
	for _, ws := range t.cfg.Project.MemberWorkspaces {
		if ws == "." || filepath.Base(ws) == filepath.Base(t.workspace) {
			d.projectRunning++
			continue
		}
		wsID := strings.ToLower(strings.ReplaceAll(filepath.Base(ws), "_", "-"))
		url := fmt.Sprintf("%s/workspaces/%s/health", nexusBase, wsID)
		resp, err := client.Get(url) //nolint:gosec // G107-TRUSTED-CONFIG-URL: nexusBase is from operator config
		if err == nil && resp != nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				d.projectRunning++
			}
		}
	}
}

// gatherOllamaAndContractMetrics populates Ollama service health, top token, and contract gap count. [319.A]
func gatherOllamaAndContractMetrics(t *RadarTool, d *briefingData) {
	// [276.A] Fetch Ollama service health from Nexus — best-effort, 500ms timeout.
	nexusPort := 9000
	if t.cfg != nil {
		nexusPort = t.cfg.Server.GossipPort + 1
	}
	svcURL := fmt.Sprintf("http://127.0.0.1:%d/api/v1/services", nexusPort) //nolint:gosec // G107-TRUSTED-CONFIG-URL
	svcClient := sre.SafeInternalHTTPClient(1)
	if resp, err := svcClient.Get(svcURL); err == nil && resp != nil { //nolint:gosec // G107-TRUSTED-CONFIG-URL
		defer resp.Body.Close()
		var svcs []ollamaServiceStatus
		if json.NewDecoder(resp.Body).Decode(&svcs) == nil {
			d.ollamaServices = svcs
		}
	}
	// [311.B / 128.3.2] Top token consumer for compact line — only when >500K out-tokens.
	if trows := collectTokenRows(); len(trows) > 0 {
		r := trows[0]
		if r.OutputTokens > 500000 {
			d.topTokenStr = fmt.Sprintf(" | ⚠️ TokenBudget: %s(%dK out)", r.Tool, r.OutputTokens/1000)
		}
	}
	// [316.C] Contract gap count — silent when not in project mode or SHARED_DEBT.md absent.
	if projDir, ok := federation.FindNeoProjectDir(t.workspace); ok {
		if entries, parseErr := federation.ParseSharedDebt(projDir); parseErr == nil {
			for _, e := range entries {
				if strings.Contains(e.Status, "pending") {
					d.contractGapCount++
				}
			}
		}
	}
}

// appendContractDriftAlerts reads and flushes the contract_alerts BoltDB bucket.
// Shows a ⚠️ block per alert source with actionable hint. [292.E]
func appendContractDriftAlerts(sb *strings.Builder) {
	alerts, err := state.ContractAlertReadAndFlush()
	if err != nil || len(alerts) == 0 {
		return
	}
	sb.WriteString("\n")
	for _, a := range alerts {
		fmt.Fprintf(sb, "⚠️ **CONTRACT_DRIFT** from `%s`: %d breaking change(s)\n", a.FromWorkspace, len(a.Breaking))
		for _, b := range a.Breaking {
			fmt.Fprintf(sb, "  - %s `%s`: `%s` → `%s`\n", b.Type, b.Endpoint, b.Old, b.New)
		}
		fmt.Fprintf(sb, "  → Run: `neo_radar(intent:\"CONTRACT_QUERY\", target_workspace:\"%s\")` for details\n\n", a.FromWorkspace)
	}
}

// appendKnowledgeSection adds the ## Knowledge Base block to the full BRIEFING. [295.D]
// Silent when KnowledgeStore is not available (knowledgeStats == nil / total == 0).
func appendKnowledgeSection(sb *strings.Builder, t *RadarTool) {
	if t.knowledgeStats == nil {
		return
	}
	hot, total := t.knowledgeStats()
	if total == 0 {
		return
	}
	sb.WriteString("## Knowledge Base\n\n")
	fmt.Fprintf(sb, "| hot | total | cold |\n|-----|-------|------|\n| %d | %d | %d |\n\n", hot, total, total-hot)
	sb.WriteString("Use `neo_memory(action:\"list\", namespace:\"*\")` for full inventory.\n\n")
}

// appendSessionMuts appends the session_mutations footer to sb. [163.A/163.C]
// Compact callers pass truncate=true; full briefing passes truncate=false.
func appendSessionMuts(sb *strings.Builder, muts []string, truncate bool) {
	if len(muts) == 0 {
		return
	}
	if truncate && len(muts) > 5 {
		shown := muts[len(muts)-5:]
		fmt.Fprintf(sb, "\n**session_mutations:** %s ... (+%d more certified)\n",
			strings.Join(shown, ", "), len(muts)-5)
		return
	}
	sb.WriteString("\n**session_mutations:**\n")
	for _, m := range muts {
		fmt.Fprintf(sb, "  - `%s`\n", m)
	}
}

// formatFullBriefing renders the full BRIEFING markdown. [SRE-118.C]
func formatFullBriefing(ctx context.Context, t *RadarTool, args map[string]any, d briefingData) string {
	var sb strings.Builder
	sb.WriteString("## SRE BRIEFING\n\n")
	appendBriefingWarnings(&sb, d)
	appendNexusDebtSection(&sb, d) // [352.A]
	appendBriefingPlanSection(&sb, d)
	fmt.Fprintf(&sb, "**Tool Inventory:** %d registered\n", len(t.graph.Nodes))
	// [148.F/155.D/290] List all 18 radar intents.
	sb.WriteString("**Radar Intents (18):** BRIEFING, BLAST_RADIUS, SEMANTIC_CODE, DB_SCHEMA, TECH_DEBT_MAP, READ_MASTER_PLAN, SEMANTIC_AST, READ_SLICE, AST_AUDIT, HUD_STATE, FRONTEND_ERRORS, WIRING_AUDIT, COMPILE_AUDIT, GRAPH_WALK, PROJECT_DIGEST, INCIDENT_SEARCH, PATTERN_AUDIT, CONTRACT_QUERY\n")
	// [148.D] Show digest age so the agent can decide whether to refresh.
	if d.hasDigest {
		fmt.Fprintf(&sb, "**digest_age_hours:** %.2f\n", d.digestAgeHours)
	} else {
		sb.WriteString("**digest_age_hours:** n/a (run PROJECT_DIGEST to generate)\n")
	}
	appendBriefingIncidentSection(&sb, d)
	appendBriefingInfraSection(&sb, t, d)
	// [Épica 260.A] Project federation — show member workspace health table when project config found.
	if t.cfg.Project != nil && len(t.cfg.Project.MemberWorkspaces) > 0 {
		sb.WriteString(t.buildProjectHealthTable(ctx))
	}
	appendSessionContextSection(&sb, d) // [127.4]
	appendBriefingArchMem(ctx, t, args, &sb) // [SRE-28.1.2]
	appendContractDriftAlerts(&sb)           // [292.E]
	appendKnowledgeSection(&sb, t)           // [295.D]
	appendBriefingStaleContracts(&sb, d)     // [298.C]
	appendSessionMuts(&sb, d.sessionMuts, false)
	appendPeerSessionMuts(&sb, d.peerMuts) // [335.A]
	appendToolNudges(&sb, t, &d)           // [373.A]
	return sb.String()
}

// populateSessionIntentCounters copies session-level intent counts and
// DS pre-certify state into the briefingData for compact/full rendering.
// Extracted from gatherBriefingData to keep CC<=15. [372.D+373.A+371.F]
func populateSessionIntentCounters(t *RadarTool, d *briefingData) {
	t.sessionIntentMu.Lock()
	d.readSliceCount = t.sessionIntentCounts["READ_SLICE"]
	d.fileExtractCount = t.sessionIntentCounts["FILE_EXTRACT"]
	d.semanticCodeCount = t.sessionIntentCounts["SEMANTIC_CODE"]
	d.graphWalkCount = t.sessionIntentCounts["GRAPH_WALK"]
	d.blastRadiusCount = t.sessionIntentCounts["BLAST_RADIUS"]
	t.sessionIntentMu.Unlock()

	d.dsPreCertifyMode = t.cfg.SRE.DeepseekPreCertify
	if d.dsPreCertifyMode == "" {
		d.dsPreCertifyMode = "manual"
	}
	for _, mut := range d.sessionMuts {
		if isHotPath(filepath.Join(t.workspace, mut), t.cfg.SRE.DeepseekHotPaths, t.workspace) {
			d.dsHotPathFileCount++
		}
	}
}

// appendToolNudges adds contextual suggestions for underutilized tools
// in full BRIEFING mode. Compact mode skips this entirely. [373.A+B+C]
func appendToolNudges(sb *strings.Builder, t *RadarTool, d *briefingData) {
	if t.cfg.SRE.ToolNudgesOff {
		return
	}
	if d.nudgeShown == nil {
		d.nudgeShown = make(map[string]bool)
	}
	nudges := collectNudges(t, d)
	if len(nudges) > 0 {
		sb.WriteString("\n### Tool Suggestions\n")
		for _, n := range nudges {
			sb.WriteString(n + "\n")
		}
	}
}

func collectNudges(t *RadarTool, d *briefingData) []string {
	var out []string
	if d.ragCoverage >= 50 && d.semanticCodeCount == 0 && d.readSliceCount >= 3 && !d.nudgeShown["SEMANTIC_CODE"] {
		out = append(out, fmt.Sprintf("💡 SEMANTIC_CODE: RAG coverage %.0f%% — try semantic search for conceptual queries.", d.ragCoverage))
		d.nudgeShown["SEMANTIC_CODE"] = true
	}
	if t.cpgManager != nil && d.graphWalkCount == 0 && d.blastRadiusCount >= 2 && !d.nudgeShown["GRAPH_WALK"] {
		out = append(out, "💡 GRAPH_WALK: CPG active — explore call subgraphs after BLAST_RADIUS.")
		d.nudgeShown["GRAPH_WALK"] = true
	}
	if !d.hasDigest && d.digestAgeHours > 24 && !d.nudgeShown["PROJECT_DIGEST"] {
		out = append(out, "💡 PROJECT_DIGEST: stale >24h — refresh hotspots + CodeRank.")
		d.nudgeShown["PROJECT_DIGEST"] = true
	}
	return out
}

// appendNexusDebtSection renders events blocking this workspace as a
// dedicated block. Omits the section entirely when no debt is affecting us. [352.A]
func appendNexusDebtSection(sb *strings.Builder, d briefingData) {
	if len(d.nexusDebtEvents) == 0 {
		return
	}
	sb.WriteString("\n### ⚠️ Nexus-Level Debt Affecting This Workspace\n\n")
	for _, e := range d.nexusDebtEvents {
		fmt.Fprintf(sb, "- **%s** `%s` %s\n", e.Priority, e.ID, e.Title)
		fmt.Fprintf(sb, "  - Detected: %s\n", e.Detected.Format("2006-01-02T15:04:05"))
		if e.Recommended != "" {
			fmt.Fprintf(sb, "  - Recommended: %s\n", e.Recommended)
		}
		fmt.Fprintf(sb, "  - Resolve via: `neo_debt(scope:\"nexus\", action:\"resolve\", id:\"%s\", resolution:\"...\")`\n", e.ID)
	}
	sb.WriteString("\n")
}

// appendPeerSessionMuts appends a section showing mutations from sibling workspaces. [335.A]
func appendPeerSessionMuts(sb *strings.Builder, peers map[string][]string) {
	if len(peers) == 0 {
		return
	}
	sb.WriteString("\n**peer_session_mutations:**\n")
	for peerID, files := range peers {
		fmt.Fprintf(sb, "  [%s] %d file(s): ", peerID, len(files))
		if len(files) <= 3 {
			sb.WriteString(strings.Join(files, ", "))
		} else {
			fmt.Fprintf(sb, "%s ... (+%d more)", strings.Join(files[:3], ", "), len(files)-3)
		}
		sb.WriteByte('\n')
	}
}

// appendBriefingWarnings writes binary-stale, schema-stale, and resume-warning banners. [319.C]
func appendBriefingWarnings(sb *strings.Builder, d briefingData) {
	// [162.C] Binary stale warning.
	if d.binaryStaleAlert {
		fmt.Fprintf(sb, "**⚠️ BINARY STALE:** El binario tiene %dm de desfase vs HEAD. "+
			"Ejecutar `go build -o bin/neo-mcp ./cmd/neo-mcp && kill -HUP $(pgrep neo-nexus)` "+
			"antes de certificar cambios en nuevas features.\n\n", d.staleMinutes)
	}
	// [PILAR XXXIX] neo_memory schema stale.
	if d.memorySchemaStale {
		sb.WriteString("**⚠️ neo_memory SCHEMA STALE (v2):** `tool_memory.go` es más nuevo que `bin/neo-mcp`. " +
			"Las acciones `store/fetch/list/drop/search` no están disponibles hasta reconstruir: " +
			"`make rebuild-restart`\n\n")
	}
	// [156.D] Resume warning block.
	if d.resumeWarning {
		sb.WriteString("**⚠️ RESUME_WARNING:** Esta sesión reanudó desde un contexto comprimido sin ejecutar " +
			"BRIEFING primero. Mutaciones previas pueden no reflejar el estado actual del orquestador. " +
			"Verificar `session_mutations` antes de continuar.\n\n")
	}
}

// appendBriefingPlanSection writes mode, tenant, master plan phase, and open task list. [319.C]
func appendBriefingPlanSection(sb *strings.Builder, d briefingData) {
	fmt.Fprintf(sb, "**Mode:** `%s`\n", d.serverMode)
	// [Épica 265.C] Tenant metadata — shown only when configured.
	if d.tenantID != "" {
		fmt.Fprintf(sb, "**tenant:** `%s`\n", d.tenantID)
	}
	if d.phaseErr != nil {
		fmt.Fprintf(sb, "**Master Plan:** error reading: %v\n", d.phaseErr)
	} else {
		fmt.Fprintf(sb, "**Master Plan:**\n%s\n", d.activePhase)
	}
	fmt.Fprintf(sb, "**Planner:** master_plan.md open=**%d** closed=%d | BoltDB queue=%d\n",
		d.planOpen, d.planClosed, d.boltPending)
	if len(d.openTaskLines) > 0 {
		sb.WriteString("\n**Open tasks (master_plan.md):**\n")
		for _, tl := range d.openTaskLines {
			fmt.Fprintf(sb, "  %s\n", tl)
		}
		sb.WriteString("\n")
	}
}

// appendBriefingIncidentSection writes the incident count / last / critical block. [319.C]
func appendBriefingIncidentSection(sb *strings.Builder, d briefingData) {
	// [154.C] Incident metrics section.
	if d.incidentCount == 0 {
		return
	}
	lastStr := "n/a"
	if d.lastIncidentAgeHours > 0 {
		lastStr = fmt.Sprintf("%.1fh ago", d.lastIncidentAgeHours)
	}
	incLine := fmt.Sprintf("count=%d | last=%s | critical_24h=%d", d.incidentCount, lastStr, d.criticalIncidents24h)
	if d.criticalIncidents24h > 0 {
		incLine += fmt.Sprintf(" ⚠️ %s", d.criticalIncidentName)
	}
	fmt.Fprintf(sb, "**Incidents:** %s\n", incLine)
}

// appendBriefingInfraSection writes heap RAM, CPG status, session IO, and tenant identity. [319.C]
func appendBriefingInfraSection(sb *strings.Builder, t *RadarTool, d briefingData) {
	fmt.Fprintf(sb, "**Heap RAM:** %.2f MB | **GC Runs:** %d\n", d.heapMB, d.gcRuns)
	// [Épica 236/263.D] CPG heap vs live-limit + boot mode.
	if d.cpgLimitMB > 0 {
		pct := d.cpgHeapMB * 100 / d.cpgLimitMB
		cpgLine := fmt.Sprintf("heap=%d MB | limit=%d MB (%d%%) | boot=%s", d.cpgHeapMB, d.cpgLimitMB, pct, d.cpgBootMode)
		if pct >= 80 {
			cpgLine += " ⚠️ acercándose al límite — raise `cpg.max_heap_mb` en neo.yaml"
		}
		fmt.Fprintf(sb, "**CPG Status:** %s\n", cpgLine)
	} else if d.cpgBootMode != "" {
		fmt.Fprintf(sb, "**CPG Status:** boot=%s\n", d.cpgBootMode)
	}
	totalKB := float64(d.recvBytes+d.sentBytes) / 1024
	fmt.Fprintf(sb, "**Session IO:** %.1f KB received, %.1f KB sent (%.1f KB total)\n",
		float64(d.recvBytes)/1024, float64(d.sentBytes)/1024, totalKB)
	if totalKB > 500 {
		sb.WriteString("⚠️ **High token usage.** Consider using `neo_compress_context` to reduce context window pressure.\n")
	}
	// [Épica 265.C] Tenant identity — shown when credentials are loaded.
	if t.cfg.Auth.TenantID != "" {
		fmt.Fprintf(sb, "**tenant:** `%s`\n", t.cfg.Auth.TenantID)
	}
}

// appendBriefingStaleContracts warns when contract knowledge entries were updated after session start. [298.C/319.C]
func appendBriefingStaleContracts(sb *strings.Builder, d briefingData) {
	if len(d.staleContractKeys) == 0 {
		return
	}
	plural := "ies"
	if len(d.staleContractKeys) == 1 {
		plural = "y"
	}
	fmt.Fprintf(sb, "⚠️ **%d contract knowledge entr%s updated since session start:** %s  \nRe-run CONTRACT_QUERY to get fresh data.\n\n",
		len(d.staleContractKeys), plural, strings.Join(d.staleContractKeys, ", "))
}

// buildProjectHealthTable fetches /api/v1/metrics from each running member workspace
// via Nexus and returns a Markdown table with RAM/coverage/lang. [Épica 260.A/268]
func (t *RadarTool) buildProjectHealthTable(_ context.Context) string {
	if t.cfg.Project == nil {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n### Project: %s (%d workspaces)\n", t.cfg.Project.ProjectName, len(t.cfg.Project.MemberWorkspaces))
	sb.WriteString("| Workspace | Status | RAM | Coverage | Lang |\n")
	sb.WriteString("|-----------|--------|-----|----------|------|\n")

	nexusBase := nexusDispatcherBase()
	childByPath := fetchProjectChildMap(nexusBase)

	for _, ws := range t.cfg.Project.MemberWorkspaces {
		name := filepath.Base(ws)
		status, ram, coverage, lang := "stopped", "—", "—", "—"
		isSelf := ws == "." || ws == "" || filepath.Clean(ws) == filepath.Clean(t.workspace)
		if isSelf {
			status = "running"
			ram, coverage, lang = selfWorkspaceMetrics(t)
		} else if ci, ok := childByPath[filepath.Clean(ws)]; ok {
			status = ci[1]
			if status == "running" {
				ram, coverage, lang = probeChildWorkspaceMetrics(nexusBase, ci[0])
			}
		}
		fmt.Fprintf(&sb, "| %s | %s | %s | %s | %s |\n", name, status, ram, coverage, lang)
	}
	return sb.String()
}

// fetchProjectChildMap calls Nexus /status and returns a map of clean-path → [id, status]. [319.B]
func fetchProjectChildMap(nexusBase string) map[string][2]string {
	childByPath := map[string][2]string{}
	if nexusBase == "" {
		return childByPath
	}
	type nexusChild struct {
		ID     string `json:"id"`
		Path   string `json:"path"`
		Status string `json:"status"`
	}
	statusClient := sre.SafeInternalHTTPClient(1)
	if resp, err := statusClient.Get(nexusBase + "/status"); err == nil && resp != nil { //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: nexusBase from NEO_EXTERNAL_URL, loopback-only
		var children []nexusChild
		_ = json.NewDecoder(resp.Body).Decode(&children)
		resp.Body.Close()
		for _, c := range children {
			childByPath[filepath.Clean(c.Path)] = [2]string{c.ID, c.Status}
		}
	}
	return childByPath
}

// selfWorkspaceMetrics reads RAM, RAG coverage, and dominant lang for the current workspace. [319.B]
func selfWorkspaceMetrics(t *RadarTool) (ram, coverage, lang string) {
	ram, coverage, lang = "—", "—", "—"
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	ram = fmt.Sprintf("%.0fMB", float64(ms.HeapInuse)/1024/1024)
	if t.graph != nil {
		cov := rag.IndexCoverage(t.graph, t.workspace)
		if cov < blastRadiusAutoFallbackThreshold {
			coverage = fmt.Sprintf("%.0f%% 🔶", cov*100)
		} else {
			coverage = fmt.Sprintf("%.0f%%", cov*100)
		}
	}
	lang = t.cfg.Workspace.DominantLang
	if lang == "" {
		lang = "go"
	}
	return
}

// probeChildWorkspaceMetrics fetches /api/v1/metrics from a running Nexus child. [319.B]
func probeChildWorkspaceMetrics(nexusBase, childID string) (ram, coverage, lang string) {
	ram, coverage, lang = "—", "—", "—"
	if nexusBase == "" || childID == "" {
		return
	}
	metricsURL := fmt.Sprintf("%s/workspaces/%s/api/v1/metrics", nexusBase, childID)
	metricsClient := sre.SafeInternalHTTPClient(2)
	mResp, mErr := metricsClient.Get(metricsURL) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: nexusBase from NEO_EXTERNAL_URL, loopback-only
	if mErr != nil || mResp == nil {
		return
	}
	defer mResp.Body.Close()
	var snap struct {
		Memory       struct{ HeapMB float64 `json:"heap_mb"` } `json:"memory"`
		IndexCov     float64 `json:"index_coverage"`
		DominantLang string  `json:"dominant_lang"`
	}
	if json.NewDecoder(mResp.Body).Decode(&snap) != nil {
		return
	}
	if snap.Memory.HeapMB > 0 {
		ram = fmt.Sprintf("%.0fMB", snap.Memory.HeapMB)
	}
	if snap.IndexCov > 0 {
		if snap.IndexCov < blastRadiusAutoFallbackThreshold {
			coverage = fmt.Sprintf("%.0f%% 🔶", snap.IndexCov*100)
		} else {
			coverage = fmt.Sprintf("%.0f%%", snap.IndexCov*100)
		}
	}
	if snap.DominantLang != "" {
		lang = snap.DominantLang
	}
	return
}

// appendBriefingArchMem injects RAG architectural memory relevant to the queried target. [SRE-28.1.2]
func appendBriefingArchMem(ctx context.Context, t *RadarTool, args map[string]any, sb *strings.Builder) {
	archTarget, ok := args["target"].(string)
	if !ok || archTarget == "" {
		return
	}
	queryVec, embedErr := t.embedder.Embed(ctx, archTarget)
	if embedErr != nil {
		return
	}
	results, searchErr := t.graph.SearchAuto(ctx, queryVec, 3, t.cpu, t.cfg.RAG.VectorQuant)
	if searchErr != nil || len(results) == 0 {
		return
	}
	sb.WriteString("\n### [MEMORIA ARQUITECTÓNICA RELEVANTE]\n")
	for idx, nodeIdx := range results {
		if int(nodeIdx) >= len(t.graph.Nodes) {
			continue
		}
		docID := t.graph.Nodes[nodeIdx].DocID
		path, content, _, _ := t.wal.GetDocMeta(docID)
		ext := strings.TrimPrefix(filepath.Ext(path), ".")
		if ext == "" {
			ext = "text"
		}
		fmt.Fprintf(sb, "#### %d. `%s`\n```%s\n%s\n```\n", idx+1, path, ext, content)
	}
}

func (t *RadarTool) handleBriefing(ctx context.Context, args map[string]any) (any, error) {
	// [Épica 78] mode parameter: "compact" | "full" (default) | "delta" [315.B].
	modeArg, _ := args["mode"].(string)
	if modeArg == "" {
		modeArg = "full"
	}
	// [156.B] Capture first-briefing state BEFORE gather so the warning reflects reality.
	isFirstBriefing := t.briefingCallCount == 0
	d := gatherBriefingData(t, isFirstBriefing)
	t.briefingCallCount++
	t.lastBriefingTime = time.Now()
	cur := snapshotFromData(d)
	defer func() { t.lastBriefingSnapshot = cur }()
	// [315.B] Delta mode — diff vs last snapshot, show only changed fields.
	if modeArg == "delta" {
		var sb strings.Builder
		sb.WriteString("## SRE BRIEFING (delta)\n\n")
		if t.lastBriefingSnapshot == nil {
			sb.WriteString(d.compactLine + "\n")
			sb.WriteString("\n_[first BRIEFING this session — no delta available]_\n")
		} else {
			sb.WriteString(diffBriefingSnapshots(t.lastBriefingSnapshot, cur))
			sb.WriteString("\n")
		}
		appendSessionMuts(&sb, d.sessionMuts, true)
		return mcpText(sb.String()), nil
	}
	// [Épica 78.2] Compact mode — one-line summary + session context. [127.4]
	if modeArg == "compact" {
		var sb strings.Builder
		sb.WriteString("## SRE BRIEFING (compact)\n\n")
		sb.WriteString(d.compactLine + "\n")
		appendSessionContextLines(&sb, &d) // [127.4]
		appendSessionMuts(&sb, d.sessionMuts, true)
		return mcpText(sb.String()), nil
	}
	// [Épica 78.3] Auto-compact if full response exceeds 8KB.
	full := formatFullBriefing(ctx, t, args, d)
	if len(full) > 8192 {
		var compactSb strings.Builder
		compactSb.WriteString("## SRE BRIEFING (auto-compact)\n\n")
		compactSb.WriteString(d.compactLine + "\n")
		appendSessionContextLines(&compactSb, &d) // [127.4]
		compactSb.WriteString("\n_[use mode:full for task detail]_\n")
		appendSessionMuts(&compactSb, d.sessionMuts, true)
		return mcpText(compactSb.String()), nil
	}
	return mcpText(full), nil
}

// computeDaemonBackendSegment renders the "Q/N tasks · backend:X"
// daemon segment when there's queue activity or daemon mode is
// active. Returns "" when no segment should appear.
//
// Extracted from gatherBriefingData to keep that function below
// CC=15 — the four-branch logic (cfg nil + queue err + total + mode)
// added measurable complexity to the parent. [138.E.4 refactor]
func computeDaemonBackendSegment(cfg *config.NeoConfig, serverMode string) string {
	if cfg == nil {
		return ""
	}
	qsum, qerr := state.GetDaemonQueueSummary("", 0)
	if qerr != nil {
		return ""
	}
	total := qsum.Pending + qsum.InProgress + qsum.Done + qsum.Failed
	mode := cfg.SRE.DaemonBackendMode
	if mode == "" {
		mode = "auto"
	}
	if total == 0 && serverMode != "daemon" {
		return ""
	}
	return fmt.Sprintf("%d/%d tasks · backend:%s", qsum.Pending, total, mode)
}

// populateTrustHighlights returns the top 3 trust scores with real
// evidence (TotalExecutions > 0) for inclusion in the compact briefing
// line. Skips fresh-prior entries (the unknown:unknown migration seed,
// freshly-created scopes) so the operator only sees buckets where
// the trust system is actually learning. [138.E.4]
//
// Errors are swallowed (returns nil) — BRIEFING is read-mostly and a
// trust query failure shouldn't break the rest of the report.
func populateTrustHighlights() []TrustStatusEntry {
	all, _, err := state.ListTrustScores()
	if err != nil || len(all) == 0 {
		return nil
	}
	now := time.Now()
	withEvidence := make([]TrustStatusEntry, 0, len(all))
	for _, s := range all {
		if s.TotalExecutions == 0 {
			continue
		}
		var lastUpdate int64
		if !s.LastUpdate.IsZero() {
			lastUpdate = s.LastUpdate.Unix()
		}
		withEvidence = append(withEvidence, TrustStatusEntry{
			Pattern:         s.Pattern,
			Scope:           s.Scope,
			Alpha:           s.Alpha,
			Beta:            s.Beta,
			TotalExecutions: s.TotalExecutions,
			Tier:            string(s.CurrentTier),
			LowerBound:      s.LowerBound(now),
			PointEstimate:   s.PointEstimate(now),
			ManualWarmup:    s.ManualWarmup,
			LastUpdateUnix:  lastUpdate,
		})
	}
	sortTrustEntriesByLowerBound(withEvidence)
	if len(withEvidence) > 3 {
		withEvidence = withEvidence[:3]
	}
	return withEvidence
}

// compactTrustSegment formats trustHighlights into the compact-line
// suffix segment. Empty when no highlights — keeps the line clean
// for fresh workspaces.
//
// Format: " | trust: refactor:.go:pkg/state=L1(α=12 β=4) | distill:.md:docs=L2(α=45 β=3)"
//
// LowerBound and PointEstimate are NOT shown — too much noise for a
// compact view. The full breakdown lives in `/daemon-trust` skill or
// the `trust_status` action. [138.E.4]
func compactTrustSegment(d *briefingData) string {
	if len(d.trustHighlights) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(" | trust:")
	for i, h := range d.trustHighlights {
		if i > 0 {
			sb.WriteString(" |")
		}
		fmt.Fprintf(&sb, " %s:%s=%s(α=%.0f β=%.0f)", h.Pattern, h.Scope, h.Tier, h.Alpha, h.Beta)
	}
	return sb.String()
}

