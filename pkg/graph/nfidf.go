package graph

import "math"

// ApplyHubDampening applies a logarithmic dampening to penalize AST nodes
// with high inbound degree (God-Objects) and preserve business logic scores.
// Uses formula: baseScore / log2(2.0 + inboundDegree)
func ApplyHubDampening(baseScore float64, inboundDegree int) float64 {
	divisor := math.Log2(2.0 + float64(inboundDegree))
	return baseScore / divisor
}
