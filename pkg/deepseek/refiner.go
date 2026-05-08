// Package deepseek — refiner.go
// Pre-flight prompt refinement (ÉPICA 151.A).
//
// RefineAuditPrompt sends the operator's raw audit prompt to DS-flash with a
// fixed meta-prompt asking it to enrich the prompt with: explicit threat
// model, invariants to verify, severity floor, and output schema expectations.
// The returned refined prompt is a DRAFT — callers (Claude) MUST validate and
// may accept, modify, or discard it before forwarding to the actual red_team
// audit call.
//
// Cost: ~$0.0001/call (≈200 in + 200 out tokens on flash).
// Activation: callers opt-in by setting refine:true (default false).
package deepseek

import (
	"context"
	"strings"
)

const refineMetaPrompt = `You are a security review meta-prompter.
Your task: improve the audit prompt below so that the downstream code-reviewer
produces higher-signal, lower-hallucination findings.

Enrich the prompt by inserting:
1. THREAT MODEL — who is the adversary, what asset are they attacking, what
   is the trust boundary crossing that would let them succeed.
2. INVARIANTS — 2-4 concrete correctness invariants the reviewer MUST check
   (e.g. "every secret MUST be zeroed after use", "no unbounded allocation
   in request handlers").
3. SEVERITY FLOOR — state explicitly: "Only report findings of severity ≥6.
   Drop speculative findings whose attack vector requires simultaneous
   multi-step failures with no mechanical trace."
4. OUTPUT SCHEMA — "For each finding include: severity (1-10), file, line,
   attack_vector (one sentence), mechanical_trace (step-by-step HOW impact
   is reached — if you cannot trace it mechanically, DROP the finding)."

Return ONLY the refined prompt text. Do NOT perform the audit.
Do NOT add preamble or explanation. Do NOT wrap in Markdown code fences.`

// RefineAuditPrompt calls DS-flash to enrich the raw audit prompt.
// Returns the refined prompt text and the number of tokens consumed.
// On any error the original prompt is returned unchanged so the caller
// can fall through to the audit without interruption.
func (c *Client) RefineAuditPrompt(ctx context.Context, rawPrompt string) (refined string, inputTok int, outputTok int, err error) {
	req := CallRequest{
		Action:    "distill_payload", // ephemeral, cheap flash call
		SystemMsg: refineMetaPrompt,
		Prompt:    rawPrompt,
		// Flash default; no per-call model override needed.
	}
	resp, callErr := c.Call(ctx, req)
	if callErr != nil || resp == nil {
		return rawPrompt, 0, 0, callErr
	}
	text := strings.TrimSpace(resp.Text)
	if text == "" {
		return rawPrompt, resp.InputTokens, resp.OutputTokens, nil
	}
	return text, resp.InputTokens, resp.OutputTokens, nil
}
