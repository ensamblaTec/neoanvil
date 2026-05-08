// Package deepseek — triage.go
// Second-pass triage helper (ÉPICA 151.C).
//
// TriageAuditFindings sends a follow-up call on an existing audit thread
// asking DS to re-rank findings by its own confidence (independently of the
// severity field it emitted in the first pass), classify mechanical_trace
// quality, and group findings for promotion vs deferral.
//
// Promoted findings (confidence >0.7, strong trace) surface immediately.
// Deferred findings (confidence <0.4 OR circular trace) are tagged
// [needs_verify] with a thread_id reference for operator follow-up.
//
// The caller is responsible for persisting deferred findings to
// technical_debt.md or an equivalent store.
package deepseek

import (
	"context"
	"fmt"
	"strings"
)

// TriageResult is the structured output of a triage pass.
type TriageResult struct {
	// PromotedIDs holds the finding identifiers (as written by DS in the
	// original audit, e.g. "VULN-001") that are high-confidence.
	PromotedIDs []string
	// DeferredIDs holds findings that need operator review before acting.
	DeferredIDs []string
	// RawText is the full triage narrative from DS, for logging / display.
	RawText      string
	InputTokens  int
	OutputTokens int
}

const triageMetaPrompt = `You are a security finding triage assistant.
Given the findings from the previous audit in this thread, perform a
second-pass analysis:

1. RE-RANK by your own confidence (0.0–1.0), independently of the severity
   you reported earlier. Confidence reflects how certain you are that the
   finding describes a REAL exploitable flaw (not a theoretical edge case).

2. CLASSIFY mechanical_trace quality for each finding:
   - "strong"   — the trace has no assumptions; every step is verified in
                  the shown code.
   - "weak"     — the trace requires an unstated assumption about caller
                  behavior or runtime environment.
   - "circular" — the trace restates the finding rather than explaining
                  HOW the impact is reached.

3. GROUP findings:
   - PROMOTE (confidence > 0.7 AND trace = strong): list finding IDs.
   - DEFER   (confidence < 0.4 OR trace = circular): list finding IDs,
             append tag [needs_verify].
   - REVIEW  (everything else): list finding IDs for operator judgment.

Format your response as:

PROMOTED: <comma-separated IDs or "none">
DEFERRED: <comma-separated IDs or "none">
REVIEW: <comma-separated IDs or "none">

RATIONALE:
<brief explanation per finding, one line each>`

// TriageAuditFindings runs a second-pass triage call on the given thread.
// threadID must be the ThreadID from the preceding red_team_audit response.
// Returns TriageResult; on error, PromotedIDs and DeferredIDs are nil and
// the error is surfaced so callers can decide whether to fall through.
func (c *Client) TriageAuditFindings(ctx context.Context, threadID string) (*TriageResult, error) {
	if threadID == "" {
		return nil, fmt.Errorf("triage: threadID is required (must continue the audit thread)")
	}
	req := CallRequest{
		Action:   "red_team_audit", // threaded continuation
		ThreadID: threadID,
		Prompt:   "Perform the triage pass as instructed in the system prompt.",
		SystemMsg: triageMetaPrompt,
	}
	resp, err := c.Call(ctx, req)
	if err != nil || resp == nil {
		return nil, err
	}
	return parseTriageResponse(resp), nil
}

// parseTriageResponse extracts PROMOTED / DEFERRED / REVIEW lines.
func parseTriageResponse(resp *CallResponse) *TriageResult {
	tr := &TriageResult{
		RawText:      resp.Text,
		InputTokens:  resp.InputTokens,
		OutputTokens: resp.OutputTokens,
	}
	for line := range strings.SplitSeq(resp.Text, "\n") {
		upper := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(upper, "PROMOTED:"):
			tr.PromotedIDs = parseIDList(line, "PROMOTED:")
		case strings.HasPrefix(upper, "DEFERRED:"):
			tr.DeferredIDs = parseIDList(line, "DEFERRED:")
		}
	}
	return tr
}

// parseIDList splits "LABEL: ID1, ID2, none" into a slice, returns nil when
// the value is "none" or empty.
func parseIDList(line, prefix string) []string {
	idx := strings.Index(strings.ToUpper(line), strings.ToUpper(prefix))
	if idx < 0 {
		return nil
	}
	raw := strings.TrimSpace(line[idx+len(prefix):])
	if raw == "" || strings.EqualFold(raw, "none") {
		return nil
	}
	var ids []string
	for part := range strings.SplitSeq(raw, ",") {
		id := strings.TrimSpace(part)
		if id != "" && !strings.EqualFold(id, "none") {
			ids = append(ids, id)
		}
	}
	return ids
}
