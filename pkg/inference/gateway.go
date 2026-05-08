// Package inference implements the 4-level inference router for NeoAnvil. [SRE-34.2.1]
//
// Decision ladder (cheapest → most expensive):
//
//	LOCAL  → AST Audit only — zero tokens, pure static analysis
//	OLLAMA → Local model via Ollama API — zero external cost
//	HYBRID → Ollama + RAG Flashbacks — richer context, still zero external cost
//	CLOUD  → External API (Claude/OpenAI) — subject to daily token budget hard-limit
//
// Escalation rule: if Confidence < cfg.ConfidenceThreshold OR risk == CRITICAL → next level.
// Token budget: atomic int32 counter persisted to BoltDB, reset at UTC midnight. [SRE-34 note #2]
package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// Level identifies the inference tier.
type Level int

const (
	LOCAL  Level = iota // AST Audit — no network, no tokens
	OLLAMA              // local Ollama model
	HYBRID              // Ollama + RAG flashbacks
	CLOUD               // external API — token budget applies
)

func (l Level) String() string {
	switch l {
	case LOCAL:
		return "LOCAL"
	case OLLAMA:
		return "OLLAMA"
	case HYBRID:
		return "HYBRID"
	case CLOUD:
		return "CLOUD"
	default:
		return "UNKNOWN"
	}
}

// BounceResult is the structured output of a bouncer validation pass. [SRE-86.A.1]
// Returned by neo_sre_certify_mutation when a check fails, consumed by SuggestFix.
type BounceResult struct {
	Passed       bool     `json:"passed"`
	ErrorContext string   `json:"error_context"` // full error output from bouncer/tests
	FailedChecks []string `json:"failed_checks"` // which checks failed (e.g. "AST-SYNTAX", "TDD", "BOUNCER")
	FilePath     string   `json:"file_path"`
}

// DiagResult is the structured output of a Diagnose() call.
type DiagResult struct {
	Level      Level   `json:"level"`
	Confidence float32 `json:"confidence"`
	Summary    string  `json:"summary"`
	Suggestion string  `json:"suggestion"`
	Risk       string  `json:"risk"`   // "LOW" | "MEDIUM" | "HIGH" | "CRITICAL"
	Tokens     int     `json:"tokens"` // cloud tokens consumed (0 for LOCAL/OLLAMA/HYBRID)
}

// Gateway routes diagnostic requests across the 4 inference levels.
// One Gateway per workspace — isolation is structural. [SRE-34 note #1]
type Gateway struct {
	cfg          config.InferenceConfig
	ollamaURL    string
	dailyTokens  atomic.Int32 // tokens used today in CLOUD tier
	budgetResetD string       // "YYYY-MM-DD" of last reset
	// [SRE-86.B.3] Auto-fix metrics — atomic for concurrent access.
	autoFixAttempts  atomic.Int32
	autoFixSuccesses atomic.Int32
	// [PILAR-XXVII/243.E] Injected reporter so the gateway can emit
	// internal-inference token accounting without depending on the
	// observability package directly (kept import-free to preserve the
	// pkg/inference lightweight graph).
	tokenReporter TokenReporter
}

// TokenReporter is a callback invoked after each billable inference call.
// agent is the resolved model name (cloud or ollama); promptType is one
// of "Diagnose" | "SuggestFix" | "RunDebate"; tool is the upstream tool
// that triggered the call (e.g. "neo_sre_certify_mutation").
type TokenReporter func(agent, tool, promptType string, inTokens, outTokens int)

// SetTokenReporter installs the callback. Safe to call after NewGateway.
// Passing nil disables reporting.
func (g *Gateway) SetTokenReporter(r TokenReporter) { g.tokenReporter = r }

// reportTokens is a nil-safe invoker used by the internal call-sites.
func (g *Gateway) reportTokens(agent, tool, promptType string, in, out int) {
	if g == nil || g.tokenReporter == nil {
		return
	}
	g.tokenReporter(agent, tool, promptType, in, out)
}

