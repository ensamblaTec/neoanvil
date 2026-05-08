package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/knowledge"
)

// TestHandleInbox_NoStore — error when project federation not active. [331.C]
func TestHandleInbox_NoStore(t *testing.T) {
	tool := &RadarTool{workspace: t.TempDir()}
	_, err := tool.handleInbox(context.Background(), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "project federation not active") {
		t.Errorf("want project federation error, got %v", err)
	}
}

// TestHandleInbox_ListEmpty — no entries → "No entries." marker. [331.C]
func TestHandleInbox_ListEmpty(t *testing.T) {
	tmp := t.TempDir()
	ks, _ := knowledge.Open(tmp + "/k.db")
	defer ks.Close()
	tool := &RadarTool{workspace: tmp, knowledgeStore: ks}
	out, err := tool.handleInbox(context.Background(), map[string]any{})
	// resolveWorkspaceID may fail for tempdir — skip if so.
	if err != nil && strings.Contains(err.Error(), "cannot resolve current workspace ID") {
		t.Skip("workspace not registered — expected on tempdir")
	}
	if err != nil {
		t.Fatalf("handleInbox: %v", err)
	}
	text := extractMCPText(t, out)
	if !strings.Contains(text, "Inbox") {
		t.Errorf("missing Inbox header in output: %q", text)
	}
}

// TestHandleInbox_FetchMode — key arg → renders entry + marks read. [331.C]
func TestHandleInbox_FetchMode(t *testing.T) {
	tmp := t.TempDir()
	ks, _ := knowledge.Open(tmp + "/k.db")
	defer ks.Close()
	key := "to-any-ws-audit-test"
	_ = ks.PutInbox("peer-sender", key, "Important message body.", "urgent", 0)

	tool := &RadarTool{workspace: tmp, knowledgeStore: ks}
	out, err := tool.handleInbox(context.Background(), map[string]any{"key": key})
	if err != nil {
		t.Fatalf("handleInbox: %v", err)
	}
	text := extractMCPText(t, out)
	for _, want := range []string{"peer-sender", "urgent", "Important message body."} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q: %s", want, text)
		}
	}
	// Entry should be marked read.
	got, _ := ks.Get(knowledge.NSInbox, key)
	if got.ReadAt == 0 {
		t.Error("entry should be marked read after fetch")
	}
}

// TestHandleInbox_FetchInvalidKey — bad key format rejected. [331.C]
func TestHandleInbox_FetchInvalidKey(t *testing.T) {
	tmp := t.TempDir()
	ks, _ := knowledge.Open(tmp + "/k.db")
	defer ks.Close()
	tool := &RadarTool{workspace: tmp, knowledgeStore: ks}
	_, err := tool.handleInbox(context.Background(), map[string]any{"key": "not-an-inbox-key"})
	if err == nil {
		t.Error("want invalid-key error")
	}
}

// TestFilterInboxUrgent — only urgent entries survive. [331.C]
func TestFilterInboxUrgent(t *testing.T) {
	entries := []knowledge.KnowledgeEntry{
		{Key: "a", Priority: "urgent"},
		{Key: "b", Priority: "normal"},
		{Key: "c", Priority: "urgent"},
		{Key: "d", Priority: "low"},
	}
	got := filterInboxUrgent(entries)
	if len(got) != 2 {
		t.Fatalf("want 2 urgent, got %d", len(got))
	}
	for _, e := range got {
		if e.Priority != "urgent" {
			t.Errorf("non-urgent entry leaked: %+v", e)
		}
	}
}

// TestFormatInboxAge — unit labels correct. [331.C]
func TestFormatInboxAge(t *testing.T) {
	cases := map[int64]string{
		5:      "5s",
		59:     "59s",
		60:     "1m",
		3599:   "59m",
		3600:   "1h",
		86399:  "23h",
		86400:  "1d",
		172800: "2d",
	}
	for in, want := range cases {
		if got := formatInboxAge(in); got != want {
			t.Errorf("formatInboxAge(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestInboxPreview — truncation + escape. [331.C]
func TestInboxPreview(t *testing.T) {
	short := inboxPreview("short body", 20)
	if short != "short body" {
		t.Errorf("short: got %q", short)
	}
	long := inboxPreview("a long body that exceeds the preview max length", 20)
	if !strings.HasSuffix(long, "…") {
		t.Errorf("long should end with ellipsis: %q", long)
	}
	pipe := inboxPreview("with|pipes", 20)
	if !strings.Contains(pipe, `\|`) {
		t.Errorf("pipes should be escaped: %q", pipe)
	}
	multiline := inboxPreview("line1\nline2", 20)
	if strings.Contains(multiline, "\n") {
		t.Errorf("newlines should be replaced: %q", multiline)
	}
}

// extractMCPText pulls the text content from an MCP envelope.
func extractMCPText(t *testing.T, out any) string {
	t.Helper()
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", out)
	}
	content, _ := m["content"].([]map[string]any)
	if len(content) == 0 {
		t.Fatalf("empty content: %v", m)
	}
	txt, _ := content[0]["text"].(string)
	return txt
}
