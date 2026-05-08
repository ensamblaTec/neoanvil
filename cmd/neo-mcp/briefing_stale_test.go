package main

import (
	"strings"
	"testing"
)

// Tests for the binary-stale-vs-HEAD banner in BRIEFING. [162.D → 166.B]
//
// These drive buildBriefingCompactLine directly with a hand-built
// briefingData so the test does not need a live BoltDB / HNSW / CPG.
// The prefix contract:
//   - binaryStaleAlert=true  → "⚠️ BINARY_STALE:<N>m | " leads the line
//                               and binAgeStr is suppressed from the tail
//   - binaryStaleAlert=false → neither prefix nor suffix mention "STALE"

// newStaleTestData builds the minimal shape the compact-line formatter
// reads. Keeping this local avoids coupling to gatherBriefingData which
// hits disk and BoltDB.
func newStaleTestData(stale bool, minutes int) *briefingData {
	d := &briefingData{
		serverMode:    "pair",
		phaseName:     "test",
		planOpen:      0,
		planClosed:    100,
		openTaskLines: nil,
		heapMB:        150,
		recvBytes:     0,
		sentBytes:     0,
		ragCoverage:   100,
		binAgeStr:     " | binary_age=1m",
	}
	if stale {
		d.binaryStaleAlert = true
		d.staleMinutes = minutes
		d.binAgeStr = " | binary_stale_vs_HEAD=" + itoa(minutes) + "m"
	}
	return d
}

// itoa avoids strconv here because buildBriefingCompactLine is already
// imported from the main package — keeping dependencies minimal.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestBriefingBinaryStaleAlert_Compact(t *testing.T) {
	d := newStaleTestData(true, 42)
	buildBriefingCompactLine(d)
	if !strings.HasPrefix(d.compactLine, "⚠️ BINARY_STALE:42m | ") {
		t.Fatalf("expected stale banner prefix, got: %s", d.compactLine)
	}
	if strings.Contains(d.compactLine, "binary_stale_vs_HEAD") {
		t.Fatalf("binAgeStr suffix should be suppressed when stale banner is shown, got: %s", d.compactLine)
	}
}

func TestBriefingBinaryStaleAlert_Full(t *testing.T) {
	// The "full" aspect: binaryStaleAlert composes cleanly with
	// resumeWarning, ragCoverage warning, and incTotal counter without
	// clobbering them. We assert order: BINARY_STALE prefix first, then
	// RESUME, then the body.
	d := newStaleTestData(true, 15)
	d.resumeWarning = true
	d.ragCoverage = 60 // triggers low_rag_coverage warn tail
	d.incTotal = 3
	d.incIndexed = 2
	buildBriefingCompactLine(d)

	idxStale := strings.Index(d.compactLine, "⚠️ BINARY_STALE:15m | ")
	idxResume := strings.Index(d.compactLine, "⚠️ RESUME | ")
	idxMode := strings.Index(d.compactLine, "Mode: pair")

	if idxStale != 0 {
		t.Fatalf("stale banner must lead the line, got idx=%d line=%s", idxStale, d.compactLine)
	}
	if idxResume <= idxStale {
		t.Fatalf("RESUME banner must follow stale banner, line=%s", d.compactLine)
	}
	if idxMode <= idxResume {
		t.Fatalf("Mode body must follow both banners, line=%s", d.compactLine)
	}
	if !strings.Contains(d.compactLine, "low_rag_coverage=60%") {
		t.Fatalf("ragCoverage warning lost when stale banner is present, got: %s", d.compactLine)
	}
	if !strings.Contains(d.compactLine, "INC-IDX: 2/3") {
		t.Fatalf("INC-IDX counter lost when stale banner is present, got: %s", d.compactLine)
	}
}

func TestBriefingBinaryStaleAlert_Absent_NoBanner(t *testing.T) {
	d := newStaleTestData(false, 0)
	buildBriefingCompactLine(d)
	if strings.Contains(d.compactLine, "BINARY_STALE") {
		t.Fatalf("stale banner must be absent when binaryStaleAlert=false, got: %s", d.compactLine)
	}
	if !strings.Contains(d.compactLine, "binary_age=1m") {
		t.Fatalf("binAgeStr suffix should be preserved when no stale alert, got: %s", d.compactLine)
	}
}
