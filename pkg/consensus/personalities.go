package consensus

// personalities.go — Synthetic Debate V2: three reasoning profiles for
// independent mutation assessment. [SRE-62]
//
// Each PersonalityProfile encodes a distinct SRE persona with a focused
// system prompt. RunDebate polls all three profiles against the same Ollama
// model (using per-request system prompts) and aggregates their verdicts.
//
// Profiles:
//   Auditor   — security and formal verification focus
//   Optimizer — thermal efficiency and throughput focus
//   Architect — hypergraph structure and abstraction quality focus
//
// This deliberately avoids separate models (requires only one Ollama
// deployment) while still producing diversified assessments.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// PersonalityProfile defines a reasoning persona for the internal debate. [SRE-62.1]
type PersonalityProfile struct {
	Name         string  // "Auditor" | "Optimizer" | "Architect"
	Role         string  // one-line role description (for logging)
	SystemPrompt string  // injected as the Ollama system field
	EvalWeight   float64 // contribution weight to final consensus (default 1.0/3)
}

// AuditorProfile returns the security-focused reasoning persona. [SRE-62.1]
func AuditorProfile() PersonalityProfile {
	return PersonalityProfile{
		Name: "Auditor",
		Role: "Security & Formal Verification",
		SystemPrompt: `You are The Auditor, a strict security-first code reviewer.
Your primary concerns are: injection vulnerabilities, race conditions, unsafe type
assertions, missing error handling, secret exposure, and violations of the
Zero-Hardcoding rule (no hardcoded IPs, ports, or credentials).
If in doubt, REJECT. False positives are preferred over missed vulnerabilities.`,
		EvalWeight: 1.0 / 3,
	}
}

// OptimizerProfile returns the performance and thermal efficiency persona. [SRE-62.1]
func OptimizerProfile() PersonalityProfile {
	return PersonalityProfile{
		Name: "Optimizer",
		Role: "Thermal Efficiency & Throughput",
		SystemPrompt: `You are The Optimizer, a performance-obsessed SRE engineer.
Your primary concerns are: allocations in hot paths (make/new inside loops),
O(N²) algorithms when O(N) or O(1) is possible, goroutine leaks, missing
context cancellation, and thermodynamic cost (RAPL watts per operation).
Approve if the mutation improves or maintains performance. Reject regressions.`,
		EvalWeight: 1.0 / 3,
	}
}

// ArchitectProfile returns the code structure and hypergraph quality persona. [SRE-62.1]
func ArchitectProfile() PersonalityProfile {
	return PersonalityProfile{
		Name: "Architect",
		Role: "Hypergraph Structure & Abstraction Quality",
		SystemPrompt: `You are The Architect, a structural code quality guardian.
Your primary concerns are: cyclomatic complexity (CC > 15 is a REJECT),
shadow variables, dead code, abstraction leaks, naming consistency, and
whether the change fits coherently into the existing package structure.
You value simplicity over cleverness. Reject mutations that increase debt.`,
		EvalWeight: 1.0 / 3,
	}
}

// PersonalityVerdict is the assessment of one profile. [SRE-62.2]
type PersonalityVerdict struct {
	Profile    string  `json:"profile"`    // "Auditor" | "Optimizer" | "Architect"
	Approved   bool    `json:"approved"`
	Confidence float32 `json:"confidence"` // 0.0–1.0
	Reason     string  `json:"reason"`
	LatencyMs  int64   `json:"latency_ms"`
}

// DebateResult is the outcome of a three-way personality debate. [SRE-62.2]
type DebateResult struct {
	Verdicts    []PersonalityVerdict `json:"verdicts"`
	Consensus   bool                 `json:"consensus"` // true if weighted approval ≥ 0.66
	WeightedAgreement float64        `json:"weighted_agreement"`
	CertifiedBy []string             `json:"certified_by,omitempty"` // profiles that approved
	VetoBy      []string             `json:"veto_by,omitempty"`      // profiles that rejected
}