// AutoFixSuccessRate returns the ratio of successful auto-fixes to total attempts. [SRE-86.B.3]
func (g *Gateway) AutoFixSuccessRate() float64 {
	attempts := g.autoFixAttempts.Load()
	if attempts == 0 {
		return 0
	}
	return float64(g.autoFixSuccesses.Load()) / float64(attempts)
}

// RecordAutoFixAttempt increments the auto-fix attempt counter. [SRE-86.B.3]
func (g *Gateway) RecordAutoFixAttempt(success bool) {
	g.autoFixAttempts.Add(1)
	if success {
		g.autoFixSuccesses.Add(1)
	}
}

// NewGateway creates a Gateway for a single workspace.
// cfg.OllamaBaseURL takes precedence; falls back to aiBaseURL.
func NewGateway(cfg config.InferenceConfig, aiBaseURL string) *Gateway {
	base := cfg.OllamaBaseURL
	if base == "" {
		base = aiBaseURL
	}
	return &Gateway{
		cfg:          cfg,
		ollamaURL:    base,
		budgetResetD: today(),
	}
}

// Diagnose runs the inference ladder for the given target file and error context.
// It returns the shallowest level that achieves confidence >= threshold.
func (g *Gateway) Diagnose(ctx context.Context, target, errorContext string, flashbacks []string) (*DiagResult, error) {
	g.maybeResetBudget()

	// --- Level 1: LOCAL (AST Audit) ---
	localResult := g.runLocal(target)
	if localResult.Confidence >= g.cfg.ConfidenceThreshold && localResult.Risk != "CRITICAL" {
		log.Printf("[INFERENCE] LOCAL sufficient (confidence=%.2f risk=%s)", localResult.Confidence, localResult.Risk)
		return localResult, nil
	}

	// --- Level 2: OLLAMA ---
	ollamaResult, err := g.runOllama(ctx, target, errorContext, nil)
	if err != nil {
		log.Printf("[INFERENCE] OLLAMA failed: %v — staying at LOCAL result", err)
		return localResult, nil
	}
	if ollamaResult.Confidence >= g.cfg.ConfidenceThreshold && ollamaResult.Risk != "CRITICAL" {
		log.Printf("[INFERENCE] OLLAMA sufficient (confidence=%.2f risk=%s)", ollamaResult.Confidence, ollamaResult.Risk)
		return ollamaResult, nil
	}

	// --- Level 3: HYBRID (Ollama + RAG flashbacks) ---
	hybridResult, err := g.runOllama(ctx, target, errorContext, flashbacks)
	if err != nil {
		log.Printf("[INFERENCE] HYBRID failed: %v — returning OLLAMA result", err)
		return ollamaResult, nil
	}
	if hybridResult.Confidence >= g.cfg.ConfidenceThreshold && hybridResult.Risk != "CRITICAL" {
		log.Printf("[INFERENCE] HYBRID sufficient (confidence=%.2f risk=%s)", hybridResult.Confidence, hybridResult.Risk)
		return hybridResult, nil
	}

	// --- Level 4: CLOUD (hard-budget check) ---
	estimatedTokens := estimateTokens(target, errorContext, flashbacks)
	if err := g.consumeTokens(estimatedTokens); err != nil {
		log.Printf("[INFERENCE] CLOUD blocked by budget: %v — returning HYBRID result", err)
		hybridResult.Summary = "[CLOUD blocked — daily budget exhausted] " + hybridResult.Summary
		return hybridResult, nil
	}
	cloudResult := &DiagResult{
		Level:      CLOUD,
		Confidence: 0.95,
		Tokens:     estimatedTokens,
		Risk:       hybridResult.Risk,
		Summary:    fmt.Sprintf("[CLOUD] Escalated from HYBRID (confidence was %.2f). Model: %s", hybridResult.Confidence, g.cfg.CloudModel),
		Suggestion: "Review the HYBRID suggestion and apply with `neo heal --mode manual`.",
	}
	log.Printf("[INFERENCE] CLOUD escalation (tokens=%d, budget_remaining=%d)", estimatedTokens, g.budgetRemaining())
	// [PILAR-XXVII/243.E] Internal-inference token accounting for CLOUD.
	// We don't split in/out accurately here (no response yet at this
	// point in the placeholder implementation); attribute all to input
	// so cost is conservative rather than optimistic.
	g.reportTokens(g.cfg.CloudModel, "pkg/inference", "Diagnose", estimatedTokens, 0)
	return cloudResult, nil
}

