// pkg/shadow/comparator.go — Response comparison engine. [SRE-92.B.1]
//
// Compares real vs shadow responses: status code, body hash, latency delta.
// Produces a DiffReport with verdict (pass/divergent/timeout).
package shadow

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"time"
)

// Response captures the essential fields of an HTTP response for comparison.
type Response struct {
	Status  int
	Body    []byte
	Latency time.Duration
}

// DiffReport is the result of comparing a real response to its shadow twin.
type DiffReport struct {
	Divergent      bool   `json:"divergent"`
	Verdict        string `json:"verdict"` // "pass", "divergent", "timeout"
	Reason         string `json:"reason"`
	StatusMismatch bool   `json:"status_mismatch"`
	RealStatus     int    `json:"real_status"`
	ShadowStatus   int    `json:"shadow_status"`
	BodyMatch      bool   `json:"body_match"`
	RealBodyHash   string `json:"real_body_hash"`
	ShadowBodyHash string `json:"shadow_body_hash"`
	LatencyDeltaMs int64  `json:"latency_delta_ms"`
	BodyDiff       string `json:"body_diff,omitempty"` // first 500 bytes of diff
}

// CompareResponses compares a real response with a shadow response. [SRE-92.B.1]
// diffThresholdMs controls the latency delta tolerance before flagging divergence.
func CompareResponses(real, shadow Response, diffThresholdMs int) DiffReport {
	bodyMatch := bytes.Equal(real.Body, shadow.Body)
	// Compute hashes only for the report (lazy — only when bodies differ).
	var realHash, shadowHash string
	if !bodyMatch {
		realHash = fmt.Sprintf("%x", sha256.Sum256(real.Body))
		shadowHash = fmt.Sprintf("%x", sha256.Sum256(shadow.Body))
	}

	report := DiffReport{
		RealStatus:     real.Status,
		ShadowStatus:   shadow.Status,
		RealBodyHash:   realHash,
		ShadowBodyHash: shadowHash,
		BodyMatch:      bodyMatch,
		StatusMismatch: real.Status != shadow.Status,
		LatencyDeltaMs: shadow.Latency.Milliseconds() - real.Latency.Milliseconds(),
		Verdict:        "pass",
	}

	// Status code divergence — most critical signal.
	if report.StatusMismatch {
		report.Divergent = true
		report.Verdict = "divergent"
		report.Reason = fmt.Sprintf("status mismatch: real=%d shadow=%d", real.Status, shadow.Status)

		// Include body diff preview.
		if !report.BodyMatch {
			report.BodyDiff = bodyDiffPreview(real.Body, shadow.Body)
		}
		return report
	}

	// Body hash divergence.
	if !report.BodyMatch {
		report.Divergent = true
		report.Verdict = "divergent"
		report.Reason = "body content differs"
		report.BodyDiff = bodyDiffPreview(real.Body, shadow.Body)
		return report
	}

	// Latency divergence beyond threshold.
	if diffThresholdMs > 0 && abs(report.LatencyDeltaMs) > int64(diffThresholdMs) {
		report.Divergent = true
		report.Verdict = "divergent"
		report.Reason = fmt.Sprintf("latency delta %dms exceeds threshold %dms", abs(report.LatencyDeltaMs), diffThresholdMs)
		return report
	}

	return report
}

// bodyDiffPreview returns the first 500 bytes of each body for quick inspection.
func bodyDiffPreview(real, shadow []byte) string {
	const maxPreview = 500
	realPrev := truncate(real, maxPreview)
	shadowPrev := truncate(shadow, maxPreview)
	return fmt.Sprintf("real: %s\nshadow: %s", realPrev, shadowPrev)
}

func truncate(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
