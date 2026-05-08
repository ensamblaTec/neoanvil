package main

import (
	"strings"
	"testing"
	"time"
)

// TestCompactNexusDebtPrefix verifies the compact-line badge rendering for the
// three cases: no events, events without P0, events with P0. [352.A]
func TestCompactNexusDebtPrefix(t *testing.T) {
	cases := []struct {
		name    string
		events  []nexusDebtBriefEntry
		wantStr string
	}{
		{"empty", nil, ""},
		{"two_p1_only", []nexusDebtBriefEntry{{Priority: "P1"}, {Priority: "P2"}}, "⚠️ NEXUS-DEBT:2 | "},
		{"one_p0_one_p1", []nexusDebtBriefEntry{{Priority: "P0"}, {Priority: "P1"}}, "⚠️ NEXUS-DEBT:2 P0:1 | "},
		{"only_p0", []nexusDebtBriefEntry{{Priority: "P0"}}, "⚠️ NEXUS-DEBT:1 P0:1 | "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &briefingData{nexusDebtEvents: c.events}
			got := compactNexusDebtPrefix(d)
			if got != c.wantStr {
				t.Errorf("got %q, want %q", got, c.wantStr)
			}
		})
	}
}

// TestAppendNexusDebtSection verifies the full-BRIEFING block renders the
// recommended remediation and resolve hint for each event. [352.A]
func TestAppendNexusDebtSection(t *testing.T) {
	detected := time.Date(2026, 4, 24, 14, 32, 15, 0, time.UTC)
	d := briefingData{nexusDebtEvents: []nexusDebtBriefEntry{
		{
			ID:          "2026-04-24-a1b2",
			Priority:    "P0",
			Title:       "strategos BoltDB lock held by zombie PID=77184",
			Detected:    detected,
			Recommended: "lsof +D .neo/db/ | kill zombie",
		},
	}}
	var sb strings.Builder
	appendNexusDebtSection(&sb, d)
	out := sb.String()

	for _, want := range []string{
		"⚠️ Nexus-Level Debt Affecting This Workspace",
		"2026-04-24-a1b2",
		"strategos BoltDB lock",
		"lsof +D .neo/db/",
		`neo_debt(scope:"nexus", action:"resolve"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

// TestAppendNexusDebtSection_EmptyIsSilent verifies zero events → zero output. [352.A]
func TestAppendNexusDebtSection_EmptyIsSilent(t *testing.T) {
	var sb strings.Builder
	appendNexusDebtSection(&sb, briefingData{})
	if sb.String() != "" {
		t.Errorf("expected empty output, got %q", sb.String())
	}
}

// TestCompactLineWithNexusDebt verifies the prefix precedes the Mode segment. [352.A]
func TestCompactLineWithNexusDebt(t *testing.T) {
	d := &briefingData{
		serverMode:      "pair",
		phaseName:       "TEST",
		planOpen:        5,
		planClosed:      10,
		heapMB:          100,
		ragCoverage:     100,
		nexusDebtEvents: []nexusDebtBriefEntry{{Priority: "P0"}, {Priority: "P1"}},
	}
	buildBriefingCompactLine(d)
	if !strings.HasPrefix(d.compactLine, "⚠️ NEXUS-DEBT:2 P0:1 | Mode: pair") {
		t.Errorf("compact line missing Nexus prefix: %q", d.compactLine)
	}
}