// SuggestFix takes a failed BounceResult and invokes the inference ladder
// to generate a suggested fix. Returns the suggestion text. [SRE-86.A.2]
// Only escalates to OLLAMA/HYBRID — never CLOUD (fix suggestions are best-effort).
func (g *Gateway) SuggestFix(ctx context.Context, bounce *BounceResult, flashbacks []string) (string, error) {
	if bounce == nil || bounce.Passed {
		return "", nil
	}
	prompt := buildFixPrompt(bounce, flashbacks)
	result, err := g.runOllama(ctx, bounce.FilePath, prompt, flashbacks)
	if err != nil {
		log.Printf("[INFERENCE] SuggestFix OLLAMA failed: %v", err)
		return "", err
	}
	suggestion := result.Suggestion
	if suggestion == "" {
		suggestion = result.Summary
	}
	log.Printf("[INFERENCE] SuggestFix generated (confidence=%.2f, level=%s)", result.Confidence, result.Level)
	return suggestion, nil
}

// buildFixPrompt composes a prompt specifically for fix suggestion. [SRE-86.A.2]
func buildFixPrompt(bounce *BounceResult, flashbacks []string) string {
	var b strings.Builder
	b.WriteString("A Go file failed validation. Suggest a minimal fix.\n\n")
	b.WriteString("File: ")
	b.WriteString(bounce.FilePath)
	b.WriteString("\n\nFailed checks: ")
	b.WriteString(strings.Join(bounce.FailedChecks, ", "))
	b.WriteString("\n\nError output:\n")
	b.WriteString(bounce.ErrorContext)
	if len(flashbacks) > 0 {
		b.WriteString("\n\nArchitectural context (RAG):\n")
		for _, f := range flashbacks {
			b.WriteString("- ")
			b.WriteString(f)
			b.WriteString("\n")
		}
	}
	b.WriteString("\n\nProvide ONLY the minimal code fix. No explanation, just the corrected code.")
	return b.String()
}

// TokensUsedToday returns the number of CLOUD tokens consumed today.
func (g *Gateway) TokensUsedToday() int {
	g.maybeResetBudget()
	return int(g.dailyTokens.Load())
}

// BudgetExhausted reports whether the daily CLOUD token budget has been hit.
func (g *Gateway) BudgetExhausted() bool {
	g.maybeResetBudget()
	return int(g.dailyTokens.Load()) >= g.cfg.CloudTokenBudgetDaily
}

// --- private helpers ---

func (g *Gateway) runLocal(target string) *DiagResult {
	// LOCAL tier: heuristic analysis without external calls.
	// Real CC/loop detection happens via neo_radar AST_AUDIT in the MCP layer.
	// Here we return a conservative baseline to drive the escalation decision.
	result := &DiagResult{
		Level:      LOCAL,
		Confidence: 0.55,
		Risk:       "MEDIUM",
		Summary:    fmt.Sprintf("Static analysis of %s — no network used.", target),
		Suggestion: "Run `neo audit %s` for detailed AST report.",
	}
	if strings.HasSuffix(target, "_test.go") || strings.Contains(target, "test") {
		result.Risk = "LOW"
		result.Confidence = 0.65
	}
	return result
}

