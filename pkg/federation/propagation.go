// pkg/federation/propagation.go — Wisdom Propagation for federated dream synthesis. [SRE-94.C]
//
// Pushes the distilled Manifest to all fleet nodes and orchestrates the full
// dream synthesis pipeline (collect → dedup → distill → propagate).
package federation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// PropagateManifest sends the distilled wisdom manifest to all healthy fleet
// nodes via POST /api/v1/memex/ingest. Idempotent by Manifest.Date. [SRE-94.C.1]
func PropagateManifest(manifest Manifest, nodes []FleetNode, client *http.Client) error {
	body, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	var succeeded, failed int

	for _, node := range nodes {
		if !node.Healthy {
			continue
		}

		scheme := "http"
		if node.UseTLS {
			scheme = "https"
		}
		url := fmt.Sprintf("%s://%s:%d/api/v1/memex/ingest", scheme, node.Host, node.Port)

		req, reqErr := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if reqErr != nil {
			log.Printf("[FEDERATION] propagate request error for node %s: %v", node.ID, reqErr)
			failed++
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, respErr := client.Do(req)
		if respErr != nil {
			log.Printf("[FEDERATION] propagate failed for node %s: %v", node.ID, respErr)
			failed++
			continue
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
			log.Printf("[FEDERATION] propagate to node %s returned status %d", node.ID, resp.StatusCode)
			failed++
			continue
		}

		succeeded++
		log.Printf("[FEDERATION] manifest propagated to node %s", node.ID)
	}

	log.Printf("[NEXUS-EVENT] manifest_propagated nodes=%d/%d directives=%d",
		succeeded, succeeded+failed, len(manifest.Directives))

	if succeeded == 0 && failed > 0 {
		return fmt.Errorf("propagation failed to all %d nodes", failed)
	}
	return nil
}

// DreamSynthesisPipeline runs the complete nightly dream synthesis cycle:
// collect → dedup → distill → propagate. [SRE-94.C.3]
func DreamSynthesisPipeline(cfg DreamConfig, nodes []FleetNode, exporter MemexExporter, localNodeID string) error {
	start := time.Now()
	log.Printf("[FEDERATION] dream_synthesis starting — %d nodes configured", len(nodes))

	// [SRE-110.B] Peer fleet nodes can be remote workspaces — use SafeHTTPClient
	// (anti-SSRF). HarvestTimeoutSec from cfg overrides the SafeHTTPClient default.
	client := sre.SafeHTTPClient()
	client.Timeout = time.Duration(cfg.HarvestTimeoutSec) * time.Second

	// 1. Export local vectors.
	localVectors := ExportDailyMemex(exporter, localNodeID)
	log.Printf("[FEDERATION] local memex: %d vectors", len(localVectors))

	// 2. Collect from fleet.
	remoteVectors := CollectFleetMemex(nodes, client, cfg.HarvestTimeoutSec)
	allVectors := append(localVectors, remoteVectors...)

	if len(allVectors) == 0 {
		log.Printf("[FEDERATION] dream_synthesis aborted — no vectors to process")
		return nil
	}

	// 3. Deduplicate.
	deduped := DeduplicateVectors(allVectors, cfg.DedupThreshold)

	// 4. Distill via LLM.
	manifest, err := DistillManifest(deduped, cfg.OllamaURL, cfg.Model, client)
	if err != nil {
		return fmt.Errorf("distill manifest: %w", err)
	}

	// 5. Propagate to all nodes.
	if err := PropagateManifest(manifest, nodes, client); err != nil {
		log.Printf("[FEDERATION] propagation partially failed: %v", err)
		// Don't return error — partial propagation is acceptable.
	}

	duration := time.Since(start)
	log.Printf("[NEXUS-EVENT] dream_synthesis_complete directives=%d nodes=%d duration=%s",
		len(manifest.Directives), len(manifest.SourceNodes), duration.Truncate(time.Second))

	return nil
}

// DreamConfig holds parameters for the dream synthesis pipeline.
// Populated from neo.yaml federation + llm sections.
type DreamConfig struct {
	DreamSchedule     string  // cron expression
	DedupThreshold    float32
	OllamaURL         string
	Model             string
	HarvestTimeoutSec int
	MaxVectorsPerNode int
}

// NewDreamConfigFromYAML creates a DreamConfig from the federation + llm sections of neo.yaml.
func NewDreamConfigFromYAML(schedule string, dedupThreshold float64, ollamaURL, model string, harvestTimeout, maxVectors int) DreamConfig {
	return DreamConfig{
		DreamSchedule:     schedule,
		DedupThreshold:    float32(dedupThreshold),
		OllamaURL:         ollamaURL,
		Model:             model,
		HarvestTimeoutSec: harvestTimeout,
		MaxVectorsPerNode: maxVectors,
	}
}
