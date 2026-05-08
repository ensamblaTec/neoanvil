// Package widgets renders small ASCII primitives reused by every view.
// [PILAR-XXVII/246.L-N]
package widgets

import (
	"fmt"
	"strings"
)

// Gauge renders `[██████░░░░] 60%` at the requested width. Pct is
// clamped to [0,100]. Width is the inner bar — total on-screen length
// is width+7 (brackets + space + 3-digit percent + % sign).
func Gauge(pct float64, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	if width < 4 {
		width = 4
	}
	filled := int(pct * float64(width) / 100.0)
	if filled > width {
		filled = width
	}
	return fmt.Sprintf("[%s%s] %3d%%",
		strings.Repeat("█", filled),
		strings.Repeat("░", width-filled),
		int(pct))
}

// Sparkline renders a value series with unicode blocks ▁▂▃▄▅▆▇█.
// Returns empty string when data is nil.
func Sparkline(data []float64) string {
	if len(data) == 0 {
		return ""
	}
	runes := []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}
	maxv := data[0]
	minv := data[0]
	for _, v := range data[1:] {
		if v > maxv {
			maxv = v
		}
		if v < minv {
			minv = v
		}
	}
	span := maxv - minv
	if span == 0 {
		// Flat line — all at middle tier.
		return strings.Repeat(string(runes[3]), len(data))
	}
	var b strings.Builder
	b.Grow(len(data))
	for _, v := range data {
		idx := int((v - minv) / span * float64(len(runes)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(runes) {
			idx = len(runes) - 1
		}
		b.WriteRune(runes[idx])
	}
	return b.String()
}