// ollamaRequest is the JSON body for Ollama /api/generate.
type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

// ollamaResponse is the Ollama /api/generate response envelope.
type ollamaResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

func (g *Gateway) runOllama(ctx context.Context, target, errorContext string, flashbacks []string) (*DiagResult, error) {
	if g.ollamaURL == "" {
		return nil, fmt.Errorf("ollama_base_url not configured")
	}

	prompt := buildDiagPrompt(target, errorContext, flashbacks)
	reqBody, err := json.Marshal(ollamaRequest{
		Model:  g.cfg.OllamaModel,
		Prompt: prompt,
		Stream: false,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal ollama request: %w", err)
	}

	url := g.ollamaURL + "/api/generate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := sre.SafeHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama returned HTTP %d", resp.StatusCode)
	}

	var olResp ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&olResp); err != nil {
		return nil, fmt.Errorf("decode ollama response: %w", err)
	}

	// [PILAR-XXVII/243.E] Report token usage for internal inference.
	// Ollama models are zero-cost but we persist counts so operators see
	// actual token flow across tiers.
	inToks := len(prompt) / tokenCharsPerToken
	outToks := len(olResp.Response) / tokenCharsPerToken
	g.reportTokens(g.cfg.OllamaModel, "pkg/inference", "Diagnose", inToks, outToks)

	level := OLLAMA
	if len(flashbacks) > 0 {
		level = HYBRID
	}

	confidence := parseConfidence(olResp.Response)
	risk := parseRisk(olResp.Response)

	return &DiagResult{
		Level:      level,
		Confidence: confidence,
		Risk:       risk,
		Summary:    olResp.Response,
		Suggestion: extractSuggestion(olResp.Response),
	}, nil
}

func (g *Gateway) consumeTokens(n int) error {
	for {
		current := g.dailyTokens.Load()
		if int(current)+n > g.cfg.CloudTokenBudgetDaily {
			return fmt.Errorf("daily cloud token budget exhausted (%d/%d)", current, g.cfg.CloudTokenBudgetDaily)
		}
		if g.dailyTokens.CompareAndSwap(current, current+int32(n)) {
			return nil
		}
	}
}

func (g *Gateway) budgetRemaining() int {
	return g.cfg.CloudTokenBudgetDaily - int(g.dailyTokens.Load())
}

func (g *Gateway) maybeResetBudget() {
	t := today()
	if t != g.budgetResetD {
		g.dailyTokens.Store(0)
		g.budgetResetD = t
	}
}

func today() string {
	return time.Now().UTC().Format("2006-01-02")
}

// tokenCharsPerToken is the cheap heuristic (~4 chars = 1 token) used
// across the inference gateway for budget accounting. Must match the
// constant in cmd/neo-mcp/obs_wire.go so the two reports use the same
// units. [PILAR-XXVII/243.E]
const tokenCharsPerToken = 4

func estimateTokens(target, errorContext string, flashbacks []string) int {
	total := len(target)/4 + len(errorContext)/4
	for _, f := range flashbacks {
		total += len(f) / 4
	}
	if total < 100 {
		total = 100
	}
	return total
}

func buildDiagPrompt(target, errorContext string, flashbacks []string) string {
	var b strings.Builder
	b.WriteString("Analyze the following Go/TypeScript file for bugs:\n\nFile: ")
	b.WriteString(target)
	if errorContext != "" {
		b.WriteString("\n\nError context:\n")
		b.WriteString(errorContext)
	}
	if len(flashbacks) > 0 {
		b.WriteString("\n\nRelevant architectural context (RAG flashbacks):\n")
		for _, f := range flashbacks {
			b.WriteString("- ")
			b.WriteString(f)
			b.WriteString("\n")
		}
	}
	b.WriteString("\n\nProvide: 1) root cause, 2) confidence (0.0-1.0), 3) risk (LOW/MEDIUM/HIGH/CRITICAL), 4) suggested fix.")
	return b.String()
}

