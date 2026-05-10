package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/coldstore"
	"github.com/ensamblatec/neoanvil/pkg/federation"
	"github.com/ensamblatec/neoanvil/pkg/kanban"
	"github.com/ensamblatec/neoanvil/pkg/rag"
	"github.com/ensamblatec/neoanvil/pkg/state"
	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

// ragMemexAdapter implements federation.MemexExporter using pkg/state.MemexRead. [SRE-94]
// Placed in cmd/neo-mcp to avoid import cycles between pkg/rag and pkg/federation.
type ragMemexAdapter struct{}

func (a *ragMemexAdapter) ExportSince(since time.Time) []federation.MemexVector {
	entries, err := state.MemexRead(since)
	if err != nil {
		log.Printf("[FEDERATION] MemexRead error: %v", err)
		return nil
	}
	vectors := make([]federation.MemexVector, 0, len(entries))
	for _, e := range entries {
		vectors = append(vectors, federation.MemexVector{
			ID:        e.ID,
			Topic:     e.Topic,
			Scope:     e.Scope,
			Content:   e.Content,
			Timestamp: time.Unix(e.Timestamp, 0),
		})
	}
	return vectors
}

func TriggerREMSleep(ctx context.Context, workspace string, wal *rag.WAL, graph *rag.Graph, embedder rag.Embedder, cpu tensorx.ComputeDevice, cold *coldstore.Engine, sg *rag.SharedGraph) {
	entries, err := state.MemexDrain()
	if err != nil {
		log.Printf("[SRE-MEMEX] Error draining memex buffer: %v", err)
		return
	}
	if len(entries) == 0 {
		return
	}

	consolidated := consolidateMemexToHNSW(ctx, entries, graph, wal, embedder, cpu)
	log.Printf("[SRE-MEMEX] Ciclo REM completado. %d episodios consolidados en memoria a largo plazo.", consolidated)

	if consolidated > 0 {
		mergeREMToSharedGraph(wal, sg) // [287.D]
	}

	// [SRE-38.2] Archive memex entries to cold storage for long-term analytics
	if cold != nil && len(entries) > 0 {
		archiveMemexToColdstore(ctx, cold, entries, workspace)
	}

	// [SRE-30.2.1] Kanban sync during idle — archive completed epics to master_done.md.
	if archived, kErr := kanban.SyncCompletedEpics(workspace); kErr != nil {
		log.Printf("[SRE-KANBAN] Sync error: %v", kErr)
	} else if archived > 0 {
		log.Printf("[SRE-KANBAN] Archived %d completed epic(s) to master_done.md.", archived)
	}

	runREMPruneGC(ctx, graph, wal)

	// [SRE-46.2] Compress similar flashbacks from consolidated entries into distilled rules.
	if len(entries) >= 5 {
		compressMemexFlashbacks(entries)
	}

	// [SRE-58.2] Wisdom Distiller — append distilled rules to master_plan.md.
	if len(entries) >= 3 {
		appendWisdomToMasterPlan(workspace, entries)
	}

	// [SRE-41] Active Dreaming — adversarial fault scenarios during REM idle.
	runREMDreamCycle(ctx, workspace)
}

func mergeREMToSharedGraph(wal *rag.WAL, sg *rag.SharedGraph) {
	if sg == nil {
		return
	}
	if lockErr := sg.TryLock(); lockErr != nil {
		return
	}
	defer sg.Unlock()
	if added, mergeErr := sg.MergeFrom(wal); mergeErr != nil {
		log.Printf("[287.D] shared merge error: %v", mergeErr)
	} else if added > 0 {
		log.Printf("[287.D] shared merge: +%d docs", added)
	}
}

func runREMPruneGC(ctx context.Context, graph *rag.Graph, wal *rag.WAL) {
	gcResult := remDistiller.IdleVectorGC(ctx, graph, wal, remFPI, 100)
	if gcResult.Evicted > 0 {
		log.Printf("[SRE-46] IdleVectorGC evicted %d idle nodes", gcResult.Evicted)
	}
	if remFPI.HitRate() < 0.3 && len(graph.Nodes) > 1000 {
		pruneResult := remDistiller.PruneByFPI(ctx, graph, wal, remFPI, 0.15)
		if pruneResult.Pruned > 0 {
			log.Printf("[SRE-72.3] PruneByFPI: pruned %d/%d nodes (threshold=0.15)", pruneResult.Pruned, pruneResult.Examined)
		}
	}
}

func consolidateMemexToHNSW(ctx context.Context, entries []state.MemexEntry, graph *rag.Graph, wal *rag.WAL, embedder rag.Embedder, cpu tensorx.ComputeDevice) int {
	if len(entries) == 0 {
		return 0
	}
	// Batch the embed calls — REM consolidation typically processes 5-50 entries
	// at once (memex_buffer accumulated during the idle window). Batched embed
	// runs ~2-3× faster than sequential at this batch size.
	texts := make([]string, len(entries))
	for i, entry := range entries {
		texts[i] = entry.Topic + " " + entry.Content
	}
	vecs, embedErr := rag.EmbedMany(ctx, embedder, texts)
	if embedErr != nil {
		log.Printf("[SRE-MEMEX] batch embed error (%d entries): %v — falling back to per-entry", len(entries), embedErr)
		return consolidateMemexPerEntry(ctx, entries, graph, wal, embedder, cpu)
	}
	consolidated := 0
	for i, entry := range entries {
		docID := docIDCounter.Add(1) + uint64(time.Now().UnixNano())
		if insertErr := graph.Insert(ctx, docID, vecs[i], 16, cpu, wal); insertErr != nil {
			log.Printf("[SRE-MEMEX] HNSW insert error for entry %s: %v", entry.ID, insertErr)
			continue
		}
		if metaErr := wal.SaveDocMeta(docID, entry.Scope, entry.Content, 0); metaErr != nil {
			log.Printf("[SRE-MEMEX] WAL meta error for entry %s: %v", entry.ID, metaErr)
		}
		consolidated++
	}
	return consolidated
}