// RunDebate polls all three personality profiles and returns a DebateResult. [SRE-62.2]
// Uses the Ollama model configured in the ConsensusEngine.
// On model unavailability, returns a degraded-mode approval (non-blocking).
func (ce *ConsensusEngine) RunDebate(ctx context.Context, mutation string) DebateResult {
	profiles := []PersonalityProfile{
		AuditorProfile(),
		OptimizerProfile(),
		ArchitectProfile(),
	}

	result := DebateResult{}
	var weightedApproval float64

	for _, p := range profiles {
		v := ce.pollWithPersonality(ctx, p, mutation)
		result.Verdicts = append(result.Verdicts, v)
		if v.Approved {
			weightedApproval += p.EvalWeight
			result.CertifiedBy = append(result.CertifiedBy, p.Name)
		} else {
			result.VetoBy = append(result.VetoBy, p.Name)
		}
	}

	result.WeightedAgreement = weightedApproval
	result.Consensus = weightedApproval >= 0.66

	log.Printf("[DEBATE] mutation=%q agreement=%.2f consensus=%v certified_by=%v veto_by=%v",
		truncate(mutation, 60), weightedApproval, result.Consensus,
		result.CertifiedBy, result.VetoBy)

	return result
}

// pollWithPersonality calls pollModelWithSystemPrompt using the configured model. [SRE-62.2]
func (ce *ConsensusEngine) pollWithPersonality(ctx context.Context, p PersonalityProfile, mutation string) PersonalityVerdict {
	model := ce.cfg.Inference.OllamaModel
	if model == "" {
		model = "qwen2:0.5b"
	}

	baseURL := ce.cfg.Inference.OllamaBaseURL
	if baseURL == "" {
		baseURL = ce.cfg.AI.BaseURL
	}
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}

	start := time.Now()
	approved, confidence, reason, err := ce.pollOllamaWithSystem(ctx, baseURL, model, p.SystemPrompt, mutation)
	if err != nil {
		log.Printf("[DEBATE] %s poll error: %v — defaulting to approve (degraded)", p.Name, err)
		// Degraded: unavailable model is not a veto (matches circuit breaker philosophy).
		return PersonalityVerdict{
			Profile:   p.Name,
			Approved:  true,
			Reason:    fmt.Sprintf("degraded: %v", err),
			LatencyMs: time.Since(start).Milliseconds(),
		}
	}

	return PersonalityVerdict{
		Profile:    p.Name,
		Approved:   approved,
		Confidence: confidence,
		Reason:     reason,
		LatencyMs:  time.Since(start).Milliseconds(),
	}
}

// pollOllamaWithSystem sends a generation request with an explicit system prompt. [SRE-62.2]
func (ce *ConsensusEngine) pollOllamaWithSystem(
	ctx context.Context,
	baseURL, model, systemPrompt, mutation string,
) (approved bool, confidence float32, reason string, err error) {
	prompt := fmt.Sprintf(`Proposed mutation to review:

%s

Respond in ONE line: APPROVE <confidence 0.0-1.0> <brief reason>  OR  REJECT <confidence 0.0-1.0> <brief reason>
Example: APPROVE 0.90 no security issues detected`, mutation)

	payload := map[string]any{
		"model":  model,
		"system": systemPrompt,
		"prompt": prompt,
		"stream": false,
		"options": map[string]any{
			"num_predict": 80,
			"temperature": 0.15,
		},
	}
	body, _ := json.Marshal(payload)

	req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/generate", bytes.NewReader(body))
	if reqErr != nil {
		return false, 0, "", reqErr
	}
	req.Header.Set("Content-Type", "application/json")

	resp, doErr := ce.client.Do(req)
	if doErr != nil {
		return false, 0, "", doErr
	}
	defer resp.Body.Close() //nolint:errcheck // Close error on response body is non-actionable

	var ollamaResp struct {
		Response string `json:"response"`
	}
	if decErr := json.NewDecoder(resp.Body).Decode(&ollamaResp); decErr != nil {
		return false, 0, "", decErr
	}

	approved, confidence, reason = parseVerdict(ollamaResp.Response)
	return approved, confidence, reason, nil
}

// DebateSignature formats CertifiedBy as a compact signature string. [SRE-62.3]
// Returns "" when debate was not run (consensus_enabled=false).
func DebateSignature(d DebateResult) string {
	if len(d.CertifiedBy) == 0 {
		return ""
	}
	return "Certified by " + strings.Join(d.CertifiedBy, ", ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
