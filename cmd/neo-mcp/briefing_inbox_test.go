package main

import (
	"strings"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/knowledge"
)

// TestCompactInboxSegment_Empty — no unread → empty string. [331.B]
func TestCompactInboxSegment_Empty(t *testing.T) {
	d := &briefingData{unreadInboxCount: 0}
	got := compactInboxSegment(d)
	if got != "" {
		t.Errorf("want empty, got %q", got)
	}
}

// TestCompactInboxSegment_WithSenders — renders senders list. [331.B]
func TestCompactInboxSegment_WithSenders(t *testing.T) {
	d := &briefingData{
		unreadInboxCount:   3,
		unreadInboxSenders: []string{"strategos-32492", "strategosia-frontend-82899"},
	}
	got := compactInboxSegment(d)
	if !strings.Contains(got, "📬 inbox: 3") {
		t.Errorf("want count visible, got %q", got)
	}
	if !strings.Contains(got, "strategos-32492") {
		t.Errorf("want sender 1 visible, got %q", got)
	}
	if !strings.Contains(got, "strategosia-frontend-82899") {
		t.Errorf("want sender 2 visible, got %q", got)
	}
}

// TestCompactInboxSegment_NoSendersStored — still render the count. [331.B]
func TestCompactInboxSegment_NoSendersStored(t *testing.T) {
	d := &briefingData{unreadInboxCount: 2} // empty senders list (e.g. all From=="")
	got := compactInboxSegment(d)
	if !strings.Contains(got, "📬 inbox: 2") {
		t.Errorf("want count visible, got %q", got)
	}
	if strings.Contains(got, "from:") {
		t.Errorf("should not render 'from:' with empty senders, got %q", got)
	}
}

// TestGatherInboxMetrics_NoStore — no knowledgeStore wired → silent no-op. [331.B]
func TestGatherInboxMetrics_NoStore(t *testing.T) {
	tool := &RadarTool{workspace: "/tmp/not-a-real-ws"}
	d := &briefingData{}
	gatherInboxMetrics(tool, d)
	if d.unreadInboxCount != 0 {
		t.Error("no KnowledgeStore wired → count should stay 0")
	}
}

// TestListInboxForKnowledgeStore_E2E — seeds 2 unread + 1 read + 1 other-target
// and validates the KnowledgeStore.ListInboxFor filter used by
// gatherInboxMetrics. This tests the storage-layer side of 331.B — the
// briefing wrapper is covered by the unit tests above. [331.B]
func TestListInboxForKnowledgeStore_E2E(t *testing.T) {
	tmp := t.TempDir()
	ks, err := knowledge.Open(tmp + "/k.db")
	if err != nil {
		t.Fatalf("ks open: %v", err)
	}
	defer ks.Close()

	const myWS = "test-ws-42"
	const otherWS = "peer-ws-99"
	// Seed: 2 unread targeting me, 1 already-read targeting me, 1 targeting other-ws.
	_ = ks.PutInbox("peer-a", "to-"+myWS+"-topic1", "body1", "", 0)
	_ = ks.PutInbox("peer-b", "to-"+myWS+"-topic2", "body2", "", 0)
	_ = ks.PutInbox("peer-a", "to-"+myWS+"-topic3", "body3", "", 0)
	_ = ks.MarkInboxRead("to-" + myWS + "-topic3")
	_ = ks.PutInbox("peer-c", "to-"+otherWS+"-topic1", "body4", "", 0)

	// My unread list must have exactly 2 entries + 2 distinct senders.
	entries, err := ks.ListInboxFor(myWS, true)
	if err != nil {
		t.Fatalf("ListInboxFor: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 unread for %s, got %d", myWS, len(entries))
	}
	senders := map[string]bool{}
	for _, e := range entries {
		senders[e.From] = true
	}
	if len(senders) != 2 {
		t.Errorf("want 2 distinct senders, got %d (%v)", len(senders), senders)
	}

	// Full list (including read) targeting me: 3 entries.
	all, _ := ks.ListInboxFor(myWS, false)
	if len(all) != 3 {
		t.Errorf("want 3 total for %s, got %d", myWS, len(all))
	}

	// other-ws sees its single entry.
	otherEntries, _ := ks.ListInboxFor(otherWS, false)
	if len(otherEntries) != 1 {
		t.Errorf("want 1 entry for %s, got %d", otherWS, len(otherEntries))
	}
}
