// Package consensus implements the Multi-Agent Consensus voting engine. [SRE-48]
//
// Before applying a high-risk mutation, ConsensusEngine queries N local Ollama
// models and tallies their verdicts. If agreement < quorum, a SRE VETO is issued
// and manual sign-off is required.
//
// Model pool is configurable: primary model + secondary models.
// Quorum is configurable via sre.consensus_quorum (default 0.66 = 2/3).
package consensus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// Verdict is a single model's assessment of a proposed mutation.
type Verdict struct {
	Model      string  `json:"model"`
	Approved   bool    `json:"approved"`
	Confidence float32 `json:"confidence"`
	Reason     string  `json:"reason"`
	LatencyMs  int64   `json:"latency_ms"`
}

// ConsensusResult is the aggregated verdict of all models. [SRE-48.1]
type ConsensusResult struct {
	Mutation    string    `json:"mutation"`
	Verdicts    []Verdict `json:"verdicts"`
	Approved    int       `json:"approved"`   // count of approving models
	Total       int       `json:"total"`      // total models polled
	Agreement   float64   `json:"agreement"`  // Approved / Total
	Quorum      float64   `json:"quorum"`     // required threshold
	VetoActive  bool      `json:"veto_active"` // true if agreement < quorum
	VetoReason  string    `json:"veto_reason,omitempty"`
}

// ConsensusEngine polls multiple local models and computes quorum. [SRE-48.1/48.2]
type ConsensusEngine struct {
	cfg       *config.NeoConfig
	client    *http.Client
	failCount atomic.Int32 // [SRE-64.2] circuit breaker: open after 3 consecutive failures
}

// NewConsensusEngine creates a consensus engine backed by the given config.
func NewConsensusEngine(cfg *config.NeoConfig) *ConsensusEngine {
	return &ConsensusEngine{
		cfg:    cfg,
		client: sre.SafeHTTPClient(),
	}
}

// Evaluate polls the configured model pool and returns a ConsensusResult. [SRE-48.1]
// If sre.consensus_enabled is false, it returns an auto-approved result immediately.
func (ce *ConsensusEngine) Evaluate(ctx context.Context, mutation string) (ConsensusResult, error) {
	result := ConsensusResult{
		Mutation: mutation,
		Quorum:   ce.cfg.SRE.ConsensusQuorum,
	}

	if !ce.cfg.SRE.ConsensusEnabled {
		result.Approved = 1
		result.Total = 1
		result.Agreement = 1.0
		return result, nil
	}

	// [SRE-64.2] Circuit breaker: after 3 consecutive poll failures, return degraded approval.
	if ce.failCount.Load() >= 3 {
		log.Printf("[CONSENSUS] circuit breaker open — degraded mode (3 consecutive failures)")
		result.Approved = 1
		result.Total = 1
		result.Agreement = 1.0
		result.VetoReason = "consensus_unavailable — degraded mode"
		return result, nil
	}

	models := ce.modelPool()
	if len(models) == 0 {
		return result, fmt.Errorf("consensus_enabled but no models configured")
	}

	result.Total = len(models)
	for _, model := range models {
		v, err := ce.pollModel(ctx, model, mutation)
		if err != nil {
			log.Printf("[CONSENSUS] model %s poll error: %v", model, err)
			ce.failCount.Add(1)
			v = Verdict{Model: model, Approved: false, Reason: fmt.Sprintf("error: %v", err)}
		} else {
			ce.failCount.Store(0)
		}
		result.Verdicts = append(result.Verdicts, v)
		if v.Approved {
			result.Approved++
		}
	}

	result.Agreement = float64(result.Approved) / float64(result.Total)
	if result.Agreement < result.Quorum {
		result.VetoActive = true
		result.VetoReason = ce.buildVetoReason(result)
		log.Printf("[CONSENSUS] SRE VETO: agreement=%.2f < quorum=%.2f — mutation blocked",
			result.Agreement, result.Quorum)
	} else {
		log.Printf("[CONSENSUS] Approved: agreement=%.2f ≥ quorum=%.2f (%d/%d models)",
			result.Agreement, result.Quorum, result.Approved, result.Total)
	}

	return result, nil
}

// modelPool returns the list of Ollama models to poll.
// Primary model + any secondary models from config.
func (ce *ConsensusEngine) modelPool() []string {
	primary := ce.cfg.Inference.OllamaModel
	if primary == "" {
		primary = "qwen2:0.5b"
	}
	// Secondary: use a different small model for independent opinion.
	// Configurable in future; for now derive from primary with ":mini" fallback.
	secondary := "phi3:mini"
	if strings.Contains(primary, "qwen") {
		secondary = "phi3:mini"
	} else if strings.Contains(primary, "phi") {
		secondary = "qwen2:0.5b"
	}
	return []string{primary, secondary}
}

// pollModel sends a prompt to an Ollama model and parses its verdict.
func (ce *ConsensusEngine) pollModel(ctx context.Context, model, mutation string) (Verdict, error) {
	start := time.Now()
	prompt := buildConsensusPrompt(mutation)

	baseURL := ce.cfg.Inference.OllamaBaseURL
	if baseURL == "" {
		baseURL = ce.cfg.AI.BaseURL
	}
	if baseURL == "" {
		baseURL = "http://127.0.0.1:11434" // [SRE-LOCAL-LLM-2026-05-15] IPv4-explicit — macOS localhost→::1 drift
	}

	payload := map[string]any{
		"model":  model,
		"prompt": prompt,
		"stream": false,
		"options": map[string]any{
			"num_predict": 80,
			"temperature": 0.1,
		},
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return Verdict{Model: model}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := ce.client.Do(req)
	if err != nil {
		return Verdict{Model: model}, err
	}
	defer resp.Body.Close()

	var ollamaResp struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return Verdict{Model: model}, err
	}

	approved, confidence, reason := parseVerdict(ollamaResp.Response)
	return Verdict{
		Model:      model,
		Approved:   approved,
		Confidence: confidence,
		Reason:     reason,
		LatencyMs:  time.Since(start).Milliseconds(),
	}, nil
}

func buildConsensusPrompt(mutation string) string {
	return fmt.Sprintf(`You are a code review assistant. Analyze this proposed code mutation and decide if it is SAFE to apply.

Mutation: %s

Respond in ONE line with format: APPROVE <confidence 0.0-1.0> <brief reason>  OR  REJECT <confidence 0.0-1.0> <brief reason>
Example: APPROVE 0.85 no regressions detected
`, mutation)
}

// parseVerdict extracts approve/reject + confidence from model response.
func parseVerdict(response string) (approved bool, confidence float32, reason string) {
	upper := strings.ToUpper(strings.TrimSpace(response))
	if strings.HasPrefix(upper, "APPROVE") {
		approved = true
	}
	// Try to parse confidence as second token.
	parts := strings.Fields(response)
	confidence = 0.5
	if len(parts) >= 2 {
		var f float32
		if _, err := fmt.Sscanf(parts[1], "%f", &f); err == nil && f >= 0 && f <= 1 {
			confidence = f
		}
	}
	if len(parts) >= 3 {
		reason = strings.Join(parts[2:], " ")
	}
	return
}

func (ce *ConsensusEngine) buildVetoReason(r ConsensusResult) string {
	var rejections []string
	for _, v := range r.Verdicts {
		if !v.Approved {
			rejections = append(rejections, fmt.Sprintf("%s: %s", v.Model, v.Reason))
		}
	}
	return fmt.Sprintf("SRE VETO: %.0f%% agreement (need %.0f%%). Rejections: %s",
		r.Agreement*100, r.Quorum*100, strings.Join(rejections, "; "))
}