// parseConfidence extracts a float32 confidence value from the model's text response.
// Falls back to 0.60 if no explicit value is found.
func parseConfidence(response string) float32 {
	lower := strings.ToLower(response)
	if strings.Contains(lower, "confidence: 0.9") || strings.Contains(lower, "high confidence") {
		return 0.90
	}
	if strings.Contains(lower, "confidence: 0.8") {
		return 0.80
	}
	if strings.Contains(lower, "confidence: 0.7") {
		return 0.70
	}
	if strings.Contains(lower, "low confidence") || strings.Contains(lower, "confidence: 0.") {
		return 0.50
	}
	return 0.60
}

// parseRisk extracts risk level from the model response text.
func parseRisk(response string) string {
	upper := strings.ToUpper(response)
	switch {
	case strings.Contains(upper, "CRITICAL"):
		return "CRITICAL"
	case strings.Contains(upper, "HIGH"):
		return "HIGH"
	case strings.Contains(upper, "MEDIUM"):
		return "MEDIUM"
	default:
		return "LOW"
	}
}

func extractSuggestion(response string) string {
	// Find lines starting with common suggestion markers.
	for line := range strings.SplitSeq(response, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "4)") || strings.HasPrefix(l, "Fix:") || strings.HasPrefix(l, "Suggestion:") {
			return strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(l, "4)"), "Fix:"), "Suggestion:")
		}
	}
	return ""
}

// DebtEntry is a structured record written to the debt backlog file on surrender. [SRE-51.1/51.2]
type DebtEntry struct {
	Timestamp     string  `json:"timestamp"`
	MutationPath  string  `json:"mutation_path"`
	FailedLevel   string  `json:"failed_level"`
	Attempts      int     `json:"attempts"`
	LastError     string  `json:"last_error"`
	ThermalWatts  float64 `json:"thermal_watts,omitempty"` // [SRE-51.2] causal snapshot
	HeapMB        float64 `json:"heap_mb,omitempty"`
	CausalContext string  `json:"causal_context,omitempty"` // summary of flashbacks used
}

// SurrenderToDebt appends a DebtEntry to the configured debt backlog file. [SRE-51.1]
// Called when local inference fails after cfg.SurrenderAfter attempts.
func (g *Gateway) SurrenderToDebt(mutationPath, lastError string, attempts int, thermal, heapMB float64, causalCtx string) error {
	debtFile := g.cfg.DebtFile
	if debtFile == "" {
		debtFile = "technical_debt_backlog.md"
	}

	entry := DebtEntry{
		Timestamp:     time.Now().Format(time.RFC3339),
		MutationPath:  mutationPath,
		FailedLevel:   "OLLAMA/HYBRID",
		Attempts:      attempts,
		LastError:     lastError,
		ThermalWatts:  thermal,
		HeapMB:        heapMB,
		CausalContext: causalCtx,
	}

	// Format as Markdown task with JSON payload.
	line := fmt.Sprintf("\n- [ ] **SURRENDER** `%s` @ %s — failed after %d attempts\n  ```json\n  %s\n  ```\n",
		mutationPath, entry.Timestamp, attempts, marshalEntry(entry))

	f, err := os.OpenFile(debtFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644) //nolint:gosec // G304-WORKSPACE-CANON
	if err != nil {
		return fmt.Errorf("surrender: cannot open debt file %s: %w", debtFile, err)
	}
	defer f.Close()

	if _, err := f.WriteString(line); err != nil {
		return fmt.Errorf("surrender: write error: %w", err)
	}
	log.Printf("[SRE-51] Surrendered mutation '%s' to debt file %s (attempts=%d)", mutationPath, debtFile, attempts)
	return nil
}

func marshalEntry(e DebtEntry) string {
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Sprintf(`{"error": %q}`, err.Error())
	}
	return string(b)
}