// consolidateMemexPerEntry is the fallback when the batch embed fails for any
// reason. Mirrors the legacy per-entry behaviour so REM consolidation never
// regresses from the previous baseline when Ollama is degraded.
func consolidateMemexPerEntry(ctx context.Context, entries []state.MemexEntry, graph *rag.Graph, wal *rag.WAL, embedder rag.Embedder, cpu tensorx.ComputeDevice) int {
	consolidated := 0
	for _, entry := range entries {
		text := entry.Topic + " " + entry.Content
		vec, embedErr := embedder.Embed(ctx, text)
		if embedErr != nil {
			log.Printf("[SRE-MEMEX] Embed error for entry %s: %v", entry.ID, embedErr)
			continue
		}
		docID := docIDCounter.Add(1) + uint64(time.Now().UnixNano())
		if insertErr := graph.Insert(ctx, docID, vec, 16, cpu, wal); insertErr != nil {
			log.Printf("[SRE-MEMEX] HNSW insert error for entry %s: %v", entry.ID, insertErr)
			continue
		}
		if metaErr := wal.SaveDocMeta(docID, entry.Scope, entry.Content, 0); metaErr != nil {
			log.Printf("[SRE-MEMEX] WAL meta error for entry %s: %v", entry.ID, metaErr)
		}
		consolidated++
	}
	return consolidated
}

func archiveMemexToColdstore(ctx context.Context, cold *coldstore.Engine, entries []state.MemexEntry, workspace string) {
	archives := make([]coldstore.MemexArchive, 0, len(entries))
	for _, e := range entries {
		archives = append(archives, coldstore.MemexArchive{
			Timestamp:   time.Now().Unix(),
			Topic:       e.Topic,
			Scope:       e.Scope,
			Content:     e.Content,
			WorkspaceID: workspace,
		})
	}
	if archived, archErr := cold.ArchiveMemex(ctx, archives); archErr != nil {
		log.Printf("[COLDSTORE] Archive error: %v", archErr)
	} else if archived > 0 {
		log.Printf("[COLDSTORE] Archived %d memex entries to cold storage", archived)
	}
}

func compressMemexFlashbacks(entries []state.MemexEntry) {
	texts := make([]string, 0, len(entries))
	for _, e := range entries {
		texts = append(texts, e.Topic+" "+e.Content)
	}
	rules := remDistiller.CompressFlashbacks(texts, 0.70)
	if len(rules) > 0 {
		log.Printf("[SRE-46] Compressed %d flashbacks into %d rules", len(texts), len(rules))
	}
}

func runREMDreamCycle(ctx context.Context, workspace string) {
	if dreamEngine == nil {
		return
	}
	count := 3
	if lc := liveConfigPtr.Load(); lc != nil && lc.Sentinel.DreamCycleCount > 0 {
		count = lc.Sentinel.DreamCycleCount
	}
	results := dreamEngine.DreamCycle(ctx, count)
	recovered, gaps := 0, 0
	for _, r := range results {
		if r.Recovered {
			recovered++
		} else {
			gaps++
			recordTechDebt(workspace, fmt.Sprintf("Dream gap: %s (%s)", r.Scenario.Category, r.Scenario.Description),
				fmt.Sprintf("No recovery strategy found for fault scenario: %s. Signature: %s", r.Scenario.Description, r.Scenario.Signature), "media")
		}
	}
	log.Printf("[SRE-41] DreamCycle: %d scenarios, %d recovered, %d gaps", len(results), recovered, gaps)
}

func computeBrainMerkle(graph *rag.Graph) string {
	if graph == nil || len(graph.Nodes) == 0 {
		return ""
	}
	ids := make([]string, 0, len(graph.Nodes))
	for _, n := range graph.Nodes {
		ids = append(ids, fmt.Sprintf("%d", n.DocID))
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		_, _ = h.Write([]byte(id))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func appendWisdomToMasterPlan(workspace string, entries []state.MemexEntry) {
	planPath := filepath.Join(workspace, ".neo", "master_plan.md")
	data, err := os.ReadFile(planPath) //nolint:gosec // G304-WORKSPACE-CANON
	if err != nil {
		return
	}
	content := string(data)
	// Don't duplicate — remove prior wisdom section before rewriting.
	const wisdomHeader = "\n## 🧠 Distilled Wisdom (Auto-generated)\n"
	if idx := strings.Index(content, wisdomHeader); idx != -1 {
		content = content[:idx]
	}

	var sb strings.Builder
	sb.WriteString(content)
	sb.WriteString(wisdomHeader)
	fmt.Fprintf(&sb, "_Last distillation: %s_\n\n", time.Now().UTC().Format("2006-01-02 15:04 UTC"))
	seen := make(map[string]struct{})
	for _, e := range entries {
		key := e.Topic
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		summary := e.Content
		if len(summary) > 120 {
			summary = summary[:120] + "…"
		}
		fmt.Fprintf(&sb, "- **%s** (`%s`): %s\n", e.Topic, e.Scope, summary)
	}
	//nolint:gosec // G304-WORKSPACE-CANON
	if writeErr := os.WriteFile(planPath, []byte(sb.String()), 0600); writeErr == nil {
		log.Printf("[SRE-58.2] Wisdom Distiller: appended %d entries to master_plan.md", len(seen))
	}
}
