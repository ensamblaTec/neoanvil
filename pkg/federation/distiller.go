// pkg/federation/distiller.go — Wisdom Distiller for federated dream synthesis. [SRE-94.B]
//
// Deduplicates vectors by cosine similarity, then distills clustered lessons
// into actionable directives via LLM. Produces a Manifest that can be
// propagated to all fleet nodes.
package federation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"time"
)

// sanitizePromptInput truncates and strips control chars from LLM prompt inputs.
// Prevents prompt injection from remote fleet nodes. [SRE-96.A.3]
func sanitizePromptInput(s string, maxLen int) string {
	if len(s) > maxLen {
		s = s[:maxLen] + "..."
	}
	// Strip characters that could break prompt structure.
	r := strings.NewReplacer(
		"```", "'''",
		"\\n", " ",
	)
	return r.Replace(s)
}

// Directive is a single distilled lesson from the fleet's collective experience. [SRE-94.B.2]
type Directive struct {
	Rule  string `json:"rule"`
	Why   string `json:"why"`
	When  string `json:"when"`
	Topic string `json:"topic"`
}

// Manifest is the daily output of the dream synthesis pipeline. [SRE-94.B.2]
type Manifest struct {
	Date        string      `json:"date"` // YYYY-MM-DD
	Directives  []Directive `json:"directives"`
	SourceNodes []string    `json:"source_nodes"`
	VectorCount int         `json:"vector_count"`
	CreatedAt   time.Time   `json:"created_at"`
}

// DeduplicateVectors removes near-duplicate vectors by cosine similarity.
// Vectors with similarity > threshold are merged (newest wins, content concatenated).
// [SRE-94.B.1]
func DeduplicateVectors(vectors []MemexVector, threshold float32) []MemexVector {
	if threshold <= 0 {
		threshold = 0.92
	}
	if len(vectors) <= 1 {
		return vectors
	}

	// Mark which vectors are absorbed by another.
	absorbed := make([]bool, len(vectors))

	for i := range vectors {
		if absorbed[i] {
			continue
		}
		for j := i + 1; j < len(vectors); j++ {
			if absorbed[j] {
				continue
			}
			if len(vectors[i].Embedding) == 0 || len(vectors[j].Embedding) == 0 {
				continue
			}
			sim := cosineSimilarity(vectors[i].Embedding, vectors[j].Embedding)
			if sim > threshold {
				// Merge: keep the newer one, concatenate content.
				if vectors[j].Timestamp.After(vectors[i].Timestamp) {
					vectors[j].Content = vectors[j].Content + "\n---\n" + vectors[i].Content
					absorbed[i] = true
					break // i is absorbed, move on
				} else {
					vectors[i].Content = vectors[i].Content + "\n---\n" + vectors[j].Content
					absorbed[j] = true
				}
			}
		}
	}

	// Collect survivors.
	result := make([]MemexVector, 0, len(vectors))
	for i, v := range vectors {
		if !absorbed[i] {
			result = append(result, v)
		}
	}

	log.Printf("[FEDERATION] dedup: %d → %d vectors (threshold %.2f)", len(vectors), len(result), threshold)
	return result
}

// cosineSimilarity computes the cosine similarity between two float32 vectors.
func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return float32(dot / denom)
}

// DistillManifest groups vectors by topic and uses an LLM to synthesize
// actionable directives from the collective lessons learned. [SRE-94.B.2]
func DistillManifest(vectors []MemexVector, ollamaURL, model string, client *http.Client) (Manifest, error) {
	// Group by topic.
	byTopic := make(map[string][]MemexVector)
	nodeSet := make(map[string]struct{})
	for _, v := range vectors {
		topic := v.Topic
		if topic == "" {
			topic = "general"
		}
		byTopic[topic] = append(byTopic[topic], v)
		nodeSet[v.NodeID] = struct{}{}
	}

	sourceNodes := make([]string, 0, len(nodeSet))
	for n := range nodeSet {
		sourceNodes = append(sourceNodes, n)
	}

	var directives []Directive
	var failedTopics int

	for topic, group := range byTopic {
		// Build the prompt with lessons from this topic.
		var sb strings.Builder
		for _, mv := range group {
			// [SRE-96.A.3] Sanitize remote content — truncate and strip control chars.
			content := sanitizePromptInput(mv.Content, 2048)
			nodeID := mv.NodeID
			if nodeID == "" {
				nodeID = "unknown"
			}
			fmt.Fprintf(&sb, "- [Node %s] %s\n", nodeID, content)
		}
		lessonsText := sb.String()

		prompt := fmt.Sprintf(`You are distilling lessons learned from %d nodes about "%s".

Lessons:
%s

Synthesize into a concise, actionable directive. Format your response as JSON:
{"rule": "...", "why": "...", "when": "..."}

The rule should be specific and actionable. The why explains the motivation.
The when describes the trigger condition.`, len(group), topic, lessonsText)

		body, _ := json.Marshal(map[string]any{
			"model":  model,
			"prompt": prompt,
			"stream": false,
			"format": "json",
			"options": map[string]any{
				"temperature": 0.3,
				"num_predict": 512,
			},
		})

		req, err := http.NewRequest(http.MethodPost, ollamaURL+"/api/generate", bytes.NewReader(body))
		if err != nil {
			log.Printf("[FEDERATION] distill request error for topic %s: %v", topic, err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[FEDERATION] distill call failed for topic %s: %v", topic, err)
			continue
		}

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
		resp.Body.Close()

		var result struct {
			Response string `json:"response"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			log.Printf("[FEDERATION] distill decode error for topic %s: %v", topic, err)
			failedTopics++
			continue
		}

		var d Directive
		if err := json.Unmarshal([]byte(result.Response), &d); err != nil {
			log.Printf("[FEDERATION] directive parse error for topic %s: %v", topic, err)
			failedTopics++
			continue
		}
		d.Topic = topic
		directives = append(directives, d)
	}

	// [SRE-96.B.3] Return error when ALL topics failed — empty manifest is not useful.
	if len(directives) == 0 && failedTopics > 0 {
		return Manifest{}, fmt.Errorf("distillation failed for all %d topics", failedTopics)
	}

	manifest := Manifest{
		Date:        time.Now().Format("2006-01-02"),
		Directives:  directives,
		SourceNodes: sourceNodes,
		VectorCount: len(vectors),
		CreatedAt:   time.Now(),
	}

	log.Printf("[FEDERATION] distilled %d directives from %d vectors across %d nodes",
		len(directives), len(vectors), len(sourceNodes))

	return manifest, nil
}
